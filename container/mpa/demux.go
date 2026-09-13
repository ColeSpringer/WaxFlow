package mpa

import (
	"fmt"
	"slices"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/id3"
	"github.com/colespringer/waxflow/container/internal/mpegframes"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ container.Demuxer = (*Demuxer)(nil)
	_ container.Seeker  = (*Demuxer)(nil)
	_ container.Warner  = (*Demuxer)(nil)
)

// Hostile-input caps (ADR-0005 invariants). The walk's own caps live in
// container/internal/mpegframes; these are the tag baggage around it.
const (
	// maxID3Tags bounds leading tag skipping (tags stack).
	maxID3Tags = 8
	// trailerScan is how far back trailing tag recognition looks.
	trailerScan = 64 << 10
	// maxWarnings caps the tolerated-damage list.
	maxWarnings = 64
)

// DemuxerOptions configures parsing.
type DemuxerOptions struct {
	// Strict turns tolerated damage (the Warnings list) into errors.
	Strict bool
}

// Demuxer reads one MP3 track from an elementary stream source.
type Demuxer struct {
	opts DemuxerOptions

	// walk is the frame index and the window it walks, pkts the packet
	// stream over it; this package owns the tags around them and the track
	// those frames describe.
	walk     *mpegframes.Walker
	pkts     *mpegframes.Reader
	track    container.Track
	warnings []container.Warning
}

// NewDemuxer parses the stream head (tags, the VBR metadata frame) and
// positions on the first audio frame. The returned Demuxer implements
// container.Seeker and container.Warner.
func NewDemuxer(src container.Source, opts *DemuxerOptions) (*Demuxer, error) {
	d := &Demuxer{}
	if opts != nil {
		d.opts = *opts
	}
	d.walk = mpegframes.New(src, src.Size(), mpegframes.Options{
		Prefix:  "mp3: ",
		Warn:    func(off int64, msg string) error { return d.warn(off, "%s", msg) },
		Trailer: d.recognizedTrailer,
	})
	if err := d.parse(); err != nil {
		return nil, err
	}
	return d, nil
}

// warn records tolerated damage, or fails in strict mode.
//
// The list is capped and deduplicated, as every tolerant demuxer in this tree
// caps its own: the frame walk reports one finding per damaged gap, and a
// payload of nothing but alternating frames and junk would otherwise grow a
// finding per frame for as long as the file runs.
func (d *Demuxer) warn(off int64, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if d.opts.Strict {
		return waxerr.Malformed("mp3: ", "%s (at offset %d)", msg, off)
	}
	w := container.Warning{Offset: off, Msg: msg, Kind: container.Damage}
	if len(d.warnings) < maxWarnings && !slices.Contains(d.warnings, w) {
		d.warnings = append(d.warnings, w)
	}
	return nil
}

func (d *Demuxer) parse() error {
	// Skip leading ID3v2 tags (stacked ones included).
	off := int64(0)
	for range maxID3Tags {
		head := d.walk.Bytes(off, 10)
		n := id3.Size(head)
		if n == 0 || off+n > d.walk.DataEnd() {
			break
		}
		off += n
	}

	tag, _, err := d.walk.Begin(off)
	if err != nil {
		return err
	}
	d.pkts = d.walk.Reader()

	f := d.walk.Header().PCMFormat()
	if err := f.Valid(); err != nil {
		return container.UnusableFormat("mp3", f, err)
	}

	samples, delay, padding := tag.Gapless(d.walk.SamplesPerFrame())
	d.track = container.Track{
		Codec:   codec.MP3,
		Fmt:     f,
		Samples: samples,
		Delay:   delay,
		Padding: padding,
		Default: true,
	}
	return nil
}

// Tracks returns the single MP3 track.
func (d *Demuxer) Tracks() []container.Track { return []container.Track{d.track} }

// Warnings returns damage tolerated during parsing.
func (d *Demuxer) Warnings() []container.Warning { return d.warnings }

// recognizedTrailer reports whether the region from off to the end of
// data is plain tag baggage: ID3v1, APEv2, an appended ID3v2, Lyrics3,
// or NUL padding, possibly stacked. It is what makes a bare stream's end
// different from a wrapped one's, where the chunk bounds the frames and
// anything else inside it is damage.
func (d *Demuxer) recognizedTrailer(off int64) bool {
	if d.walk.DataEnd()-off > trailerScan {
		return false
	}
	b := d.walk.Bytes(off, int(d.walk.DataEnd()-off))
	for len(b) > 0 {
		switch {
		case len(b) >= 3 && string(b[:3]) == "TAG":
			b = b[min(128, len(b)):]
		case len(b) >= 8 && string(b[:8]) == "APETAGEX":
			return true // size fields point forward; accept the rest
		case id3.Size(b) > 0:
			n := id3.Size(b)
			if n > int64(len(b)) {
				return true // truncated tag is still a tag
			}
			b = b[n:]
		case len(b) >= 11 && string(b[:11]) == "LYRICSBEGIN":
			return true
		case b[0] == 0:
			i := 0
			for i < len(b) && b[i] == 0 {
				i++
			}
			b = b[i:]
		default:
			return false
		}
	}
	return true
}

// ReadPacket yields one whole frame. Packet data is reused across calls.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error { return d.pkts.ReadPacket(pkt) }

// SeekSample repositions so the reader is far enough before the target
// that decoder state converges; see mpegframes.Walker.Landing. format.Media
// decodes and discards from the landing, so the seek is sample-exact
// regardless of the backoff depth.
func (d *Demuxer) SeekSample(track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("mp3: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "mp3: negative seek target")
	}
	return d.pkts.SeekSample(sample)
}
