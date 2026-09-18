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
	// Note records a finding Strict must not escalate, at an offset, with
	// the message already formatted for the same reason Warn's is. A count
	// that overruns the run, or comes up one frame short of it on an
	// otherwise clean end, is the file being loose rather than damaged, and
	// the walk settles the length either way. A nil hook drops findings.
	Note func(off int64, msg string)
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

	// declared is the frame count a metadata frame stated, 0 when there was
	// none or it was too large for the payload to hold. compared marks the
	// one comparison of it against the finished index as run, and endFinding
	// records that the run ended on damage rather than cleanly, which is
	// what decides whether a one-frame shortfall is a Note or damage. A
	// snapshot has no room for endFinding, so it never marks such a run
	// complete: a restore would otherwise compare with the damage forgotten.
	declared   int64
	compared   bool
	endFinding bool
}

// New returns a Walker reading up to end, which is the logical end of frame
// data: the source's size for a bare stream, the end of the payload chunk
// for a wrapped one. Nothing is read until Begin.
func New(src container.Source, end int64, opts Options) *Walker {
	return &Walker{w: srcwin.New(src, end, opts.Prefix+"reading frame data"), opts: opts}
}

// Bytes returns up to n bytes at off, clamped to the end of data, from the
// walk's own window. Owners read their surrounding structure through it
// (leading tags, a trailer) rather than opening a second one; those are
// fixed structures read once, so the read is exact rather than a window.
func (w *Walker) Bytes(off int64, n int) []byte { return w.w.Peek(off, n) }

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

func (w *Walker) note(off int64, format string, args ...any) {
	if w.opts.Note == nil {
		return
	}
	w.opts.Note(off, fmt.Sprintf(format, args...))
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
	if fh, err := mp3.ParseHeader(w.w.Peek(off, mp3.HeaderLen)); err == nil && fh.Size() == 0 {
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
	tag, hasTag, tagOff := none, false, int64(0)
	if frame := w.w.Peek(first, h.Size()); len(frame) == h.Size() {
		if t, ok := ParseVBRTag(h, frame); ok {
			tag, hasTag, tagOff = t, true, first
			first += int64(h.Size())
			fh, err := mp3.ParseHeader(w.w.Peek(first, mp3.HeaderLen))
			if err != nil || !h.Kin(fh) || fh.Size() == 0 {
				// Tag frame with no audio behind it (or damage): scan. An
				// unsized header is damage too: taken as the reference it
				// would index a frame of no length that Frame cannot read.
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

	// Bound the declared count by what the payload can hold, the way riff
	// bounds its fact chunk: the smallest compliant frame puts a ceiling on
	// the run at O(1), and a count above it is one the bytes are not
	// describing. Clearing it takes the trims with it, since Gapless adopts
	// the length and the trims together or not at all.
	if hasTag {
		if room := w.w.DataEnd() - first; tag.Frames > MaxFrames(room) {
			if err := w.warn(tagOff, "the metadata frame declares %d frames, more than %d bytes can hold; ignored",
				tag.Frames, room); err != nil {
				return none, false, err
			}
			tag.Frames = 0
		}
	}
	w.declared = tag.Frames

	w.hdr = h
	w.spf = int64(h.SamplesPerFrame())
	w.first = first
	if first+int64(h.Size()) <= w.w.DataEnd() {
		w.idx = append(w.idx, first)
	} else {
		w.endFinding = true
		if err := w.warn(first, "the only frame is truncated, dropped"); err != nil {
			return none, false, err
		}
	}
	return tag, hasTag, nil
}

// latchDone marks the index complete and settles the declared count against
// it. Every arm that ends the walk goes through it, on the read path as on
// the walk path, so a strict read and a strict walk of the same run reach
// the same answer.
func (w *Walker) latchDone() error {
	w.done = true
	return w.compare()
}

// compare checks the count a metadata frame declared against the frames the
// finished index holds, once. A shortfall shrinks the length the walk
// settles, so it is damage: the file promised audio its bytes do not hold.
// Two shapes are the file being loose rather than damaged, and are Notes: an
// overrun, where the tag simply undercounts a run that is all there; and a
// shortfall of exactly one frame on a run that ended cleanly, which is what
// an encoder counting its own metadata frame produces on an intact file.
func (w *Walker) compare() error {
	if w.compared {
		return nil
	}
	w.compared = true
	if w.declared <= 0 {
		return nil
	}
	n := w.Frames()
	if n == w.declared {
		return nil // runEnd reads a header; the agreeing case must not pay it
	}
	end := w.runEnd()
	switch {
	case n > w.declared:
		w.note(end, "the run holds %d frames past the %d the metadata frame declares", n-w.declared, w.declared)
		return nil
	case w.declared-n == 1 && !w.endFinding:
		w.note(end, "the metadata frame declares %d frames but the run holds %d", w.declared, n)
		return nil
	default:
		return w.warn(end, "the metadata frame declares %d frames but the run holds %d", w.declared, n)
	}
}

// runEnd is where the last indexed frame ends, which is where a count that
// disagrees with the run is reported: the offset the frames it promised
// would have continued from. An empty index reports at the head of the run.
func (w *Walker) runEnd() int64 {
	if len(w.idx) == 0 {
		return w.first
	}
	n := int64(len(w.idx)) - 1
	return w.idx[n] + w.frameSize(n)
}

// nextCandidate scans [from, limit) for a parsable, sized frame header; when
// a reference header exists it must also be kin, and a candidate is confirmed
// by the header its size points at (the end of data counts).
//
// The reads are exact, and a clean run costs the header and one frame: the
// header at from is confirmed by one contiguous read of the frame with the
// header behind it. Only junk pays a scan, in spans that start at 1 KiB and
// double up to a window, so the bytes read stay within about twice the junk
// and the reads grow with its length rather than its content: every sync
// byte in a span is tried from the same view before more is asked for, and a
// header straddling a span's end is tried from the start of the next. The
// old loop asked for a whole window from every false candidate, and forward
// extension turned that into a read per sync byte in the junk.
func (w *Walker) nextCandidate(from, limit int64) (int64, mp3.Header, bool) {
	limit = min(limit, w.w.DataEnd())
	if from >= limit {
		return 0, mp3.Header{}, false
	}
	if h, ok := w.confirmed(from, w.w.Peek(from, mp3.HeaderLen)); ok {
		return from, h, true
	}
	span := int64(1 << 10)
	for off := from; off < limit; {
		// A candidate below limit may carry its header past it.
		n := min(span, limit-off+mp3.HeaderLen-1)
		buf := w.w.Peek(off, int(n))
		if len(buf) < mp3.HeaderLen {
			return 0, mp3.Header{}, false
		}
		next := off + int64(len(buf))
		for i := 0; i < len(buf); {
			j := bytes.IndexByte(buf[i:], 0xFF)
			if j < 0 {
				break
			}
			i += j
			cand := off + int64(i)
			if cand >= limit {
				return 0, mp3.Header{}, false
			}
			if i+mp3.HeaderLen > len(buf) {
				next = cand // straddles the span: the next span starts on it
				break
			}
			if h, ok := w.confirmed(cand, buf[i:i+mp3.HeaderLen]); ok {
				return cand, h, true
			}
			i++
		}
		if int64(len(buf)) < n {
			return 0, mp3.Header{}, false // the data ended inside the span
		}
		off = next
		span = min(span*2, srcwin.Chunk)
	}
	return 0, mp3.Header{}, false
}

// confirmed parses the candidate header hdr found at off and confirms it by
// the header its size points at, read in one exact read with the frame: the
// read starts inside the window, so it extends it rather than rebasing, and
// a candidate that is a frame costs the frame and nothing more.
func (w *Walker) confirmed(off int64, hdr []byte) (mp3.Header, bool) {
	h, err := mp3.ParseHeader(hdr)
	if err != nil || h.Size() == 0 || (w.spf != 0 && !w.hdr.Kin(h)) {
		return mp3.Header{}, false
	}
	next := off + int64(h.Size())
	if next >= w.w.DataEnd() {
		return h, true // runs to the end: nothing to confirm against
	}
	frame := w.w.Peek(off, h.Size()+mp3.HeaderLen)
	if len(frame) < h.Size()+mp3.HeaderLen {
		return mp3.Header{}, false
	}
	if nh, err := mp3.ParseHeader(frame[h.Size():]); err != nil || !h.Kin(nh) {
		return mp3.Header{}, false
	}
	return h, true
}

// headerAt parses and validates the frame header at the exact offset. It is
// the packet path's read: a miss loads a window of read-ahead, which is what
// makes a linear walk cost one read per window.
func (w *Walker) headerAt(off int64) (mp3.Header, bool) {
	return w.kin(w.w.BytesAt(off, mp3.HeaderLen))
}

// headerPeek is headerAt through an exact read, for the open path: a
// restore's spread of probes costs a header each rather than a window each.
func (w *Walker) headerPeek(off int64) (mp3.Header, bool) {
	return w.kin(w.w.Peek(off, mp3.HeaderLen))
}

// headerAfter parses the header behind the frame at off, reading it with
// the frame rather than on its own. The read starts inside the window and so
// extends it in place; one starting at the next frame would rebase into
// fresh storage at every window boundary the index walk crosses, which is a
// window allocated per window walked on the measure pass.
func (w *Walker) headerAfter(off int64, h mp3.Header) (mp3.Header, bool) {
	b := w.w.BytesAt(off, h.Size()+mp3.HeaderLen)
	if len(b) < h.Size()+mp3.HeaderLen {
		return mp3.Header{}, false
	}
	return w.kin(b[h.Size():])
}

// kin parses a header and requires it sized and kin to the reference.
func (w *Walker) kin(b []byte) (mp3.Header, bool) {
	h, err := mp3.ParseHeader(b)
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
//
// The window is trimmed to the last indexed frame first. Nothing holds a
// view across extend (Frame takes its own after its own Trim), and without
// it a walk that never reads a packet, which is what a seek to the end or a
// measure pass is, has nothing else keeping the window from growing.
func (w *Walker) extend() (bool, error) {
	if w.done {
		return false, nil
	}
	if w.w.Err() != nil {
		return false, w.w.Err()
	}
	if len(w.idx) == 0 {
		return false, w.latchDone() // Begin dropped a truncated lone frame
	}
	last := w.idx[len(w.idx)-1]
	w.w.Trim(last)
	h, ok := w.headerAt(last)
	if !ok {
		// The indexed frame itself went unreadable (shrinking source);
		// treat as end.
		w.done, w.endFinding = true, true
		if err := w.w.Err(); err != nil {
			return false, err
		}
		return false, w.compare()
	}
	next := last + int64(h.Size())
	if next >= w.w.DataEnd() {
		// The last frame ends exactly at (or is clamped by) dataEnd.
		return false, w.latchDone()
	}
	cand := next
	nh, ok := w.headerAfter(last, h)
	if !ok {
		// Damage: resync within bounds, or recognize a trailer and end.
		cand, nh, ok = w.nextCandidate(next, next+maxResync)
		if !ok {
			if w.w.Err() != nil {
				return false, w.w.Err()
			}
			w.done = true
			if tail := w.w.DataEnd() - next; tail > 0 && !w.recognizedTrailer(next) {
				w.endFinding = true
				if err := w.warn(next, "%d trailing bytes are not frames, dropped", tail); err != nil {
					return false, err
				}
			}
			return false, w.compare()
		}
		if err := w.warn(next, "%d unparsable bytes skipped", cand-next); err != nil {
			return false, err
		}
	}
	if cand+int64(nh.Size()) > w.w.DataEnd() {
		w.done, w.endFinding = true, true
		if err := w.warn(cand, "truncated final frame dropped"); err != nil {
			return false, err
		}
		return false, w.compare()
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

// Complete finishes the walk: the index is extended until it stops growing,
// each finding reported through the owner's hook as it is reached, and the
// first error the hook returns ends it. A run already walked to its end
// returns at once. Nothing is delivered, so the window slides behind the
// walk at one window of residency whatever the run's length.
func (w *Walker) Complete() error {
	for {
		grew, err := w.extend()
		if err != nil {
			return err
		}
		if !grew {
			return nil
		}
	}
}

// Done reports whether the index reaches the end of the run, by a walk or by
// a verified restore of a complete snapshot. A walk the hook refused on its
// last arm (trailing bytes, a truncated final frame) is done too: there is
// nothing past it to index. One refused in the middle of the run is not.
func (w *Walker) Done() bool { return w.done }

// Frames is the number of frames the index holds, the run's whole count
// once Done.
func (w *Walker) Frames() int64 { return int64(len(w.idx)) }

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
