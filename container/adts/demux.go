package adts

import (
	"bytes"
	"fmt"
	"io"

	"github.com/colespringer/waxflow/codec"
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

// DemuxerOptions configures parsing.
type DemuxerOptions struct {
	// Strict turns tolerated damage (the Warnings list) into errors.
	Strict bool
}

// Demuxer reads one AAC track from an ADTS elementary stream. It carries no
// gapless trims (ADTS has none) and seeks one frame early for the decoder's
// IMDCT overlap.
type Demuxer struct {
	src  container.Source
	opts DemuxerOptions

	ref      header
	haveRef  bool // ref is set (distinct from firstFrame, which can be 0)
	track    container.Track
	warnings []container.Warning

	firstFrame int64   // offset of the first audio frame
	idx        []int64 // lazy frame offsets; idx[i] is frame i's byte offset
	done       bool    // the index has reached the end of the stream
	cur        int64   // next frame number ReadPacket delivers

	w srcwin.Window
}

// NewDemuxer parses the stream head (ID3 tags, the first frame) and derives
// the AudioSpecificConfig. The returned Demuxer implements container.Seeker
// and container.Warner.
func NewDemuxer(src container.Source, opts *DemuxerOptions) (*Demuxer, error) {
	d := &Demuxer{src: src, w: srcwin.New(src, src.Size(), "adts: reading frame data")}
	if opts != nil {
		d.opts = *opts
	}
	if err := d.parse(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Demuxer) warn(off int64, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if d.opts.Strict {
		return malformed("%s (at offset %d)", msg, off)
	}
	d.warnings = append(d.warnings, container.Warning{Offset: off, Msg: msg})
	return nil
}

func (d *Demuxer) parse() error {
	off := int64(0)
	for range maxID3Tags {
		if n := id3.Size(d.w.BytesAt(off, 10)); n > 0 && off+n <= d.w.DataEnd() {
			off += n
		} else {
			break
		}
	}

	first, h, ok := d.nextFrame(off, off+maxResync)
	if !ok {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return malformed("no ADTS frames found")
	}
	if h.blocks != 0 {
		// A frame carrying multiple raw_data_blocks would need to be split
		// into one packet per block; we deliver one block per frame, so
		// refuse rather than undercount the timeline.
		return malformed("%d raw_data_blocks per frame is unsupported", h.blocks+1)
	}
	if first != off {
		if err := d.warn(off, "%d unparsable bytes before the first frame", first-off); err != nil {
			return err
		}
	}
	d.ref = h
	d.haveRef = true
	d.firstFrame = first
	d.idx = append(d.idx, first)

	cfg, err := h.config()
	if err != nil {
		return err
	}
	f := cfg.Format()
	if err := f.Valid(); err != nil {
		return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "adts: unusable format", err)
	}
	d.track = container.Track{
		Codec:       codec.AACLC,
		CodecConfig: h.asc(),
		Fmt:         f,
		Samples:     -1, // ADTS declares no length; the stream runs to EOF
		Default:     true,
	}
	return nil
}

// nextFrame scans [from, limit) for a valid, length-confirmed frame header.
func (d *Demuxer) nextFrame(from, limit int64) (int64, header, bool) {
	limit = min(limit, d.w.DataEnd())
	for off := from; off < limit; {
		buf := d.w.BytesAt(off, srcwin.Chunk)
		if len(buf) < headerLen {
			return 0, header{}, false
		}
		i := bytes.IndexByte(buf, 0xFF)
		if i < 0 {
			off += int64(len(buf)) - headerLen + 1
			continue
		}
		cand := off + int64(i)
		if cand >= limit {
			return 0, header{}, false
		}
		if h, ok := d.frameAt(cand); ok {
			return cand, h, true
		}
		off = cand + 1
	}
	return 0, header{}, false
}

// frameAt parses and confirms the frame at the exact offset: its declared
// length must point at a kin frame header (or the end of data).
func (d *Demuxer) frameAt(off int64) (header, bool) {
	h, ok := parseHeader(d.w.BytesAt(off, 9))
	if !ok {
		return header{}, false
	}
	if d.haveRef && !d.ref.kin(h) {
		return header{}, false
	}
	next := off + int64(h.frameLen)
	if next >= d.w.DataEnd() {
		return h, true // runs to the end: nothing to confirm against
	}
	if nh, ok := parseHeader(d.w.BytesAt(next, 9)); ok && h.kin(nh) {
		return h, true
	}
	return header{}, false
}

// Tracks returns the single AAC track.
func (d *Demuxer) Tracks() []container.Track { return []container.Track{d.track} }

// Warnings returns damage tolerated during parsing.
func (d *Demuxer) Warnings() []container.Warning { return d.warnings }

// extend grows the frame index by one and reports whether it did.
func (d *Demuxer) extend() (bool, error) {
	if d.done {
		return false, nil
	}
	if d.w.Err() != nil {
		return false, d.w.Err()
	}
	last := d.idx[len(d.idx)-1]
	h, ok := parseHeader(d.w.BytesAt(last, 9))
	if !ok {
		d.done = true
		return false, d.w.Err()
	}
	next := last + int64(h.frameLen)
	if next >= d.w.DataEnd() {
		d.done = true
		return false, nil
	}
	if _, ok := d.frameAt(next); !ok {
		// Damage or trailing junk: resync within bounds, else end.
		cand, _, ok := d.nextFrame(next, next+maxResync)
		if !ok {
			d.done = true
			if tail := d.w.DataEnd() - next; tail > 0 {
				return false, d.warn(next, "%d trailing bytes are not frames, dropped", tail)
			}
			return false, nil
		}
		if err := d.warn(next, "%d unparsable bytes skipped", cand-next); err != nil {
			return false, err
		}
		next = cand
	}
	d.idx = append(d.idx, next)
	return true, nil
}

// frameNo extends the index up to frame n, returning the highest available.
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

// ReadPacket yields one raw_data_block. Packet data is reused across calls.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error {
	lastNo, err := d.frameNo(d.cur)
	if err != nil {
		return err
	}
	if d.cur > lastNo {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return io.EOF
	}
	off := d.idx[d.cur]
	h, ok := parseHeader(d.w.BytesAt(off, 9))
	if !ok {
		return waxerr.New(waxerr.CodeSourceUnreadable, "adts: indexed frame vanished")
	}
	d.w.Trim(off)
	frame := d.w.BytesAt(off, h.frameLen)
	if len(frame) < h.frameLen {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		// A final frame whose declared length runs past EOF is a truncated
		// tail (it is always the last indexed frame): drop it with a warning
		// rather than a hard error, matching the damage-tolerant contract.
		d.cur++ // consume it so a re-read returns clean EOF
		if werr := d.warn(off, "final frame truncated (%d of %d bytes), dropped", len(frame), h.frameLen); werr != nil {
			return werr
		}
		return io.EOF
	}
	*pkt = container.Packet{
		Track: 0,
		Packet: codec.Packet{
			Data: frame[h.hdrLen:],
			PTS:  d.cur * samplesPerFrame,
			Dur:  samplesPerFrame,
			Sync: true,
		},
	}
	d.cur++
	return nil
}

// SeekSample lands one frame before the target (for the AAC IMDCT overlap)
// and returns that frame's first sample; format.Media pre-rolls the rest.
func (d *Demuxer) SeekSample(track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("adts: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "adts: negative seek target")
	}
	target := sample / samplesPerFrame
	lastNo, err := d.frameNo(target)
	if err != nil {
		return 0, err
	}
	if lastNo < 0 {
		return 0, nil
	}
	target = min(target, lastNo)
	land := max(target-1, 0)
	d.cur = land
	return land * samplesPerFrame, nil
}
