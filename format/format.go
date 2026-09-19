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
	// `waxflow probe --strict`).
	//
	// It also decides how much of a file is read. A tolerant Probe reads
	// headers, and so does every Open: the payload walks a demuxer defers stay
	// deferred (container.Walker: the MP3 frame index, bare or in a WAV or
	// AIFF-C; the ADTS frame index; a Matroska cluster walk). A strict Probe
	// finishes all of them, so its verdict covers the whole file at the cost
	// of reading it.
	//
	// No open reads a payload. A Matroska track's tail trim rides on the block
	// that carries it (container.Packet.Padding), so a read is gapless without
	// the total and only the number waits for Walk. Open and OpenDemuxer never
	// call Walk, so damage past the head of a deferred payload is found by the
	// read that reaches it.
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
	// Tags are the source's embedded tags under canonical uppercase keys
	// (TITLE, ARTIST, REPLAYGAIN_TRACK_GAIN, ...), nil when the container
	// carries none or cannot hold them.
	//
	// Read here for the same reason as Chapters, and it is the same gap:
	// the mapper route reaches a transcode only through an injected
	// mapper, so a server embedded by anyone else silently dropped every
	// tag a file plainly carried. Cover art is not here; see
	// container.Tagger.
	Tags map[string][]string
	// Warnings describe input damage the tolerant parser worked around.
	// Damage only: what this build did with a well-formed file is in Notes.
	Warnings []string
	// Notes describe what this build did with a file that is not damaged: a
	// stream it ignored, a list it capped, a timeline it rescaled, a band it
	// does not synthesize. Strict mode never refuses over one of these, which
	// is the whole reason they are not Warnings; a caller showing "input
	// damage" wants Warnings alone.
	Notes []string
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
//
// A read delivers the gapless stream on every format, whether or not the
// length is known: a container that states its trims per packet is trimmed as
// the packets arrive, and one that states a total is capped at it.
//
// Info's Warnings and Notes are live, and nothing else in it is: the two
// lists are current as of the last ReadChunk or SeekSample, on the goroutine
// that reads, because a demuxer whose walk is lazy finds damage where the
// read reaches it. A caller that wants the whole verdict reads or seeks to
// the end first and asks again; Tracks, Chapters and Tags are what opening
// found, until Walk settles the default track's length to what the payload
// actually holds. Probe's Info is a detached snapshot, complete under Strict.
type Media interface {
	Info() *Info
	ReadChunk(dst *audio.Buffer) error
	SeekSample(target int64) (landed int64, err error)
	Close() error
}

// Walker is the per-file view of container.Walker on a Media: whether the
// walk its demuxer defers has run, and running it. Every Media opened from a
// single source implements it, answering nil and true when the demuxer defers
// nothing, so a consumer that needs to know whether measuring the file costs
// a full scan asks the file rather than a method set. A type assertion would
// answer per format where the question is per file and per moment: the same
// WAV demuxer walks for an MP3 payload and not for PCM, the same Matroska
// demuxer walks for an advisory length and not for one it measured at open,
// and an MP3 whose sidecar index restored complete walks for nothing. A
// concatenated timeline and a slice do not implement it: a consumer opens
// the member files one at a time.
// A finished walk has measured the payload, and the Media's own Info carries
// that measurement from then on: the walk is observable on the Media that ran
// it, without a reopen.
type Walker interface {
	Walk() error
	Walked() bool
}

// LengthConfirmer is how a Media that is not a file confirms the length it
// reports: run whatever the source underneath it defers, re-derive this
// Media's own length from the result, and refuse when the confirmed source no
// longer covers what this Media promised.
//
// It exists because Walker answers a different question. Walker is the per-file
// view (see above), and a consumer reads it as "one file I can measure"; a
// wrapper that forwarded it would tell a job gate that a span is a file. So the
// two halves are split: Walker says what a file costs to measure, and this says
// that a Media can make its own declared length good. A run that is about to
// write a count into headers it cannot go back and patch asks for the second.
//
// A bounded span implements it and usually has nothing to do: its length is
// its own arithmetic (see waxflow.SpanTrack), so only an open-ended span of a
// source whose own total is a declaration takes the work. A concatenated
// timeline does not implement it at all, and that is a contract rather than an
// omission: waxflow.ConcatSource.Track makes the caller's declared length the
// timeline's own, and the run holds each member to it, so confirming members
// behind the caller's back would contradict what the timeline promises (see
// ADR-0009). Measure the members first.
type LengthConfirmer interface {
	ConfirmLength() error
}

// A wrapper around a Media forwards this only when what it wraps implements
// it, which is the opposite of the rule for Walker (see WalkMedia) and is not
// an oversight. Walker's forwarding is safe unconditionally because Walked
// answers true for a Media that defers nothing, so a wrapper over one reports
// "already walked" and nothing acts. This interface has no such answer: merely
// implementing it says there is confirming to do, and a run about to write a
// header reads that as "do not commit the count". A wrapper that forwarded it
// blindly would drop the projection from every source underneath it that had
// nothing to confirm. Nothing in this tree wraps a span today, so nothing
// forwards it; a wrapper that comes to needs a type assertion on what it wraps
// rather than a method that always answers.

// WalkMedia runs med's deferred walk, and nothing when it defers none.
// MediaWalked reports whether that walk has run, true when there is none to
// run. Together they are Walker asked of a Media that may or may not be one.
//
// They exist for the wrappers. Embedding the Media interface promotes no
// method outside it, so a wrapper that does not write these two by hand
// answers "nothing deferred" for a media that defers a whole-file walk, and
// every consumer downstream of it quietly gets the wrong answer: a job gate
// calls an expensive member cheap, a run commits a length nothing confirmed.
// The forwarding cannot be inherited, so it is at least written once.
func WalkMedia(med Media) error {
	if w, ok := med.(Walker); ok {
		return w.Walk()
	}
	return nil
}

func MediaWalked(med Media) bool {
	w, ok := med.(Walker)
	return !ok || w.Walked()
}

// MixedWidth is answered by a Media whose channels were conformed to its own
// width from different ones: a concatenated timeline whose members are not all
// as wide as the envelope. It is one bit rather than a member list because the
// question a consumer asks is a yes or no, and it is a property of the samples
// this Media delivers rather than of how it was assembled.
//
// The consumer is a channel conversion. dsp/mix normalizes each output row over
// every source column, so a fold applied to such a Media is not any member's
// own fold: a member placed into a wider envelope has silent columns that still
// count in the divisor, and the result is 1 to 6 dB under what that member
// folded alone would give. waxflow refuses the conversion rather than deliver
// it (see waxflow.ConcatOptions.Channels), and this is how it asks.
//
// Unlike Composite this is forwarded by a wrapper and by a slice: Composite's
// "what are the members of a span of a timeline" has no right answer, and this
// one does, since a slice of a mixed-width timeline is still mixed-width
// samples. Every wrapper that forwards Walked forwards this too.
type MixedWidth interface {
	MixedWidth() bool
}

// MediaMixedWidth asks MixedWidth of a Media that may not implement it. False
// is the answer for anything opened from one file, which has one width.
func MediaMixedWidth(med Media) bool {
	m, ok := med.(MixedWidth)
	return ok && m.MixedWidth()
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
	strict := opts != nil && opts.Strict
	demux, err := d.open(src, openOptions{strict: strict})
	if err != nil {
		return nil, err
	}
	// A strict verdict covers the whole file: the walk a demuxer defers is
	// finished before the snapshot is taken, so what it found is in it. That
	// includes the one a probe asked the driver to defer.
	if strict {
		if w, ok := demux.(container.Walker); ok {
			if err := w.Walk(); err != nil {
				return nil, err
			}
		}
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
	demux, err := d.open(src, openOptions{strict: opts != nil && opts.Strict})
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
	demux, err := d.open(src, openOptions{strict: opts != nil && opts.Strict})
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
	if t, ok := demux.(container.Tagger); ok {
		info.Tags = t.Tags()
	}
	if w, ok := demux.(container.Warner); ok {
		foldWarnings(info, w.Warnings())
	}
	return info
}

// RefreshWarnings refolds info's Warnings and Notes from demux, for a caller
// that drives a demuxer it opened through OpenDemuxer: the two lists are
// live on a Media (see Media), and this is the same refresh for the packet
// path, where damage past the head is found by the read that reaches it. A
// demuxer that records no warnings leaves info as it was.
func RefreshWarnings(info *Info, demux container.Demuxer) {
	if w, ok := demux.(container.Warner); ok {
		foldWarnings(info, w.Warnings())
	}
}

// foldWarnings sets info's Warnings and Notes from a demuxer's list, into
// fresh slices: an earlier holder of the old ones keeps them as they were.
// Media.Info calls it on every call, which is what makes the lists live.
func foldWarnings(info *Info, ws []container.Warning) {
	info.Warnings, info.Notes = nil, nil
	for _, warn := range ws {
		msg := warn.Msg
		if warn.Offset >= 0 {
			msg = fmt.Sprintf("%s (offset %d)", warn.Msg, warn.Offset)
		}
		if warn.Kind == container.Note {
			info.Notes = append(info.Notes, msg)
		} else {
			info.Warnings = append(info.Warnings, msg)
		}
	}
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
