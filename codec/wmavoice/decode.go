//go:build !wmavoicetablesgen

package wmavoice

import (
	"math"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// The packet, superframe and frame walk (notes 3 and 4).
//
// Three nested units, and the names do not line up with the other codecs here.
// A packet is one ASF media object of exactly nBlockAlign bytes. A superframe
// is the output unit, 480 samples of three frames, and it may span two
// packets. A frame is 160 samples and carries one to eight blocks.
//
// The packet header's superframe count is one MORE than the number of
// superframes taken out of the packet body, because the last one always
// becomes the carry even when the packet holds it complete. It is decoded at
// the top of the next packet, and at the end of the stream there is no next
// packet: Drain is what emits it, and without that flush every file comes out
// exactly one superframe short.
//
// A failure does not latch. Every media object is a decode start point
// (note 13), so a refused packet costs its own superframes and the next packet
// resumes; what it does not do is come back exact, because a CELP decoder's
// adaptive codebook is a copy of its own past output. Note 13 measures the
// convergence and it is not one frame.

// referenceCarryBits is the size of the reference's superframe cache. A carry
// longer than this is what makes the reference decode the next superframe out
// of stale bytes (note 3.3); this decoder has no such limit, and counts the
// crossings so the differential gate can skip the superframes the reference
// gets wrong instead of widening its tolerance to cover them.
const referenceCarryBits = 2048

// excSlack is how far past the superframe the interpolation filters may read.
// Every pitch this codec produces is at least minPitch and the forward taps
// reach eight samples, so nothing actually crosses the boundary; the slack is
// here because the note says the buffer needs it and an off-by-one in that
// reasoning would be an out-of-range panic rather than a wrong sample.
const excSlack = 16

// The seams format.Media reaches through type assertions rather than the
// Decoder interface, pinned so a drifted signature is a compile error and not
// a seek that quietly stops converging or a pooled buffer that leaks.
var (
	_ codec.Decoder    = (*Decoder)(nil)
	_ codec.Positioner = (*Decoder)(nil)
	_ codec.Releaser   = (*Decoder)(nil)
)

// Decoder decodes one WMA Voice track.
type Decoder struct {
	cfg  Config
	fmt  audio.Format
	g    geometry
	lsps int
	tf   *transforms

	// The packet layer.
	r      bitReader   // over the current packet
	cr     bitReader   // over the carry joined with the next packet's spillover
	carry  bitAppender // the tail of a packet, as a bit string
	joined bitAppender
	// residualLSP is the current packet's flag. A superframe that spans two
	// packets is decoded under the FOLLOWING packet's flag, not the one in
	// force when its first bits were written.
	residualLSP bool

	// State that crosses a superframe boundary. Nothing else does: there is no
	// reservoir, no window tail and no lookahead.
	exc          []float32
	excBase      int
	synth        []float32
	synthBase    int
	zero         []float32
	zeroBase     int
	resynth      []float32
	smoothed     []float32
	prevLSF      [maxLSPs]float64
	pitchState   int
	prevACB      acbKind
	gainPredErr  [6]float32
	frameCounter int
	agcMem       float32
	dcMem        [2]float32
	cache        [pfCacheLen]float32
	cacheLen     int

	// Within one superframe.
	lsf [framesPerSuperframe][maxLSPs]float64
	// frameIdx is the frame's position in the superframe, which the
	// comfort-noise generator adds to the frame counter.
	frameIdx int

	// Within one frame.
	pitch           [8]int
	pitchQ          [8]int
	asymBase        int
	curPitch        int
	pitchSlope      int
	lastPitchField  int
	firstBlockPitch int
	silenceGain     float32
	aw              awCoords
	awNextOff       int
	pulses          [frameSamples]float32

	// Scratch, held so the hot path never allocates.
	lpc        [maxLSPs]float32
	lspScratch [maxLSPs]float64
	paScratch  [maxLSPs/2 + 1]float64
	qaScratch  [maxLSPs/2 + 1]float64
	pfWork     [pfWindow]float32
	pfSpec     [dftLen]float32
	pfIR       [dftLen]float32
	pfLPCSpec  [dftCoefs]float32
	pfCoefs    [dftCoefs]float32
	pfSigSpec  [dftCoefs]float32
	pfIRSpec   [dftCoefs]float32
	pfGain     [dftBins]float32
	pfMag      [dftBins]float32

	out   *audio.Buffer
	plane []float32

	paths pathCounts
}

// pathCounts counts the bitstream paths a decode takes. It is for the tests:
// the corpus note's coverage matrix was measured with the analysis pass's own
// instrumented decoder, and pinning the same counts here says this walk takes
// the same branches the same number of times, which no sample comparison can
// say.
type pathCounts struct {
	Packets, Superframes, Frames, Blocks int
	// The LSP layer (note 5).
	ResidualLSP, IndependentLSP   int
	LSPInterp                     [32]bool
	StabFirst, StabLast, StabSort int
	// The superframe and packet layers (notes 3.1 to 3.4).
	SampleCount, LastSampleCount    int
	Statistics                      int
	SpilloverZero, SpilloverNonZero int
	SuperframeCounts                [64]bool
	CountEscape                     int
	// StaleCarry counts the carries that exceed the reference's cache, so the
	// superframe the NEXT call emits first is one the reference gets wrong.
	StaleCarry int
	// The frame layer (note 4) and the two adaptive codebooks (notes 6, 9).
	FrameType                        [17]int
	AsymBlocks, AsymRuns             int
	AsymPhase                        [9]bool
	PitchReset, PitchClamp           int
	BlockPitchFirst, BlockPitchDelta int
	HammingPhase                     [4]int
	ConvArm                          [4]int
	// Gains and pulses (notes 7, 8).
	GainWeight              [9]int
	SilenceGain             [256]bool
	AWRange24, AWRange16    int
	AWExtended, AWZeroCount int
	AWConceal               int
	// The postfilter (note 10).
	KalmanOK, KalmanFail int
	DenoiseRuns          int
	DCRemoval            int
}

// NewDecoder builds a decoder for one track. fm must be the format Config
// produces; the caller passes the container's so a disagreement is caught here
// rather than by a downstream stage.
func NewDecoder(cfg Config, fm audio.Format) (*Decoder, error) {
	// Validate before anything is derived from the config. A Config built by
	// hand rather than by ParseConfig reaches here unchecked, and every
	// geometry below is computed from it.
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	want := cfg.Format()
	if fm != want {
		return nil, malformed("track format %+v does not match the codec config's %+v", fm, want)
	}
	if err := fm.Valid(); err != nil {
		return nil, err
	}
	g := cfg.geom()
	l := cfg.LSPs()
	d := &Decoder{
		cfg:       cfg,
		fmt:       fm,
		g:         g,
		lsps:      l,
		tf:        newTransforms(),
		exc:       make([]float32, g.history+superframeSamples+excSlack),
		excBase:   g.history,
		synth:     make([]float32, l+superframeSamples),
		synthBase: l,
		zero:      make([]float32, g.history+superframeSamples),
		zeroBase:  g.history,
		resynth:   make([]float32, l+halfFrameSamples),
		smoothed:  make([]float32, halfFrameSamples),
	}
	d.out = audio.Get(fm, superframeSamples)
	// ChanF is bounded by N, and Get leaves N zero, so the plane has to be
	// taken with N at the superframe length or it comes back empty and every
	// write is a reslice past the length into the capacity: legal, and not
	// what anyone reading it would assume. emitSuperframe sets N to what the
	// superframe actually declared just before handing the buffer over.
	d.out.N = superframeSamples
	d.plane = d.out.ChanF(0)
	d.Reset()
	return d, nil
}

// Decode consumes one container packet and emits the superframes it completes,
// which is the one this packet's spillover finishes plus all but one of the
// packet's own.
func (d *Decoder) Decode(pkt []byte, emit func(*audio.Buffer) error) error {
	if len(pkt) == 0 {
		// The reference's end of stream is a call with an empty packet, which
		// decodes the carry with no spillover. The same here, at any point:
		// a carry never lives on across an empty packet, so what it holds is
		// emitted if it is complete and refused if it is not, and the next
		// packet's spillover is skipped either way.
		return d.Drain(emit)
	}
	// A container shape rather than a stream failure, but the object is gone
	// either way, and the carry is the head of a superframe whose tail went
	// with it: kept, it would be joined with the NEXT packet's spillover,
	// which finishes a different superframe. A packet SHORTER than
	// nBlockAlign is accepted, since container/asf assembles compressed
	// payloads independently of it.
	if len(pkt) > d.cfg.BlockAlign {
		d.carry.reset()
		return malformed("packet of %d bytes, longer than nBlockAlign %d", len(pkt), d.cfg.BlockAlign)
	}
	err := d.decode(pkt, emit)
	if err != nil {
		// The carry goes with any failure: the walk stopped partway through
		// the packet, and leaving it in place would decode bits that belong to
		// a superframe this packet never finished.
		d.carry.reset()
	}
	return err
}

func (d *Decoder) decode(pkt []byte, emit func(*audio.Buffer) error) error {
	d.paths.Packets++
	d.r.reset(pkt)
	d.r.skip(4) // the packet sequence number, which nothing reads
	d.residualLSP = d.r.bit() != 0

	// The superframe count is a run of 6-bit fields: read one, add it, and
	// read another whenever the value read was exactly 63.
	count := 0
	for {
		if d.r.left() < 6+d.g.spilloverBits {
			return malformed("the packet header runs past the packet")
		}
		v := int(d.r.bits(6))
		count += v
		if v != 63 {
			break
		}
		d.paths.CountEscape++
	}
	spill := int(d.r.bits(d.g.spilloverBits))
	if d.r.err != nil {
		return d.r.err
	}
	if count == 0 {
		// The count is one more than the body yields, since the last
		// superframe always becomes the carry, so zero describes no packet.
		// Accepted, the whole body would become the carry, one superframe of
		// it decoded at the top of the next packet and the rest dropped
		// without a word.
		return malformed("a superframe count of zero, which no packet has")
	}
	if count < 64 {
		d.paths.SuperframeCounts[count] = true
	}
	if spill == 0 {
		d.paths.SpilloverZero++
	} else {
		d.paths.SpilloverNonZero++
	}

	payload := d.r.pos
	if spill > d.r.left() {
		return malformed("the packet declares %d spillover bits with %d left", spill, d.r.left())
	}

	// A carried superframe is finished with this packet's spillover bits. With
	// no carry those bits are the tail of a superframe whose head this decoder
	// never saw, at a stream start, after a seek or after a refused packet,
	// and they are skipped unread.
	if d.carry.bits > 0 {
		if d.carry.bits > referenceCarryBits {
			d.paths.StaleCarry++
		}
		d.joined.reset()
		d.joined.appendFrom(d.carry.buf, 0, d.carry.bits)
		d.joined.appendFrom(pkt, payload, spill)
		d.cr.resetBits(d.joined.buf, d.joined.bits)
		d.carry.reset()
		if err := d.superframe(&d.cr, emit); err != nil {
			return err
		}
	}
	d.r.seek(payload + spill)

	for range count - 1 {
		if err := d.superframe(&d.r, emit); err != nil {
			return err
		}
	}

	// Whatever is left is the next superframe's head, and it stops mid-byte,
	// so it travels as a bit string.
	d.carry.reset()
	d.carry.appendFrom(pkt, d.r.pos, d.r.left())
	return nil
}

// Drain emits the superframe the last packet carried. It is not an
// optimisation: the packet layer always leaves one behind, so without this
// every stream ends one superframe short. An empty packet does the same, which
// is the reference's own flush.
func (d *Decoder) Drain(emit func(*audio.Buffer) error) error {
	if d.carry.bits == 0 {
		return nil
	}
	// The last carry counts toward the reference's defect like any other: on
	// two of the seven cells it is the one that overruns, which is why those
	// two come out longer than their source there.
	if d.carry.bits > referenceCarryBits {
		d.paths.StaleCarry++
	}
	d.cr.resetBits(d.carry.buf, d.carry.bits)
	d.carry.reset()
	return d.superframe(&d.cr, emit)
}

// superframe decodes one 480-sample unit: three frames, and the optional
// declared length that is the only thing that ever makes the output shorter.
func (d *Decoder) superframe(r *bitReader, emit func(*audio.Buffer) error) error {
	speech := r.bit()
	if r.err != nil {
		// An overrun at the first bit is damage (a count that claims a
		// superframe the packet does not hold, or a truncated carry), and
		// the reader hands back a zero bit with the error latched, so it is
		// checked before the bit means anything.
		return r.err
	}
	if speech == 0 {
		// A WMA Pro payload inside a WMA Voice superframe, which is why the
		// extra bytes open with a WMA Pro extension block. The reference
		// refuses it by this name and has never seen one; padding read as a
		// superframe after a count that overstates the packet looks the same
		// and is the likelier cause, so the message names both.
		return unsupported("the superframe carries a WMA Pro payload rather than speech, unless a count overstated the packet and this is its padding")
	}
	samples := superframeSamples
	if r.bit() != 0 {
		samples = int(r.bits(12))
		if r.err != nil {
			return r.err
		}
		if samples > superframeSamples {
			return malformed("the superframe declares %d samples, want at most %d",
				samples, superframeSamples)
		}
		d.paths.SampleCount++
		d.paths.LastSampleCount = samples
	}
	if r.err != nil {
		return r.err
	}

	if d.residualLSP {
		d.residualLSFs(r, &d.lsf)
		if r.err != nil {
			return r.err
		}
		for f := range framesPerSuperframe {
			d.stabilise(d.lsf[f][:d.lsps])
		}
		d.paths.ResidualLSP++
	}
	for f := range framesPerSuperframe {
		if !d.residualLSP {
			d.independentFrameLSFs(r, d.lsf[f][:])
			if r.err != nil {
				return r.err
			}
			d.stabilise(d.lsf[f][:d.lsps])
			d.paths.IndependentLSP++
		}
		if err := d.frame(r, f); err != nil {
			return err
		}
	}

	// The statistics block, which is measured never present and is skipped by
	// its own declared length rather than refused.
	if r.bit() != 0 {
		k := int(r.bits(4))
		r.skip(10 * (k + 1))
		d.paths.Statistics++
	}
	if r.err != nil {
		return r.err
	}
	if !d.bounded() {
		// The superframe was decoded in full and only its output is refused,
		// so the frame counter and the LSF set advance as for any other: the
		// reference emits it and counts it. The histories go with the
		// refusal: the next packet is a decode start, and it must not start
		// from the runaway's last samples.
		d.roll()
		d.clearHistory()
		return malformed("the synthesis diverged past a magnitude of %d", maxMagnitude)
	}
	d.paths.Superframes++
	return d.emit(samples, emit)
}

// frame decodes one 160-sample frame: its type, the per-frame fields that type
// carries, then its blocks, then the two postfilter halves.
func (d *Decoder) frame(r *bitReader, idx int) error {
	d.frameIdx = idx
	ft, err := d.readFrameType(r)
	if err != nil {
		return err
	}
	desc := frameTypes[ft]
	d.paths.FrameType[ft]++

	// The per-frame fields, in bitstream order: the one pitch field an
	// asymmetric frame carries, then a comfort-noise frame's gain, then a
	// window-pulse frame's coordinates, which come last because they are
	// computed from both blocks' pitches.
	if desc.acb == acbAsym {
		d.framePitch(r, desc)
	}
	d.silenceGain = 0
	if desc.fcb == fcbSilence {
		gi := int(r.bits(8))
		d.silenceGain = gainSilence[gi]
		d.paths.SilenceGain[gi] = true
	}
	if desc.fcb == fcbWindowPulses {
		if err := d.awReadCoords(r); err != nil {
			return err
		}
		if d.aw.rng == 24 {
			d.paths.AWRange24++
		} else {
			d.paths.AWRange16++
		}
		if d.aw.extended {
			d.paths.AWExtended++
		}
	}
	if r.err != nil {
		return r.err
	}

	prev := d.prevLSF[:]
	if idx > 0 {
		prev = d.lsf[idx-1][:]
	}
	this := d.lsf[idx][:]

	base := idx * frameSamples
	size := frameSamples / desc.blocks
	for b := range desc.blocks {
		at := base + b*size
		pitchLag := 0
		switch desc.acb {
		case acbAsym:
			pitchLag = d.pitch[b]
		case acbHamming:
			d.pitchQ[b] = d.blockPitch(r, b == 0)
			if b == 0 {
				d.paths.BlockPitchFirst++
			} else {
				d.paths.BlockPitchDelta++
			}
			d.paths.HammingPhase[d.pitchQ[b]&3]++
			pitchLag = d.pitchQ[b] >> 2
		}
		if b == 0 {
			d.firstBlockPitch = pitchLag
		}
		if err := d.block(r, at, size, b, pitchLag, desc); err != nil {
			return err
		}
		// The block's own LPC set, interpolated across the frame in the LSF
		// domain and only then turned into cosines.
		d.lsfToLPC(d.lpc[:d.lsps], prev, this, (float64(b)+0.5)/float64(desc.blocks))
		d.synthesise(at, size, d.lpc[:d.lsps])
		d.paths.Blocks++
	}

	if d.cfg.Postfilter() {
		// The two halves take the frame-centre and frame-end LSF sets, not the
		// per-block ones.
		d.lsfToLPC(d.lpc[:d.lsps], prev, this, 0.5)
		d.postfilter(d.plane[base:base+halfFrameSamples], base, d.lpc[:d.lsps], desc.fcb, d.firstBlockPitch)
		d.lsfToLPC(d.lpc[:d.lsps], prev, this, 1)
		d.postfilter(d.plane[base+halfFrameSamples:base+frameSamples], base+halfFrameSamples,
			d.lpc[:d.lsps], desc.fcb, d.firstBlockPitch)
	} else {
		copy(d.plane[base:base+frameSamples], d.synth[d.synthBase+base:])
	}

	// The pitch state after the frame is not always this frame's pitch.
	switch desc.acb {
	case acbNone:
		d.pitchState = 0
	case acbAsym:
		d.pitchState = d.curPitch
	case acbHamming:
		d.pitchState = d.pitchQ[desc.blocks-1] >> 2
	}
	d.prevACB = desc.acb
	d.paths.Frames++
	return r.err
}

// block builds one block's excitation: its fixed codebook contribution, then
// its adaptive one written in place, then the mix of the two.
func (d *Decoder) block(r *bitReader, at, size, idx, pitchLag int, desc frameDesc) error {
	exc := d.exc[d.excBase+at : d.excBase+at+size]
	switch desc.fcb {
	case fcbSilence:
		d.comfortNoise(exc, idx, d.silenceGain)
		clear(d.gainPredErr[:])
		return r.err
	case fcbHardcoded:
		off := int(r.bits(8))
		gain := gainUniversal[r.bits(6)]
		if r.err != nil {
			return r.err
		}
		for m := range size {
			exc[m] = stdCodebook[off+m] * gain
		}
		clear(d.gainPredErr[:])
		return nil
	}

	pulses := d.pulses[:size]
	clear(pulses)
	if desc.fcb == fcbWindowPulses {
		d.awFirstSet(r, pulses, idx, pitchLag)
		if d.aw.nPulses[idx] <= 0 {
			d.paths.AWZeroCount++
		}
		if r.err != nil {
			return r.err
		}
		if !d.awSecondSet(r, pulses, idx, pitchLag) {
			// No free position is left, so the block cannot be coded. The
			// reference conceals it rather than failing: eight bits (the
			// pulse's sign and the block's gain index) are skipped so the next
			// block starts at the right offset, and the block becomes comfort
			// noise. A window-pulse frame reads no silence gain, so the gain
			// is zero and the concealment is a silent block; that is an
			// inference on a path nothing can reach.
			r.skip(8)
			d.comfortNoise(exc, idx, d.silenceGain)
			d.paths.AWConceal++
			return r.err
		}
	} else {
		d.innovationPulses(r, pulses, desc)
	}
	if r.err != nil {
		return r.err
	}

	switch desc.acb {
	case acbAsym:
		d.acbAsymmetric(d.excBase+at-idx*size, idx, size)
		d.paths.AsymBlocks++
	case acbHamming:
		d.acbHammingBlock(d.excBase+at, size, d.pitchQ[idx])
	}

	acbGain, fcbGain := d.blockGains(r, desc.log2)
	d.paths.GainWeight[8>>desc.log2]++
	if r.err != nil {
		return r.err
	}
	for m := range size {
		exc[m] = acbGain*exc[m] + fcbGain*pulses[m]
	}
	return nil
}

// emit hands the superframe over and rolls every buffer that crosses the
// boundary.
func (d *Decoder) emit(samples int, emit func(*audio.Buffer) error) error {
	defer d.roll()
	if samples <= 0 {
		return nil
	}
	d.out.N = samples
	return emit(d.out)
}

// roll moves the tail of each persistent buffer down to its history, advances
// the frame counter by the superframe's three frames, and keeps the third
// frame's LSF set as the next superframe's predecessor.
func (d *Decoder) roll() {
	copy(d.exc[:d.excBase], d.exc[superframeSamples:superframeSamples+d.excBase])
	copy(d.synth[:d.synthBase], d.synth[superframeSamples:superframeSamples+d.synthBase])
	copy(d.zero[:d.zeroBase], d.zero[superframeSamples:superframeSamples+d.zeroBase])
	d.prevLSF = d.lsf[framesPerSuperframe-1]
	d.frameCounter += framesPerSuperframe
	if d.frameCounter >= 0xFFFF {
		d.frameCounter -= 0xFFFF
	}
}

// maxMagnitude bounds an emitted sample. Full scale is 1 and the corpus peaks
// under 0.9, so a sample past this is a recursion that has run away, not
// audio: the adaptive codebook gain reaches 1.36, and a crafted LSF set can
// hand both synthesis filters a denominator float32 cannot hold stable. The
// reference emits whatever float32 makes of either. This decoder refuses the
// superframe and drops the histories it poisoned. The bound is also low enough
// to square and sum downstream without leaving float32.
const maxMagnitude = 1 << 20

// bounded reports whether every sample of the superframe is inside
// maxMagnitude. The comparison is negated so a NaN fails it, where it would
// pass any ordinary one.
func (d *Decoder) bounded() bool {
	for _, x := range d.plane[:superframeSamples] {
		if !(absF(x) <= maxMagnitude) {
			return false
		}
	}
	return true
}

// clearHistory zeroes every buffer a recursion feeds on: the excitation and
// synthesis histories, the postfilter's three, its overlap cache and its two
// filter memories.
func (d *Decoder) clearHistory() {
	clear(d.exc)
	clear(d.synth)
	clear(d.zero)
	clear(d.resynth)
	clear(d.smoothed)
	clear(d.cache[:])
	d.cacheLen = 0
	d.agcMem = 0
	d.dcMem = [2]float32{}
}

// Reset discards everything after a seek. There is no fixed lead-in that makes
// a resumed decode exact: the adaptive codebook is a pitch-lag copy of the
// decoder's own past output with a gain near 1, so a wrong history decays only
// as fast as that gain lets it. Note 13 measures the convergence, and a caller
// that wants a resumed decode to converge at all must also call SetPosition.
func (d *Decoder) Reset() {
	d.carry.reset()
	d.clearHistory()
	clear(d.gainPredErr[:])
	d.frameCounter = 0
	d.pitchState = 40
	d.prevACB = acbNone
	d.awNextOff = 0
	// An evenly spaced set spanning the band, which is what the previous LSF
	// set is before any frame has produced one.
	for n := range d.lsps {
		d.prevLSF[n] = float64(n+1) * math.Pi / float64(d.lsps+1)
	}
}

// SetPosition tells the decoder the output sample the next packet's first
// decoded sample carries, which for this codec is not bookkeeping.
//
// Comfort-noise frames draw from a codebook at an offset that is a function of
// the frame index since the decoder started (note 11). A resumed decode whose
// counter starts at zero therefore draws different noise in every silent
// passage, for good: measured, 14 to 18 superframes of such a decode differ
// from a linear one indefinitely, against 2 to 4 with the counter restored.
func (d *Decoder) SetPosition(sample int64) {
	if sample < 0 {
		sample = 0
	}
	// The nearest superframe, not the floor: the container lands on the
	// 480-sample grid for every real file, and its fallback for a file whose
	// first object is off the grid is a millisecond-derived time that can
	// sit either side of the superframe the decode actually starts at.
	d.frameCounter = int((framesPerSuperframe * ((sample + superframeSamples/2) / superframeSamples)) % 0xFFFF)
}

// Release returns the pooled output buffer (format.Media calls it on Close).
func (d *Decoder) Release() {
	if d.out != nil {
		audio.Put(d.out)
		d.out, d.plane = nil, nil
	}
}

// stabilise is note 5.6 with the counters the coverage matrix pins.
func (d *Decoder) stabilise(lsf []float64) {
	floored, capped, sorted := stabilise(lsf)
	if floored {
		d.paths.StabFirst++
	}
	if capped {
		d.paths.StabLast++
	}
	if sorted {
		d.paths.StabSort++
	}
}
