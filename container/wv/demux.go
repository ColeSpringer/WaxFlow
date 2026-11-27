package wv

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"math"
	"slices"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/apev2"
	"github.com/colespringer/waxflow/container/internal/diag"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/container/internal/trailer"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ container.Demuxer  = (*Demuxer)(nil)
	_ container.Seeker   = (*Demuxer)(nil)
	_ container.Measurer = (*Demuxer)(nil)
	_ container.Warner   = (*Demuxer)(nil)
	_ container.Tagger   = (*Demuxer)(nil)
)

// Hostile-input caps (ADR-0005 invariants).
const (
	// maxResync bounds the scan for a block header after damage.
	maxResync = 1 << 20
	// seekWindow is the bisection cutoff: below this span, walk blocks.
	seekWindow = 128 << 10
	// maxMetaBlocks bounds the run of sample-free blocks the header walk will
	// step over on its way to the first audio block. Real files hold at most
	// one; the reference decoder gives up after sixteen.
	maxMetaBlocks = 16
	// maxTailScan bounds the backward scan that recovers the length of a
	// stream whose header never learned it.
	maxTailScan = 1 << 22
	// maxProbe bounds one bisection probe's scan for a block header. A block
	// is under a megabyte (SyncOK), so a probe landing just past one block's
	// start finds the next inside this; without the bound a probe over a
	// damaged tail would pull the rest of the file into the read window.
	maxProbe = 2 << 20
	// maxHops and maxLookahead bound the walk of the stream's headers: the
	// blocks it steps to (6 hours of this encoder's, 4 MiB read as headers)
	// and the bytes its resyncs scan.
	maxHops      = 1 << 17
	maxLookahead = 1 << 22
	// maxGaps bounds the gaps in the indexes kept as holes.
	maxGaps = 1 << 10
)

// DemuxerVersion is this demuxer's term in the ADR-0004 cache key, carried
// by its track. Bump it for a change that makes the demuxer deliver other
// samples or tags for a file that has not changed.
const DemuxerVersion = "wv-demux-3"

// Demuxer reads one WavPack track from a native .wv source.
type Demuxer struct {
	cfg   wavpack.Config
	track container.Track
	tags  map[string][]string
	log   diag.Log

	firstBlock int64 // byte offset of the first audio block
	// truncated is how far the stated length runs past the blocks
	// (container.Truncated).
	truncated int64
	// declared is the header's total and promised the length the open called
	// exact, which a decode is cut at: -1 for none.
	declared, promised int64
	// reach is where the open found the stream's last block to end, and far
	// where the last audio block of any shape does: -1 with none found.
	reach, far int64
	// initialIndex is the first audio block's own sample index. A .wv file
	// carved out of a longer encode starts partway up the numbering, so
	// packet positions are measured from here rather than from zero.
	initialIndex int64

	off   int64 // offset of the current undelivered block
	cur   wavpack.BlockHeader
	valid bool
	// pts and lost are where the current audio block goes on the timeline and
	// what a hole in front of it took. next is where the read reaches once it
	// is delivered, and own where its own numbering says the next one starts.
	pts, lost, next, own int64
	// outrun is what lets a read deliver more than the open called exact. It
	// is kept for the fuzz target's length relaxation, since only the read
	// sees it; nothing in the demuxer reads it.
	outrun struct {
		backs    int  // blocks placed behind a step back in the indexes
		placed   bool // an audio block has been placed
		headBack bool // the first one placed stepped back
		// tail is the block whose end agreeing with the total alone made the
		// length exact (-1 for none), and tailRead says a read delivered it.
		tail     int64
		tailRead bool
	}
	// whole says next counts from the first block: the read began there, or
	// at a seek whose walk from there found every block at its own index.
	whole bool

	first wavpack.BlockHeader // the first audio block's header
	path  path

	// tagFloor is the lowest offset a tag may start at, the first block's end,
	// and loose says the tail scan found no block that tiles onto the data's
	// end: bytes that are no block lie behind the stream.
	tagFloor int64
	loose    bool

	// w is the shared read-ahead window; its data end is the end of block
	// data (trailing tags stripped during the header parse) and its sticky
	// error surfaces on the packet and seek paths.
	w srcwin.Window
}

// NewDemuxer parses the header of a native WavPack source and positions on
// the first audio block. The returned Demuxer implements container.Seeker,
// container.Warner, and container.Tagger.
func NewDemuxer(src container.Source, opts *container.DemuxerOptions) (*Demuxer, error) {
	d := &Demuxer{w: srcwin.New(src, src.Size(), "wavpack: reading block data"), log: diag.New("wavpack: ", opts)}
	if err := d.parse(); err != nil {
		return nil, err
	}
	return d, nil
}

// malformed reports bytes that deviate from this format: truncated,
// inconsistent, or out of range. See [waxerr.Malformed] for the rule that
// divides it from unsupported.
func malformed(format string, args ...any) error {
	return waxerr.Malformed("wavpack: ", format, args...)
}

// warn records tolerated damage, or fails in strict mode.
func (d *Demuxer) warn(off int64, format string, args ...any) error {
	return d.log.Warn(off, format, args...)
}

// note records a Warning that Strict must not escalate: this build doing
// something with a well-formed file that a caller should know about, rather
// than damage in the file. See [container.Note]. It shares warn's cap and
// de-duplication, since the list is one list.
func (d *Demuxer) note(off int64, format string, args ...any) {
	d.log.Note(off, format, args...)
}

func (d *Demuxer) parse() error {
	if !Match(d.w.Peek(0, 4)) {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return malformed("not a WavPack file")
	}
	if err := d.stripTrailers(); err != nil {
		return err
	}

	// Step over any leading sample-free blocks: an encoder can put the source
	// file's RIFF wrapper in one of its own before the audio starts.
	off := int64(0)
	var h wavpack.BlockHeader
	for i := 0; ; i++ {
		var ok bool
		h, ok = d.blockPeek(off)
		if !ok {
			if d.w.Err() != nil {
				return d.w.Err()
			}
			// A first block refused only for its version is named for it.
			if i == 0 {
				if v, ok := wavpack.StreamHead(d.w.Peek(off, wavpack.BlockHeaderLen)); ok {
					if err := wavpack.CheckStreamVersion(v); err != nil {
						return err
					}
				}
			}
			return malformed("no parsable block at offset %d", d.log.At(off))
		}
		if h.Audio() {
			break
		}
		if i >= maxMetaBlocks {
			return malformed("more than %d blocks before the first audio block", maxMetaBlocks)
		}
		off += h.Size
	}

	block := d.w.Peek(off, int(h.Size))
	if int64(len(block)) != h.Size {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return malformed("first block truncated")
	}
	cfg, err := wavpack.ProbeBlock(block)
	if err != nil {
		return err
	}
	d.cfg = cfg

	f := cfg.Format()
	if err := f.Valid(); err != nil {
		return container.UnusableFormat("wavpack", f, err)
	}
	cfgBlob, err := cfg.MarshalBinary()
	if err != nil {
		return err
	}
	d.firstBlock, d.initialIndex, d.first = off, h.BlockIndex, h
	d.off, d.cur, d.valid, d.whole = off, h, true, true
	d.settle()

	// The declared total is the encoder's back-patched length and counts from
	// sample zero, so it only applies to a stream that starts there; anything
	// else (a carved-out .wv, an interrupted encode with no total at all) has
	// its length read off the last block instead.
	//
	// Either way the length is measured from the blocks and is exact. The total
	// is a claim the first block carries, back-patched once the input ran out;
	// a file cut short afterwards still carries it, and one whose blocks outrun
	// it is the mirror image. Left unchecked, the first hands every caller a
	// duration no read can reach and strict mode a damaged file it has nothing
	// to object to, and the second hands every caller that sums lengths a
	// number the read then exceeds. A shortfall is damage and warns; an overrun
	// is the file disagreeing with itself in a way this build believes the
	// blocks about, which is a Note.
	//
	// SamplesExact says the number is a measurement, and format.Media caps a
	// decode at it: the two ends agree, or the block beside each bears its
	// index out. What lies between is the read's to find (settleEnd).
	samples, exact := int64(-1), false
	d.declared, d.reach, d.far, d.outrun.tail = -1, -1, -1, -1
	if h.BlockIndex == 0 && h.TotalSamples >= 0 {
		d.declared = h.TotalSamples
		// A tail too damaged to locate, past what the bytes can hold, or at
		// odds with the total and the block in front of it leaves the total
		// standing and unexact: the read reports what it finds where it lies.
		samples = h.TotalSamples
		t, ok := d.deliverableEnd()
		d.setFar(t)
		if ok = ok && d.vouched(t.end-d.initialIndex, d.w.DataEnd()); ok {
			d.reach = t.end - d.initialIndex
		}
		// Only a tail that could be called exact has its index checked: one
		// already unsure is the cut file it was, and warns as it did.
		front := int64(-1) // where the block in front of the tail ends
		bears := func() (ok bool) {
			if ok, front = d.frontBears(t); d.w.Err() != nil {
				front = -1
			}
			return ok
		}
		if ok && (d.reach == samples || !t.sure || d.borne(bears)) {
			// Exact only when nothing was resynced past: format.Media caps a
			// decode at an authoritative length, and a block index read across
			// a hole counts samples the file does not hold.
			if d.reach == samples && t.sure {
				d.outrun.tail = t.off
			}
			samples, exact = d.reach, t.sure
		} else if ok && d.reach < samples && front >= 0 && front <= t.index {
			// A tail short of the total and past the block in front of it is
			// a cut file either way, and not exact. Its tail is its last audio
			// block, so the rule below finds nothing past it.
			samples = d.reach
		}
	} else {
		t, ok := d.scanTail()
		d.setFar(t)
		// No total to hold the tail to: the length is the two ends' indexes,
		// each borne out by the block beside it, or there is none.
		if ok && d.vouched(t.end-d.initialIndex, d.w.DataEnd()) {
			d.reach = t.end - d.initialIndex
			if d.borne(func() bool { front, _ := d.frontBears(t); return front && d.headBears(h, t) }) {
				samples, exact = d.reach, t.sure
			}
		}
	}
	// A length stated and not exact is no less than the last audio block's
	// own end, where every open once took it, wherever the bytes could hold
	// it: a read, a seek or a measure may reach that far.
	stated := samples
	if !exact && samples >= 0 && d.far > samples && d.vouched(d.far, d.w.DataEnd()) {
		samples = d.far
	}
	switch {
	case d.declared >= 0 && samples < d.declared:
		if err := d.warn(off, "the header declares %d samples but the blocks end at %d", d.declared, samples); err != nil {
			return err
		}
		d.truncated = d.declared - samples
	case d.declared >= 0 && samples > d.declared:
		d.note(off, "the header declares %d samples but the blocks run to %d", d.declared, samples)
	case samples > stated:
		d.note(off, "the tail ends at %d samples but the blocks run to %d", stated, samples)
	}
	d.promised = -1
	if exact {
		d.promised = samples
	}
	d.tagBehindJunk()
	d.track = container.Track{
		Codec:          codec.WavPack,
		CodecConfig:    cfgBlob,
		Fmt:            f,
		DemuxerVersion: DemuxerVersion,
		Samples:        samples,
		SamplesExact:   exact,
		Default:        true,
	}
	// The stream's own depth, when the container width it decodes at is wider
	// (a 20-bit source in 24-bit words). Probe reports this rather than the
	// width, the same way a 64-bit float WAV reports 64.
	if cfg.ValidBits != f.BitDepth {
		d.track.SourceBitDepth = cfg.ValidBits
	}
	return d.w.Err()
}

// stripTrailers peels the tags a tagger appended after the audio, shrinking
// the window's data end so the block walk never runs into them. The APEv2
// block is also where the track's tags come from.
func (d *Demuxer) stripTrailers() error {
	// A tag has to leave the first block whole behind it. NUL padding is not
	// peeled: this pass has nothing to confirm a recognition against, and a
	// block declares its own length, so trailing zeros end the walk on their
	// own.
	floor := int64(wavpack.BlockHeaderLen)
	if h, ok := d.blockPeek(0); ok {
		floor = h.Size
	}
	end, tags := trailer.PeelAll(&d.w, trailer.APEv2|trailer.ID3v1|trailer.ID3v2|trailer.Lyrics3,
		floor, d.w.DataEnd())
	d.tags, d.tagFloor = tags, floor
	// No warning for a tag, unlike the FLAC demuxer: one after the audio is the
	// normal shape of a .wv file rather than damage recovered from, and
	// warning would make strict mode reject every tagged WavPack file. A
	// footer that points into the first block is not a tag, and stays.
	if at, ok := trailer.Refused(&d.w, floor, end); ok {
		if err := d.warn(end-apev2.FooterLen, "an APEv2 footer places its tag at offset %d, inside the first block; not read", d.log.At(at)); err != nil {
			return err
		}
	}
	d.w.SetDataEnd(end)
	return d.w.Err()
}

// tagBehindJunk reads the APEv2 tag that bytes no peel recognizes hide from
// it, when the peel found none and the blocks stop short of the data's end.
// The blocks still say where the audio ends. A read that fails is no answer.
func (d *Demuxer) tagBehindJunk() {
	if d.tags != nil || !d.loose {
		return
	}
	d.w.Try(false, func() bool {
		d.tags, _, _ = trailer.Behind(&d.w, d.tagFloor, d.w.DataEnd())
		return true
	})
}

// tail is the last audio block a read can deliver, as the open measured it.
type tail struct {
	off        int64 // where the block starts
	held       int64 // the window holds the bytes from here to it
	index, end int64 // its first sample's index, and the index one past its last
	// any is the index the last audio block of any shape ends at, which is
	// where every open once took its length from; 0 for none.
	any int64
	// sure says nothing on the way puts the measurement in doubt: no bytes
	// stepped over, no index that does not carry on, no block of another
	// shape behind the stream's last.
	sure bool
}

// scanTail looks back from the end of the audio for the last block of the
// stream's shape whose length lands on that end, which tells it from a
// "wvpk" a payload spells. One of another shape leaves the answer unsure.
func (d *Demuxer) scanTail() (tail, bool) {
	t := tail{sure: true}
	audioEnd := d.w.DataEnd()
	lo := max(d.firstBlock, audioEnd-maxTailScan)
	passed, step := int64(-1), int64(0) // the block passed over, and its step's from
	try := func(cand, from int64) bool {
		if passed >= 0 && cand >= passed {
			return false // offered before the pass
		}
		h, ok := d.blockAt(cand)
		if !ok || !h.Audio() || !d.tilesToEnd(cand+h.Size, audioEnd) {
			return false
		}
		if t.any == 0 {
			t.any = h.BlockIndex + int64(h.BlockSamples)
		}
		if !d.fits(h) {
			if t.sure {
				t.sure, passed, step = false, cand, from
				return true
			}
			return false
		}
		t.off, t.held, t.index, t.end = cand, from, h.BlockIndex, h.BlockIndex+int64(h.BlockSamples)
		return true
	}
	found := d.scanBack(lo, audioEnd, try)
	if t.sure {
		d.loose = !found
		return t, found
	}
	// The scan once ended at the block passed over, so behind it the scan
	// only strengthens the answer. Resumed at the step's own from, it offers
	// what that step had left, from bytes the window holds.
	hi := min(audioEnd, step+srcwin.Chunk)
	found, _ = d.w.Try(true, func() bool { return d.scanBack(lo, hi, try) })
	return t, found
}

// scanBack offers try each "wvpk" in [lo, hi), the last first, with the
// offset its step read from, until try takes one. A step reads a Chunk in
// front of the window, so a hop forward from a candidate reads nothing.
func (d *Demuxer) scanBack(lo, hi int64, try func(cand, from int64) bool) bool {
	for from := max(lo, hi-srcwin.Chunk); hi > lo; from = max(lo, from-srcwin.Chunk) {
		buf := d.w.Back(from, int(hi-from))
		if len(buf) == 0 {
			return false
		}
		for rel := len(buf); ; {
			i := bytes.LastIndex(buf[:rel], magic)
			if i < 0 {
				break
			}
			rel = i + len(magic) - 1
			if try(from+int64(i), from) {
				return true
			}
		}
		if from == lo {
			break // the whole range is scanned
		}
		// Overlap by the magic length so a candidate straddling the chunk
		// boundary is not lost between passes.
		hi = from + int64(len(magic)) - 1
	}
	return false
}

// deliverableEnd is the last block a read can deliver, which verifies the
// header's total. The tail scan answers for a stream whose blocks tile onto
// the end of the audio, in one window; one cut mid-block is walked.
func (d *Demuxer) deliverableEnd() (tail, bool) {
	t, ok := d.scanTail()
	if ok {
		return t, true
	}
	if t.sure {
		return d.walkTail()
	}
	// The scan passed over a block of another shape, where it once ended:
	// the walk is this open's own extra, and meets that block again. A read
	// that fails in it is no answer.
	any := t.any
	ok, _ = d.w.Try(false, func() (found bool) { t, found = d.walkTail(); return found })
	if any != 0 {
		t.any = any
	}
	return t, ok
}

// walkTail bisects to the closing window and walks it by the read's steps to
// the last audio block whose bytes are all there. It reports nothing: what
// it crosses is the read's to report, and leaves the answer unsure.
func (d *Demuxer) walkTail() (t tail, ok bool) {
	off, h, found := d.bisect(math.MaxInt64) // no target: land on the last window
	if !found {
		return tail{}, false
	}
	t.sure = true
	changed := false // behind a change of shape only any is still counted
	for {
		if h.Audio() {
			t.any = h.BlockIndex + int64(h.BlockSamples)
		}
		switch {
		case changed || !h.Audio():
		case !d.fits(h):
			// A stray block is stepped over; a change of shape ends the read.
			t.sure = false
			changed = !ok || !d.stray(h, t.end-d.initialIndex)
		default:
			if ok && h.BlockIndex != t.end {
				t.sure = false
			}
			t.off, t.held, t.index, t.end, ok = off, off, h.BlockIndex, h.BlockIndex+int64(h.BlockSamples), true
		}
		d.w.Trim(off)
		end := off + h.Size
		if off, h, found = d.after(end); !found {
			if d.w.Err() != nil {
				return tail{}, false
			}
			t.sure = t.sure && end >= d.w.DataEnd()
			break
		}
		t.sure = t.sure && off == end
	}
	return t, ok
}

// borne runs a check of an index that an exact length would rest on. A read
// that fails in it is no answer and not the open's error.
func (d *Demuxer) borne(check func() bool) bool {
	ok, _ := d.w.Try(true, check)
	return ok
}

// frontBears reports whether the block in front of the tail carries on to
// it, and the index that block ends at, -1 with none found. It is the nearest,
// a probe's reach back, that a read steps to, or a nearer one that carries on.
func (d *Demuxer) frontBears(t tail) (bears bool, end int64) {
	if t.off == d.firstBlock {
		return true, t.index
	}
	end = -1
	try := func(cand, _ int64) bool {
		hdr := d.w.BytesAt(cand, wavpack.BlockHeaderLen)
		h, ok := d.block(cand, hdr)
		if !ok || !h.Audio() || !d.fits(h) || cand+h.Size > t.off {
			return false
		}
		at := h.BlockIndex + int64(h.BlockSamples)
		if bears = at == t.index; !bears && cand != d.firstBlock {
			if _, stepped := d.confirm(cand, hdr); !stepped {
				return false
			}
		}
		end = at
		return true
	}
	// What the window holds first: an intact stream's is there, unread.
	lo := max(d.firstBlock, t.off-maxProbe)
	if held := max(lo, t.held); d.scanBack(held, t.off, try) || held == lo {
		return bears, end
	}
	d.scanBack(lo, min(t.off, t.held+int64(len(magic))-1), try)
	return bears, end
}

// headBears reports whether the audio block a read steps to behind the first
// bears the first one's index out by carrying on from it. A first block that
// is the tail has none behind.
func (d *Demuxer) headBears(first wavpack.BlockHeader, t tail) bool {
	if t.off == d.firstBlock {
		return true
	}
	end := d.firstBlock + first.Size
	for range maxMetaBlocks + 1 {
		// An exact read first: an intact stream holds the header here.
		off, h, ok := end, wavpack.BlockHeader{}, false
		if h, ok = d.blockPeek(end); !ok {
			if off, h, ok = d.nextCandidate(end, end+maxResync); !ok {
				return false
			}
		}
		if h.Audio() {
			return d.fits(h) && h.BlockIndex == first.BlockIndex+int64(first.BlockSamples)
		}
		end = off + h.Size
	}
	return false
}

// tilesToEnd reports whether the blocks from off tile exactly to end. It is
// the tail scan's confirmation: a real last audio block is followed only by
// the sample-free blocks a stream signs off with, and they land on the end of
// the audio exactly. A "wvpk" the payload happened to spell does not.
func (d *Demuxer) tilesToEnd(off, end int64) bool {
	for range maxMetaBlocks + 1 {
		switch {
		case off == end:
			return true
		case off > end:
			return false
		}
		h, ok := d.blockAt(off)
		if !ok {
			return false
		}
		off += h.Size
	}
	return false
}

// blockAt parses the block header at off and reports whether a whole block
// starts there: the header parses and its declared length fits inside the
// audio data. That is all a sequential step needs, because a WavPack block
// carries its own length and the position it steps from was already trusted.
// It is the packet path's read, with a window of read-ahead behind it.
func (d *Demuxer) blockAt(off int64) (wavpack.BlockHeader, bool) {
	return d.block(off, d.w.BytesAt(off, wavpack.BlockHeaderLen))
}

// blockPeek is blockAt through an exact read, for the open path: the blocks
// ahead of the first audio block are read once, and a window of read-ahead
// there is one the tail scan pays again.
func (d *Demuxer) blockPeek(off int64) (wavpack.BlockHeader, bool) {
	return d.block(off, d.w.Peek(off, wavpack.BlockHeaderLen))
}

func (d *Demuxer) block(off int64, buf []byte) (wavpack.BlockHeader, bool) {
	if len(buf) < wavpack.BlockHeaderLen || !wavpack.SyncOK(buf) {
		return wavpack.BlockHeader{}, false
	}
	h, err := wavpack.ParseBlockHeader(buf)
	if err != nil || off+h.Size > d.w.DataEnd() {
		return wavpack.BlockHeader{}, false
	}
	return h, true
}

// confirm is block plus the checks a scan needs: what follows the block must
// be either the end of the audio or another block header, and the block must
// agree with the track. Compressed payload can spell "wvpk" and pass the
// header sniff; chaining to a second header is what tells a real boundary from
// that coincidence. Only the paths that guess an offset (resync, bisection
// probes) pay for it. hdr is the scan's view at off; the block and the header
// behind it are one read from off, which extends the window.
func (d *Demuxer) confirm(off int64, hdr []byte) (wavpack.BlockHeader, bool) {
	h, ok := d.block(off, hdr)
	if !ok {
		return wavpack.BlockHeader{}, false
	}
	// A candidate that disagrees with the track is not this stream's block, so
	// the scan keeps looking. Only the sequential walk treats a shape change
	// as the hard error it is; a guess that lands on one is just a bad guess.
	if h.Audio() && !d.fits(h) {
		return wavpack.BlockHeader{}, false
	}
	end := off + h.Size
	if end == d.w.DataEnd() {
		return h, true
	}
	b := d.w.Ahead(off, int(h.Size)+wavpack.BlockHeaderLen)
	if int64(len(b)) < h.Size+wavpack.BlockHeaderLen {
		return wavpack.BlockHeader{}, false
	}
	if _, ok := d.block(end, b[h.Size:]); !ok {
		return wavpack.BlockHeader{}, false
	}
	return h, true
}

// magic is the block marker the scans look for.
var magic = []byte("wvpk")

// nextCandidate scans [from, limit) for a confirmable block header. It is the
// resync path after damage and the bisection probe.
func (d *Demuxer) nextCandidate(from, limit int64) (int64, wavpack.BlockHeader, bool) {
	var h wavpack.BlockHeader
	off, ok := d.w.Scan(from, limit, magic, wavpack.BlockHeaderLen, func(off int64, hdr []byte) srcwin.Verdict {
		got, ok := d.confirm(off, hdr)
		if !ok {
			return srcwin.Skip
		}
		h = got
		return srcwin.Take
	})
	return off, h, ok
}

// after is the block a read steps to from end, where the one before it
// ended: the block that starts there, or the first one a resync confirms.
func (d *Demuxer) after(end int64) (int64, wavpack.BlockHeader, bool) {
	if end >= d.w.DataEnd() {
		return 0, wavpack.BlockHeader{}, false
	}
	if h, ok := d.blockAt(end); ok {
		return end, h, true
	}
	return d.nextCandidate(end, end+maxResync)
}

// stray reports whether h is an audio block of another shape whose index does
// not carry on from the stream's own numbering: not this stream's, and
// stepped over. One that carries on is a change of shape, where a read fails.
func (d *Demuxer) stray(h wavpack.BlockHeader, own int64) bool {
	return h.Audio() && !d.fits(h) && d.pos(h) != own
}

// advance moves to the block after the current one, resyncing over damage
// and stepping over stray blocks, and places it when it carries samples. The
// step is taken once its findings are reported, so Strict leaves the read be.
// ctx stops a walk that places it, with the step not taken.
func (d *Demuxer) advance(ctx context.Context) error {
	end := d.off + d.cur.Size
	type finding struct {
		off int64
		msg string
	}
	var found []finding
	// One finding for a run of stray blocks, at the first of them.
	strays := func(at, n int64) {
		if n > 0 {
			found = append(found, finding{at, fmt.Sprintf("%d bytes of blocks of another shape are not this stream's, stepped over", n)})
		}
	}
	report := func() error {
		for _, f := range found {
			if err := d.warn(f.off, "%s", f.msg); err != nil {
				return err
			}
		}
		return nil
	}
	var strayAt, strayLen int64
	for {
		if end >= d.w.DataEnd() {
			strays(strayAt, strayLen)
			if err := report(); err != nil {
				return err
			}
			d.valid = false
			return nil
		}
		off, h, ok := d.after(end)
		if !ok {
			if d.w.Err() != nil {
				return d.w.Err()
			}
			strays(strayAt, strayLen)
			found = append(found, finding{end, fmt.Sprintf("%d trailing bytes are not a block, dropped", d.w.DataEnd()-end)})
			if err := report(); err != nil {
				return err
			}
			d.valid = false
			return nil
		}
		if d.stray(h, d.own) {
			if off > end {
				strays(strayAt, strayLen)
				strayLen = 0
				found = append(found, finding{end, fmt.Sprintf("%d unparsable bytes between blocks", off-end)})
			}
			if strayLen == 0 {
				strayAt = off
			}
			strayLen += h.Size
			end = off + h.Size
			continue
		}
		strays(strayAt, strayLen)
		// A sample-free block's index is no position, and a change of shape
		// is refused where it is read.
		var at placement
		alone, behind := "", ""
		if h.Audio() && d.fits(h) {
			var err error
			if at, alone, behind, err = d.place(ctx, off, h); err != nil {
				return err
			}
		}
		switch {
		case off > end && behind != "":
			found = append(found, finding{end, fmt.Sprintf("%d unparsable bytes between blocks: %s", off-end, behind)})
		case off > end:
			found = append(found, finding{end, fmt.Sprintf("%d unparsable bytes between blocks", off-end)})
		case alone != "":
			found = append(found, finding{off, alone})
		}
		if err := report(); err != nil {
			return err
		}
		d.off, d.cur = off, h
		if h.Audio() && d.fits(h) {
			d.pts, d.lost, d.next, d.own = at.pts, at.lost, at.next, at.own
			if at.back {
				d.outrun.backs++
				d.outrun.headBack = d.outrun.headBack || !d.outrun.placed
			}
			d.outrun.placed = true
		}
		return nil
	}
}

// fits reports whether an audio block is the track's shape, its channels and
// depth. The read delivers no other, so a scan that guesses an offset takes no
// other either. Before the track is known, every shape fits.
func (d *Demuxer) fits(h wavpack.BlockHeader) bool {
	return d.cfg.Channels == 0 || h.Channels() == d.cfg.Channels && h.BytesPerSample()*8 == d.cfg.BitDepth
}

// vouched reports whether a block ending at sample end, and at byte endOff,
// could be where it says: the bytes in front of its end hold that many samples.
func (d *Demuxer) vouched(end, endOff int64) bool {
	return end >= 0 && end <= wavpack.MaxSamplesIn(endOff-d.firstBlock)
}

// settle places the current block at its own index, where a read starts: at
// the open and on a seek's landing.
func (d *Demuxer) settle() {
	p, n := d.pos(d.cur), int64(d.cur.BlockSamples)
	d.pts, d.lost, d.next, d.own = p, 0, p+n, p+n
}

// placement is where an audio block goes and where the read then stands.
// back says its index stepped back from the read.
type placement struct {
	pts, lost, next, own int64
	back                 bool
}

// place puts the audio block h at off where the read has reached, or past a
// hole at its own index when the stream bears the skip out. Any other skip,
// and a step back, are findings: it returns one alone and as said behind a resync.
func (d *Demuxer) place(ctx context.Context, off int64, h wavpack.BlockHeader) (at placement, alone, behind string, err error) {
	p, n := d.pos(h), int64(h.BlockSamples)
	from := d.next
	at.pts = from
	switch {
	case p == from || p == d.own:
	case p > from:
		hole, checked, err := d.hole(ctx, off)
		if err != nil {
			return at, "", "", err
		}
		if hole {
			at.pts, at.lost = p, p-from
			alone = fmt.Sprintf("the blocks skip from sample %d to %d: %d samples lost", from, p, p-from)
			behind = fmt.Sprintf("%d samples lost", p-from)
			break
		}
		why := "which nothing bears out"
		if !checked {
			why = "which could not be checked"
		}
		alone = fmt.Sprintf("the blocks skip from sample %d to %d, %s; read on in order", from, p, why)
		behind = alone
	default:
		at.back = true
		alone = fmt.Sprintf("the blocks step back from sample %d to %d; read on in order", from, p)
		behind = alone
	}
	at.next, at.own = at.pts+n, p+n
	return at, alone, behind, nil
}

// hole reports whether the skip that lands on the block at off is a hole, and
// whether the headers could be walked to say. It is one only in a stream
// that bears every skip out (path.back) and ends inside the promised length.
// A read that fails in the walk is no answer; ctx ending pauses it.
func (d *Demuxer) hole(ctx context.Context, off int64) (hole, checked bool, err error) {
	p := &d.path
	if !p.done && !p.spent {
		var walked error
		d.w.Try(false, func() bool { walked = d.walkPath(ctx); return walked == nil })
		if walked != nil && ctx.Err() != nil {
			return false, false, walked
		}
	}
	if !p.done {
		return false, false, nil
	}
	if p.back || d.promised >= 0 && p.own-d.initialIndex > d.promised {
		return false, true, nil
	}
	_, hole = slices.BinarySearch(p.gaps, off)
	return hole, true, nil
}

// path is what one walk of the stream's headers found, from the first block
// by the read's own steps: a function of the file's bytes, which every skip
// and the measured length are answered from.
type path struct {
	end int64 // where the next step starts
	own int64 // the index the last block of the stream ends at
	// agree is where a read from the first block, read on in order, ends
	// the run of blocks each at its own index: a seek reaches as far.
	agree int64
	split bool    // a block since sat off its own index
	gaps  []int64 // the offsets of the blocks behind a gap in the indexes
	// back says the stream does not bear its skips out: an index steps back,
	// a gap is past what the bytes could hold, there are more gaps than are
	// kept, or the stream changes shape.
	back bool
	// change is where a block of another shape carries on from the stream,
	// which is where a read fails, 0 for none; told once a measure warned it.
	change      int64
	told        bool
	hops        int
	resynced    int64 // bytes scanned across resyncs
	done, spent bool  // at the stream's end; out of hops or bytes, for good
}

// walkPath takes the walk on to the stream's end through a window of its
// own: one exact read of a header a block, a scan only where a read would
// resync. A failed read, or ctx ending, leaves it where it stood.
func (d *Demuxer) walkPath(ctx context.Context) error {
	p := &d.path
	if p.end == 0 {
		n := int64(d.first.BlockSamples)
		p.end, p.own, p.agree = d.firstBlock+d.first.Size, d.first.BlockIndex+n, n
	}
	for !p.done && !p.spent {
		if err := container.WalkCanceled(ctx, "wavpack: "); err != nil {
			return err
		}
		if p.end >= d.w.DataEnd() {
			p.done = true
			break
		}
		if p.hops >= maxHops || p.resynced > maxLookahead {
			p.spent = true
			break
		}
		d.w.Trim(p.end)
		off := p.end
		h, ok := d.blockPeek(off)
		if d.w.Err() != nil {
			return d.w.Err()
		}
		if !ok {
			off, h, ok = d.nextCandidate(p.end, p.end+maxResync)
			if d.w.Err() != nil {
				return d.w.Err()
			}
			if !ok {
				p.done = true // trailing bytes: a read ends here
				break
			}
			p.resynced += off - p.end
		}
		p.hops++
		switch own := p.own - d.initialIndex; {
		case !h.Audio() || d.stray(h, own):
		case !d.fits(h):
			p.change, p.back, p.done = off, true, true
		default:
			at, n := d.pos(h), int64(h.BlockSamples)
			switch {
			case at < own:
				p.back = true
			case at > own:
				if len(p.gaps) < maxGaps && d.vouched(at+n, off+h.Size) {
					p.gaps = append(p.gaps, off)
				} else {
					p.back = true
				}
			}
			if p.split = p.split || at != p.agree; !p.split {
				p.agree += n
			}
			p.own = h.BlockIndex + n
		}
		if !p.done {
			p.end = off + h.Size
		}
	}
	return nil
}

// Tracks returns the single WavPack track.
func (d *Demuxer) Tracks() []container.Track { return []container.Track{d.track} }

// Warnings returns damage tolerated during parsing.
func (d *Demuxer) Warnings() []container.Warning { return d.log.List() }

// DamageCount implements container.Damaged.
func (d *Demuxer) DamageCount() int { return d.log.DamageCount() }

// TruncatedSamples implements container.Truncated.
func (d *Demuxer) TruncatedSamples(int) int64 { return d.truncated }

// Tags returns the APEv2 tag's fields under canonical uppercase keys.
func (d *Demuxer) Tags() map[string][]string { return d.tags }

// ReadPacket yields one whole block, header included, its data reused across
// calls; blocks with no samples are metadata and stepped over. A block past
// a hole is marked Discont, with Lost saying how many samples the hole took.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error {
	for d.valid {
		d.w.Trim(d.off)
		h := d.cur
		if !h.Audio() {
			// Stream metadata (a RIFF trailer, an MD5 signature), not a packet.
			if err := d.advance(context.Background()); err != nil {
				return err
			}
			continue
		}
		if err := d.checkShape(h); err != nil {
			return err
		}
		data := d.w.BytesAt(d.off, int(h.Size))
		if int64(len(data)) != h.Size {
			if d.w.Err() != nil {
				return d.w.Err()
			}
			return waxerr.New(waxerr.CodeSourceUnreadable, "wavpack: reading block data")
		}
		pts, lost := d.pts, d.lost
		d.outrun.tailRead = d.outrun.tailRead || d.off == d.outrun.tail
		if err := d.advance(context.Background()); err != nil {
			return err
		}
		*pkt = container.Packet{
			Track: 0,
			Lost:  lost,
			Packet: codec.Packet{
				Data:    data,
				PTS:     pts,
				Dur:     int64(h.BlockSamples),
				Sync:    true,
				Discont: lost > 0,
			},
		}
		return nil
	}
	if d.w.Err() != nil {
		return d.w.Err()
	}
	if err := d.settleEnd(); err != nil {
		return err
	}
	return io.EOF
}

// settleEnd holds the length the open called exact against where a whole
// read ended. Short of it the track takes what is there and counts the rest
// truncated; past it the overrun is noted; either way it is no longer exact.
func (d *Demuxer) settleEnd() error {
	t := d.track
	if !d.whole || !t.SamplesExact || d.next == t.Samples {
		return nil
	}
	of := "the header declares"
	if t.Samples != d.declared {
		of = "the tail ends at"
	}
	if d.next > t.Samples {
		d.note(d.firstBlock, "%s %d samples but the blocks run to %d", of, t.Samples, d.next)
	} else if err := d.warn(d.firstBlock, "%s %d samples but the blocks end at %d", of, t.Samples, d.next); err != nil {
		return err
	}
	// The config states no length, so it is restated as it is.
	tr, n, err := container.SettleRead(t, d.next, func(config []byte, _ int64) ([]byte, error) { return config, nil })
	d.track, d.truncated = tr, d.truncated+n
	return err
}

// MeasureSamples implements container.Measurer by a walk of the headers
// where the open did not call the length exact; -1 says the walk has no
// answer. The track stays as it was, so nothing is cut at the measure.
func (d *Demuxer) MeasureSamples(ctx context.Context, track int) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("wavpack: no track %d", track))
	}
	if d.promised >= 0 {
		return d.promised, nil
	}
	// The walk decides the answer, so a read that fails in it is the
	// measure's error.
	var err error
	_, read := d.w.Try(false, func() bool {
		if err = d.walkPath(ctx); err == nil && d.far < 0 && d.path.done {
			// The open found no tail: where a seek past the end lands.
			t, _ := d.walkTail()
			d.setFar(t)
		}
		return err == nil
	})
	err = cmp.Or(read, err)
	p := &d.path
	if err != nil || !p.done {
		return -1, err
	}
	// A change of shape ends the walk as it ends a read, and is its finding.
	if p.change > 0 && !p.told {
		if err := d.warn(p.change, "a block of another shape carries on from the stream, which changes shape there"); err != nil {
			return -1, err
		}
		p.told = true
	}
	if !p.back {
		// Every gap is a hole, so a read ends where the indexes do, and a
		// seek reaches the tail the open found.
		return max(p.own-d.initialIndex, d.reach), nil
	}
	// No skip is a hole, so every read is the one it was before the indexes
	// were read: the further of the length it was held to and where a read
	// leaves the run of blocks at their own indexes, which a seek reaches.
	return max(d.far, p.agree), nil
}

// setFar keeps where the last audio block of any shape ends, which is no
// length when it lies in front of the first block's index.
func (d *Demuxer) setFar(t tail) {
	if t.any != 0 && t.any >= d.initialIndex {
		d.far = t.any - d.initialIndex
	}
}

// checkShape rejects a block whose channel count or width disagrees with the
// track. WavPack permits the change; the fixed-format pipeline does not, so
// it is a hard error rather than tolerated damage.
func (d *Demuxer) checkShape(h wavpack.BlockHeader) error {
	if !d.fits(h) {
		return malformed("mid-stream format change at offset %d", d.log.At(d.off))
	}
	return nil
}

// SeekSample repositions to the block containing the target sample and
// returns that block's first sample; format.Media pre-rolls the remainder.
// Bisection on the blocks' own sample indexes narrows the range, then the
// remaining span is walked. A skip it crosses walks the headers under ctx.
func (d *Demuxer) SeekSample(ctx context.Context, track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("wavpack: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "wavpack: negative seek target")
	}
	off, h, ok := d.bisect(sample)
	if !ok {
		if d.w.Err() != nil {
			return 0, d.w.Err()
		}
		return 0, malformed("cannot relocate any block for seeking")
	}
	d.off, d.cur, d.valid = off, h, true
	d.settle()
	// Walk forward to the block holding the target, stepping over the
	// sample-free blocks a stream ends with (an MD5 signature, a RIFF
	// trailer): their block index is not a position, so landing on one would
	// report whatever the encoder happened to write there. A past-the-end
	// target runs out of blocks and the walk stands on the last one that
	// carried samples, which is where the seek lands and where reads resume.
	// The walk places the blocks it steps to as a read does, in the read's
	// words, and lands on a block's own index. A block of another shape is
	// stepped over and never landed on: a read delivers none.
	lastOff, lastH := d.off, d.cur
	// whole holds while the landing still counts from the first block.
	whole := off == d.firstBlock
	d.whole = false
	for d.valid {
		if d.cur.Audio() && !d.fits(d.cur) {
			whole = false
		} else if d.cur.Audio() {
			whole = whole && d.pts == d.pos(d.cur)
			// A block answers the seek by containing the target, not merely
			// by ending after it. Nothing checksums a block header, so a
			// damaged stream can leave a hole in the timeline, and the block
			// past a hole ends after a target inside it while starting later
			// than one already walked over. Landing early is recoverable (the
			// caller decodes forward and discards), landing late is not; the
			// walk stops there, since bisect already takes positions as
			// ordered.
			if d.pos(d.cur) > sample {
				break
			}
			if d.pos(d.cur)+int64(d.cur.BlockSamples) > sample {
				d.settle()
				d.whole = whole
				return d.pts, nil
			}
			lastOff, lastH = d.off, d.cur
		}
		d.w.Trim(d.off)
		if err := d.advance(ctx); err != nil {
			return 0, err
		}
	}
	d.off, d.cur, d.valid = lastOff, lastH, true
	d.settle()
	d.whole = whole
	return d.pts, nil
}

// pos is a block's position in the track timeline.
func (d *Demuxer) pos(h wavpack.BlockHeader) int64 { return h.BlockIndex - d.initialIndex }

// bisect narrows the byte range holding the target sample using block headers
// found from probe offsets, then hands back the earliest block of the final
// window.
func (d *Demuxer) bisect(sample int64) (int64, wavpack.BlockHeader, bool) {
	lo, loH, ok := d.firstBlock, wavpack.BlockHeader{}, false
	if loH, ok = d.blockAt(lo); !ok || !loH.Audio() {
		lo, loH, ok = d.nextAudio(lo, lo+maxResync)
		if !ok {
			return 0, wavpack.BlockHeader{}, false
		}
	}
	if d.pos(loH) > sample {
		return lo, loH, true
	}
	hi := d.w.DataEnd()
	for hi-lo > seekWindow {
		mid := lo + (hi-lo)/2
		off, h, found := d.probe(mid, hi)
		// A probe a read failed in places nothing, and no later one will.
		if d.w.Err() != nil {
			return 0, wavpack.BlockHeader{}, false
		}
		if !found || d.pos(h) > sample {
			hi = mid
			continue
		}
		lo, loH = off, h
	}
	return lo, loH, true
}

// probe is one bisection probe: the first audio block from mid, short of hi.
// It reads a window at mid first, not cut at hi, since that block may end past
// it; past a block's worth of scan the span is damaged and halving answers it.
func (d *Demuxer) probe(mid, hi int64) (int64, wavpack.BlockHeader, bool) {
	d.w.Trim(mid)
	d.w.Peek(mid, srcwin.Chunk)
	return d.nextAudio(mid, min(hi, mid+maxProbe))
}

// nextAudio is nextCandidate restricted to blocks that carry samples, which
// are the only ones a seek can land on.
func (d *Demuxer) nextAudio(from, limit int64) (int64, wavpack.BlockHeader, bool) {
	for off := from; off < limit; {
		cand, h, ok := d.nextCandidate(off, limit)
		if !ok {
			return 0, wavpack.BlockHeader{}, false
		}
		if h.Audio() {
			return cand, h, true
		}
		off = cand + 1
	}
	return 0, wavpack.BlockHeader{}, false
}
