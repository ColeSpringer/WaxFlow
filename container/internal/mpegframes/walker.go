// Package mpegframes walks a run of MPEG audio frames and keeps the exact
// frame index that walk produces.
//
// An MPEG audio frame carries no position, so sample-exact seeking needs an
// exact frame index: each frame header gives the next frame's offset, and
// nothing else in a stream of them says where frame n begins. That walk is
// the same wherever the frames are, and this tree finds them in three places
// (bare in container/mpa, inside a WAV data chunk, inside an AIFF-C SSND
// payload). What differs is the range, the vocabulary a finding is reported
// in, and what counts as the end of the run, so the walker takes a range and
// two hooks and owns everything between them.
//
// Seek landings back off from the target far enough that the codec's bit
// reservoir (up to 511 bytes of backward reference) and filterbank history
// converge before the target frame: the frames in between decode (or silence
// out) and are discarded by format.Media's pre-roll, so post-seek output is
// bit-identical to a linear decode.
//
// The package is internal to the container tree: it is plumbing shared by
// demuxers, not API, and it must not become one (the v1.0 surface audit
// prunes exactly this kind of helper when exported).
package mpegframes

import (
	"bytes"
	"fmt"
	"io"

	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/waxerr"
)

// Hostile-input caps (ADR-0005 invariants).
const (
	// maxResync bounds the scan for the next frame sync after damage.
	maxResync = 1 << 20
	// minFrameLen is the smallest compliant frame (8 kbit/s at 24 kHz),
	// the hostile-input bound on index entry counts.
	minFrameLen = 24
	// reservoirCover is the main-data backlog a seek backoff must
	// accumulate before the target region: the reservoir's 511-byte
	// reach plus a margin.
	reservoirCover = 511 + 64
	// stateFrames is how many frames before the target must decode from
	// a satisfied reservoir for the filterbank history (IMDCT overlap
	// plus synthesis window) to converge exactly.
	stateFrames = 3
)

// Options are the owner's vocabulary and hooks.
type Options struct {
	// Prefix is the owner's public container name with its separator
	// ("mp3: ", "wav: "), which every error the walk returns carries: these
	// messages reach users verbatim, and they must name the format the
	// request asked for rather than this package.
	Prefix string
	// Warn records tolerated damage at an offset. The message arrives already
	// formatted, which is what keeps the walk's own call sites inside go
	// vet's printf analysis: a hook taking a format string and arguments
	// would make every one of them invisible to it. The owner adds its
	// prefix and applies its own Strict policy, so a non-nil return aborts
	// the walk. A nil hook drops findings, which only a test does.
	Warn func(off int64, msg string) error
	// Trailer reports whether the region from off to the end of data is tag
	// baggage rather than damage. It is nil wherever the container bounds
	// the run, which is every wrapper: only a bare stream can have tags
	// appended to the frames themselves.
	Trailer func(off int64) bool
}

// Walker owns the lazy exact frame index over a run of frames. Construct
// with New and parse the head with Begin; the zero value is unusable.
type Walker struct {
	w    srcwin.Window
	opts Options

	hdr   mp3.Header // reference header: the first audio frame's
	spf   int64      // samples per frame
	first int64      // offset of the first audio frame

	// idx is the lazy exact frame index: idx[i] is the absolute byte
	// offset of frame i. It grows as the walk advances and is complete
	// once done is set; sizes come from neighboring entries. grew marks
	// growth since Begin or Restore, so snapshots are taken only when
	// there is something new to keep.
	idx  []int64
	done bool
	grew bool
}

// New returns a Walker reading up to end, which is the logical end of frame
// data: the source's size for a bare stream, the end of the payload chunk
// for a wrapped one. Nothing is read until Begin.
func New(src container.Source, end int64, opts Options) *Walker {
	return &Walker{w: srcwin.New(src, end, opts.Prefix+"reading frame data"), opts: opts}
}

// Bytes returns up to n bytes at off, clamped to the end of data, from the
// walk's own read-ahead window. Owners read their surrounding structure
// through it (leading tags, a trailer) rather than opening a second one.
func (w *Walker) Bytes(off int64, n int) []byte { return w.w.BytesAt(off, n) }

// DataEnd returns the logical end of frame data.
func (w *Walker) DataEnd() int64 { return w.w.DataEnd() }

// Err returns the sticky read failure, nil while reads work.
func (w *Walker) Err() error { return w.w.Err() }

// Header returns the reference header: the first audio frame's, which every
// later frame must be kin to.
func (w *Walker) Header() mp3.Header { return w.hdr }

// SamplesPerFrame is what one frame decodes to, and so the multiplier
// between the frame timeline and the sample timeline.
func (w *Walker) SamplesPerFrame() int64 { return w.spf }

// FirstFrame is the offset the run's first audio frame begins at, past any
// leading junk and past a consumed metadata frame.
func (w *Walker) FirstFrame() int64 { return w.first }

// MaxFrames bounds how many frames a payload of n bytes can hold, which is
// the only thing a container can know about a frame run without walking it:
// the smallest compliant Layer III frame is 24 bytes (8 kbit/s at 24 kHz), so
// a declared length above this many frames' worth is one the payload cannot
// be describing.
func MaxFrames(n int64) int64 { return n / minFrameLen }

func (w *Walker) malformed(format string, args ...any) error {
	return waxerr.Malformed(w.opts.Prefix, format, args...)
}

func (w *Walker) warn(off int64, format string, args ...any) error {
	if w.opts.Warn == nil {
		return nil
	}
	return w.opts.Warn(off, fmt.Sprintf(format, args...))
}

// Begin parses the head of the run at off and positions on the first audio
// frame: a free-format stream is refused, leading junk is scanned past and
// reported, and a Xing, Info, or VBRI metadata frame is consumed rather than
// delivered. It reports what that frame declared, and whether there was one.
func (w *Walker) Begin(off int64) (VBRInfo, bool, error) {
	none := VBRInfo{Delay: -1, Padding: -1}

	// Free-format streams fix the frame size by convention, not by
	// header; nothing ships them anymore and supporting them would
	// weaken every sync heuristic here. Diagnose the head directly
	// (the candidate scan below treats unsized frames as junk).
	if fh, err := mp3.ParseHeader(w.w.BytesAt(off, mp3.HeaderLen)); err == nil && fh.Size() == 0 {
		return none, false, waxerr.Unsupported(w.opts.Prefix, "free-format stream")
	}

	// Find the first frame: parse at off, else bounded junk scan.
	first, h, ok := w.nextCandidate(off, off+maxResync)
	if !ok {
		if w.w.Err() != nil {
			return none, false, w.w.Err()
		}
		return none, false, w.malformed("no Layer III frames found")
	}
	if first != off {
		if err := w.warn(off, "%d unparsable bytes before the first frame", first-off); err != nil {
			return none, false, err
		}
	}

	// A Xing, Info, or VBRI frame is metadata, not audio: consume it.
	tag, hasTag := none, false
	if frame := w.w.BytesAt(first, h.Size()); len(frame) == h.Size() {
		if t, ok := ParseVBRTag(h, frame); ok {
			tag, hasTag = t, true
			first += int64(h.Size())
			fh, err := mp3.ParseHeader(w.w.BytesAt(first, mp3.HeaderLen))
			if err != nil || !h.Kin(fh) {
				// Tag frame with no audio behind it (or damage): scan.
				var ok bool
				first, fh, ok = w.nextCandidate(first, first+maxResync)
				if !ok {
					if w.w.Err() != nil {
						return none, false, w.w.Err()
					}
					return none, false, w.malformed("no audio frames after the VBR tag")
				}
			}
			h = fh
		}
	}
	if w.w.Err() != nil {
		return none, false, w.w.Err()
	}

	w.hdr = h
	w.spf = int64(h.SamplesPerFrame())
	w.first = first
	if first+int64(h.Size()) <= w.w.DataEnd() {
		w.idx = append(w.idx, first)
	} else if err := w.warn(first, "the only frame is truncated, dropped"); err != nil {
		return none, false, err
	}
	return tag, hasTag, nil
}

// nextCandidate scans [from, limit) for a parsable, sized frame header;
// when a reference header exists it must also be kin, and candidates are
// confirmed by the header their size points at (end of data counts).
func (w *Walker) nextCandidate(from, limit int64) (int64, mp3.Header, bool) {
	limit = min(limit, w.w.DataEnd())
	ref := w.hdr
	haveRef := w.spf != 0
	for off := from; off < limit; {
		buf := w.w.BytesAt(off, srcwin.Chunk)
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
		h, err := mp3.ParseHeader(w.w.BytesAt(cand, mp3.HeaderLen))
		if err == nil && h.Size() != 0 && (!haveRef || ref.Kin(h)) {
			next := cand + int64(h.Size())
			if next >= w.w.DataEnd() {
				return cand, h, true // runs to the end: nothing to confirm against
			}
			nh, nerr := mp3.ParseHeader(w.w.BytesAt(next, mp3.HeaderLen))
			if nerr == nil && h.Kin(nh) {
				return cand, h, true
			}
		}
		off = cand + 1
	}
	return 0, mp3.Header{}, false
}

// headerAt parses and validates the frame header at the exact offset.
func (w *Walker) headerAt(off int64) (mp3.Header, bool) {
	h, err := mp3.ParseHeader(w.w.BytesAt(off, mp3.HeaderLen))
	if err != nil || !w.hdr.Kin(h) || h.Size() == 0 {
		return mp3.Header{}, false
	}
	return h, true
}

// extend grows the frame index by one frame and reports whether it did.
// Only whole frames enter the index (Frame trusts it), so a final frame
// cut off by the end of data is dropped with a warning. Once the walk
// cannot continue (clean end, trailing tags, or damage), done latches and
// the index is complete.
func (w *Walker) extend() (bool, error) {
	if w.done {
		return false, nil
	}
	if w.w.Err() != nil {
		return false, w.w.Err()
	}
	if len(w.idx) == 0 {
		w.done = true
		return false, nil
	}
	last := w.idx[len(w.idx)-1]
	h, ok := w.headerAt(last)
	if !ok {
		// The indexed frame itself went unreadable (shrinking source);
		// treat as end.
		w.done = true
		return false, w.w.Err()
	}
	next := last + int64(h.Size())
	if next >= w.w.DataEnd() {
		w.done = true
		return false, nil // the last frame ends exactly at (or is clamped by) dataEnd
	}
	cand := next
	nh, ok := w.headerAt(next)
	if !ok {
		// Damage: resync within bounds, or recognize a trailer and end.
		cand, nh, ok = w.nextCandidate(next, next+maxResync)
		if !ok {
			if w.w.Err() != nil {
				return false, w.w.Err()
			}
			w.done = true
			if tail := w.w.DataEnd() - next; tail > 0 && !w.recognizedTrailer(next) {
				return false, w.warn(next, "%d trailing bytes are not frames, dropped", tail)
			}
			return false, nil
		}
		if err := w.warn(next, "%d unparsable bytes skipped", cand-next); err != nil {
			return false, err
		}
	}
	if cand+int64(nh.Size()) > w.w.DataEnd() {
		w.done = true
		return false, w.warn(cand, "truncated final frame dropped")
	}
	w.idx = append(w.idx, cand)
	w.grew = true
	return true, nil
}

// recognizedTrailer asks the owner whether the tail is tag baggage. A
// container that bounds the run passes no hook, and for it the answer is no:
// bytes inside the payload that are not frames are damage.
func (w *Walker) recognizedTrailer(off int64) bool {
	return w.opts.Trailer != nil && w.opts.Trailer(off)
}

// frameNo extends the index up to frame n and reports the highest frame
// number available (which is n when the run is long enough).
func (w *Walker) frameNo(n int64) (int64, error) {
	for int64(len(w.idx)) <= n {
		grew, err := w.extend()
		if err != nil {
			return 0, err
		}
		if !grew {
			break
		}
	}
	return int64(len(w.idx)) - 1, nil
}

// Frame is one frame of the run: its bytes, its header, and whether it
// decodes in isolation.
type Frame struct {
	Data   []byte
	Header mp3.Header
	Sync   bool
}

// Frame returns frame n, walking the index out to it. The data is the
// window's own bytes and is reused by later calls, which is the contract
// container.Packet.Data already carries. Past the last frame the walk can
// reach it returns io.EOF.
func (w *Walker) Frame(n int64) (Frame, error) {
	lastNo, err := w.frameNo(n)
	if err != nil {
		return Frame{}, err
	}
	if n > lastNo || lastNo < 0 {
		if w.w.Err() != nil {
			return Frame{}, w.w.Err()
		}
		return Frame{}, io.EOF
	}
	off := w.idx[n]
	h, ok := w.headerAt(off)
	if !ok {
		if w.w.Err() != nil {
			return Frame{}, w.w.Err()
		}
		return Frame{}, waxerr.New(waxerr.CodeSourceUnreadable, w.opts.Prefix+"indexed frame vanished")
	}
	w.w.Trim(off)
	data := w.w.BytesAt(off, h.Size())
	if len(data) != h.Size() {
		if w.w.Err() != nil {
			return Frame{}, w.w.Err()
		}
		return Frame{}, waxerr.New(waxerr.CodeSourceUnreadable, w.opts.Prefix+"reading frame data")
	}
	return Frame{Data: data, Header: h, Sync: syncFrame(h, data)}, nil
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

// Landing reports the frame a decode aiming at target must begin at: far
// enough before it that decoder state converges, which is stateFrames for
// the filterbank plus however many frames the bit reservoir's reach needs.
// The caller decodes and discards from there, so the seek is sample-exact
// regardless of the backoff depth.
//
// A target past the end of the run lands on the last frame's backoff; a run
// with no frames at all lands at 0, where reads stay EOF.
func (w *Walker) Landing(target int64) (int64, error) {
	lastNo, err := w.frameNo(target)
	if err != nil {
		return 0, err
	}
	if lastNo < 0 {
		return 0, nil
	}
	target = min(target, lastNo)

	// Back off: stateFrames for the filterbank, then keep going until
	// the skipped frames carry enough main data to satisfy any reservoir
	// reference at the state frames themselves. Only bytes past the
	// header, the optional CRC, and the side info feed the reservoir.
	overhead := int64(mp3.HeaderLen + w.hdr.SideInfoLen())
	if w.hdr.Protected {
		overhead += 2
	}
	land := max(target-stateFrames, 0)
	cover := int64(0)
	for land > 0 && cover < reservoirCover {
		land--
		cover += max(w.frameSize(land)-overhead, 0)
	}
	return land, nil
}

// frameSize is the byte length of indexed frame n.
func (w *Walker) frameSize(n int64) int64 {
	if n+1 < int64(len(w.idx)) {
		return w.idx[n+1] - w.idx[n]
	}
	if h, ok := w.headerAt(w.idx[n]); ok {
		return int64(h.Size())
	}
	return 0
}
