package mpa

import (
	"bytes"
	"fmt"
	"io"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/id3"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ container.Demuxer = (*Demuxer)(nil)
	_ container.Seeker  = (*Demuxer)(nil)
	_ container.Warner  = (*Demuxer)(nil)
)

// Hostile-input caps (ADR-0005 invariants).
const (
	// maxResync bounds the scan for the next frame sync after damage.
	maxResync = 1 << 20
	// minFrameLen is the smallest compliant frame (8 kbit/s at 24 kHz),
	// the hostile-input bound on index entry counts.
	minFrameLen = 24
	// maxID3Tags bounds leading tag skipping (tags stack).
	maxID3Tags = 8
	// trailerScan is how far back trailing tag recognition looks.
	trailerScan = 64 << 10
	// reservoirCover is the main-data backlog a seek backoff must
	// accumulate before the target region: the reservoir's 511-byte
	// reach plus a margin.
	reservoirCover = 511 + 64
	// stateFrames is how many frames before the target must decode from
	// a satisfied reservoir for the filterbank history (IMDCT overlap
	// plus synthesis window) to converge exactly.
	stateFrames = 3
)

// DemuxerOptions configures parsing.
type DemuxerOptions struct {
	// Strict turns tolerated damage (the Warnings list) into errors.
	Strict bool
}

// Demuxer reads one MP3 track from an elementary stream source.
type Demuxer struct {
	src  container.Source
	opts DemuxerOptions

	hdr      mp3.Header // reference header: the first audio frame's
	spf      int64      // samples per frame
	track    container.Track
	warnings []container.Warning

	firstFrame int64 // offset of the first audio frame (tags skipped)

	// idx is the lazy exact frame index: idx[i] is the absolute byte
	// offset of audio frame i. It grows as the walk advances and is
	// complete once done is set; sizes come from neighboring entries.
	// grew marks growth since open or RestoreIndex, so snapshots are
	// taken only when there is something new to keep.
	idx  []int64
	done bool
	grew bool

	cur int64 // next frame number ReadPacket delivers

	// w is the shared read-ahead window; its data end is the logical end
	// of frame data and its sticky error surfaces on the packet path.
	w srcwin.Window
}

// NewDemuxer parses the stream head (tags, the VBR metadata frame) and
// positions on the first audio frame. The returned Demuxer implements
// container.Seeker and container.Warner.
func NewDemuxer(src container.Source, opts *DemuxerOptions) (*Demuxer, error) {
	d := &Demuxer{src: src, w: srcwin.New(src, src.Size(), "mpa: reading frame data")}
	if opts != nil {
		d.opts = *opts
	}
	if err := d.parse(); err != nil {
		return nil, err
	}
	return d, nil
}

// warn records tolerated damage, or fails in strict mode.
func (d *Demuxer) warn(off int64, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if d.opts.Strict {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("mpa: %s (at offset %d)", msg, off))
	}
	d.warnings = append(d.warnings, container.Warning{Offset: off, Msg: msg})
	return nil
}

func (d *Demuxer) parse() error {
	// Skip leading ID3v2 tags (stacked ones included).
	off := int64(0)
	for range maxID3Tags {
		head := d.w.BytesAt(off, 10)
		n := id3.Size(head)
		if n == 0 || off+n > d.w.DataEnd() {
			break
		}
		off += n
	}

	// Free-format streams fix the frame size by convention, not by
	// header; nothing ships them anymore and supporting them would
	// weaken every sync heuristic here. Diagnose the head directly
	// (the candidate scan below treats unsized frames as junk).
	if fh, err := mp3.ParseHeader(d.w.BytesAt(off, mp3.HeaderLen)); err == nil && fh.Size() == 0 {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "mpa: free-format stream")
	}

	// Find the first frame: parse at off, else bounded junk scan.
	first, h, ok := d.nextCandidate(off, off+maxResync)
	if !ok {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return waxerr.New(waxerr.CodeUnsupportedFormat, "mpa: no Layer III frames found")
	}
	if first != off {
		if err := d.warn(off, "%d unparsable bytes before the first frame", first-off); err != nil {
			return err
		}
	}

	// A Xing, Info, or VBRI frame is metadata, not audio: consume it.
	tag, hasTag := vbrTag{delay: -1, padding: -1}, false
	if frame := d.w.BytesAt(first, h.Size()); len(frame) == h.Size() {
		if t, ok := parseVBRTag(h, frame); ok {
			tag, hasTag = t, true
			first += int64(h.Size())
			fh, err := mp3.ParseHeader(d.w.BytesAt(first, mp3.HeaderLen))
			if err != nil || !h.Kin(fh) {
				// Tag frame with no audio behind it (or damage): scan.
				var ok bool
				first, fh, ok = d.nextCandidate(first, first+maxResync)
				if !ok {
					if d.w.Err() != nil {
						return d.w.Err()
					}
					return waxerr.New(waxerr.CodeUnsupportedFormat, "mpa: no audio frames after the VBR tag")
				}
			}
			h = fh
		}
	}
	if d.w.Err() != nil {
		return d.w.Err()
	}

	d.hdr = h
	d.spf = int64(h.SamplesPerFrame())
	d.firstFrame = first
	if first+int64(h.Size()) <= d.w.DataEnd() {
		d.idx = append(d.idx, first)
	} else if err := d.warn(first, "the only frame is truncated, dropped"); err != nil {
		return err
	}

	f := h.PCMFormat()
	if err := f.Valid(); err != nil {
		return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "mpa: unusable format", err)
	}

	samples, delay, padding := int64(-1), int64(0), int64(0)
	if hasTag && tag.delay >= 0 && tag.frames > 0 {
		// LAME gapless: the decoder's fixed latency joins the encoder
		// delay at the front and comes off the padding at the back. The
		// trims and the length are adopted together or not at all: the
		// back trim is only expressible through a known length, and a
		// front-trim-only stream would be neither raw nor gapless.
		delay = tag.delay + decoderDelay
		padding = max(tag.padding-decoderDelay, 0)
		samples = max(tag.frames*d.spf-delay-padding, 0)
	} else if hasTag && tag.frames > 0 {
		samples = tag.frames * d.spf
	}
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

// nextCandidate scans [from, limit) for a parsable, sized frame header;
// when a reference header exists it must also be kin, and candidates are
// confirmed by the header their size points at (end of data counts).
func (d *Demuxer) nextCandidate(from, limit int64) (int64, mp3.Header, bool) {
	limit = min(limit, d.w.DataEnd())
	ref := d.hdr
	haveRef := d.spf != 0
	for off := from; off < limit; {
		buf := d.w.BytesAt(off, srcwin.Chunk)
		if len(buf) == 0 {
			return 0, mp3.Header{}, false
		}
		i := bytes.IndexByte(buf, 0xFF)
		if i < 0 {
			off += int64(len(buf))
			continue
		}
		cand := off + int64(i)
		if cand >= limit {
			return 0, mp3.Header{}, false
		}
		h, err := mp3.ParseHeader(d.w.BytesAt(cand, mp3.HeaderLen))
		if err == nil && h.Size() != 0 && (!haveRef || ref.Kin(h)) {
			next := cand + int64(h.Size())
			if next >= d.w.DataEnd() {
				return cand, h, true // runs to the end: nothing to confirm against
			}
			nh, nerr := mp3.ParseHeader(d.w.BytesAt(next, mp3.HeaderLen))
			if nerr == nil && h.Kin(nh) {
				return cand, h, true
			}
		}
		off = cand + 1
	}
	return 0, mp3.Header{}, false
}

// frameAt parses and validates the frame header at the exact offset.
func (d *Demuxer) frameAt(off int64) (mp3.Header, bool) {
	h, err := mp3.ParseHeader(d.w.BytesAt(off, mp3.HeaderLen))
	if err != nil || !d.hdr.Kin(h) || h.Size() == 0 {
		return mp3.Header{}, false
	}
	return h, true
}

// extend grows the frame index by one frame and reports whether it did.
// Only whole frames enter the index (ReadPacket trusts it), so a final
// frame cut off by the end of data is dropped with a warning. Once the
// walk cannot continue (clean end, trailing tags, or damage), done
// latches and the index is complete.
func (d *Demuxer) extend() (bool, error) {
	if d.done {
		return false, nil
	}
	if d.w.Err() != nil {
		return false, d.w.Err()
	}
	if len(d.idx) == 0 {
		d.done = true
		return false, nil
	}
	last := d.idx[len(d.idx)-1]
	h, ok := d.frameAt(last)
	if !ok {
		// The indexed frame itself went unreadable (shrinking source);
		// treat as end.
		d.done = true
		return false, d.w.Err()
	}
	next := last + int64(h.Size())
	if next >= d.w.DataEnd() {
		d.done = true
		return false, nil // the last frame ends exactly at (or is clamped by) dataEnd
	}
	cand := next
	nh, ok := d.frameAt(next)
	if !ok {
		// Damage: resync within bounds, or recognize a trailer and end.
		cand, nh, ok = d.nextCandidate(next, next+maxResync)
		if !ok {
			if d.w.Err() != nil {
				return false, d.w.Err()
			}
			d.done = true
			if tail := d.w.DataEnd() - next; tail > 0 && !d.recognizedTrailer(next) {
				return false, d.warn(next, "%d trailing bytes are not frames, dropped", tail)
			}
			return false, nil
		}
		if err := d.warn(next, "%d unparsable bytes skipped", cand-next); err != nil {
			return false, err
		}
	}
	if cand+int64(nh.Size()) > d.w.DataEnd() {
		d.done = true
		return false, d.warn(cand, "truncated final frame dropped")
	}
	d.idx = append(d.idx, cand)
	d.grew = true
	return true, nil
}

// recognizedTrailer reports whether the region from off to the end of
// data is plain tag baggage: ID3v1, APEv2, an appended ID3v2, Lyrics3,
// or NUL padding, possibly stacked.
func (d *Demuxer) recognizedTrailer(off int64) bool {
	if d.w.DataEnd()-off > trailerScan {
		return false
	}
	b := d.w.BytesAt(off, int(d.w.DataEnd()-off))
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

// frameNo extends the index up to frame n and reports the highest frame
// number available (which is n when the stream is long enough).
func (d *Demuxer) frameNo(n int64) (int64, error) {
	for int64(len(d.idx)) <= n {
		grew, err := d.extend()
		if err != nil {
			return 0, err
		}
		if !grew {
			break
		}
	}
	return int64(len(d.idx)) - 1, nil
}

// ReadPacket yields one whole frame. Packet data is reused across calls.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error {
	lastNo, err := d.frameNo(d.cur)
	if err != nil {
		return err
	}
	if d.cur > lastNo || lastNo < 0 {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return io.EOF
	}
	off := d.idx[d.cur]
	h, ok := d.frameAt(off)
	if !ok {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return waxerr.New(waxerr.CodeSourceUnreadable, "mpa: indexed frame vanished")
	}
	d.w.Trim(off)
	data := d.w.BytesAt(off, h.Size())
	if len(data) != h.Size() {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return waxerr.New(waxerr.CodeSourceUnreadable, "mpa: reading frame data")
	}
	*pkt = container.Packet{
		Track: 0,
		Packet: codec.Packet{
			Data: data,
			PTS:  d.cur * d.spf,
			Dur:  d.spf,
			Sync: syncFrame(h, data),
		},
	}
	d.cur++
	return nil
}

// syncFrame reports whether a frame is decodable in isolation: its main
// data reaches zero bytes back into the reservoir.
func syncFrame(h mp3.Header, frame []byte) bool {
	off := mp3.HeaderLen
	if h.Protected {
		off += 2
	}
	if len(frame) <= off+1 {
		return false
	}
	if h.Version == mp3.MPEG1 {
		return frame[off] == 0 && frame[off+1]&0x80 == 0 // 9 bits
	}
	return frame[off] == 0 // 8 bits
}

// SeekSample repositions so the reader is far enough before the target
// that decoder state converges: landings precede the target frame by
// stateFrames plus however many frames the bit reservoir's reach needs.
// format.Media decodes and discards from the landing, so the seek is
// sample-exact regardless of the backoff depth.
func (d *Demuxer) SeekSample(track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("mpa: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "mpa: negative seek target")
	}
	target := sample / d.spf
	lastNo, err := d.frameNo(target)
	if err != nil {
		return 0, err
	}
	if lastNo < 0 {
		return 0, nil // nothing to land on; reads stay EOF
	}
	target = min(target, lastNo)

	// Back off: stateFrames for the filterbank, then keep going until
	// the skipped frames carry enough main data to satisfy any reservoir
	// reference at the state frames themselves. Only bytes past the
	// header, the optional CRC, and the side info feed the reservoir.
	overhead := int64(mp3.HeaderLen + d.hdr.SideInfoLen())
	if d.hdr.Protected {
		overhead += 2
	}
	land := max(target-stateFrames, 0)
	cover := int64(0)
	for land > 0 && cover < reservoirCover {
		land--
		cover += max(d.frameSize(land)-overhead, 0)
	}
	d.cur = land
	return land * d.spf, nil
}

// frameSize is the byte length of indexed frame n.
func (d *Demuxer) frameSize(n int64) int64 {
	if n+1 < int64(len(d.idx)) {
		return d.idx[n+1] - d.idx[n]
	}
	if h, ok := d.frameAt(d.idx[n]); ok {
		return int64(h.Size())
	}
	return 0
}
