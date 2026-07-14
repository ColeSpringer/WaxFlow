package vorbis

import (
	"fmt"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/dsp/psy"
	"github.com/colespringer/waxflow/waxerr"
)

var _ codec.Encoder = (*Encoder)(nil)

// EncoderVersion identifies the encode algorithm/bitstream revision for the
// ADR-0004 cache key. It composes the psychoacoustic model's revision, so
// retuning dsp/psy invalidates exactly these streams. Bump it whenever a change
// alters the produced bytes.
const EncoderVersion = "vorbis-enc-7+" + psy.Version

// encVendor is the fixed vendor string the standalone encoder stamps into the
// comment header, so deterministic-mode output stays byte-identical. In the
// container path the muxer owns the comment header.
const encVendor = "WaxFlow"

// EncoderOptions configures NewEncoder. The zero value is a sensible default
// quality.
type EncoderOptions struct {
	// Quality selects VBR quality on libvorbis's -q scale (-1..10); higher is
	// larger and better. The zero value selects DefaultQuality (matching the
	// repo's "0 means default" option idiom); pass a small nonzero value near 0
	// for the lowest qualities. Vorbis is natively quality/VBR-based, so this is
	// the primary knob.
	Quality float64
	// Bitrate is a reserved ABR target in bits per second. ABR rate control is
	// not implemented, so a nonzero value is rejected by NewEncoder rather than
	// silently ignored (which would make Bitrate() report a rate the VBR stream
	// never holds); leave it 0 for quality-driven VBR.
	Bitrate int
}

// DefaultQuality is the VBR quality used when EncoderOptions.Quality is unset.
const DefaultQuality = 3.0

// plannedBlock is one scheduled block: its transform size and its center in
// absolute input-sample coordinates. Consecutive centers advance by
// (prevSize+size)/4, the spacing the decoder's overlap-add assumes.
type plannedBlock struct {
	size   int
	center int64
}

// Encoder is a Vorbis I encoder producing raw Vorbis audio packets; a container
// muxer (Ogg, Matroska) frames them and carries CodecConfig's three headers.
// It uses psychoacoustic bit allocation and switches to short blocks on
// transients to contain pre-echo. Channels are coded independently (coupling is
// a separate step).
type Encoder struct {
	fmt      audio.Format
	channels int
	cfg      *encConfig
	config   []byte // cached CodecConfig blob

	quality  float64
	offsetDB float64

	fwd    [2]*mdctForward     // per slot (short, long) forward transform
	psy    [2][]*psy.Model     // per slot, per channel masking model
	attack *psy.AttackDetector // transient detector over the mono downmix
	long   int                 // long block size
	short  int                 // short block size

	// Absolute per-channel input buffer. buf[c][0] holds the sample at absolute
	// input index bufBase. The buffer keeps a block of history behind the current
	// center (block windows can reach back) plus the look-ahead the block-size
	// planner and the next-neighbour window need.
	buf       [][]float32
	bufBase   int64
	inSamples int64

	// Block schedule. pending holds decided-but-unemitted blocks in center order;
	// a block is emitted once its successor is decided (its right window ramp
	// needs the neighbour size) and its window is fully buffered.
	pending      []plannedBlock
	lastCenter   int64   // center of the most recently planned block
	lastN        int     // size of the most recently planned block
	prevN        int     // size of the most recently emitted block (left neighbour)
	firstBlock   bool    // the next emit is the overlap-priming packet
	plannedFirst bool    // the first (always-long) block has been planned
	shortRun     int     // remaining forced short blocks around a transient
	scanPos      int64   // absolute position up to which transients are scanned
	attackPos    []int64 // sorted absolute onset positions still ahead of emission
	decoded      int64   // cumulative decoded output position

	// Per-block scratch, sized to the long block, sliced to n2 per block.
	wbuf    []float32
	spec    [][]float32
	curve   [][]float32
	resid   [][]float32
	vals    [][]int // per-channel floor post values (reused per block)
	targets []int   // floor1Fit scratch (reused per block/channel)
	classes [][]int // per-channel residue classes (magnitude and angle when coupled)
	thrLine []float64
	fFinal  []int
	fStep2  []bool
	monobuf []float32 // mono downmix window for the attack detector
	psyIn   []float32 // raw block scratch for the psy analysis
	w       bitWriter
}

// NewEncoder returns a Vorbis encoder for f, which must be float32 (Vorbis is
// inherently float) with 1..MaxChannels channels at a positive rate.
func NewEncoder(f audio.Format, opts *EncoderOptions) (*Encoder, error) {
	if err := f.Valid(); err != nil {
		return nil, err
	}
	if f.Type != audio.Float || f.BitDepth != 32 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat, "vorbis: encoder input must be float32")
	}
	if f.Channels < 1 || f.Channels > maxChannels {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("vorbis: %d channels outside 1..%d", f.Channels, maxChannels))
	}
	quality := DefaultQuality
	if opts != nil && opts.Quality != 0 {
		quality = opts.Quality
	}
	if quality < -1 || quality > 10 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("vorbis: quality %g outside -1..10", quality))
	}
	if opts != nil && opts.Bitrate != 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			"vorbis: ABR (Bitrate target) is not implemented; leave Bitrate 0 for quality-driven VBR")
	}

	cfg := newEncConfig(f.Channels, f.Rate)
	long := cfg.blockSizes[slotLong]
	short := cfg.blockSizes[slotShort]
	e := &Encoder{
		fmt:      f,
		channels: f.Channels,
		cfg:      cfg,
		quality:  quality,
		offsetDB: qualityToOffsetDB(quality),
		long:     long,
		short:    short,
		attack:   psy.NewAttackDetector(attackRatio),
	}
	e.fwd[slotLong] = newMDCTForward(long)
	e.fwd[slotShort] = newMDCTForward(short)
	e.config = cfg.codecConfig(encVendor, nil)

	maxN2 := long / 2
	maxPosts := len(cfg.floors[slotLong].xs) // long floor has the most posts
	e.wbuf = make([]float32, long)
	e.monobuf = make([]float32, short)
	e.buf = make([][]float32, f.Channels)
	e.spec = make([][]float32, f.Channels)
	e.curve = make([][]float32, f.Channels)
	e.resid = make([][]float32, f.Channels)
	e.vals = make([][]int, f.Channels)
	e.classes = make([][]int, f.Channels)
	for s := 0; s < 2; s++ {
		e.psy[s] = make([]*psy.Model, f.Channels)
	}
	// Prime: a half-long block of silence lead-in so the first (priming) packet's
	// overlap consumes it and no real sample is lost. buf[0] then sits at absolute
	// index -long/2; the first real block is centered at 0.
	e.bufBase = -int64(long / 2)
	for c := 0; c < f.Channels; c++ {
		e.buf[c] = make([]float32, long/2, long/2+4*long)
		e.spec[c] = make([]float32, maxN2)
		e.curve[c] = make([]float32, maxN2)
		e.resid[c] = make([]float32, maxN2)
		e.vals[c] = make([]int, maxPosts)
		e.classes[c] = make([]int, maxN2/resPartSize)
		for s := 0; s < 2; s++ {
			n2 := cfg.blockSizes[s] / 2
			m, err := newPsyModel(f.Rate, n2, e.offsetDB)
			if err != nil {
				return nil, err
			}
			e.psy[s][c] = m
		}
	}
	e.thrLine = make([]float64, maxN2)
	e.targets = make([]int, maxPosts)
	e.fFinal = make([]int, maxPosts)
	e.fStep2 = make([]bool, maxPosts)

	// The virtual block before the first is a long block centered at -long/2, so
	// the first planned block lands at center 0 with a long left neighbour.
	e.lastCenter = -int64(long / 2)
	e.lastN = long
	e.prevN = long
	e.firstBlock = true
	e.scanPos = e.bufBase
	return e, nil
}

// InputFormat implements codec.Encoder: Vorbis consumes the source float format
// unchanged (native rate and channel count).
func (e *Encoder) InputFormat() audio.Format { return e.fmt }

// FrameSize implements codec.Encoder. Vorbis block sizes vary per packet and
// the encoder does its own overlap buffering, so it accepts any input length.
func (e *Encoder) FrameSize() int { return 0 }

// Bitrate reports the rate the stream can be relied on to hold. The encoder is
// pure quality-driven VBR (ABR is not implemented; NewEncoder rejects a nonzero
// Bitrate target), so this is always 0: an unconstrained VBR size is honestly
// unknown, matching how the Opus/MP3 rows report 0 for VBR.
func (e *Encoder) Bitrate() int { return 0 }

// Delay reports the encoder priming to trim from the front of the decoded
// output. Vorbis bakes its half-block of priming into the first packet's lead-in,
// which the decoder emits nothing for, so the front trim is 0.
func (e *Encoder) Delay() int { return 0 }

// CodecConfig returns the three Vorbis headers packed with Xiph lacing.
func (e *Encoder) CodecConfig() []byte { return e.config }

// bufEnd returns the absolute index one past the last buffered input sample.
func (e *Encoder) bufEnd() int64 { return e.bufBase + int64(len(e.buf[0])) }

// Encode buffers src and emits every whole block that becomes available.
func (e *Encoder) Encode(src *audio.Buffer, emit func(codec.Packet) error) error {
	if src.Fmt != e.fmt {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("vorbis: encode input %v disagrees with %v", src.Fmt, e.fmt))
	}
	for c := 0; c < e.channels; c++ {
		e.buf[c] = append(e.buf[c], src.ChanF(c)[:src.N]...)
	}
	e.inSamples += int64(src.N)
	return e.drain(emit)
}

// drain plans and emits every block whose analysis window is fully buffered and
// whose successor is decided (the right window ramp needs the neighbour size).
func (e *Encoder) drain(emit func(codec.Packet) error) error {
	for {
		e.scanTransients(false)
		for len(e.pending) < 2 && e.planNext(false) {
		}
		if len(e.pending) < 2 {
			return nil // need more input to decide the successor
		}
		blk := e.pending[0]
		if e.bufEnd() < blk.center+int64(blk.size/2) {
			return nil // window not fully buffered yet
		}
		if err := e.emitPending(emit, e.pending[1].size, false); err != nil {
			return err
		}
	}
}

// planNext decides and appends the next block after the last planned one. It
// returns false when more input is needed to decide (unless flush is set, when it
// commits with whatever is buffered). Consecutive centers advance by
// (prevSize+size)/4.
func (e *Encoder) planNext(flush bool) bool {
	// Decide the size from transients revealed just past the last center. The
	// detector needs input up to one long block ahead of the last center.
	detectTo := e.lastCenter + int64(e.long)
	if !flush && e.scanPos < detectTo {
		return false
	}
	size := e.long
	if e.plannedFirst {
		size = e.decideSize(e.lastCenter)
	} else {
		// The first (overlap-priming) block is always long: the priming lead-in is
		// a half-long block, so a short first block would shift the whole decoded
		// stream by (long-short)/4. It precedes any real audio (only silence), so a
		// transient there needs no short block.
		e.plannedFirst = true
	}
	center := e.lastCenter + int64((e.lastN+size)/4)
	e.pending = append(e.pending, plannedBlock{size: size, center: center})
	e.lastCenter = center
	e.lastN = size
	return true
}

// decideSize picks the block size for a block following center `from`: short
// while a transient sits in the look-ahead (and for a short run after it, so the
// attack's onset and immediate decay are all in short blocks), long otherwise.
func (e *Encoder) decideSize(from int64) int {
	if e.attackBetween(from, from+int64(e.long)) {
		e.shortRun = shortRunBlocks
	}
	if e.shortRun > 0 {
		e.shortRun--
		return e.short
	}
	return e.long
}

// emitPending emits pending[0] with nextN as its right neighbour size (used to
// size the right window ramp and set the next-window flag). last marks the final
// block, whose successor is a virtual long neighbour.
func (e *Encoder) emitPending(emit func(codec.Packet) error, nextN int, last bool) error {
	blk := e.pending[0]
	if err := e.emitBlock(emit, blk.size, e.prevN, nextN); err != nil {
		return err
	}
	e.prevN = blk.size
	e.pending = e.pending[1:]
	if !last {
		e.trimBuffer()
	}
	return nil
}

// trimBuffer drops input no future block will read. The next block to emit reads
// from pending[0].center - long/2 at the earliest, and centers only advance, so
// anything a long block behind that is safe to discard.
func (e *Encoder) trimBuffer() {
	if len(e.pending) == 0 {
		return
	}
	keepFrom := e.pending[0].center - int64(e.long)
	drop := int(keepFrom - e.bufBase)
	if drop <= 0 {
		return
	}
	if drop > len(e.buf[0]) {
		drop = len(e.buf[0]) // never discard past the buffered tail (flush padding)
	}
	for c := 0; c < e.channels; c++ {
		e.buf[c] = append(e.buf[c][:0], e.buf[c][drop:]...)
	}
	e.bufBase += int64(drop)
	// Drop onset positions now behind the buffer so attackPos stays bounded.
	k := 0
	for k < len(e.attackPos) && e.attackPos[k] < e.bufBase {
		k++
	}
	if k > 0 {
		e.attackPos = append(e.attackPos[:0], e.attackPos[k:]...)
	}
}

// emitBlock analyses one block (size n, centered at pending[0].center) and emits
// one packet. prevN/nextN are the neighbour block sizes, which pick the left/right
// window ramp widths (min of the two sizes) exactly as the decoder's applyWindow
// does, so analysis and synthesis windows match for perfect reconstruction.
func (e *Encoder) emitBlock(emit func(codec.Packet) error, n, prevN, nextN int) error {
	slot := e.cfg.slotFor(n)
	n2 := n / 2
	fl := e.cfg.floors[slot]
	res := e.cfg.residues[slot]
	postCount := len(fl.xs)
	ln, rn := neighbourWin(n, prevN), neighbourWin(n, nextN)
	center := e.pending[0].center
	start := center - int64(n/2)

	// The noise cap coarsens steady broadband bands but must spare a transient's
	// broadband attack: a sharp event smears across the whole spectrum in a long
	// block, so frequency flatness alone cannot tell steady noise (coarsen-safe,
	// its quantization noise is simultaneously masked) from an attack (must stay
	// sharp). Only temporally steady blocks cap.
	capNoise := e.blockTemporallySteady(start, n)

	for c := 0; c < e.channels; c++ {
		e.windowBlock(c, start, n, ln, rn)
		e.fwd[slot].forward(e.wbuf[:n], e.spec[c][:n2])
		floor1Fit(fl, e.spec[c][:n2], e.targets[:postCount], n2)
		floor1EncodeVals(fl, e.targets[:postCount], e.vals[c][:postCount], e.fFinal[:postCount])
		fl.curve(e.vals[c][:postCount], e.curve[c][:n2], e.fFinal[:postCount], e.fStep2[:postCount], n2)
		normalizeResidue(e.spec[c][:n2], e.curve[c][:n2], e.resid[c][:n2], n2)

		block := e.blockSamples(c, start, n)
		psyRes, err := e.psy[slot][c].Analyze(block)
		if err != nil {
			return err
		}
		lineThresholds(psyRes, e.spec[c][:n2], e.thrLine[:n2], n2)
		classifyPartitions(e.spec[c][:n2], e.curve[c][:n2], e.thrLine[:n2], e.classes[c][:n2/resPartSize], resPartSize, n2, capNoise)
		maskResidue(e.spec[c][:n2], e.resid[c][:n2], e.thrLine[:n2], n2)
	}

	// Stereo coupling: replace the two channels' residues with the magnitude and
	// angle the decoder decouples, then reclassify the pair as magnitude and angle
	// (the per-channel L/R classes computed above no longer describe them): the
	// magnitude takes the band's masking allocation, the angle its own precision.
	magChannel := -1
	if len(e.cfg.mappings[slot].couplingMag) > 0 {
		coupleResidues(e.resid, n2)
		deriveCoupledClasses(e.classes[0][:n2/resPartSize], e.classes[1][:n2/resPartSize], e.resid[1][:n2], n2)
		magChannel = e.cfg.mappings[slot].couplingMag[0]
	}

	e.w.reset()
	e.w.writeBit(0) // audio packet
	e.w.writeBits(uint(e.cfg.modeBits), uint32(modeForSlot(slot)))
	if slot == slotLong {
		e.w.writeBit(boolBit(prevN == e.long)) // prevWindowFlag: long left neighbour
		e.w.writeBit(boolBit(nextN == e.long)) // nextWindowFlag: long right neighbour
	}
	for c := 0; c < e.channels; c++ {
		writeFloorData(&e.w, fl, e.vals[c][:postCount], e.cfg.books[bookFloorPosts])
	}
	encodeResidueType1(&e.w, res, e.cfg.books, e.resid, e.classes, n2, magChannel)

	l := int64((prevN + n) / 4)
	out := l
	pts := e.decoded
	if e.firstBlock {
		out = 0 // the priming packet primes the decoder overlap and emits nothing
		e.firstBlock = false
	}
	e.decoded += out
	return emit(codec.Packet{Data: e.w.bytes(), PTS: pts, Dur: out, Sync: true})
}

// neighbourWin returns the overlap-window size on the side facing a neighbour of
// size other: the smaller of the two block sizes, matching applyWindow's ln/rn.
func neighbourWin(n, other int) int {
	if other < n {
		return other
	}
	return n
}

// windowBlock writes the analysis-windowed block [start, start+n) of channel c
// into wbuf, applying the transition window with left/right ramp sizes ln/rn.
// It is the analysis twin of the decoder's applyWindow (same ramp geometry, same
// window table), so forward + inverse + overlap-add reconstructs the input.
func (e *Encoder) windowBlock(c int, start int64, n, ln, rn int) {
	leftWin := getPlan(ln).window
	rightWin := getPlan(rn).window
	leftBegin := n/4 - ln/4
	leftEnd := leftBegin + ln/2
	rightBegin := 3*n/4 - rn/4
	rightEnd := rightBegin + rn/2
	off := int(start - e.bufBase)
	src := e.buf[c]
	for i := 0; i < leftBegin; i++ {
		e.wbuf[i] = 0
	}
	for i := leftBegin; i < leftEnd; i++ {
		e.wbuf[i] = e.at(src, off+i) * leftWin[i-leftBegin]
	}
	for i := leftEnd; i < rightBegin; i++ {
		e.wbuf[i] = e.at(src, off+i)
	}
	for i := rightBegin; i < rightEnd; i++ {
		e.wbuf[i] = e.at(src, off+i) * rightWin[rightEnd-1-i]
	}
	for i := rightEnd; i < n; i++ {
		e.wbuf[i] = 0
	}
}

// blockSamples returns the raw (unwindowed) block [start, start+n) of channel c
// for the psychoacoustic analysis, in a reused contiguous scratch slice (the psy
// model needs exactly its FFTSize samples). Out-of-range samples (before buffered
// input, or in the flush tail) read as zero.
func (e *Encoder) blockSamples(c int, start int64, n int) []float32 {
	off := int(start - e.bufBase)
	src := e.buf[c]
	if cap(e.psyIn) < n {
		e.psyIn = make([]float32, n)
	}
	e.psyIn = e.psyIn[:n]
	for i := 0; i < n; i++ {
		e.psyIn[i] = e.at(src, off+i)
	}
	return e.psyIn
}

// blockTemporallySteady reports whether the block [start, start+n) has roughly
// even energy across its span, so the noise cap only coarsens genuinely steady
// broadband content. It splits the (channel-summed) block into sub-windows and
// compares the loudest to the average: steady noise sits near 1, a transient
// burst concentrates energy into one sub-window and rises far above it.
func (e *Encoder) blockTemporallySteady(start int64, n int) bool {
	const subs = 16
	sw := n / subs
	if sw == 0 {
		return true
	}
	var maxE, sumE float64
	for s := 0; s < subs; s++ {
		off := int(start-e.bufBase) + s*sw
		var eSub float64
		for i := 0; i < sw; i++ {
			var v float32
			for c := 0; c < e.channels; c++ {
				v += e.at(e.buf[c], off+i)
			}
			eSub += float64(v) * float64(v)
		}
		sumE += eSub
		if eSub > maxE {
			maxE = eSub
		}
	}
	if sumE <= 0 {
		return true // silence: nothing to smear
	}
	return maxE/(sumE/subs) < steadyPeakRatio
}

// steadyPeakRatio is the loudest-to-average sub-window energy below which a block
// counts as temporally steady (noise-cap eligible). A pure sine sits near 1; a
// transient burst pushes one sub-window far above the average.
const steadyPeakRatio = 4.0

// at reads src[i], or 0 when i is outside the buffered range (priming lead-in
// already lives in the buffer as real zeros; this guards the flush tail).
func (e *Encoder) at(src []float32, i int) float32 {
	if i < 0 || i >= len(src) {
		return 0
	}
	return src[i]
}

// scanTransients runs the attack detector forward over newly buffered input in
// short-block windows, advancing scanPos. It records nothing structured: the
// detector's carried level is consulted on demand by attackBetween, so instead
// this only advances the scan frontier that gates planning. Attacks are stored in
// the sorted attackPos list.
func (e *Encoder) scanTransients(flush bool) {
	for e.scanPos+int64(e.short) <= e.bufEnd() {
		start := e.scanPos
		off := int(start - e.bufBase)
		for i := 0; i < e.short; i++ {
			var v float32
			for c := 0; c < e.channels; c++ {
				v += e.at(e.buf[c], off+i)
			}
			e.monobuf[i] = v / float32(e.channels)
		}
		if attack, pos := e.attack.Scan(e.monobuf, attackSubWindows); attack {
			e.attackPos = append(e.attackPos, start+int64(pos*(e.short/attackSubWindows)))
		}
		e.scanPos += int64(e.short)
	}
	if flush {
		e.scanPos = e.bufEnd()
	}
}

// attackBetween reports whether a detected transient onset lies in [lo, hi).
func (e *Encoder) attackBetween(lo, hi int64) bool {
	for _, p := range e.attackPos {
		if p >= lo && p < hi {
			return true
		}
	}
	return false
}

// Finish pads and emits enough trailing blocks that the decoder outputs every
// real sample, then reports the gapless trailer.
func (e *Encoder) Finish(emit func(codec.Packet) error) (codec.Trailer, error) {
	if e.inSamples == 0 {
		return codec.Trailer{}, nil
	}
	// Emit remaining input first, then keep emitting (short-run state permitting)
	// until the decoder will have output every real sample. Pad the buffer with
	// silence so each remaining block's window is fully covered; the tail is
	// trimmed by Padding. A generous pad covers the deepest look-ahead the
	// scheduler can reach before decoded catches inSamples.
	if err := e.drain(emit); err != nil {
		return codec.Trailer{}, err
	}
	e.scanTransients(true) // stop switching to short at the very end
	for e.decoded < e.inSamples {
		for len(e.pending) < 2 {
			e.planNext(true)
		}
		e.ensureBuffered(e.pending[0].center + int64(e.long))
		if err := e.emitPending(emit, e.pending[1].size, false); err != nil {
			return codec.Trailer{}, err
		}
	}
	padding := e.decoded - e.inSamples
	if padding < 0 {
		padding = 0
	}
	return codec.Trailer{Samples: e.inSamples, Delay: 0, Padding: padding}, nil
}

// ensureBuffered appends silence so the buffer extends to at least the absolute
// index end, covering a flush-tail block window with zeros.
func (e *Encoder) ensureBuffered(end int64) {
	if have := e.bufEnd(); have < end {
		pad := int(end - have)
		for c := 0; c < e.channels; c++ {
			e.buf[c] = append(e.buf[c], make([]float32, pad)...)
		}
	}
}

// Block-switching tuning.
const (
	// attackRatio fires the transient detector on a sub-window that jumps this
	// many times above the running level.
	attackRatio = 4.0
	// attackSubWindows splits a short-block scan window for onset localization.
	attackSubWindows = 4
	// shortRunBlocks is how many short blocks to force once a transient is seen,
	// covering the onset and its immediate decay before returning to long blocks.
	shortRunBlocks = 12
)
