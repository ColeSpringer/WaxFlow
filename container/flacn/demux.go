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

// Metadata block types (RFC 9639 section 8).
const (
	blockStreamInfo = 0
	blockSeekTable  = 3
	blockInvalid    = 127
)

// DemuxerOptions configures parsing.
type DemuxerOptions struct {
	// Strict turns tolerated damage (the Warnings list) into errors.
	Strict bool
}

type seekPoint struct {
	sample int64
	off    int64 // relative to the first frame
}

// Demuxer reads one FLAC track from a native FLAC source.
type Demuxer struct {
	src  container.Source
	opts DemuxerOptions

	si        flac.StreamInfo
	track     container.Track
	seekTable []seekPoint
	// seekBase is what the SEEKTABLE counts from (tableBase), once read.
	seekBase     int64
	seekBaseRead bool
	warnings     container.Warnings
	// tailDamage is the first damage the open's measurement met.
	tailDamage *container.Warning
	residual   []int64 // FrameLen's scratch

	firstFrame int64 // byte offset of the first audio frame

	// num maps coded numbers to sample positions (flac.Numbering,
	// latched from the first frame); varBit is the blocking-strategy
	// header bit, which must not change within a stream.
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

	// w is the shared read-ahead window; its data end is the end of
	// frame data (trailing tags stripped lazily) and its sticky error
	// surfaces on the packet and seek paths.
	w srcwin.Window
}

// NewDemuxer parses the headers of a native FLAC source and positions on
// the first frame. The returned Demuxer implements container.Seeker and
// container.Warner.
func NewDemuxer(src container.Source, opts *DemuxerOptions) (*Demuxer, error) {
	d := &Demuxer{src: src, w: srcwin.New(src, src.Size(), "flac: reading frame data")}
	if opts != nil {
		d.opts = *opts
	}
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
	msg := fmt.Sprintf(format, args...)
	if d.opts.Strict {
		return malformed("%s (at offset %d)", msg, off)
	}
	d.warnings.Damage(off, msg)
	return nil
}

// note records a Warning that Strict must not escalate: this build doing
// something with a well-formed file that a caller should know about, rather
// than damage in the file. See [container.Note].
func (d *Demuxer) note(off int64, format string, args ...any) {
	d.warnings.Note(off, fmt.Sprintf(format, args...))
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
		Codec:       codec.FLAC,
		CodecConfig: siRaw,
		Fmt:         f,
		Samples:     samples,
		Default:     true,
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
	d.origin, d.first = d.num.Start(fi), fi
	if skipped > 0 {
		// Bytes that could hold the frames the first one's number skips are
		// those frames, damaged: a hole from 0, not a stream starting late.
		if lost := d.origin; lost > 0 && lost <= flac.MaxSamplesIn(skipped) {
			if werr := d.warn(off, "%d bytes are not a valid frame, dropped: %d samples lost", skipped, lost); werr != nil {
				return werr
			}
			d.origin, d.lost, d.discont = 0, lost, true
		} else if werr := d.warn(off, "%d unparsable bytes before the first frame", skipped); werr != nil {
			return werr
		}
	}
	d.off, d.cur, d.valid = d.firstFrame, fi, true
	return d.verifyLength()
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
	if n := d.w.DataEnd() - d.firstFrame; span > flac.MaxSamplesIn(n) {
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
	strict, mark, seen, dataEnd := d.opts.Strict, d.warnings.Mark(), d.warnings.DamageCount(), d.w.DataEnd()
	off0, cur0, valid0 := d.off, d.cur, d.valid
	d.opts.Strict = false
	defer func() {
		d.opts.Strict = strict
		clean = clean && d.warnings.DamageCount() == seen
		for _, w := range d.warnings.Since(mark) {
			if w.Kind == container.Damage {
				d.tailDamage = &w
				break
			}
		}
		d.warnings.Rollback(mark)
		d.w.SetDataEnd(dataEnd)
		// The walk ends past the audio; reads start where it began.
		d.off, d.cur, d.valid = off0, cur0, valid0
	}()

	if end, ok := d.tailBacks(); ok {
		return end, true, true
	}
	off, fi, found := d.bisect(math.MaxInt64) // no target: land on the last window
	if !found {
		return 0, false, false
	}
	d.off, d.cur, d.valid = off, fi, true
	for {
		d.w.Trim(d.off)
		fe, err := d.findEnd()
		if err != nil || d.w.Err() != nil {
			return 0, false, false
		}
		switch {
		case !fe.nextOK && fe.end < 0:
			// Stops inside this frame: the answer is where the audio ends and
			// the file is damaged, which is exactly the pair the flag splits.
			return d.num.Start(d.cur), true, false
		case !fe.nextOK:
			return d.num.Start(d.cur) + int64(d.cur.BlockSize), true, true // clean end
		}
		d.off, d.cur = fe.resync, fe.next
	}
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
			d.off, d.cur, d.valid = cand, fi, true
			// A complete frame with nothing after it: findEnd peels any
			// trailing tag on the way, which the caller unpeels. What a
			// rejected candidate reported is not the file's.
			mark := d.warnings.Mark()
			if fe, err := d.findEnd(); err == nil && fe.end >= 0 && !fe.nextOK {
				return d.num.Start(fi) + int64(fi.BlockSize), true
			}
			d.warnings.Rollback(mark)
		}
		if from == lo {
			break // the whole window is scanned
		}
		hi = from + 1 // overlap by one, for a sync byte on the chunk edge
	}
	return 0, false
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
func (d *Demuxer) Warnings() []container.Warning { return d.warnings.List() }

// DamageCount implements container.Damaged.
func (d *Demuxer) DamageCount() int { return d.warnings.DamageCount() }

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

// nextCandidate scans [from, limit) for a sync candidate whose header
// parses and agrees with STREAMINFO. Used for initial positioning,
// resync after damage, and bisection probes; packet boundaries demand
// the stronger findEnd confirmation.
func (d *Demuxer) nextCandidate(from, limit int64) (int64, flac.FrameInfo, bool) {
	limit = min(limit, d.w.DataEnd())
	for off := from; off < limit; {
		buf := d.w.BytesAt(off, srcwin.Chunk)
		if len(buf) < 2 {
			return 0, flac.FrameInfo{}, false
		}
		i := bytes.IndexByte(buf, 0xFF)
		if i < 0 {
			off += int64(len(buf))
			continue
		}
		cand := off + int64(i)
		if cand >= limit {
			return 0, flac.FrameInfo{}, false
		}
		hdr := d.w.BytesAt(cand, flac.MaxFrameHeaderLen)
		if flac.SyncOK(hdr) {
			if fi, err := flac.ParseFrameHeader(hdr); err == nil && d.consistent(fi) {
				return cand, fi, true
			}
		}
		off = cand + 1
	}
	return 0, flac.FrameInfo{}, false
}

// frameEnd is where findEnd settled the frame at d.off.
type frameEnd struct {
	end    int64          // the frame's end; -1 when its bytes do not check
	next   flac.FrameInfo // the header of the frame after it, when nextOK
	nextOK bool
	resync int64 // where that frame starts: end, or past damage
	lost   int64 // the samples lost before it, the frame at d.off's too when end < 0
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
}

// findEnd locates the end of the current frame at d.off: the next sync
// candidate that parses consistently, carries the expected position, and
// where the bytes so far checksum as a complete frame (CRC-16). At end
// of data the final span must checksum likewise; recognized trailers
// (ID3v1, APEv2, appended ID3v2, NUL padding) are peeled and retried
// before the tail is declared damaged. end < 0 with a nil error reports
// a tolerantly dropped tail, or, with hole set, a damaged frame.
//
// A frame whose bytes do not check gives way to the first consistent frame
// after it that does, making it a hole: one warning, and the read goes on from
// there, for about two frames' scan. A later-numbered header where the frame
// does check is a gap in the numbers, not damage, and is passed over (see
// cut.go's FLAC row).
func (d *Demuxer) findEnd() (frameEnd, error) {
	start := d.off
	cur := pending{off: start, fi: d.cur, crcPos: start}
	succ := d.num.Next(d.cur)
	var held [maxHeld]pending
	nHeld := 0
	own := int64(-1) // where the frame at start ends by its own parse, once found

	// A boundary needs at least a header and CRC-16 before it; starting
	// past the fixed header bytes keeps the scan out of our own header.
	scan := start + 4
	for {
		if scan-start > maxFrameLen {
			return frameEnd{}, malformed("frame at offset %d exceeds %d bytes", start, int64(maxFrameLen))
		}
		buf := d.w.BytesAt(scan, srcwin.Chunk)
		if len(buf) == 0 {
			break // end of data
		}
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
			checks := false
			if fi.Coded == succ || fits {
				checks = d.checksTo(&cur, cand)
			}
			if checks && fi.Coded == succ {
				// Confirmed boundary. A frame here that disagrees with
				// STREAMINFO is a mid-stream format change: not damage but
				// a stream shape the fixed-format pipeline rejects (RFC
				// 9639 permits this), so it is a hard error either mode.
				if !d.consistent(fi) {
					return frameEnd{}, malformed("mid-stream format change at offset %d", cand)
				}
				return boundary(cand, fi), nil
			}
			if checks && fi.Coded > succ {
				// The bytes check across a gap in the numbers. Where this
				// frame ends here by its own parse, the frames are whole and
				// only the numbers skip. Where it ends short with no frame
				// there, what lies between is damage that checks, as zeroed
				// bytes do (CRC-16 starts from zero).
				if own < 0 {
					if n, ok := d.ownEnd(cand); ok {
						own = start + int64(n)
					}
				}
				h := pending{off: cand, fi: fi, crcPos: cand}
				switch {
				case own == cand:
					return d.jump(h)
				case own >= 0 && own < cand && !d.headerAt(own):
					return d.holeAfter(own, h)
				}
			}
			for j := range nHeld {
				if fi.Coded == d.num.Next(held[j].fi) && d.checksTo(&held[j], cand) {
					return d.holeAt(held[j])
				}
			}
			if fits && !checks {
				if nHeld == maxHeld {
					copy(held[:], held[1:])
					nHeld--
				}
				held[nHeld] = pending{off: cand, fi: fi, crcPos: cand}
				nHeld++
			}
		}
		scan += int64(len(buf))
	}
	if d.w.Err() != nil {
		return frameEnd{}, d.w.Err()
	}

	// End of data: the last frame must checksum to exactly here.
	if end, ok := d.tailEnd(cur); ok {
		return last(end), nil
	}
	for _, h := range held[:nHeld] {
		if _, ok := d.tailEnd(h); ok {
			return d.holeAt(h)
		}
	}
	if d.w.Err() != nil {
		return frameEnd{}, d.w.Err()
	}
	if n, ok := d.ownEnd(d.w.DataEnd()); ok {
		end := start + int64(n)
		if werr := d.warn(end, "%d trailing bytes are not a valid frame, dropped", d.w.DataEnd()-end); werr != nil {
			return frameEnd{}, werr
		}
		return last(end), nil
	}
	if werr := d.warn(start, "%d trailing bytes are not a valid frame, dropped", d.w.DataEnd()-start); werr != nil {
		return frameEnd{}, werr
	}
	return last(-1), nil
}

// checksTo reports whether p's bytes up to cand check as a whole frame, its
// CRC-16 being the two bytes before cand, and moves p's checksum on to cand.
func (d *Demuxer) checksTo(p *pending, cand int64) bool {
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
	// Check first, peel second, and check once more after the last peel: the
	// peel that uncovers the real end is the one the bound would otherwise
	// spend without ever validating it.
	for i := 0; ; i++ {
		if d.tailChecks(p.crc, p.crcPos, end) {
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
		// The floor is the frame's own start plus its two checksum bytes.
		// NUL padding is peeled here and nowhere else: it has no magic to
		// recognize, and this is the only caller that confirms a peel.
		stripped, _, ok := trailer.Peel(&d.w, trailer.APEv2|trailer.ID3v1|trailer.ID3v2|trailer.Padding,
			p.off+2, end)
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
	t := d.track
	return !t.SamplesExact || t.Samples < 0 || d.num.Start(next)-d.origin <= t.Samples
}

// holeAt reports the damage before h, the next frame that checks. When the
// frame at d.off is whole by its own CRC-16 and only the header after it is
// damaged, that frame is delivered and the hole is what lies between them.
func (d *Demuxer) holeAt(h pending) (frameEnd, error) {
	if n, ok := d.ownEnd(h.off); ok {
		return d.holeAfter(d.off+int64(n), h)
	}
	return d.hole(h)
}

// jump reports numbers that skip from the frame at d.off to h, which starts
// where that frame ends: frames gone without their numbers. Where the stream
// could have h, the samples skipped are lost, as a hole's are, so a read
// places what follows where a seek and a measure do; numbers past that vouch
// for nothing, so h is joined on.
func (d *Demuxer) jump(h pending) (frameEnd, error) {
	from := d.num.Start(d.cur) + int64(d.cur.BlockSize) - d.origin
	to := d.num.Start(h.fi) - d.origin
	if d.vouched(to) {
		if err := d.warn(h.off, "the frames skip from sample %d to %d: %d samples lost", from, to, to-from); err != nil {
			return frameEnd{}, err
		}
		fe := boundary(h.off, h.fi)
		fe.lost = to - from
		return fe, nil
	}
	if err := d.warn(h.off, "the frames skip from sample %d to %d, past what the stream holds; joined", from, to); err != nil {
		return frameEnd{}, err
	}
	return boundary(h.off, h.fi), nil
}

// vouched reports whether a frame at sample to lies where this stream could
// have one: inside its exact length, or short of one, inside what its frames'
// bytes can hold.
func (d *Demuxer) vouched(to int64) bool {
	if t := d.track; t.SamplesExact && t.Samples >= 0 {
		return to <= t.Samples
	}
	return to <= flac.MaxSamplesIn(d.w.DataEnd()-d.firstFrame)
}

// headerAt reports whether a frame header of this stream starts at off.
func (d *Demuxer) headerAt(off int64) bool {
	fi, err := flac.ParseFrameHeader(d.w.BytesAt(off, flac.MaxFrameHeaderLen))
	return err == nil && fi.Variable == d.varBit
}

// ownEnd parses the frame at d.off to its end, short of limit, and reports its
// length when its CRC-16 checks there.
func (d *Demuxer) ownEnd(limit int64) (int, bool) {
	limit = min(limit, d.off+maxFrameLen)
	n, err := flac.FrameLen(d.w.BytesAt(d.off, int(limit-d.off)), d.si, &d.residual)
	return n, err == nil
}

// holeAfter reports the bytes between end, where the whole frame at d.off
// ends, and h: once, with the samples between.
func (d *Demuxer) holeAfter(end int64, h pending) (frameEnd, error) {
	lost := d.num.Start(h.fi) - d.num.Start(d.cur) - int64(d.cur.BlockSize)
	if !d.believable(lost, h.off-end, h.fi) {
		if err := d.warn(end, "%d trailing bytes are not a valid frame, dropped", d.w.DataEnd()-end); err != nil {
			return frameEnd{}, err
		}
		return last(end), nil
	}
	if err := d.warn(end, "%d bytes are not a valid frame, dropped: %d samples lost", h.off-end, lost); err != nil {
		return frameEnd{}, err
	}
	return frameEnd{end: end, next: h.fi, nextOK: true, resync: h.off, lost: lost}, nil
}

// hole reports the frame at d.off, whose bytes do not check, giving way to h,
// which does: once, at the damaged frame, with the bytes and samples between.
// Positions go on from h's own number, so what follows sits where it belongs.
func (d *Demuxer) hole(h pending) (frameEnd, error) {
	lost := d.num.Start(h.fi) - d.num.Start(d.cur)
	if !d.believable(lost, h.off-d.off, h.fi) {
		if err := d.warn(d.off, "%d trailing bytes are not a valid frame, dropped", d.w.DataEnd()-d.off); err != nil {
			return frameEnd{}, err
		}
		return last(-1), nil
	}
	if err := d.warn(d.off, "%d bytes are not a valid frame, dropped: %d samples lost", h.off-d.off, lost); err != nil {
		return frameEnd{}, err
	}
	return frameEnd{end: -1, next: h.fi, nextOK: true, resync: h.off, lost: lost}, nil
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
		return io.EOF
	}
	for {
		d.w.Trim(d.off)
		fe, err := d.findEnd()
		if err != nil {
			return err
		}
		if fe.end < 0 {
			if !fe.nextOK {
				// Damaged tail, tolerantly dropped.
				d.valid = false
				return io.EOF
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
		// What follows is discontinuous past damage, a hole or a jump.
		d.discont = fe.nextOK && (fe.resync != fe.end ||
			d.num.Start(fe.next) != d.num.Start(d.cur)+int64(d.cur.BlockSize))
		d.lost = fe.lost
		d.off, d.cur, d.valid = fe.resync, fe.next, fe.nextOK
		return nil
	}
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
func (d *Demuxer) SeekSample(_ context.Context, track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("flac: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "flac: negative seek target")
	}
	if d.empty {
		return 0, nil // nothing to land on; reads stay EOF
	}
	// Frames are numbered from origin and positions from 0.
	at := int64(math.MaxInt64)
	if sample <= math.MaxInt64-d.origin {
		at = sample + d.origin
	}
	if start := d.num.Start(d.first); at < start {
		// In the hole the stream starts with: land at 0, the read owing it.
		d.off, d.cur, d.valid, d.discont, d.lost = d.firstFrame, d.first, true, true, start-d.origin
		return 0, nil
	}
	// Each start is strictly earlier than the last, so the search ends: at
	// the first frame at the latest.
	from, back := at, int64(0)
	for {
		off, fi, fe, err := d.locate(from)
		if err != nil {
			return 0, err
		}
		landed, ok, err := d.walkTo(off, fi, fe, at, from == d.origin)
		if err != nil {
			return 0, err
		}
		if ok {
			return landed - d.origin, nil
		}
		back = max(2*back, int64(fi.BlockSize))
		from = max(d.num.Start(fi)-back, d.origin)
	}
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
		mark := d.warnings.Mark()
		fe, err := d.findEnd()
		if err == nil && fe.end >= 0 {
			return off, fi, &fe, nil
		}
		if d.w.Err() != nil {
			return 0, flac.FrameInfo{}, nil, d.w.Err()
		}
		d.warnings.Rollback(mark)
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

// walkTo walks from the frame at off to the one containing at, stopping on the
// last frame for past-the-end targets. It steps over holes as a read does and
// confirms the frame it stops on, so the landing is where the next packet
// starts. ok is false when the walk began in a hole that took the target, with
// no frame of its own before it to land on; a first walk, which began where
// the stream does, lands on the frame after the hole instead. confirmed, when
// not nil, is the frame at off's end, already found.
func (d *Demuxer) walkTo(off int64, fi flac.FrameInfo, confirmed *frameEnd, at int64, first bool) (landed int64, ok bool, err error) {
	d.off, d.cur, d.valid, d.discont, d.lost = off, fi, true, false, 0
	var prev pending // the last frame the walk confirmed
	havePrev := false
	for {
		var fe frameEnd
		if confirmed != nil {
			fe, confirmed = *confirmed, nil
		} else {
			d.w.Trim(d.off)
			if fe, err = d.findEnd(); err != nil {
				return 0, false, err
			}
		}
		if fe.end < 0 && fe.nextOK { // a hole, the frame at d.off in it
			if d.num.Start(fe.next) <= at {
				d.off, d.cur = fe.resync, fe.next
				continue
			}
			switch {
			case havePrev:
				d.off, d.cur = prev.off, prev.fi
			case !first:
				return 0, false, nil
			default:
				// The stream begins in this hole: land at its start, the read
				// owing it.
				landed := d.num.Start(d.cur)
				d.lost = fe.lost
				d.off, d.cur, d.discont = fe.resync, fe.next, true
				return landed, true, nil
			}
			return d.num.Start(d.cur), true, nil
		}
		if fe.end < 0 || !fe.nextOK || d.num.Start(fe.next) > at {
			return d.num.Start(d.cur), true, nil // in this frame, or the gap after it
		}
		prev, havePrev = pending{off: d.off, fi: d.cur}, true
		d.off, d.cur = fe.resync, fe.next
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
	lo := d.firstFrame
	loFi, err := flac.ParseFrameHeader(d.w.BytesAt(lo, flac.MaxFrameHeaderLen))
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
	hi := d.w.DataEnd()
	for hi-lo > seekWindow {
		mid := lo + (hi-lo)/2
		off, fi, ok := d.nextCandidate(mid, hi)
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
