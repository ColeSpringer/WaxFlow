// Package format identifies containers and opens them as decodable media:
// a bounded magic-byte sniff over an ordered driver table (extension hints
// only break ties), then demuxer plus decoder wired into a Media that
// reads planar PCM chunks and seeks sample-exact.
//
// The driver table is capability-gated: codecs and containers register
// here only once they actually work, so probe and /caps never claim
// what does not work.
package format

import (
	"fmt"
	"io"
	"strings"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// Options configures probing and opening.
type Options struct {
	// Strict turns tolerated input damage into errors (conformance tests,
	// `waxflow probe --strict`). Playback paths stay tolerant.
	Strict bool
}

// Info is a probe result.
type Info struct {
	// Container is the identified container name ("wav", "aiff").
	Container string
	// Tracks are the container's audio tracks.
	Tracks []container.Track
	// Chapters are the source's chapter markers, nil when the container
	// carries none or cannot hold them.
	//
	// They are read here rather than only through a metadata mapper
	// because the parser is in the container package already: the mapper
	// route reaches a transcode only via the CLI's injected mapper, so a
	// server embedded by anyone else silently dropped them. A capability
	// gate the demuxer already satisfies costs nothing to ask.
	Chapters []container.Chapter
	// Warnings describe input damage the tolerant parser worked around.
	Warnings []string
}

// Default returns the container's designated default track, or the first
// track. Probe and Open never return an Info without tracks; Default
// panics on a hand-built empty one, like any out-of-range index.
func (i *Info) Default() container.Track {
	for _, t := range i.Tracks {
		if t.Default {
			return t
		}
	}
	return i.Tracks[0]
}

// Media is an opened source: probe info plus sample-exact PCM access.
//
// ReadChunk fills dst (whose format must equal the default track's) to
// capacity, stamps dst.Pos with the first frame's position in the source
// timeline and dst.Discont on the first chunk after a seek (ADR-0006),
// and returns io.EOF at end of stream. Seeks land sample-exact: the
// demuxer positions on a sync point at or before the target and Media
// pre-rolls the remainder internally, decoding and discarding.
type Media interface {
	Info() *Info
	ReadChunk(dst *audio.Buffer) error
	SeekSample(target int64) (landed int64, err error)
	Close() error
}

// Composite is implemented by a Media assembled from several sources rather
// than opened from one: a concatenated timeline. Like container.Indexer and
// container.Warner, it is an honest capability gate rather than a universal
// method, since a Media opened from a single file has no members to report
// and does not implement it.
//
// Info describes what the assembled stream delivers, which is a synthetic
// envelope: it names no member's codec, format, or length. A consumer keying
// its own cache on what the stream is made of needs those facts, so this
// hands back the members' own tracks.
type Composite interface {
	Members() []container.Track
}

// sniffLen is the hard upper bound on the probe read. The actual read is
// sized to what the registered drivers declare they need (12 bytes: fLaC
// and OggS need 4, RIFF and FORM need 12); deep-window formats joining
// later (ftyp scans, EBML, sync-word searches) raise it toward this cap.
const sniffLen = 64 * 1024

// maxSniffNeed is the largest head the current driver table uses.
var maxSniffNeed = func() int64 {
	need := 0
	for i := range drivers {
		need = max(need, drivers[i].need)
	}
	return int64(min(need, sniffLen))
}()

// Probe identifies src and returns its parsed headers. The hint is an
// optional file extension (with or without the dot) used only to pick a
// driver when no magic matches.
func Probe(src container.Source, hint string, opts *Options) (*Info, error) {
	src, d, err := resolve(src, hint)
	if err != nil {
		return nil, err
	}
	demux, err := d.open(src, opts)
	if err != nil {
		return nil, err
	}
	info := buildInfo(d.name, demux)
	if len(info.Tracks) == 0 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat, "format: no audio tracks")
	}
	return info, nil
}

// Open identifies src and wires demuxer and decoder into Media.
func Open(src container.Source, hint string, opts *Options) (Media, error) {
	src, d, err := resolve(src, hint)
	if err != nil {
		return nil, err
	}
	demux, err := d.open(src, opts)
	if err != nil {
		return nil, err
	}
	// newMedia rejects a trackless demux, so Open shares Probe's
	// guarantee: a returned Info always has at least one track.
	return newMedia(buildInfo(d.name, demux), demux)
}

// OpenDemuxer identifies src and opens its demuxer without wiring a decoder,
// for callers that move encoded packets rather than samples (the remux rung).
// Info is built exactly as Open's is, so the two cannot disagree about a
// container's tracks.
//
// The caller drives the demuxer directly and so inherits its contract: packets
// are borrowed, and ReadPacket returns the bare io.EOF sentinel at the end. A
// Demuxer has no Close; what needs closing is src, which the caller owns, as it
// does for Probe.
func OpenDemuxer(src container.Source, hint string, opts *Options) (container.Demuxer, *Info, error) {
	src, d, err := resolve(src, hint)
	if err != nil {
		return nil, nil, err
	}
	demux, err := d.open(src, opts)
	if err != nil {
		return nil, nil, err
	}
	// The trackless check is Probe's, restated rather than borrowed: Open gets
	// it from newMedia, and this path reaches neither. All three entry points
	// promise an Info with at least one track.
	info := buildInfo(d.name, demux)
	if len(info.Tracks) == 0 {
		return nil, nil, waxerr.New(waxerr.CodeUnsupportedFormat, "format: no audio tracks")
	}
	return demux, info, nil
}

// FromDemuxer wraps an already-opened demuxer into a Media, for sources that
// are assembled rather than sniffed from one byte stream. The HLS client builds
// a fragmented-MP4 demuxer over concatenated media segments behind an
// out-of-band init segment, which has no single sniffable Source for Open to
// resolve; name labels the synthetic container in Info. The decoder wiring,
// gapless trims, and seek support are identical to Open's.
func FromDemuxer(name string, demux container.Demuxer) (Media, error) {
	info := buildInfo(name, demux)
	if len(info.Tracks) == 0 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat, "format: no audio tracks")
	}
	return newMedia(info, demux)
}

func buildInfo(name string, demux container.Demuxer) *Info {
	info := &Info{Container: name, Tracks: demux.Tracks()}
	if c, ok := demux.(container.Chapterer); ok {
		info.Chapters = c.Chapters()
	}
	if w, ok := demux.(container.Warner); ok {
		for _, warn := range w.Warnings() {
			if warn.Offset >= 0 {
				info.Warnings = append(info.Warnings, fmt.Sprintf("%s (offset %d)", warn.Msg, warn.Offset))
			} else {
				info.Warnings = append(info.Warnings, warn.Msg)
			}
		}
	}
	return info
}

// resolve picks a driver: bounded sniff first (skipping a leading ID3v2
// tag if present), extension hint as the tiebreak for unrecognized magic.
func resolve(src container.Source, hint string) (container.Source, *driver, error) {
	// One read covers both the ID3v2 check and the sniff; only a present
	// tag forces a second read past it.
	head, err := readHead(src, max(maxSniffNeed, 10))
	if err != nil {
		return nil, nil, err
	}
	if skip := id3v2Size(head); skip > 0 && skip < src.Size() {
		src = sectionSource{src, skip}
		head, err = readHead(src, maxSniffNeed)
		if err != nil {
			return nil, nil, err
		}
	}
	for i := range drivers {
		if drivers[i].match(head) {
			return src, &drivers[i], nil
		}
	}
	if ext := strings.ToLower(strings.TrimPrefix(hint, ".")); ext != "" {
		for i := range drivers {
			for _, e := range drivers[i].exts {
				if e == ext {
					return src, &drivers[i], nil
				}
			}
		}
	}
	return src, nil, waxerr.New(waxerr.CodeUnsupportedFormat, "format: unrecognized input (no magic bytes matched)")
}

// readHead reads up to n leading bytes. Sources smaller than n yield a
// short head (that is what unrecognized-format errors are for), but a
// genuine read failure propagates as source-unreadable rather than being
// misclassified as an unsupported file.
func readHead(src container.Source, n int64) ([]byte, error) {
	if size := src.Size(); size < n {
		n = size
	}
	if n <= 0 {
		return nil, nil
	}
	head := make([]byte, n)
	got, err := src.ReadAt(head, 0)
	if got == len(head) || err == io.EOF {
		return head[:got], nil
	}
	return nil, waxerr.Wrap(waxerr.CodeSourceUnreadable, "format: reading file head", err)
}

// id3v2Size returns the total byte length of a leading ID3v2 tag, or 0.
// WAV and AIFF never carry one, but MP3 and ADTS sources routinely do, so
// the sniff table always looks past it.
func id3v2Size(head []byte) int64 {
	if len(head) < 10 || string(head[:3]) != "ID3" {
		return 0
	}
	for _, b := range head[6:10] {
		if b&0x80 != 0 {
			return 0 // not syncsafe: treat as absent rather than guess
		}
	}
	n := int64(head[6])<<21 | int64(head[7])<<14 | int64(head[8])<<7 | int64(head[9])
	n += 10
	if head[5]&0x10 != 0 {
		n += 10 // footer
	}
	return n
}

// sectionSource offsets a Source, hiding a leading tag from drivers.
type sectionSource struct {
	src container.Source
	off int64
}

func (s sectionSource) ReadAt(p []byte, off int64) (int, error) {
	return s.src.ReadAt(p, off+s.off)
}

func (s sectionSource) Size() int64 { return s.src.Size() - s.off }
