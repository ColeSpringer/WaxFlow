package flacn

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/diag"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/container/internal/trailer"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ container.Demuxer = (*Demuxer)(nil)
	_ container.Seeker  = (*Demuxer)(nil)
	_ container.Warner  = (*Demuxer)(nil)
)

// Hostile-input caps (ADR-0005 invariants).
const (
	// maxMetaBlocks bounds the metadata walk; real files hold a handful,
	// pathological repetition (the IETF suite ships a 1000-block file)
	// stays well under.
	maxMetaBlocks = 4096
	// maxSeekPoints caps the SEEKTABLE allocation; points past the cap
	// only coarsen seeks, so the prefix is kept and the rest skipped.
	maxSeekPoints = 1 << 16
	// maxFrameLen caps a single frame scan. STREAMINFO's frame size field
	// is 24-bit, so no compliant frame can exceed it.
	maxFrameLen = 1 << 24
	// maxResync bounds the scan for a frame sync after damage.
	maxResync = 1 << 20
	// seekWindow is the bisection cutoff: below this span, walk frames.
	seekWindow = 128 << 10
	// maxHeld bounds the frames a boundary scan holds as resync points at
	// once: a damaged frame's successor, and the odd header its payload
	// spells. The oldest goes first.
	maxHeld = 8
)

// DemuxerVersion is this demuxer's term in the ADR-0004 cache key, carried
// by its track. Bump it for a change that makes the demuxer
// deliver other samples for a file that has not changed.
const DemuxerVersion = "flacn-demux-2"

// Metadata block types (RFC 9639 section 8).
const (
	blockStreamInfo = 0
	blockSeekTable  = 3
	blockInvalid    = 127
)

type seekPoint struct {
	sample int64
	off    int64 // relative to the first frame
}

// Demuxer reads one FLAC track from a native FLAC source.
type Demuxer struct {
	src container.Source

	si        flac.StreamInfo
	track     container.Track
	seekTable []seekPoint
	// seekBase is what the SEEKTABLE counts from (tableBase), once read.
	seekBase     int64
	seekBaseRead bool
	log          diag.Log
	// tailDamage is the first damage the open's measurement met.
	tailDamage *container.Warning
	// closingStray is why the closing frame the open found is one no read
	// comes to (strayBehind), empty where one does.
	closingStray string
	// truncated is how far STREAMINFO's total runs past the frames
	// (container.Truncated).
	truncated int64
	residual  []int64 // FrameLen's scratch

	firstFrame int64 // byte offset of the first audio frame
	// headHole is the bytes in front of it that a hole from sample 0 took.
	headHole int64
	// seeking is the context of the seek in flight, nil outside one: its
	// walks and the scans under them stop when it ends (canceled), and cut
	// is the cancel that stopped one.
	seeking context.Context
	cut     error
	// pair is the frame in front of the first that is numbered right behind
	// it: a run held for any walk that stands on the first frame.
	pair aside

	// num maps coded numbers to sample positions (flac.Numbering, latched
	// from the first frame and, where that leaves it open, the frame behind
	// it); varBit is the blocking-strategy header bit, which must not change
	// within a stream.
	num    flac.Numbering
	varBit bool
	// origin is where positions run from 0: the first frame's sample
	// position, whatever number it carries, or 0 when damage took the frames
	// before it. first is that frame's header.
	origin int64
	first  flac.FrameInfo

	off     int64          // offset of the current undelivered frame
	cur     flac.FrameInfo // its parsed header
	valid   bool           // false once the stream is exhausted
	empty   bool           // stream has zero frames (legal: metadata only)
	discont bool           // the current frame follows a hole the read stepped over
	lost    int64          // samples the holes before the current frame took

	// promised is the length the open called exact, where exact says it
	// did: what a decode may be cut at, so what a hole is held to on every
	// pass.
	promised int64
	exact    bool
	// count is what a read from the first frame has delivered, durations and
	// what holes took, while whole says the read counts from there.
	count int64
	whole bool
	// aside is a run of frames read out of their place in the bytes.
	aside aside
	// strays says a scan met frames out of sequence since a walk last
	// cleared it: the walk may have begun among frames numbered ahead.
	strays bool
	// ahead is the end a seek found for the frame at aheadAt, which the read
	// behind it takes rather than finding it again.
	ahead   frameEnd
	aheadAt int64

	// w is the shared read-ahead window; its data end is the end of
	// frame data (trailing tags stripped lazily) and its sticky error
	// surfaces on the packet and seek paths.
	w srcwin.Window
}

// NewDemuxer parses the headers of a native FLAC source and positions on
// the first frame. The returned Demuxer implements container.Seeker and
// container.Warner.
func NewDemuxer(src container.Source, opts *container.DemuxerOptions) (*Demuxer, error) {
	d := &Demuxer{src: src, w: srcwin.New(src, src.Size(), "flac: reading frame data"), log: diag.New("flac: ", opts),
		whole: true, aheadAt: -1}
	if err := d.parse(); err != nil {
		return nil, err
	}
	return d, nil
}

// malformed reports bytes that deviate from this format: truncated,
// inconsistent, or out of range. See [waxerr.Malformed] for the rule that
// divides it from unsupported.
func malformed(format string, args ...any) error {
	return waxerr.Malformed("flac: ", format, args...)
}

// warn records tolerated damage, or fails in strict mode.
func (d *Demuxer) warn(off int64, format string, args ...any) error {
	return d.log.Warn(off, format, args...)
}

// note records a Warning that Strict must not escalate: this build doing
// something with a well-formed file that a caller should know about, rather
// than damage in the file. See [container.Note].
func (d *Demuxer) note(off int64, format string, args ...any) {
	d.log.Note(off, format, args...)
}

func (d *Demuxer) parse() error {
	size := d.src.Size()
	var head [4]byte
	if err := container.ReadFull(d.src, head[:], 0); err != nil {
		return container.ShortRead("flac: reading marker", err)
	}
	if !Match(head[:]) {
		return malformed("not a FLAC file")
	}

	var (
		siRaw   []byte
		haveSI  bool
		off     = int64(4)
		last    = false
		blockNo = 0
	)
	for !last {
		if blockNo++; blockNo > maxMetaBlocks {
			return malformed("more than %d metadata blocks", maxMetaBlocks)
		}
		var hdr [4]byte
		if err := container.ReadFull(d.src, hdr[:], off); err != nil {
			return container.ShortRead("flac: reading metadata block header", err)
		}
		last = hdr[0]&0x80 != 0
		typ := int(hdr[0] & 0x7F)
		length := int64(hdr[1])<<16 | int64(hdr[2])<<8 | int64(hdr[3])
		if off+4+length > size {
			return malformed("metadata block extends past end of file")
		}
		switch typ {
		case blockStreamInfo:
			if haveSI {
				if err := d.warn(off, "duplicate STREAMINFO ignored"); err != nil {
					return err
				}
				break
			}
			if blockNo != 1 {
				if err := d.warn(off, "STREAMINFO is not the first metadata block"); err != nil {
					return err
				}
			}
			if length != flac.StreamInfoLen {
				return malformed("STREAMINFO of %d bytes", length)
			}
			siRaw = make([]byte, flac.StreamInfoLen)
			if err := container.ReadFull(d.src, siRaw, off+4); err != nil {
				return container.ShortRead("flac: reading STREAMINFO", err)
			}
			var err error
			if d.si, err = flac.ParseStreamInfo(siRaw); err != nil {
				return err
			}
			haveSI = true
		case blockSeekTable:
			if err := d.parseSeekTable(off+4, length); err != nil {
				return err
			}
		case blockInvalid:
			return malformed("forbidden metadata block type 127")
		}
		off += 4 + length
	}
	if !haveSI {
		return malformed("no STREAMINFO metadata block")
	}

	d.firstFrame = off

	f := d.si.PCMFormat()
	if err := f.Valid(); err != nil {
		return container.UnusableFormat("flac", f, err)
	}
	samples := d.si.Samples
	if samples == 0 {
		samples = -1
	}
	d.track = container.Track{
		DemuxerVersion: DemuxerVersion,
		Codec:          codec.FLAC,
		CodecConfig:    siRaw,
		Fmt:            f,
		Samples:        samples,
		Default:        true,
	}

	// Latch the first frame: it fixes the blocking strategy and the
	// fixed-strategy constant block size that converts frame numbers to
	// sample positions everywhere else.
	fi, err := flac.ParseFrameHeader(d.w.Peek(off, flac.MaxFrameHeaderLen))
	skipped := int64(0)
	if err != nil || !d.consistent(fi) {
		if d.firstFrame >= d.w.DataEnd() {
			// Zero frames and zero audio bytes: a legal stream (encoding
			// empty input produces exactly this, STREAMINFO total 0 and
			// all). Reads return EOF, seeks land at 0. A declared total
			// with no frames to back it is an inconsistency, though.
			if d.si.Samples != 0 {
				if werr := d.warn(off, "STREAMINFO declares %d samples but the stream has no frames", d.si.Samples); werr != nil {
					return werr
				}
				d.truncated = d.si.Samples
			}
			d.track.Samples = 0
			d.empty = true
			return d.w.Err()
		}
		fOff, ffi, ok := d.nextCandidate(off, off+maxResync)
		if !ok {
			// A read failure looks identical from here (empty scans); it
			// must not be misreported as an unsupported file.
			if d.w.Err() != nil {
				return d.w.Err()
			}
			return malformed("no audio frame after the metadata blocks")
		}
		d.firstFrame, fi, skipped = fOff, ffi, fOff-off
	}
	if d.w.Err() != nil {
		return d.w.Err()
	}
	d.varBit = fi.Variable
	d.num = d.si.Numbering(fi)
	if d.si.NumberingUnsettled(fi) {
		switch whole, next, followed, closing := d.frameBehind(d.firstFrame, fi); {
		case followed:
			d.num = d.si.NumberingFrom(fi, whole, next)
		case closing:
			d.num = d.si.NumberingLast(fi)
		}
	}
	d.origin, d.first = d.num.Start(fi), fi
	if skipped == 0 && d.origin > 0 {
		if end, next, ok := d.pairBehind(fi); ok {
			// The frame due right in front of the first lies behind it: the
			// stream starts at that one, and this one is held for its turn.
			d.pair = aside{ok: true, held: true, off: d.firstFrame, end: end, first: fi, at: -1, from: -1, resume: -1}
			d.firstFrame, fi = end, next
			d.origin, d.first = d.num.Start(fi), fi
		}
	}
	mark, stated := d.log.Mark(), d.track
	if skipped > 0 {
		// Bytes that could hold the frames the first one's number skips are
		// those frames, damaged: a hole from 0, not a stream starting late.
		if lost := d.origin; lost > 0 && lost <= flac.MaxSamplesIn(skipped) {
			if werr := d.warn(off, "%d bytes are not a valid frame, dropped: %d samples lost", skipped, lost); werr != nil {
				return werr
			}
			d.origin, d.lost, d.discont, d.headHole = 0, lost, true, skipped
		} else if werr := d.warn(off, "%d unparsable bytes before the first frame", skipped); werr != nil {
			return werr
		}
	}
	d.off, d.cur, d.valid = d.firstFrame, fi, true
	if err := d.verifyLength(); err != nil || d.headHole == 0 || !d.exact || d.lost < d.promised {
		return err
	}
	// The first frame is numbered at or past where the closing frame ends,
	// so no hole from 0 holds it: the stream starts at it, late.
	d.log.Rollback(mark)
	d.track, d.truncated, d.tailDamage = stated, 0, nil
	d.origin, d.lost, d.discont, d.headHole = d.num.Start(fi), 0, false, 0
	if werr := d.warn(off, "%d unparsable bytes before the first frame", skipped); werr != nil {
		return werr
	}
	return d.verifyLength()
}

// pairBehind is where the first frame fi ends, whole by its own parse, and
// the header there, where that is the frame numbered right in front of it.
// It reads as frameBehind does; parse asks it of a stream numbered from
// past 0 with nothing skipped in front, which is all it can answer for.
func (d *Demuxer) pairBehind(fi flac.FrameInfo) (end int64, next flac.FrameInfo, ok bool) {
	// A look that strengthens a claim: a read that fails is no answer.
	ok, _ = d.w.Try(d.w.DataEnd()-d.firstFrame <= srcwin.Chunk, func() bool {
		b := d.w.BytesAt(d.firstFrame, srcwin.Chunk)
		n, err := flac.FrameLen(b, d.si, &d.residual)
		if err != nil {
			return false
		}
		end = d.firstFrame + int64(n)
		next, err = flac.ParseFrameHeader(b[n:min(len(b), n+flac.MaxFrameHeaderLen)])
		return err == nil && next.Variable == fi.Variable && d.consistent(next) && d.num.Next(next) == fi.Coded
	})
	return end, next, ok
}

// frameBehind is the header behind first, the frame at off, or closing at the
// data's end; where first does not check, the header behind whole, the first
// frame in the window that does. A failed read is no answer, not an error.
func (d *Demuxer) frameBehind(off int64, first flac.FrameInfo) (whole, next flac.FrameInfo, followed, closing bool) {
	whole = first
	// A look that strengthens a claim: a read that fails is no answer.
	ok, _ := d.w.Try(d.w.DataEnd()-off <= srcwin.Chunk, func() bool {
		b := d.w.BytesAt(off, srcwin.Chunk)
		n, err := flac.FrameLen(b, d.si, &d.residual)
		switch {
		case err == nil && off+int64(n) == d.w.DataEnd():
			closing = true
			return true
		case err != nil:
			at, ok := 0, false
			if at, n, whole, ok = d.wholeIn(b, first); !ok {
				return true
			}
			b = b[at:]
		}
		next, err = flac.ParseFrameHeader(b[n:min(len(b), n+flac.MaxFrameHeaderLen)])
		followed = err == nil
		return true
	})
	if !ok {
		return first, flac.FrameInfo{}, false, false
	}
	return whole, next, followed, closing
}

// wholeIn finds in b, behind the frame at its start, the first frame of
// first's blocking strategy and STREAMINFO's format that checks whole,
// trying at most maxHeld headers that parse.
func (d *Demuxer) wholeIn(b []byte, first flac.FrameInfo) (at, n int, fi flac.FrameInfo, ok bool) {
	for at, tries := 1, 0; at+1 < len(b) && tries < maxHeld; at++ {
		if b[at] != 0xFF || b[at+1]&0xFE != 0xF8 {
			continue
		}
		h, err := flac.ParseFrameHeader(b[at:min(len(b), at+flac.MaxFrameHeaderLen)])
		if err != nil || h.Variable != first.Variable || !d.consistent(h) {
			continue
		}
		tries++
		if n, err = flac.FrameLen(b[at:], d.si, &d.residual); err == nil {
			return at, n, h, true
		}
	}
	return 0, 0, first, false
}

// verifyLength settles the track's length against the frames the file actually
// carries (flac.Settle), so a length the probe reports is a length a read
// delivers. An intact file settles from its closing frame alone; see
// deliverableEnd for what a damaged one costs. Only a measurement that met no
// damage on the way is exact: format.Media caps a decode at an exact length, so
// a number found by walking past damage would become a hard end.
func (d *Demuxer) verifyLength() error {
	if d.empty {
		return nil
	}
	end, ok, clean := d.deliverableEnd()
	if w := d.tailDamage; !clean && w != nil {
		// What the measurement met, reported where a read will meet it too:
		// a strict open refuses it, and a tolerant one lists it once.
		if err := d.warn(w.Offset, "%s", w.Msg); err != nil {
			return err
		}
	}
	if err := d.w.Err(); err != nil {
		return err
	}
	span := int64(-1)
	if ok && end >= d.origin {
		span = end - d.origin
	}
	// A hole at the head is in the span, so the bytes it took count.
	if n := d.w.DataEnd() - d.firstFrame + d.headHole + d.pair.end - d.pair.off; span > flac.MaxSamplesIn(n) {
		if err := d.warn(d.firstFrame, "the frames claim %d samples, more than %d bytes of frames can hold", span, n); err != nil {
			return err
		}
		span = -1
	}
	st, err := flac.Settle(d.track.CodecConfig, d.origin, span)
	if err != nil {
		return err
	}
	for _, n := range st.Notes {
		d.note(d.firstFrame, "%s", n)
	}
	if st.Damage != "" {
		if err := d.warn(d.firstFrame, "%s", st.Damage); err != nil {
			return err
		}
	}
	d.track.CodecConfig, d.track.Samples = st.StreamInfo, st.Samples
	d.track.SamplesExact = st.Measured && clean
	d.truncated = st.Truncated
	// A closing frame no read comes to, numbered inside the frames in front
	// of it or out of a resync's reach behind them, is out of sequence to
	// every read: none is held to that length. One that ends at or before
	// where the first frame starts is parse's to settle.
	if d.track.SamplesExact && d.closingStray != "" && d.track.Samples > d.num.Start(d.first)-d.origin {
		d.note(d.firstFrame, "the closing frame ends at sample %d, %s; the length is not exact", d.track.Samples, d.closingStray)
		d.track.SamplesExact = false
	}
	d.promised, d.exact = d.track.Samples, d.track.SamplesExact
	return nil
}

// deliverableEnd returns the sample index one past the last frame a read can
// deliver, which is what verifies STREAMINFO's declared total.
//
// An undamaged stream is settled by tailBacks without any of the walking
// below, and that is the whole cost in the common case. Only a file whose tail
// will not confirm pays for the measurement, which bisects to the closing
// window and then advances frame by frame, since only a checksummed boundary
// proves a frame is whole. A last frame whose end never confirms is the one
// the file stops inside, and the audio ends where that frame began.
//
// The pass is a measurement and leaves nothing behind: the warnings it records
// are dropped, the trailers findEnd confirms and peels are unpeeled, the
// position is put back, and strict mode is off throughout. All of that is
// read-time damage which a read reports when it reaches it, and which strict
// mode refuses there. Were any of it left standing, a tagged file would be
// refused at open and the peel would consume the very damage the read is meant
// to find. The one thing this pass may object to is the length, which
// verifyLength warns about itself.
//
// clean reports that the pass met no damage. The warnings it dropped are still
// the read path's to report, but whether there were any is the caller's to
// know: a length walked past damage is a best answer rather than an
// authoritative one.
//
// The walk steps over a hole as a read does, so the end it finds is where a
// read's frames end.
func (d *Demuxer) deliverableEnd() (end int64, ok, clean bool) {
	dataEnd := d.w.DataEnd()
	off0, cur0, valid0, aside0 := d.off, d.cur, d.valid, d.aside
	d.aside = aside{}
	quiet, first := d.log.Probe(func() { end, ok, clean = d.measureEnd() })
	if first != nil {
		d.tailDamage = first
	}
	d.w.SetDataEnd(dataEnd)
	// The walk ends past the audio; reads start where it began.
	d.off, d.cur, d.valid, d.aside = off0, cur0, valid0, aside0
	return end, ok, clean && quiet
}

// measureEnd is deliverableEnd's pass, which the caller undoes.
func (d *Demuxer) measureEnd() (end int64, ok, clean bool) {
	d.closingStray = ""
	if end, ok := d.tailBacks(); ok {
		return end, true, true
	}
	off, fi, found := d.bisect(math.MaxInt64) // no target: land on the last window
	if !found {
		return 0, false, false
	}
	end, ok, clean, _, again := d.walkEnd(off, fi, off != d.firstFrame, off != d.firstFrame)
	// What lies behind the walk's first frame may be placed from in front of
	// it: as a seek does, the walk starts again further back, and counts
	// where it comes past the frame the first began on.
	for back := int64(fi.BlockSize); again; back *= 2 {
		from := max(d.num.Start(fi)-back, d.origin)
		o, f, found := d.bisect(from)
		if !found {
			break
		}
		e, k, c, last, unsure := d.walkEnd(o, f, from != d.origin, false)
		if unsure {
			continue
		}
		if last > off {
			return e, k, c
		}
		break
	}
	if again {
		end, ok, clean, _, _ = d.walkEnd(off, fi, false, false)
	}
	return end, ok, clean
}

// walkEnd walks from the frame at off to the stream's end and returns the
// sample index one past the last frame it places, with where that frame
// lies. again says the walk stopped: at its first frame, unanchored, where
// retry is set, or among frames out of sequence, where strays is.
func (d *Demuxer) walkEnd(off int64, fi flac.FrameInfo, retry, strays bool) (end int64, ok, clean bool, last int64, again bool) {
	d.off, d.cur, d.valid, d.aside, d.strays = off, fi, true, aside{}, false
	for first := true; ; first = false {
		d.w.Trim(d.trimTo())
		fe, err := d.findEnd()
		if err != nil || d.w.Err() != nil {
			return 0, false, false, 0, false
		}
		switch {
		case retry && first && d.unanchored(fe), strays && d.strays:
			return 0, false, false, 0, true
		case !fe.nextOK && fe.end < 0:
			// Stops inside this frame: the answer is where the audio ends and
			// the file is damaged, which is exactly the pair the flag splits.
			return d.num.Start(d.cur), true, false, d.off, false
		case !fe.nextOK:
			return d.num.Start(d.cur) + int64(d.cur.BlockSize), true, true, d.off, false // clean end
		}
		d.off, d.cur = fe.resync, fe.next
	}
}

// unanchored reports whether fe, the end of the first frame of a walk that began
// part way, is one a read from further back may place otherwise: the numbers
// skip behind the frame, or a run was taken aside there.
func (d *Demuxer) unanchored(fe frameEnd) bool {
	return fe.end >= 0 && fe.nextOK && (d.aside.ok && d.aside.at == d.off ||
		!fe.ordered && d.num.Start(fe.next) > d.num.Start(d.cur)+int64(d.cur.BlockSize))
}

// tailBacks finds the stream's closing frame in the tail window and returns the
// sample index one past it, cheaply enough for every undamaged file to pay it
// and nothing more.
//
// Scanning backwards from the data end, the first candidate whose frame
// checksums to the end of the data with nothing after it is the last frame:
// only the real thing gets both halves right. consistent rejects most of the
// sync bytes a payload spells without checksumming anything, and findEnd
// confirms the boundary by CRC-16 and chains the frame number, so a false sync
// inside the last frame's payload does not survive.
//
// It does not consult STREAMINFO's total, which is what lets it settle a
// streaming encoder's file (total 0) and a file whose frames outrun its total,
// not only one that matches. A false negative is harmless: it falls through to
// the measurement, which is exact. So this may only ever answer "here is the
// end", never "truncated".
//
// maxTailCRCs is what makes the cost a bound rather than a hope. A candidate
// that consistent lets through costs a CRC over the rest of the window, and a
// hostile file chooses both the headers it plants and the STREAMINFO they are
// checked against, so nothing in the data limits how many there are. Scanning
// backwards, the real closing frame is the first candidate to confirm: every
// FLAC in the corpus finds it on attempt one. Past the cap this gives up and
// the exact walk takes over, which is bounded by the payload.
// tailCRCBudget is maxTailCRCs, as a variable so a test can turn it down and
// exercise the fall-through without crafting a tail to exhaust it.
var tailCRCBudget = maxTailCRCs

// maxTailCRCs bounds the frame checksums the cheap tail confirmation may pay
// before giving up; see tailBacks. Every FLAC in the corpus confirms on the
// first, so the room above that is for a file with an unusual tail rather than
// for a hostile one.
const maxTailCRCs = 64

func (d *Demuxer) tailBacks() (int64, bool) {
	tries := 0
	lo := max(d.firstFrame, d.w.DataEnd()-seekWindow)
	for hi := d.w.DataEnd(); hi > lo; {
		from := max(lo, hi-srcwin.Chunk)
		buf := d.w.BytesAt(from, int(hi-from))
		if len(buf) == 0 {
			return 0, false
		}
		for rel := len(buf); ; {
			i := bytes.LastIndexByte(buf[:rel], 0xFF)
			if i < 0 {
				break
			}
			rel = i
			cand := from + int64(i)
			hdr := d.w.BytesAt(cand, flac.MaxFrameHeaderLen)
			if !flac.SyncOK(hdr) {
				continue
			}
			fi, err := flac.ParseFrameHeader(hdr)
			if err != nil || !d.consistent(fi) {
				continue
			}
			if tries++; tries > tailCRCBudget {
				return 0, false
			}
			d.off, d.cur, d.valid, d.aside = cand, fi, true, aside{}
			// A complete frame with nothing after it: findEnd peels any
			// trailing tag on the way, which the caller unpeels. What a
			// rejected candidate reported is not the file's.
			mark := d.log.Mark()
			if fe, err := d.findEnd(); err == nil && fe.end >= 0 && !fe.nextOK {
				d.closingStray = d.strayBehind(from, cand, fi)
				return d.num.Start(fi) + int64(fi.BlockSize), true
			}
			d.log.Rollback(mark)
		}
		if from == lo {
			break // the whole window is scanned
		}
		hi = from + 1 // overlap by one, for a sync byte on the chunk edge
	}
	return 0, false
}

// strayBehind is why the frame fi at cand, the last in the data, is one no
// read comes to, empty where one does: a walk from the nearest whole frame
// in front that it could follow, as a read steps, must stand on it: from
// the first frame where none in front is whole. With frames further in
// front than a resync reaches and none whole between, no read comes to it.
//
// The bytes the tail scan holds from from answer a healthy stream, or one
// read back to where the largest frame could start. Damage in front reads a
// window at a time. The parses are given twice the bytes the search can
// hold, once for the frames found and once for headers with none behind.
func (d *Demuxer) strayBehind(from, cand int64, fi flac.FrameInfo) string {
	near := max(d.firstFrame, cand-d.frameReach())
	floor := max(d.firstFrame, near-maxResync)
	budget := 2 * (cand - floor)
	// before is the nearest whole frame that ends at or before end, of
	// those the bytes back to floor hold.
	before := func(end int64) (int64, flac.FrameInfo, bool) {
		for below := end; ; {
			buf := d.w.Back(from, int(end-from))
			if int64(len(buf)) < end-from {
				return 0, flac.FrameInfo{}, false // a read failed, which the open reports
			}
			if at, p, ok := d.wholeBefore(buf, int(below-from), &budget); ok {
				return from + int64(at), p, true
			}
			if budget < 0 || from <= floor {
				return 0, flac.FrameInfo{}, false
			}
			if below = from; from > near {
				from = near
			} else {
				from = max(floor, from-srcwin.Chunk)
			}
		}
	}
	at, p, ok := before(cand)
	switch {
	case ok:
	case budget < 0 || d.w.Err() != nil:
		return ""
	case floor > d.firstFrame:
		return "with no whole frame in front of it as far as a resync reaches"
	default:
		// None whole in front at all: a read starts on the first frame.
		at, p = d.firstFrame, d.first
	}
	// The frame it could follow, or the furthest one back the bytes hold.
	why := "and no read from the frame in front of it comes to it"
	for ok && fi.Coded < d.num.Next(p) {
		why = "before the frame in front of it does"
		a, q, ok := before(at)
		if !ok {
			break
		}
		at, p = a, q
	}
	// The walk is the read's: it steps as findEnd does, a run held on the
	// way, and what it finds is not the open's.
	mark := d.log.Mark()
	defer func() { d.log.Rollback(mark) }()
	d.off, d.cur, d.valid, d.aside = at, p, true, aside{}
	// Each step takes a frame of the bytes between, which bounds them.
	for steps := (cand-at)/8 + 64; d.off < cand && steps > 0; steps-- {
		fe, err := d.findEnd()
		if err != nil || !fe.nextOK {
			break
		}
		d.off, d.cur = fe.resync, fe.next
	}
	if d.off == cand {
		return ""
	}
	return why
}

// wholeBefore finds the nearest frame in b that is whole by its own parse
// and starts before below, the bytes each parse reads taken off budget.
func (d *Demuxer) wholeBefore(b []byte, below int, budget *int64) (at int, fi flac.FrameInfo, ok bool) {
	for rel := below; *budget >= 0; {
		i := bytes.LastIndexByte(b[:rel], 0xFF)
		if i < 0 {
			break
		}
		rel = i
		p, err := flac.ParseFrameHeader(b[i:min(len(b), i+flac.MaxFrameHeaderLen)])
		if !flac.SyncOK(b[i:]) || err != nil || p.Variable != d.varBit || !d.consistent(p) {
			continue
		}
		_, read, err := flac.FrameLenRead(b[i:], d.si, &d.residual)
		if *budget -= int64(read); err == nil {
			return i, p, true
		}
	}
	return 0, flac.FrameInfo{}, false
}

// parseSeekTable reads up to maxSeekPoints seek points, dropping
// placeholders and non-monotonic entries.
func (d *Demuxer) parseSeekTable(off, length int64) error {
	if length%18 != 0 {
		if err := d.warn(off, "SEEKTABLE length %d is not whole seek points", length); err != nil {
			return err
		}
	}
	points := min(length/18, maxSeekPoints)
	raw := make([]byte, points*18)
	if err := container.ReadFull(d.src, raw, off); err != nil {
		return container.ShortRead("flac: reading SEEKTABLE", err)
	}
	d.seekTable = make([]seekPoint, 0, points)
	for i := range points {
		b := raw[i*18:]
		sample := binary.BigEndian.Uint64(b)
		if sample == 1<<64-1 {
			continue // placeholder point
		}
		p := seekPoint{sample: int64(sample), off: int64(binary.BigEndian.Uint64(b[8:]))}
		if p.sample < 0 || p.off < 0 ||
			(len(d.seekTable) > 0 && p.sample <= d.seekTable[len(d.seekTable)-1].sample) {
			if err := d.warn(off+i*18, "invalid seek point dropped"); err != nil {
				return err
			}
			continue
		}
		d.seekTable = append(d.seekTable, p)
	}
	return nil
}

// Tracks returns the single FLAC track.
func (d *Demuxer) Tracks() []container.Track { return []container.Track{d.track} }

// Warnings returns damage tolerated during parsing.
func (d *Demuxer) Warnings() []container.Warning { return d.log.List() }

// DamageCount implements container.Damaged.
func (d *Demuxer) DamageCount() int { return d.log.DamageCount() }

// TruncatedSamples implements container.Truncated.
func (d *Demuxer) TruncatedSamples(int) int64 { return d.truncated }

// consistent reports whether a frame header agrees with STREAMINFO; the
// pipeline's track format is fixed, and consistency is also what makes
// sync-candidate scanning trustworthy.
func (d *Demuxer) consistent(fi flac.FrameInfo) bool {
	rate, bits := fi.Rate, fi.Bits
	if rate == 0 {
		rate = d.si.Rate
	}
	if bits == 0 {
		bits = d.si.Bits
	}
	return rate == d.si.Rate && bits == d.si.Bits && fi.Channels == d.si.Channels
}

// nextCandidate scans [from, limit) for a sync candidate whose header parses
// and agrees with STREAMINFO. It positions the open behind junk and lands a
// bisection probe; a packet boundary takes findEnd's stronger confirmation.
func (d *Demuxer) nextCandidate(from, limit int64) (int64, flac.FrameInfo, bool) {
	var fi flac.FrameInfo
	off, ok := d.w.Scan(from, limit, []byte{0xFF}, flac.MaxFrameHeaderLen, func(_ int64, hdr []byte) srcwin.Verdict {
		if !flac.SyncOK(hdr) {
			return srcwin.Skip
		}
		got, err := flac.ParseFrameHeader(hdr)
		if err != nil || !d.consistent(got) {
			return srcwin.Skip
		}
		fi = got
		return srcwin.Take
	})
	return off, fi, ok
}

// probe is nextCandidate over a range of any length, a piece at a time, the
// window trimmed to each piece's start so it holds one. A candidate at a
// piece's end carries its view into the next, so the answer is one scan's.
// A frame starts within the largest frame's bytes of from (frameReach): one
// read holds them and a header, and they are scanned first.
func (d *Demuxer) probe(from, limit, piece int64) (int64, flac.FrameInfo, bool) {
	limit = min(limit, d.w.DataEnd())
	for at := from; at < limit && d.canceled() == nil; at += piece {
		d.w.Trim(at)
		scan, end := at, min(at+piece, limit)
		switch {
		case at > from:
			// A whole piece held nothing: read on a window at a time, the
			// rate its scan had reached.
			d.w.Peek(at, int(min(srcwin.Chunk, limit-at+flac.MaxFrameHeaderLen-1)))
		default:
			scan = min(at+d.frameReach()+1, end)
			d.w.Peek(at, int(scan-at+flac.MaxFrameHeaderLen-1))
			if off, fi, ok := d.nextCandidate(at, scan); ok || d.w.Err() != nil {
				return off, fi, ok
			}
			// No frame where one must start: read on as much again, the
			// rate a scan of those bytes would have reached.
			d.w.Peek(scan, int(min(scan-at, srcwin.Chunk, limit-scan+flac.MaxFrameHeaderLen-1)))
		}
		if off, fi, ok := d.nextCandidate(scan, end); ok || d.w.Err() != nil {
			return off, fi, ok
		}
	}
	return 0, flac.FrameInfo{}, false
}

// frameReach is the most bytes a frame takes: what its largest block codes
// verbatim, a header, a footer and each channel's samples a bit over their
// depth, or the largest frame STREAMINFO states where that is less. A
// stated size past it is not believed, nor a block size no stream has.
func (d *Demuxer) frameReach() int64 {
	block := int64(d.si.MaxBlock)
	if block < 16 || d.si.MinBlock > d.si.MaxBlock {
		block = flac.MaxBlockSize
	}
	reach := flac.MaxFrameHeaderLen + 2 + int64(d.si.Channels)*(1+(int64(d.si.Bits+1)*block+7)/8)
	if d.si.MaxFrame > 0 {
		reach = min(reach, int64(d.si.MaxFrame))
	}
	return reach
}

// frameEnd is where findEnd settled the frame at d.off.
type frameEnd struct {
	end    int64          // the frame's end; -1 when its bytes do not check
	next   flac.FrameInfo // the header of the frame after it, when nextOK
	nextOK bool
	resync int64 // where that frame starts: end, or past damage
	lost   int64 // the samples lost before it, the frame at d.off's too when end < 0
	// ordered: the bytes between end and resync are frames read in order
	// elsewhere, so the next frame does not follow damage.
	ordered bool
}

// aside is a run of whole frames numbered ahead of the frame behind them:
// held until the read would pass its numbers, or, numbered off the gap in
// front of that frame, read there with its numbers shifted.
type aside struct {
	ok, held bool
	off, end int64          // the run's bytes
	first    flac.FrameInfo // its first frame, as read
	shift    int64          // added to the run's coded numbers
	// at is the frame the run was found behind, and from where that scan
	// began (found.from), so that asked again it finds the same.
	at, from int64
	// resume is where the bytes behind the frame a held run is read behind
	// lie: the read goes on there by the run's last numbers. -1 until then.
	resume int64
}

// boundary is the frame at d.off ending at end, where the frame next starts.
func boundary(end int64, next flac.FrameInfo) frameEnd {
	return frameEnd{end: end, next: next, nextOK: true, resync: end}
}

// last is the frame at d.off ending the stream at end, or a damaged tail at -1.
func last(end int64) frameEnd { return frameEnd{end: end, resync: end} }

// pending is a frame start whose end the scan has yet to find, with the
// checksum of its bytes so far.
type pending struct {
	off    int64
	fi     flac.FrameInfo
	crc    uint16 // over [off, crcPos)
	crcPos int64
	// bare: no frame starts at off, a whole one ends there, so the bytes
	// from off check from nothing and a frame may start at off itself.
	bare bool
}

// found is what a scan for the end of the frame at d.off holds so far.
type found struct {
	// from is where the scan began where that is not the frame's own bytes
	// (scanEnd), else -1.
	from int64
	own  int64 // where the frame ends by its own parse, once found, else -1
	// stray is where a frame numbered before the successor begins with the
	// frame's bytes checking in front of it: the frame ends there, and what
	// lies behind is frames out of sequence. -1 for none.
	stray int64
	// far, once waiting, is the first frame the read could go on to with
	// the numbers skipping: the scan looks on for a frame numbered in the
	// gap, and every answer it then gives reads far or settles its run. run
	// is the last frame numbered on from far, dur the samples from far to
	// run. jump says far starts where the frame ends, where the read always
	// took the skip at once. strays says frames out of sequence lie between,
	// where it always waited for the successor: first is then the first
	// other frame numbered in the gap, kept for the scan's end. past is
	// where the first frame numbered past the run lies behind it, out of
	// run, else -1.
	far, run, first                 pending
	dur, past                       int64
	waiting, jump, strays, hasFirst bool
}

// findEnd locates the end of the current frame at d.off: the next sync
// candidate that parses consistently, carries the expected position, and
// where the bytes so far checksum as a complete frame (CRC-16). At end
// of data the final span must checksum likewise; recognized trailers
// (ID3v1, APEv2, appended ID3v2, NUL padding) are peeled and retried
// before the tail is declared damaged. end < 0 with a nil error reports
// a tolerantly dropped tail, or, with nextOK set, a damaged frame.
//
// A frame whose bytes do not check gives way to the first consistent frame
// after it that does and that the stream could have, making it a hole: one
// warning, and the read goes on from there. A later-numbered header where the
// frame does check is a gap in the numbers, not damage, and is passed over
// (see cut.go's FLAC row).
//
// A run taken aside is read by where the read stands, so a frame asked
// again answers the same: in the run, in front of a held run's numbers, or
// at the frame the run was found behind, which finds it again.
func (d *Demuxer) findEnd() (frameEnd, error) {
	if p := d.pair; p.ok && !d.aside.ok && d.off == d.firstFrame {
		// The frame in front of the first is held as a run found behind
		// any frame is, and read behind it.
		if err := d.warn(p.off, "%d bytes hold frames numbered ahead of the one behind them, read in order", p.end-p.off); err != nil {
			return frameEnd{}, err
		}
		d.aside = p
	}
	switch a := d.aside; {
	case !a.ok:
	case d.off == a.at:
		d.aside = aside{}
		if a.from < 0 {
			break
		}
		if n, whole := d.ownEnd(d.w.DataEnd()); whole {
			return d.scanFrom(a.from, d.off+int64(n))
		}
	case d.off >= a.off && d.off < a.end:
		return d.inRun()
	case a.held && d.off >= a.end && d.cur.Coded < a.first.Coded:
		return d.inFrontOfRun()
	}
	return d.scanEnd(-1, false)
}

// inRun is findEnd for a frame of a run taken aside: the run's next frame,
// or behind its last what follows by the run's numbers, where a run read at
// its gap lies or in the bytes a held one was read in front of.
func (d *Demuxer) inRun() (frameEnd, error) {
	a := d.aside
	n, whole := d.ownEnd(a.end)
	end := d.off + int64(n)
	switch {
	case !whole:
		d.aside = aside{} // a read failed: the run is not what it was taken for
		return d.scanEnd(-1, false)
	case end == a.end:
		return d.scanFrom(a.resume, end)
	}
	fi, err := flac.ParseFrameHeader(d.w.BytesAt(end, flac.MaxFrameHeaderLen))
	if err != nil {
		d.aside = aside{}
		return d.scanEnd(-1, false)
	}
	fi.Coded = uint64(int64(fi.Coded) + a.shift)
	return frameEnd{end: end, next: fi, nextOK: true, resync: end, ordered: true}, nil
}

// inFrontOfRun is findEnd for a frame read in front of a held run's
// numbers. The run is read behind it unless a frame numbered before the run
// follows, so no answer passes the run by: a frame gone, damage, another
// skip and the stream's end all read it next.
func (d *Demuxer) inFrontOfRun() (frameEnd, error) {
	a := &d.aside
	mark, dataEnd := d.log.Mark(), d.w.DataEnd()
	fe, err := d.scanEnd(-1, true)
	if err == nil && fe.nextOK && fe.next.Coded < a.first.Coded {
		return fe, nil
	}
	if d.w.Err() != nil {
		return frameEnd{}, d.w.Err()
	}
	// What that scan found lies behind the run in the numbers: it is found
	// again from there, by the run's last frame.
	d.log.Rollback(mark)
	d.w.SetDataEnd(dataEnd)
	if err != nil {
		fe.end = -1
		if n, whole := d.ownEnd(d.w.DataEnd()); whole {
			fe.end = d.off + int64(n)
		}
	}
	// A frame that does not check is left where it lies, for that scan.
	from, to := d.num.Start(d.cur)-d.origin, d.num.Start(a.first)-d.origin
	a.resume = d.off
	if fe.end >= 0 {
		from, a.resume = from+int64(d.cur.BlockSize), fe.end
	}
	lost := max(to-from, 0)
	if lost > 0 {
		if err := d.warn(a.off, "the frames skip from sample %d to %d: %d samples lost", from, to, lost); err != nil {
			return frameEnd{}, err
		}
	}
	return frameEnd{end: fe.end, next: a.first, nextOK: true, resync: a.off, ordered: true, lost: lost}, nil
}

// scanFrom is scanEnd for a frame that ends at end, whole, with what follows
// it lying at from: the end the scan names is put back where the frame lies.
func (d *Demuxer) scanFrom(from, end int64) (frameEnd, error) {
	fe, err := d.scanEnd(from, false)
	if from >= 0 && err == nil {
		fe.ordered = fe.ordered || fe.end == fe.resync
		fe.end = end
	}
	return fe, err
}

// scanEnd is findEnd's scan of the bytes behind the frame at d.off. With
// from not negative the frame is whole and what follows it lies at from, as
// behind a held run. nowait answers a skip in the numbers at once, as while
// a run is held: one run is aside at a time.
func (d *Demuxer) scanEnd(from int64, nowait bool) (frameEnd, error) {
	start := d.off
	cur := pending{off: start, fi: d.cur, crcPos: start}
	s := found{from: from, own: -1, stray: -1, past: -1}
	// A boundary needs a header and CRC-16 before it, so the scan starts past
	// our own header's fixed bytes. Its first view starts at the frame's start
	// all the same: checksTo reads back to there, and one read holds both.
	scan, lead := start+4, int64(4)
	if from >= 0 {
		start, s.own, scan, lead = from, from, from, 0
		cur = pending{off: from, fi: d.cur, crcPos: from, bare: true}
	}
	succ := d.num.Next(d.cur)
	var held [maxHeld]pending
	nHeld := 0

	for {
		if err := d.canceled(); err != nil {
			return frameEnd{}, err
		}
		if s.waiting && s.jump && scan-s.far.off > maxResync {
			return d.skip(s.far, s.own)
		}
		if scan-start > maxFrameLen {
			if s.waiting {
				return d.waitOver(&s)
			}
			// No frame is this long. One that is whole by its own parse
			// ends here, and what lies behind it is a tail no scan confirms.
			if d.ownTo(&s, start+maxFrameLen) {
				msg := "%d trailing bytes are not a valid frame, dropped"
				if d.exact && d.num.Start(d.cur)+int64(d.cur.BlockSize)-d.origin < d.promised {
					// The closing frame the open found lies further on.
					msg = "%d bytes behind the frame hold no frame within reach, dropped short of the closing frame"
				}
				if werr := d.warn(s.own, msg, d.w.DataEnd()-s.own); werr != nil {
					return frameEnd{}, werr
				}
				return last(s.own), nil
			}
			return frameEnd{}, malformed("frame at offset %d exceeds %d bytes", d.log.At(start), int64(maxFrameLen))
		}
		// A view is a window, the last one cut at the scan's reach: four
		// bytes past the frame bound.
		buf := d.w.Ahead(scan-lead, int(min(srcwin.Chunk, start+4+maxFrameLen-scan+lead)))
		if int64(len(buf)) <= lead {
			break // end of data
		}
		buf, lead = buf[lead:], 0
		rel := 0
		for {
			i := bytes.IndexByte(buf[rel:], 0xFF)
			if i < 0 {
				break
			}
			cand := scan + int64(rel+i)
			rel += i + 1
			if cand+2 > d.w.DataEnd() {
				continue
			}
			hdr := d.w.BytesAt(cand, flac.MaxFrameHeaderLen)
			if !flac.SyncOK(hdr) {
				continue
			}
			fi, perr := flac.ParseFrameHeader(hdr)
			if perr != nil || fi.Variable != d.varBit {
				continue
			}
			// A frame the read could go on from: the successor or, past
			// damage, any consistent frame numbered after it.
			fits := fi.Coded >= succ && d.consistent(fi)
			h := pending{off: cand, fi: fi, crcPos: cand}
			if s.waiting {
				// Behind far only a frame numbered in the gap in front of
				// it, once confirmed, changes the answer.
				if s.jump && cand-s.far.off > maxResync {
					return d.skip(s.far, s.own) // no frame in reach carries on in front of it
				}
				if d.carriesOn(&s, h) {
					continue
				}
				for j := range nHeld {
					if fi.Coded != d.num.Next(held[j].fi) || !d.checksTo(&held[j], cand) {
						continue
					}
					if fe, ok, err := d.confirmed(&s, held[j]); ok || err != nil {
						return fe, err
					}
					nHeld = copy(held[:], append(held[:j:j], held[j+1:nHeld]...))
					break
				}
				inGap := fits && fi.Coded < s.far.fi.Coded
				if !inGap && fits && fi.Coded > s.run.fi.Coded && s.past < 0 {
					s.past = cand
				}
				switch chained := fits && d.checksTo(&s.run, cand); {
				case chained && inGap && s.strays && fi.Coded != succ:
					if !s.hasFirst {
						s.first, s.hasFirst = h, true
					}
				case chained && inGap:
					if fe, ok, err := d.settle(&s, h, true); ok || err != nil {
						return fe, err
					}
				case chained:
					if s.jump && d.vouched(d.num.Start(s.far.fi)-d.origin) {
						return d.skip(s.far, s.own) // no frame numbered in the gap lies before this one
					}
				case inGap || s.strays && fits:
					// Behind frames out of sequence any frame behind damage
					// was read on from, as in front of far.
					if nHeld == maxHeld {
						copy(held[:], held[1:])
						nHeld--
					}
					held[nHeld] = h
					nHeld++
				}
				continue
			}
			if fi.Coded < succ {
				if s.stray < 0 && d.consistent(fi) && d.checksTo(&cur, cand) {
					s.stray, d.strays = cand, true
				}
				continue
			}
			checks := (fi.Coded == succ || fits) && d.checksTo(&cur, cand)
			if checks && fi.Coded == succ {
				// Confirmed boundary. A frame here that disagrees with
				// STREAMINFO is a mid-stream format change: not damage but
				// a stream shape the fixed-format pipeline rejects (RFC
				// 9639 permits this), so it is a hard error either mode.
				if !d.consistent(fi) {
					return frameEnd{}, malformed("mid-stream format change at offset %d", d.log.At(cand))
				}
				if s.stray < 0 {
					return boundary(cand, fi), nil
				}
				// Frames numbered before the successor are dropped, not
				// audio it follows.
				if err := d.warn(s.stray, outOfSequence, cand-s.stray); err != nil {
					return frameEnd{}, err
				}
				return frameEnd{end: s.stray, next: fi, nextOK: true, resync: cand, ordered: true}, nil
			}
			if checks {
				// The bytes check across a gap in the numbers. Where this
				// frame ends here by its own parse, the frames are whole and
				// only the numbers skip. Where it ends short with no frame
				// there, what lies between is damage that checks, as zeroed
				// bytes do (CRC-16 starts from zero).
				if s.own < 0 && s.stray >= 0 {
					s.own = s.stray
				}
				switch {
				case !d.ownTo(&s, cand):
				case s.own == cand || s.own == s.stray:
					// The numbers skip, with only frames out of sequence in
					// front: taken as a gap unless a frame numbered in it
					// follows within a resync's reach.
					// Behind a held run the frames read in front of it were
					// such frames to a read that took the run first.
					s.far, s.run, s.dur, s.waiting = h, h, int64(fi.BlockSize), true
					s.jump = s.own == cand && s.from < 0
					s.strays = !s.jump
				case s.own < cand:
					fe, isFar, err := d.holeFrom(s.own, h, false)
					if !isFar {
						return fe, err
					}
					s.far, s.run, s.dur, s.waiting = h, h, int64(fi.BlockSize), true
				}
			}
			for j := 0; j < nHeld && !s.waiting; j++ {
				if fi.Coded != d.num.Next(held[j].fi) || !d.checksTo(&held[j], cand) {
					continue
				}
				fe, isFar, err := d.holeAt(&s, held[j], false)
				if !isFar {
					return fe, err
				}
				// What was held behind far lies inside it, or behind it with
				// this frame, which is past the run where it does not carry on.
				s.far, s.run, s.dur, s.waiting = held[j], held[j], int64(held[j].fi.BlockSize), true
				if nHeld = j; !d.carriesOn(&s, h) {
					s.past = cand
				}
			}
			switch {
			case s.waiting && nowait:
				return d.toFar(&s)
			case s.waiting:
			case fits && !checks:
				if nHeld == maxHeld {
					copy(held[:], held[1:])
					nHeld--
				}
				held[nHeld] = h
				nHeld++
			}
		}
		scan += int64(len(buf))
	}
	if d.w.Err() != nil {
		return frameEnd{}, d.w.Err()
	}

	// End of data: the last frame must checksum to exactly here. Whole
	// frames behind it check with it (a frame's own checksum leaves zero),
	// so with a stray one found behind it, it ends there.
	if end, ok := d.tailEnd(cur); ok && !s.waiting {
		if s.stray >= 0 {
			if werr := d.warn(s.stray, outOfSequence, end-s.stray); werr != nil {
				return frameEnd{}, werr
			}
			return last(s.stray), nil
		}
		return last(end), nil
	}
	for _, h := range held[:nHeld] {
		if _, ok := d.tailEnd(h); !ok {
			continue
		}
		if s.waiting {
			if fe, ok, err := d.confirmed(&s, h); ok || err != nil {
				return fe, err
			}
			continue
		}
		fe, isFar, err := d.holeAt(&s, h, false)
		if !isFar {
			return fe, err
		}
		s.far, s.run, s.dur, s.waiting = h, h, int64(h.fi.BlockSize), true
	}
	if d.w.Err() != nil {
		return frameEnd{}, d.w.Err()
	}
	if s.waiting {
		return d.waitOver(&s)
	}
	if d.ownTo(&s, d.w.DataEnd()) {
		if werr := d.warn(s.own, "%d trailing bytes are not a valid frame, dropped", d.w.DataEnd()-s.own); werr != nil {
			return frameEnd{}, werr
		}
		return last(s.own), nil
	}
	// A read that failed there says nothing about the frame.
	if d.w.Err() != nil {
		return frameEnd{}, d.w.Err()
	}
	if werr := d.warn(start, "%d trailing bytes are not a valid frame, dropped", d.w.DataEnd()-start); werr != nil {
		return frameEnd{}, werr
	}
	return last(-1), nil
}

// checksTo reports whether p's bytes up to cand check as a whole frame, its
// CRC-16 being the two bytes before cand, and moves p's checksum on to cand.
func (d *Demuxer) checksTo(p *pending, cand int64) bool {
	if p.bare && cand == p.off {
		return true
	}
	if cand-2 < p.crcPos {
		return false
	}
	p.crc = flac.UpdateCRC16(p.crc, d.w.BytesAt(p.crcPos, int(cand-2-p.crcPos)))
	tail := d.w.BytesAt(cand-2, 2)
	ok := len(tail) == 2 && p.crc == uint16(tail[0])<<8|uint16(tail[1])
	p.crc = flac.UpdateCRC16(p.crc, tail)
	p.crcPos = cand - 2 + int64(len(tail))
	return ok
}

// tailEnd reports where p's frame ends when it is the last: its bytes must
// check to exactly the end of data, after peeling any recognized trailing
// tags. Every peel is provisional, dataEnd shrinking only once the checksum
// confirms it, so a false recognition costs a retry rather than data.
func (d *Demuxer) tailEnd(p pending) (int64, bool) {
	end := d.w.DataEnd()
	// The floor is the frame's own start plus its two checksum bytes, or
	// for bare bytes their start: nothing but tags may lie there.
	floor := p.off + 2
	if p.bare {
		floor = p.off
	}
	// Check first, peel second, and check once more after the last peel: the
	// peel that uncovers the real end is the one the bound would otherwise
	// spend without ever validating it.
	for i := 0; ; i++ {
		if d.tailChecks(p.crc, p.crcPos, end) || p.bare && end == p.off {
			if end != d.w.DataEnd() {
				// Where taggers put an ID3v1 or APE trailer: bytes after the
				// last frame that the file is entitled to carry.
				d.note(end, "%d trailing tag or padding bytes ignored", d.w.DataEnd()-end)
				d.w.SetDataEnd(end)
			}
			return end, true
		}
		if i == trailer.Max {
			return 0, false
		}
		// NUL padding is peeled here and nowhere else: it has no magic to
		// recognize, and this is the only caller that confirms a peel.
		stripped, _, ok := trailer.Peel(&d.w, trailer.APEv2|trailer.ID3v1|trailer.ID3v2|trailer.Lyrics3|trailer.Padding,
			floor, end)
		if !ok {
			return 0, false
		}
		end = stripped
	}
}

// believable reports whether a hole of lost samples over n bytes is one the
// file could have lost, ending inside the track's length when that is known.
func (d *Demuxer) believable(lost, n int64, next flac.FrameInfo) bool {
	if lost > flac.MaxSamplesIn(n) {
		return false
	}
	return !d.exact || d.num.Start(next)-d.origin <= d.promised
}

// ownTo finds where the frame at d.off ends by its own parse, short of
// limit, unless the scan has it, and reports whether it ends whole.
func (d *Demuxer) ownTo(s *found, limit int64) bool {
	if s.own < 0 {
		if n, ok := d.ownEnd(limit); ok {
			s.own = d.off + int64(n)
		}
	}
	return s.own >= 0
}

// carriesOn takes h into the run where it is numbered on from the run's last
// frame and starts where that frame ends by its own parse. A checksum does
// not say so: whole frames check across the whole frames between them.
func (d *Demuxer) carriesOn(s *found, h pending) bool {
	if h.fi.Coded != d.num.Next(s.run.fi) {
		return false
	}
	n, err := flac.FrameLen(d.w.BytesAt(s.run.off, int(min(h.off-s.run.off, maxFrameLen))), d.si, &d.residual)
	if err != nil || s.run.off+int64(n) != h.off {
		return false
	}
	s.run, s.dur = h, s.dur+int64(h.fi.BlockSize)
	return true
}

// holeAt reports the damage before h, the next frame that checks. When the
// frame at d.off is whole by its own CRC-16 and only the header after it is
// damaged, that frame is delivered and the hole is what lies between them.
func (d *Demuxer) holeAt(s *found, h pending, join bool) (fe frameEnd, far bool, err error) {
	// A read that failed there says nothing about the frame.
	if !d.ownTo(s, h.off) && d.w.Err() != nil {
		return frameEnd{}, false, d.w.Err()
	}
	return d.holeFrom(s.own, h, join)
}

// toFar ends a wait with no frame numbered in the gap to read first: the
// read goes on from far as it always did, the frames the numbers skip lost
// where the stream could have far, and far joined on where not.
func (d *Demuxer) toFar(s *found) (frameEnd, error) {
	if s.jump || s.strays {
		return d.skip(s.far, s.own)
	}
	// It checks, so it is read on in order, and its number places no silence.
	fe, _, err := d.holeAt(s, s.far, true)
	return fe, err
}

// waitOver ends a wait at the scan's end. Behind frames out of sequence no
// successor came, so the first frame numbered in the gap may settle it.
func (d *Demuxer) waitOver(s *found) (frameEnd, error) {
	if s.hasFirst {
		if fe, ok, err := d.settle(s, s.first, true); ok || err != nil {
			return fe, err
		}
	}
	return d.toFar(s)
}

// confirmed answers a wait with h, held until a frame behind it or the end
// of data confirmed it: in front of far a hole leads to it where the stream
// could have it, and behind far it counts where it is numbered in the gap.
// Behind frames out of sequence a frame the read comes to behind the run
// ends the wait, and any other is the hole the read always took: one
// numbered inside the run, or before where the run ends.
func (d *Demuxer) confirmed(s *found, h pending) (frameEnd, bool, error) {
	switch {
	case h.off > s.far.off && h.fi.Coded < s.far.fi.Coded:
		return d.settle(s, h, false)
	case h.off > s.far.off && !s.strays:
		return frameEnd{}, false, nil
	case h.off > s.far.off && h.fi.Coded >= d.num.Next(s.run.fi):
		fe, err := d.toFar(s)
		return fe, true, err
	}
	fe, isFar, err := d.holeAt(s, h, false)
	return fe, !isFar, err
}

// settle ends a wait with h, a frame numbered in the gap in front of far
// and confirmed behind it: the whole frames from far are held until their
// numbers are due, read at the gap, or joined on at their own numbers, and
// never passed over. chained says they check up to h. ok false leaves the
// wait on.
func (d *Demuxer) settle(s *found, h pending, chained bool) (fe frameEnd, ok bool, err error) {
	// The run's last frame is whole where its own parse ends it in front of
	// h; one that is not is left to the damage behind the run.
	end, dur := s.run.off, s.dur-int64(s.run.fi.BlockSize)
	if n, err := flac.FrameLen(d.w.BytesAt(s.run.off, int(min(h.off-s.run.off, maxFrameLen))), d.si, &d.residual); err == nil {
		end, dur = s.run.off+int64(n), s.dur
	}
	if s.own < 0 || dur == 0 {
		// No whole frame of the read's in front of h: a hole, where the
		// stream could have h.
		fe, isFar, err := d.holeFrom(s.own, h, false)
		return fe, !isFar, err
	}
	gap := d.num.Start(h.fi) - d.num.Start(d.cur) - int64(d.cur.BlockSize)
	fills := gap == dur && s.far.fi.Coded != d.num.Next(h.fi)
	switch vouched := d.vouched(d.num.Start(s.far.fi) - d.origin); {
	case fills, !vouched && dur < gap:
		fe, err = d.fillGap(s, end, fills)
	case vouched && (s.past < 0 || s.past > h.off):
		fe, err = d.holdAhead(s, end, h, chained)
	case !s.strays || chained && h.fi.Coded != d.num.Next(d.cur):
		// Numbered past the stream, in no gap that holds it, or in front of
		// a frame the read went on to behind it, which holding would drop.
		fe, err = d.toFar(s)
	case chained:
		// Behind frames out of sequence the read went on from the successor,
		// and the run was one of them.
		if err = d.warn(s.own, outOfSequence, h.off-s.own); err == nil {
			fe = frameEnd{end: s.own, next: h.fi, nextOK: true, resync: h.off, ordered: true}
		}
	default:
		// And from a frame behind damage, where the stream could have it.
		fe, isFar, err := d.holeFrom(s.own, h, false)
		return fe, !isFar, err
	}
	return fe, true, err
}

// skipFrom reports numbers that skip from the frame at d.off, ending at end,
// to h: frames gone without their numbers. Where the stream could have h,
// the samples skipped are lost, as a hole's are, so a read places what
// follows where a seek and a measure do; numbers past that vouch for
// nothing, so h is joined on.
func (d *Demuxer) skipFrom(end int64, h pending) (frameEnd, error) {
	from := d.num.Start(d.cur) + int64(d.cur.BlockSize) - d.origin
	to := d.num.Start(h.fi) - d.origin
	fe := frameEnd{end: end, next: h.fi, nextOK: true, resync: h.off}
	if d.vouched(to) {
		if err := d.warn(h.off, "the frames skip from sample %d to %d: %d samples lost", from, to, to-from); err != nil {
			return frameEnd{}, err
		}
		fe.lost = to - from
		return fe, nil
	}
	if err := d.warn(h.off, "the frames skip from sample %d to %d, past what the stream holds; joined", from, to); err != nil {
		return frameEnd{}, err
	}
	return fe, nil
}

// skip takes the gap in front of far, the frame at d.off ending at own, where
// what lies between is frames out of sequence.
func (d *Demuxer) skip(far pending, own int64) (frameEnd, error) {
	if own < far.off {
		if err := d.warn(own, outOfSequence, far.off-own); err != nil {
			return frameEnd{}, err
		}
	}
	return d.skipFrom(own, far)
}

// inFront reports what lies between the frame at d.off, ending at its own
// end, and far: frames out of sequence where they begin at stray, else damage.
func (d *Demuxer) inFront(s *found) error {
	switch s.own {
	case s.far.off:
		return nil
	case s.stray:
		return d.warn(s.own, outOfSequence, s.far.off-s.own)
	}
	return d.warn(s.own, "%d bytes are not a valid frame, dropped", s.far.off-s.own)
}

// holdAhead reads on from h, numbered in front of the run of whole frames
// from far to end, and holds that run until the read would pass its
// numbers. What lies between the run and h is dropped: no frame there is
// numbered past the run (found.past).
func (d *Demuxer) holdAhead(s *found, end int64, h pending, chained bool) (frameEnd, error) {
	if err := d.inFront(s); err != nil {
		return frameEnd{}, err
	}
	if err := d.warn(s.far.off, "%d bytes hold frames numbered ahead of the one behind them, read in order", end-s.far.off); err != nil {
		return frameEnd{}, err
	}
	if end < h.off {
		msg := "%d bytes are not a valid frame, dropped"
		if chained {
			msg = outOfSequence
		}
		if err := d.warn(end, msg, h.off-end); err != nil {
			return frameEnd{}, err
		}
	}
	d.aside = aside{ok: true, held: true, off: s.far.off, end: end, first: s.far.fi, at: d.off, from: s.from, resume: -1}
	fe := frameEnd{end: s.own, next: h.fi, nextOK: true, resync: h.off}
	if h.fi.Coded != d.num.Next(d.cur) {
		var err error
		if fe, err = d.skipFrom(s.own, h); err != nil {
			return frameEnd{}, err
		}
	}
	fe.ordered = chained
	return fe, nil
}

// fillGap reads the run of whole frames from far to end, numbered off the
// gap in front of the frame behind them, at that gap: filling it where
// exact, else at its head.
func (d *Demuxer) fillGap(s *found, end int64, exact bool) (frameEnd, error) {
	if err := d.inFront(s); err != nil {
		return frameEnd{}, err
	}
	msg := "%d bytes hold frames numbered off the gap they fill, read there"
	if !exact {
		msg = "%d bytes hold frames numbered past the stream, read at the head of the gap behind them"
	}
	if err := d.warn(s.far.off, msg, end-s.far.off); err != nil {
		return frameEnd{}, err
	}
	shift := int64(d.num.Next(d.cur)) - int64(s.far.fi.Coded)
	first := s.far.fi
	first.Coded = uint64(int64(first.Coded) + shift)
	d.aside = aside{ok: true, off: s.far.off, end: end, first: first, shift: shift, at: d.off, from: s.from, resume: -1}
	return frameEnd{end: s.own, next: first, nextOK: true, resync: s.far.off, ordered: true}, nil
}

// trimTo is where the window may let go of bytes: the read's place, or a
// held run in front of it while that lies within a resync's reach. Past
// that the run is read again when it is due.
func (d *Demuxer) trimTo() int64 {
	if a := d.aside; a.held && d.off >= a.end && d.cur.Coded < a.first.Coded && d.off-a.off <= maxResync {
		return a.off
	}
	return d.off
}

// vouched reports whether a frame at sample to lies where this stream could
// have one: inside its exact length, or short of one, inside what its frames'
// bytes can hold.
func (d *Demuxer) vouched(to int64) bool {
	if d.exact {
		return to <= d.promised
	}
	return to <= flac.MaxSamplesIn(d.w.DataEnd()-d.firstFrame)
}

// ownEnd parses the frame at d.off to its end, short of limit, and reports its
// length when its CRC-16 checks there.
func (d *Demuxer) ownEnd(limit int64) (int, bool) {
	limit = min(limit, d.off+maxFrameLen)
	n, err := flac.FrameLen(d.w.BytesAt(d.off, int(limit-d.off)), d.si, &d.residual)
	return n, err == nil
}

// outOfSequence words whole frames behind a frame that are numbered before
// its successor: the frame ends where they begin, and they are dropped.
const outOfSequence = "%d bytes hold frames out of sequence, dropped"

// holeFrom reports the hole between the frame at d.off and h, once, with
// the bytes and samples between: from end, where that frame ends whole, or
// with it in the hole when end is negative. far, with nothing reported,
// says the stream could not have h where its number puts it, unless join.
func (d *Demuxer) holeFrom(end int64, h pending, join bool) (fe frameEnd, far bool, err error) {
	from, lost := d.off, d.num.Start(h.fi)-d.num.Start(d.cur)
	if end >= 0 {
		from, lost = end, lost-int64(d.cur.BlockSize)
	}
	fe = frameEnd{end: end, next: h.fi, nextOK: true, resync: h.off}
	switch {
	case d.believable(lost, h.off-from, h.fi):
		// Positions go on from h's own number, so what follows sits where
		// it belongs.
		fe.lost = lost
		err = d.warn(from, "%d bytes are not a valid frame, dropped: %d samples lost", h.off-from, lost)
	case join:
		err = d.warn(from, "%d bytes are not a valid frame, dropped; the frame behind them is numbered past what the stream holds, joined", h.off-from)
	default:
		return frameEnd{}, true, nil
	}
	if err != nil {
		return frameEnd{}, false, err
	}
	return fe, false, nil
}

// tailChecks reports whether [start, end) checksums as a complete frame,
// given crc already covering [start, crcPos).
func (d *Demuxer) tailChecks(crc uint16, crcPos, end int64) bool {
	if end-crcPos < 2 || end > d.w.DataEnd() {
		return false
	}
	crc = flac.UpdateCRC16(crc, d.w.BytesAt(crcPos, int(end-2-crcPos)))
	tail := d.w.BytesAt(end-2, 2)
	if len(tail) < 2 {
		return false
	}
	return crc == uint16(tail[0])<<8|uint16(tail[1])
}

// ReadPacket yields one whole frame, checksum included. Packet data is
// reused across calls. A frame whose bytes do not check is stepped over, and
// the frame after it is the next packet, marked Discont, at its own position,
// with Lost saying how many samples the hole took.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error {
	if !d.valid {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return d.end()
	}
	for {
		d.w.Trim(d.trimTo())
		fe, err := d.nextEnd()
		if err != nil {
			return err
		}
		if fe.end < 0 {
			if !fe.nextOK {
				// Damaged tail, tolerantly dropped.
				d.valid = false
				return d.end()
			}
			d.lost += fe.lost // a hole, the frame at d.off in it
			d.off, d.cur, d.discont = fe.resync, fe.next, true
			continue
		}
		data := d.w.BytesAt(d.off, int(fe.end-d.off))
		if int64(len(data)) != fe.end-d.off {
			if d.w.Err() != nil {
				return d.w.Err()
			}
			return waxerr.New(waxerr.CodeSourceUnreadable, "flac: reading frame data")
		}
		*pkt = container.Packet{
			Track: 0,
			Lost:  d.lost,
			Packet: codec.Packet{
				Data:    data,
				PTS:     d.num.Start(d.cur) - d.origin,
				Dur:     int64(d.cur.BlockSize),
				Sync:    true,
				Discont: d.discont,
			},
		}
		d.count += d.lost + int64(d.cur.BlockSize)
		// What follows is discontinuous past damage, a hole or a jump.
		d.discont = fe.nextOK && ((fe.resync != fe.end && !fe.ordered) ||
			d.num.Start(fe.next) != d.num.Start(d.cur)+int64(d.cur.BlockSize))
		d.lost = fe.lost
		d.off, d.cur, d.valid = fe.resync, fe.next, fe.nextOK
		return nil
	}
}

// end is a read's clean end, where a whole read's count settles the length.
func (d *Demuxer) end() error {
	if err := d.settleEnd(); err != nil {
		return err
	}
	return io.EOF
}

// settleEnd holds the exact length against where a whole read ended. Off
// it, the read says so and the length is no longer exact; short of it the
// track takes what is there and counts the rest truncated.
func (d *Demuxer) settleEnd() error {
	t := d.track
	if !d.whole || !t.SamplesExact || d.count == t.Samples {
		return nil
	}
	// Short of it the frames are damaged; past it they are only more than
	// the open saw, which flac.Settle notes too.
	if msg := flac.ReadOff(d.si.Samples, t.Samples, d.count); d.count > t.Samples {
		d.note(d.firstFrame, "%s", msg)
	} else if err := d.warn(d.firstFrame, "%s", msg); err != nil {
		return err
	}
	tr, n, err := container.SettleRead(t, d.count, flac.RestateStreamInfo)
	d.track, d.truncated = tr, d.truncated+n
	return err
}

// SeekSample repositions to the frame containing the target sample and
// returns that frame's first sample; format.Media pre-rolls the
// remainder. The SEEKTABLE provides the starting point when present and
// trustworthy; otherwise bisection on the frame headers' own position
// numbers narrows the range.
//
// A target a hole took lands on the last frame before the hole, so the
// pre-roll runs through the hole as a read does. A walk that begins in the
// hole starts again twice as far back each time, down to the first frame.
func (d *Demuxer) SeekSample(ctx context.Context, track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("flac: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "flac: negative seek target")
	}
	if d.empty {
		return 0, nil // nothing to land on; reads stay EOF
	}
	// A seek canceled on the way leaves the read, its findings and the data
	// end as they stood; the window is the bytes it happens to hold.
	was, mark, dataEnd := *d, d.log.Mark(), d.w.DataEnd()
	d.seeking, d.cut = ctx, nil
	landed, err := d.seek(sample)
	if d.cut != nil {
		// A scan that was cut answers nothing, whatever its caller made of
		// it: a landing, a frame with no end, a run that is due.
		landed, err = 0, d.cut
	}
	if d.seeking = nil; waxerr.CodeOf(err) == waxerr.CodeCanceled {
		d.log.Rollback(mark)
		was.w, was.log, was.residual = d.w, d.log, d.residual
		*d = was
		d.w.SetDataEnd(dataEnd)
	}
	return landed, err
}

// canceled is the error of a seek whose context has ended (the walk's
// CodeCanceled), nil while it has not and outside a seek. It is kept in
// cut, so the seek returns it wherever the scan it stopped was asked from.
func (d *Demuxer) canceled() error {
	if d.seeking == nil {
		return nil
	}
	if err := container.WalkCanceled(d.seeking, "flac: "); err != nil {
		d.cut = err
	}
	return d.cut
}

// seek is SeekSample for a valid target on a stream with frames. Every
// piece a probe scans and every view of a frame's scan asks canceled, so
// each step of each walk does. Once one is cut every later one is, and
// SeekSample returns the cancel over whatever this made of them.
func (d *Demuxer) seek(sample int64) (int64, error) {
	d.aside, d.aheadAt = aside{}, -1 // a walk finds again what it reads out of place
	// Frames are numbered from origin and positions from 0.
	at := int64(math.MaxInt64)
	if sample <= math.MaxInt64-d.origin {
		at = sample + d.origin
	}
	if start := d.num.Start(d.first); at < start {
		// In the hole the stream starts with: land at 0, the read owing it.
		d.off, d.cur, d.valid, d.discont, d.lost = d.firstFrame, d.first, true, true, start-d.origin
		d.count, d.whole = 0, true
		return d.headLanding(start), nil
	}
	// Each start is strictly earlier than the last, so the search ends: at
	// the first frame at the latest.
	from, back := at, int64(0)
	var began *pending // the first walk's frame, where what lay behind it sent the seek back
	for {
		d.aside = aside{} // each walk finds its own
		off, fi, fe, err := d.locate(from)
		if err != nil {
			return 0, err
		}
		mark := d.log.Mark()
		landed, how, err := d.walkTo(off, fi, fe, at, from == d.origin, back == 0)
		// shortOf: past the end, the walk from further back ends in front of
		// the frame the first walk began on, which a seek always came to.
		shortOf := began != nil && how == ended && d.off < began.off
		if err == nil && began != nil && (how == short || shortOf ||
			d.off != began.off && at < d.num.Start(began.fi)+int64(began.fi.BlockSize)) {
			// The walk from further back does not come to the target as the
			// first did: it lies past frames joined on, or in the frame the
			// first walk began on, which no read from there reaches. A seek
			// always reached it, so it lands by the first walk.
			end := landed + int64(d.cur.BlockSize)
			d.log.Rollback(mark)
			d.aside = aside{}
			landed, how, err = d.walkTo(began.off, began.fi, nil, at, false, false)
			if err == nil && shortOf && landed+int64(d.cur.BlockSize) <= end {
				// Past the end the seek lands where the frames run furthest,
				// and the first walk's run no further.
				d.log.Rollback(mark)
				d.aside = aside{}
				landed, how, err = d.walkTo(off, fi, nil, at, from == d.origin, false)
			}
		}
		if err != nil {
			return 0, err
		}
		if how == lands || how == short || how == ended {
			return landed - d.origin, nil
		}
		// The walk lands nowhere: what it found is not this seek's to report.
		d.log.Rollback(mark)
		if began = nil; how == unsure {
			began = &pending{off: off, fi: fi}
		}
		back = max(2*back, int64(fi.BlockSize))
		from = max(d.num.Start(fi)-back, d.origin)
	}
}

// headLanding is where the read from the first frame, at start and owing
// the hole in front of it, places what it owes: sample 0, unless that frame
// lies in a hole joined on, which owes nothing past the head. The read takes
// the end found here; a failure here is the read's to report.
func (d *Demuxer) headLanding(start int64) int64 {
	mark := d.log.Mark()
	fe, err := d.findEnd()
	if err != nil {
		d.log.Rollback(mark)
		return 0
	}
	d.ahead, d.aheadAt = fe, d.off
	if fe.end >= 0 || !fe.nextOK {
		return 0
	}
	return d.num.Start(fe.next) - start - fe.lost
}

// nextEnd is findEnd for the read, taking the end a seek found where it
// found one for this frame.
func (d *Demuxer) nextEnd() (frameEnd, error) {
	if d.aheadAt >= 0 && d.aheadAt == d.off {
		d.aheadAt = -1
		return d.ahead, nil
	}
	d.aheadAt = -1
	return d.findEnd()
}

// locate finds the frame a seek to at walks from: the SEEKTABLE's, when it
// checks, with its confirmed end, else bisection's.
func (d *Demuxer) locate(at int64) (int64, flac.FrameInfo, *frameEnd, error) {
	off, fi, ok := d.seekTableHint(at)
	if ok {
		// The table is data, not truth: like every other boundary here,
		// the landing only counts once the pointed-at frame's end is
		// checksum-confirmed. Warnings from a failed probe are dropped
		// with it.
		d.off, d.cur, d.valid = off, fi, true
		mark := d.log.Mark()
		fe, err := d.findEnd()
		if err == nil && fe.end >= 0 {
			return off, fi, &fe, nil
		}
		if d.w.Err() != nil {
			return 0, flac.FrameInfo{}, nil, d.w.Err()
		}
		d.log.Rollback(mark)
		d.aside = aside{}
	}
	off, fi, ok = d.bisect(at)
	if !ok {
		if d.w.Err() != nil {
			return 0, flac.FrameInfo{}, nil, d.w.Err()
		}
		return 0, flac.FrameInfo{}, nil, malformed("cannot relocate any frame for seeking")
	}
	return off, fi, nil, nil
}

// walked is how a seek's walk ended.
type walked int

const (
	// lands: on the frame that holds the target, or in front of the hole
	// that took it.
	lands walked = iota
	// ended: on the stream's last frame, the target past it.
	ended
	// short: in front of the target, which lies past frames joined on with
	// nothing counted lost between, where no read from the walk's start
	// comes to it.
	short
	// inHole: nowhere. The walk began in a hole that took the target, with
	// no frame of its own in front of it.
	inHole
	// unsure: nowhere. The numbers skip behind the walk's first frame, a
	// run was taken aside there, or the walk met frames out of sequence, and
	// what a read places there may lie in front of it.
	unsure
)

// walkTo walks from the frame at off to the one containing at, stopping on the
// last frame for past-the-end targets. It steps over holes as a read does and
// confirms the frame it stops on, so the landing is where the next packet
// starts. A first walk began where the stream does, and lands behind a hole
// it begins in; only a walk with retry set answers unsure. confirmed, when
// not nil, is the frame at off's end, already found.
func (d *Demuxer) walkTo(off int64, fi flac.FrameInfo, confirmed *frameEnd, at int64, first, retry bool) (landed int64, how walked, err error) {
	d.off, d.cur, d.valid, d.discont, d.lost, d.strays = off, fi, true, false, 0, false
	var prev pending                 // the last frame the walk confirmed
	havePrev, joined := false, false // joined: the walk passed frames joined on
	// count is what a read from the first frame delivers in front of the
	// frame the walk stands on, where the walk began there.
	count, before := d.num.Start(d.first)-d.origin, int64(0)
	d.whole = off == d.firstFrame
	defer func() { d.count = count }()
	for {
		var fe frameEnd
		if confirmed != nil {
			fe, confirmed = *confirmed, nil
		} else {
			d.w.Trim(d.trimTo())
			if fe, err = d.findEnd(); err != nil {
				return 0, 0, err
			}
		}
		end := d.num.Start(d.cur) + int64(d.cur.BlockSize)
		if retry && !first && (!havePrev && d.unanchored(fe) || d.strays) {
			return 0, unsure, nil
		}
		if fe.end < 0 && fe.nextOK { // a hole, the frame at d.off in it
			if d.num.Start(fe.next) <= at {
				count += fe.lost
				d.off, d.cur = fe.resync, fe.next
				continue
			}
			switch {
			case havePrev:
				d.off, d.cur, count = prev.off, prev.fi, before
			case !first:
				return 0, inHole, nil
			default:
				// The stream begins in this hole: land at its start, the read
				// owing it, or on the frame behind it where that is joined on
				// and owes nothing.
				landed := d.num.Start(fe.next) - fe.lost
				d.lost = fe.lost
				d.off, d.cur, d.discont = fe.resync, fe.next, true
				return landed, lands, nil
			}
			return d.num.Start(d.cur), lands, nil
		}
		switch {
		case fe.end >= 0 && fe.nextOK && d.num.Start(fe.next) <= at:
			prev, havePrev, before = pending{off: d.off, fi: d.cur}, true, count
			count += int64(d.cur.BlockSize) + fe.lost
			joined = joined || d.num.Start(fe.next) > end+fe.lost
			d.off, d.cur = fe.resync, fe.next
		case fe.end >= 0 && at < end+fe.lost:
			return d.num.Start(d.cur), lands, nil // in this frame, or the hole behind it
		case !fe.nextOK && !joined:
			return d.num.Start(d.cur), ended, nil
		default:
			return d.num.Start(d.cur), short, nil
		}
	}
}

// seekTableHint resolves the target through the SEEKTABLE, verifying the
// pointed-at bytes really are a consistent frame header.
func (d *Demuxer) seekTableHint(sample int64) (int64, flac.FrameInfo, bool) {
	base := d.tableBase()
	lo, hi := 0, len(d.seekTable)
	for lo < hi {
		mid := (lo + hi) / 2
		if base+d.seekTable[mid].sample <= sample {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return 0, flac.FrameInfo{}, false
	}
	pt := d.seekTable[lo-1]
	off := d.firstFrame + pt.off
	if off < d.firstFrame || off >= d.w.DataEnd() {
		return 0, flac.FrameInfo{}, false
	}
	hdr := d.w.BytesAt(off, flac.MaxFrameHeaderLen)
	fi, err := flac.ParseFrameHeader(hdr)
	if err != nil || !d.consistent(fi) || d.num.Start(fi) > sample || d.num.Start(fi) < d.origin {
		return 0, flac.FrameInfo{}, false
	}
	return off, fi, true
}

// tableBase is what the SEEKTABLE's sample numbers count from: the first
// frame's own number for a table counted from the stream's first frame (a
// stream-copy cut's, as ffmpeg writes one), which its first point's frame
// says, else 0.
func (d *Demuxer) tableBase() int64 {
	if !d.seekBaseRead {
		d.seekBaseRead = true
		if d.origin > 0 && len(d.seekTable) > 0 {
			pt := d.seekTable[0]
			fi, err := flac.ParseFrameHeader(d.w.BytesAt(d.firstFrame+pt.off, flac.MaxFrameHeaderLen))
			if err == nil && d.consistent(fi) && d.num.Start(fi) == d.origin+pt.sample {
				d.seekBase = d.origin
			}
		}
	}
	return d.seekBase
}

// bisect narrows the byte range containing the target sample using frame
// headers found from probe offsets, then hands back the earliest frame
// of the final window.
func (d *Demuxer) bisect(sample int64) (int64, flac.FrameInfo, bool) {
	lo, hi := d.firstFrame, d.w.DataEnd()
	// The first frame's header is read alone where a probe will move the
	// window. Under two windows of frames at most one probe starts, inside a
	// window from here, which then serves it and a landing here.
	read := d.w.Peek
	if hi-lo < 2*srcwin.Chunk {
		read = d.w.BytesAt
	}
	loFi, err := flac.ParseFrameHeader(read(lo, flac.MaxFrameHeaderLen))
	if err != nil || !d.consistent(loFi) {
		var ok bool
		lo, loFi, ok = d.nextCandidate(lo, lo+maxResync)
		if !ok {
			return 0, flac.FrameInfo{}, false
		}
	}
	if d.num.Start(loFi) > sample {
		return lo, loFi, true
	}
	for hi-lo > seekWindow {
		mid := lo + (hi-lo)/2
		off, fi, ok := d.probe(mid, hi, maxResync)
		// A probe a read failed in places nothing, and no later one will.
		if d.w.Err() != nil {
			return 0, flac.FrameInfo{}, false
		}
		// A frame numbered before the first one is not this stream's, so it
		// places nothing.
		if !ok || d.num.Start(fi) > sample || d.num.Start(fi) < d.origin {
			hi = mid
			continue
		}
		lo, loFi = off, fi
	}
	return lo, loFi, true
}
