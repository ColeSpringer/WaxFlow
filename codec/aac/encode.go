package aac

import (
	"fmt"
	"math"
	"slices"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/dsp/psy"
	"github.com/colespringer/waxflow/waxerr"
)

var _ codec.Encoder = (*Encoder)(nil)

// EncoderVersion identifies the encode algorithm revision for cache keys
// (ADR-0004). It composes the psychoacoustic model's revision: retuning
// dsp/psy changes these streams too.
const EncoderVersion = "aac-enc-2+" + psy.Version

// EncoderDelay is the codec priming in output samples: one frame of
// zeros ahead of the first real sample, so frame 0's MDCT window (which
// reaches one frame into the past) sees defined history. Carried in
// Trailer.Delay and the container's edit list.
const EncoderDelay = 1024

// frameLen is the AAC-LC long frame in samples per channel.
const frameLen = 1024

// DefaultBitrate is used when EncoderOptions.Bitrate is zero.
const DefaultBitrate = 128000

// thrCalib maps psy thresholds (FFT energy of unit-full-scale input)
// onto the encoder's MDCT energy scale: the analytic window/scale ratio
// (8/3: Hann FFT energy 3N/8 against the scale-2 sine-window MDCT's N)
// times the 32768 PCM scaling, squared.
const thrCalib = (8.0 / 3.0) * 32768 * 32768

// psyOffsetDB is the model's SNR-demand offset, the encoder's master
// quality tuning constant (positive demands lower thresholds and so
// more bits per band before the rate loop pushes back).
const psyOffsetDB = 0.0

// EncoderOptions configures NewEncoder. The zero value is the default.
type EncoderOptions struct {
	// Bitrate is the target in bits per second for the whole stream
	// (all channels), 0 for DefaultBitrate. AAC frames are inherently
	// variable-size; the encoder holds the long-term mean at the target
	// with a bit reservoir (ABR), which is what both fMP4 and ADTS
	// carry naturally.
	Bitrate int
	// ParametricStereo selects HE-AAC v2 for NewHEEncoder: the stereo
	// input is folded into a phase-aligned mono downmix coded by a mono
	// SCE core, with the stereo image carried as ps_data inside the SBR
	// extension (an AOT-29 stream). Stereo input only; NewEncoder
	// (AAC-LC) ignores it.
	ParametricStereo bool
}

// Encoder is an AAC-LC encoder producing raw access units (one packet
// per 1024-sample frame). The fMP4 muxer stores CodecConfig's
// AudioSpecificConfig; the ADTS muxer derives its header fields from it.
type Encoder struct {
	fmt      audio.Format
	channels int
	rate     int
	rateIdx  int
	bitrate  int
	asc      [2]byte

	swbLong     []uint16
	swbShort    []uint16
	numSwbLong  int
	numSwbShort int
	maxSfbLong  int
	maxSfbShort int

	// Input pipeline: pending holds not-yet-complete source blocks; hist
	// slides three whole blocks. The encoder runs one block deferred:
	// when block m arrives, it first encodes the AU windowing blocks
	// m-2 and m-1 (hist[1024:3072] before the shift), so the window
	// decision sees attacks one whole block past the AU's window, then
	// shifts m in. Decoder output block is the FIRST half of each AU's
	// window, which puts the first real sample at output position 1024,
	// the declared EncoderDelay: deferral moves only when an AU is
	// emitted relative to Encode calls, never the output timeline.
	pending   [2][]float32
	hist      [2][3 * frameLen]float32
	inSamples int64
	outFrames int64
	// primed marks that a block has been shifted into hist, so the next
	// push has a deferred AU to encode (the first arriving block
	// completes none).
	primed bool

	// Window decision state: attack info for the AU's output block, its
	// lookahead (second window) block, and the block after that. Each
	// block is scanned as two half-block detector calls (identical
	// arithmetic to one call, the level state carries), so an attack in
	// each half is reported even when the other half attacks first; the
	// halves are the shorts' coverage split, so each half belongs to
	// exactly one AU's decision.
	det       [2]*psy.AttackDetector
	attackOut [2]blockAttacks
	attackLA  [2]blockAttacks
	attackNew [2]blockAttacks
	prevSeq   int

	// Psychoacoustics, per channel.
	psyLong  [2]*psy.Model
	psyShort [2]*psy.Model

	// Rate control.
	meanBits  float64
	reservoir float64
	avgPE     float64

	// SBR side chain, nil for plain LC. When set, each frame pulls its
	// fill-element payload before budgeting, counts its exact cost into
	// the frame overhead, and writes the fill before END.
	sbr *sbrEnc

	// Per-frame scratch.
	spec  [2][1024]float64
	cq    [2]chanQuant
	tns   [2]tnsEnc
	thr   [2][maxWindowGroups][maxSFBCount]float64
	msUse [maxWindowGroups][maxSFBCount]bool
	w     bitWriter
}

type attackInfo struct {
	attack bool
	pos    int
}

// blockAttacks is one source block's attack report: the first attack in
// each half, pos in 8ths of the whole block (early 0-3, late 4-7).
type blockAttacks struct {
	early, late attackInfo
}

// NewEncoder returns an Encoder for f, which must be float32 with 1 or
// 2 channels at one of the 13 AAC sampling rates.
func NewEncoder(f audio.Format, opts *EncoderOptions) (*Encoder, error) {
	if err := f.Valid(); err != nil {
		return nil, err
	}
	if f.Type != audio.Float || f.BitDepth != 32 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat, "aac: encoder input must be float32")
	}
	if f.Channels < 1 || f.Channels > 2 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("aac: %d channels unsupported (mono or stereo)", f.Channels))
	}
	rateIdx := samplingIndex(f.Rate)
	if rateIdx < 0 || rateIdx >= len(swbOffsetLong) {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("aac: sample rate %d is not an AAC rate", f.Rate))
	}

	var o EncoderOptions
	if opts != nil {
		o = *opts
	}
	if o.Bitrate == 0 {
		o.Bitrate = DefaultBitrate
	}
	// Floor keeps the rate loop meaningful; the ceiling is the spec's
	// 6144-bit-per-channel decoder buffer drained at frame rate.
	minRate := 8000 * f.Channels
	maxRate := 6 * f.Rate * f.Channels
	bitrate := min(max(o.Bitrate, minRate), maxRate)

	e := &Encoder{
		fmt:      f,
		channels: f.Channels,
		rate:     f.Rate,
		rateIdx:  rateIdx,
		bitrate:  bitrate,
		prevSeq:  onlyLong,
	}
	e.asc[0] = byte(aotAACLC<<3 | rateIdx>>1)
	e.asc[1] = byte(rateIdx<<7 | f.Channels<<3)

	e.swbLong = swbOffsetLong[rateIdx]
	e.swbShort = swbOffsetShort[rateIdx]
	e.numSwbLong = swbCountLong(rateIdx)
	e.numSwbShort = swbCountShort(rateIdx)

	// Bandwidth cutoff: spending the budget below the cutoff beats
	// coding shaped noise at the top; the offsets scale with the
	// per-channel rate. maxSfb is the first band wholly past cutoff.
	cutoff := 3000.0 + float64(bitrate)/float64(f.Channels)/5
	cutoff = math.Min(cutoff, 0.94*float64(f.Rate)/2)
	e.maxSfbLong = coveringSfb(e.swbLong, e.numSwbLong, cutoff, f.Rate, 2048)
	e.maxSfbShort = coveringSfb(e.swbShort, e.numSwbShort, cutoff, f.Rate, 256)

	longBands := make([]int, e.numSwbLong+1)
	for i := range longBands {
		longBands[i] = int(e.swbLong[i])
	}
	shortBands := make([]int, e.numSwbShort+1)
	for i := range shortBands {
		shortBands[i] = int(e.swbShort[i])
	}
	for c := 0; c < f.Channels; c++ {
		var err error
		e.psyLong[c], err = psy.New(psy.Config{
			Rate: f.Rate, Lines: 1024, FFTSize: 2048,
			BandOffsets: longBands, OffsetDB: psyOffsetDB,
		})
		if err != nil {
			return nil, err
		}
		e.psyShort[c], err = psy.New(psy.Config{
			Rate: f.Rate, Lines: 128, FFTSize: 256,
			BandOffsets: shortBands, NoPredict: true, FixedC: 0.4,
			OffsetDB: psyOffsetDB,
		})
		if err != nil {
			return nil, err
		}
		// The refractory (~18 ms in 128-sample sub-windows) keeps pulse
		// trains at pitch rate from reading as one attack per pulse; see
		// psy.AttackDetector.
		e.det[c] = psy.NewAttackDetector(0, int(math.Round(0.018*float64(f.Rate)/128)))
	}

	e.meanBits = float64(bitrate) * frameLen / float64(f.Rate)
	e.avgPE = e.meanBits * 0.4 // settles onto real content within a few frames
	return e, nil
}

// coveringSfb returns the smallest max_sfb whose bands reach cutoff Hz
// (at least 1, at most numSwb). n is the full transform length.
func coveringSfb(swb []uint16, numSwb int, cutoff float64, rate, n int) int {
	lineHz := float64(rate) / float64(n)
	for sfb := 1; sfb <= numSwb; sfb++ {
		if float64(swb[sfb])*lineHz >= cutoff {
			return sfb
		}
	}
	return numSwb
}

// InputFormat implements codec.Encoder.
func (e *Encoder) InputFormat() audio.Format { return e.fmt }

// FrameSize implements codec.Encoder: 1024 samples per frame.
func (e *Encoder) FrameSize() int { return frameLen }

// Bitrate reports the clamped target bit rate the plan advertises.
func (e *Encoder) Bitrate() int { return e.bitrate }

// Delay reports the encoder priming in output samples.
func (e *Encoder) Delay() int { return EncoderDelay }

// CodecConfig returns the two-byte AudioSpecificConfig (AAC-LC, this
// stream's rate index and channel configuration).
func (e *Encoder) CodecConfig() []byte { return e.asc[:] }

// maxSample bounds accepted input magnitudes (nominal full scale is 1;
// the bound is far above any legitimate pipeline level). Non-finite
// samples become 0 and larger magnitudes clamp: unbounded spectra would
// leave the rate loop no fitting solution and break the 6144-bit-per-
// channel access-unit ceiling.
const maxSample = 8.0

// auCeilingSlack is held back from the 6144-bit-per-channel access unit when
// the rate loop's hard cap is computed. totalBits and overheadBits predict the
// writer rather than being it, and run under it by up to 84 bits; this clears
// that with room to spare. encodeFrame's post-assembly passes are the backstop.
const auCeilingSlack = 256

// Encode buffers src and emits an access unit for every whole source
// block that becomes available, running one block behind the input (the
// deferred window decision); Finish flushes the remainder.
func (e *Encoder) Encode(src *audio.Buffer, emit func(codec.Packet) error) error {
	if src.Fmt != e.fmt {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("aac: encode input %v disagrees with %v", src.Fmt, e.fmt))
	}
	for c := 0; c < e.channels; c++ {
		e.pending[c] = appendSanitized(e.pending[c], src.ChanF(c)[:src.N])
	}
	e.inSamples += int64(src.N)
	for len(e.pending[0]) >= frameLen {
		if err := e.pushBlock(emit); err != nil {
			return err
		}
	}
	return nil
}

// appendSanitized appends src to dst with non-finite samples zeroed and
// magnitudes clamped to maxSample.
func appendSanitized(dst, src []float32) []float32 {
	for _, v := range src {
		switch {
		case math.IsNaN(float64(v)) || math.IsInf(float64(v), 0):
			v = 0
		case v > maxSample:
			v = maxSample
		case v < -maxSample:
			v = -maxSample
		}
		dst = append(dst, v)
	}
	return dst
}

// pushBlock consumes one whole source block from the FIFO. The arriving
// block is scanned for attacks first, then the AU one block behind it is
// encoded (its window is hist[1024:3072] before the shift), so a window
// decision always sees attacks a whole block past the AU it covers; an
// attack early in a block can then make the AU whose short windows reach
// into that block EIGHT_SHORT before it is emitted. The first arriving
// block completes no AU.
func (e *Encoder) pushBlock(emit func(codec.Packet) error) error {
	for c := 0; c < e.channels; c++ {
		aE, pE := e.det[c].Scan(e.pending[c][:frameLen/2], 4)
		aL, pL := e.det[c].Scan(e.pending[c][frameLen/2:frameLen], 4)
		e.attackNew[c] = blockAttacks{
			early: attackInfo{attack: aE, pos: pE},
			late:  attackInfo{attack: aL, pos: pL + 4},
		}
	}
	var err error
	if e.primed {
		err = e.encodeFrame(emit)
	}
	for c := 0; c < e.channels; c++ {
		h := &e.hist[c]
		copy(h[:2*frameLen], h[frameLen:])
		copy(h[2*frameLen:], e.pending[c][:frameLen])
		e.pending[c] = append(e.pending[c][:0], e.pending[c][frameLen:]...)
		e.attackOut[c] = e.attackLA[c]
		e.attackLA[c] = e.attackNew[c]
	}
	e.primed = true
	return err
}

// windowSeq runs the window-sequence state machine for the AU being
// encoded: shortNow reflects an attack inside its output block,
// shortNext one inside the next.
func (e *Encoder) windowSeq(shortNow, shortNext bool) int {
	switch {
	case shortNow:
		return eightShort
	case e.prevSeq == eightShort && shortNext:
		// Bridging short: the left overlap is short and the right must
		// be too; a plain long window cannot sit between two shorts.
		return eightShort
	case e.prevSeq == eightShort:
		return longStop
	case shortNext:
		return longStart
	default:
		return onlyLong
	}
}

// groupingFor maps the attack windows to isolate (unsorted, possibly
// duplicated; up to one per channel per covering half) onto the
// short-window grouping: windows before an attack group together, each
// attack window stands alone, the tail groups. With no attack window
// (a bridging short between two attack frames) all eight windows share
// one group. Four isolated windows produce at most eight groups
// (maxWindowGroups): each attack costs its own group plus at most one
// separator, and four separated attacks span the whole frame, leaving
// no tail.
//
// Short window i spans window offsets [448+128i, 704+128i). An attack
// in the output block at offset 128p+64 (p >= 4, this AU's shortNow
// clause) lands in window p-3; one in the lookahead block at that
// offset (p <= 3) lands in window p+5, clamped to 7.
func groupingFor(wins []int) []int {
	slices.Sort(wins)
	g := make([]int, 0, 2*len(wins)+1)
	pos := 0
	for _, w := range wins {
		if w < pos || w > 7 {
			continue
		}
		if w > pos {
			g = append(g, w-pos)
		}
		g = append(g, 1)
		pos = w + 1
	}
	if pos == 0 {
		return []int{8}
	}
	if pos < 8 {
		g = append(g, 8-pos)
	}
	return g
}

var longGroup = []int{1}

// encodeFrame encodes one access unit from the history window
// (hist[1024:3072], blocks the shift has not yet retired).
//
// The switch clauses follow the short windows' reach: this AU's eight
// shorts cover its output block from offset 448 on plus the first 576
// samples of the lookahead block, so an attack in the output block's
// late half or the lookahead block's early half makes THIS AU short,
// and one in the lookahead block's late half makes the NEXT one short
// (LONG_START now). The half-block reports mean a second attack in a
// block is seen even when its other half attacked first (two clicks in
// one block put a real attack in each neighbor AU's territory). The
// clauses shift index-for-index between consecutive frames, so
// shortNext here IS shortNow next frame: a LONG_START is always
// followed by its EIGHT_SHORT and every attack the detector reports
// lands inside some frame's short windows. Before the deferred
// decision an attack early in a block after quiet frames was coded at
// full weight by the preceding LONG_START's flat region instead, the
// HE quality ledger's core-bound transient deficit.
func (e *Encoder) encodeFrame(emit func(codec.Packet) error) error {
	shortNow := false
	shortNext := false
	// Attack windows to isolate, one candidate per channel per covering
	// half: output-block late half -> window pos-3, lookahead early
	// half -> window pos+5 (clamped; both are the window whose span
	// begins right after the onset's sub-window starts).
	var atkWins [4]int
	nWins := 0
	bridge7 := false
	for c := 0; c < e.channels; c++ {
		if a := e.attackOut[c].late; a.attack {
			shortNow = true
			atkWins[nWins] = a.pos - 3
			nWins++
		}
		if a := e.attackLA[c].early; a.attack {
			shortNow = true
			atkWins[nWins] = min(a.pos+5, 7)
			nWins++
		}
		if a := e.attackLA[c].late; a.attack {
			shortNext = true
			// Only a pos-4 onset (offset 512..640) reaches back into
			// window 7's span (lookahead offsets 320..576); later ones
			// are wholly the next AU's.
			bridge7 = bridge7 || a.pos == 4
		}
		if a := e.attackNew[c].early; a.attack {
			shortNext = true
		}
	}
	seq := e.windowSeq(shortNow, shortNext)
	if seq == eightShort && nWins == 0 && bridge7 {
		// Bridging short whose lookahead attack's first samples fall in
		// the last window: keep it out of the steady group.
		atkWins[0] = 7
		nWins = 1
	}

	// The SBR payload for this AU rides in-band, so its exact cost joins
	// the overhead below and the spectral budget shrinks by it. meanBits
	// stays the full per-AU rate: charging the side bits there too would
	// subtract them twice and deliver 3-11% under the requested rate.
	var sbrPayload *bitWriter
	sbrOverhead := 0
	if e.sbr != nil {
		sbrPayload = e.sbr.payloadFor(e.outFrames)
		if (sbrPayload.bitLen()+7)/8 > maxFILPayloadBytes {
			// Unrepresentable in the fill element's escaped count; the
			// decoder conceals this frame's high band instead of parsing a
			// desynchronized element stream, and the encoder's delta-time
			// mirrors roll back to match the concealment (the decoder
			// keeps its anchors through a concealed frame). See
			// maxFILPayloadBytes.
			e.sbr.rollbackPayload()
			sbrPayload = nil
		}
	}
	if sbrPayload != nil {
		sbrOverhead = filBits(sbrPayload.bitLen())
	}

	groupLen := longGroup
	swb := e.swbLong
	maxSfb := e.maxSfbLong
	if seq == eightShort {
		groupLen = groupingFor(atkWins[:nWins])
		swb = e.swbShort
		maxSfb = e.maxSfbShort
	}

	// Psychoacoustics. The long model runs every frame to keep its
	// prediction history continuous; PE feeds the bit reservoir.
	pe := 0.0
	for c := 0; c < e.channels; c++ {
		rl, err := e.psyLong[c].Analyze(e.hist[c][frameLen : frameLen+2048])
		if err != nil {
			return err
		}
		pe = math.Max(pe, rl.PE)
		if seq != eightShort {
			for sfb := 0; sfb < e.numSwbLong; sfb++ {
				e.thr[c][0][sfb] = rl.Thr[sfb] * thrCalib
			}
			continue
		}
		// Short thresholds accumulate over each group's windows.
		var wThr [8][maxSFBCount]float64
		for i := 0; i < 8; i++ {
			off := frameLen + 448 + i*128
			rs, err := e.psyShort[c].Analyze(e.hist[c][off : off+256])
			if err != nil {
				return err
			}
			for sfb := 0; sfb < e.numSwbShort; sfb++ {
				wThr[i][sfb] = rs.Thr[sfb] * thrCalib
			}
		}
		win := 0
		for g, L := range groupLen {
			for sfb := 0; sfb < e.numSwbShort; sfb++ {
				t := 0.0
				for w := 0; w < L; w++ {
					t += wThr[win+w][sfb]
				}
				e.thr[c][g][sfb] = t
			}
			win += L
		}
	}

	// MDCT on the 32768-scaled block (the decoder normalizes by 1/32768).
	for c := 0; c < e.channels; c++ {
		var tblk [2048]float64
		for i := range tblk {
			tblk[i] = float64(e.hist[c][frameLen+i]) * 32768
		}
		mdctFrame(&tblk, seq, &e.spec[c])
	}

	// TNS per channel (long windows), before the stereo transform: the
	// decoder recombines M/S first and inverse-filters L/R after.
	for c := 0; c < e.channels; c++ {
		e.tns[c] = tnsEnc{}
		if seq != eightShort {
			e.tns[c] = analyzeTNS(&e.spec[c], e.swbLong, e.numSwbLong, maxSfb, e.rateIdx, e.rate)
		}
	}

	// M/S decision and transform (stereo only).
	msMask := 0
	if e.channels == 2 {
		msMask = e.decideMS(groupLen, swb, maxSfb)
	}

	// Band tables and thresholds feed the two-loop quantizer.
	for c := 0; c < e.channels; c++ {
		ch := c
		e.cq[c].buildBands(&e.spec[c], groupLen, swb, maxSfb,
			func(g, sfb int) float64 { return e.thr[ch][g][sfb] }, seq == eightShort)
	}

	// Frame bit budget: reservoir-smoothed, difficulty-modulated.
	e.avgPE = 0.95*e.avgPE + 0.05*pe
	difficulty := 1.0
	if e.avgPE > 0 {
		difficulty = min(max(pe/e.avgPE, 0.65), 1.7)
	}
	target := e.meanBits * difficulty
	target = math.Min(target, e.meanBits+math.Max(e.reservoir, 0)*0.5)
	if e.sbr != nil && e.reservoir < 0 {
		// The HE operating points sit low enough that demanding content
		// can outrun the mean for long stretches; a depleted reservoir
		// must push back or the ABR contract fails by half again. LC keeps
		// its historical no-repayment behavior (its rates never pinned the
		// reservoir; changing its output would be an encoder revision).
		target = math.Min(target, e.meanBits+e.reservoir*0.25)
	}
	target = math.Max(target, e.meanBits*0.3)
	target = math.Min(target, float64(6144*e.channels)*0.93)

	overhead := e.overheadBits(seq, maxSfb, msMask, len(groupLen)) + sbrOverhead
	spectral := int(target) - overhead
	if spectral < 0 {
		spectral = 0
	}
	// The spectral ceiling the rate loop may never cross, as against the target
	// above, which it may. The target is already well under it, so neither this
	// clamp nor quantizeChannel's fires today. They are not dead: the fallback
	// quantizeChannel takes on a stale candidate only fits the hard cap because
	// the budget it was fitted to is capped. Raise 0.93 and they start working.
	hard := 6144*e.channels - overhead - auCeilingSlack
	if e.sbr != nil {
		// The quantizer's best-scored fallback may exceed the soft budget
		// up to this cap (quantizeChannel's contract). At LC rates that
		// slack is modest; at HE rates the whole spec ceiling is many
		// times the frame budget, so an uncapped fallback overshoots every
		// frame and no reservoir can hold the mean. Bound the overshoot to
		// a quarter frame. (A larger allowance for short-window frames was
		// tried and measured WORSE on the transient quality leg: the loan's
		// repayment squeezes the steady frames that follow harder than the
		// extra attack bits help.)
		hard = min(hard, spectral+int(e.meanBits*0.25))
	}
	if hard < 0 {
		hard = 0
	}
	spectral = min(spectral, hard)

	// Resolved once, so the corrective passes keep the same channel balance.
	frac := 0.5
	if e.channels == 2 {
		if dl, dr := e.cq[0].demand, e.cq[1].demand; dl+dr > 0 {
			frac = min(max(dl/(dl+dr), 0.2), 0.8)
		}
	}
	// build quantizes both channels and assembles the unit. The hard cap splits
	// like the budget, so the channels' caps still sum to the frame's.
	build := func(spectral, hard int) {
		if e.channels == 2 {
			lSpectral, lHard := int(float64(spectral)*frac), int(float64(hard)*frac)
			e.cq[0].quantizeChannel(lSpectral, lHard)
			e.cq[1].quantizeChannel(spectral-lSpectral, hard-lHard)
			e.w.reset()
			e.writeCPE(seq, groupLen, maxSfb, msMask)
		} else {
			e.cq[0].quantizeChannel(spectral, hard)
			e.w.reset()
			e.writeSCE(seq, groupLen, maxSfb)
		}
		if sbrPayload != nil {
			writeFIL(&e.w, sbrPayload)
		}
		e.w.writeBits(3, elEND)
		e.w.align()
	}
	build(spectral, hard)

	// Enforce the ceiling on the bytes emitted, not on the estimate the rate
	// loop worked from: the estimates run under the writer, so drift in them
	// must fail on the frame that caused it rather than silently emit a unit a
	// conforming decoder cannot buffer. No frame reaches this today.
	//
	// The first pass cuts the budget by the overshoot it measured, the second
	// cuts it to nothing, which zeroes every band. Bounded at three passes.
	ceiling := 6144 * e.channels
	for pass := 1; e.w.bitLen() > ceiling && pass <= 2; pass++ {
		if pass == 1 {
			spectral = max(0, spectral-(e.w.bitLen()-ceiling)-auCeilingSlack)
		} else {
			spectral = 0
		}
		build(spectral, spectral)
	}

	e.reservoir += e.meanBits - float64(e.w.bitLen())
	if e.reservoir > float64(6144*e.channels) {
		e.reservoir = float64(6144 * e.channels)
	}
	if e.reservoir < -2*e.meanBits {
		e.reservoir = -2 * e.meanBits
	}

	e.prevSeq = seq
	e.outFrames++
	// Packets are borrowed (valid during emit only), so the writer's
	// buffer goes out directly.
	return emit(codec.Packet{Data: e.w.buf, PTS: (e.outFrames - 1) * frameLen, Dur: frameLen, Sync: true})
}

// decideMS chooses the per-band M/S mask by comparing the perceptual
// bit demand of L/R against M/S coding under the conservative shared
// threshold, transforms the chosen bands in place, and rewrites both
// channels' thresholds. Returns ms_mask_present (0, 1, or 2).
func (e *Encoder) decideMS(groupLen []int, swb []uint16, maxSfb int) int {
	all, none := true, true
	winBase := 0
	for g, L := range groupLen {
		for sfb := 0; sfb < maxSfb; sfb++ {
			lo, hi := int(swb[sfb]), int(swb[sfb+1])
			var eL, eR, eM, eS float64
			for w := 0; w < L; w++ {
				base := (winBase + w) * 128
				for k := lo; k < hi; k++ {
					l, r := e.spec[0][base+k], e.spec[1][base+k]
					eL += l * l
					eR += r * r
					m, s := (l+r)/2, (l-r)/2
					eM += m * m
					eS += s * s
				}
			}
			thrL, thrR := e.thr[0][g][sfb], e.thr[1][g][sfb]
			thrMS := math.Min(thrL, thrR) / 2
			w := float64(hi - lo)
			costLR := demandOf(eL, thrL, w) + demandOf(eR, thrR, w)
			costMS := demandOf(eM, thrMS, w) + demandOf(eS, thrMS, w)
			if costMS < costLR {
				e.msUse[g][sfb] = true
				none = false
			} else {
				e.msUse[g][sfb] = false
				all = false
			}
		}
		winBase += L
	}
	if none {
		return 0
	}
	// Transform the chosen bands and install the shared thresholds.
	winBase = 0
	for g, L := range groupLen {
		for sfb := 0; sfb < maxSfb; sfb++ {
			if !e.msUse[g][sfb] {
				continue
			}
			thrMS := math.Min(e.thr[0][g][sfb], e.thr[1][g][sfb]) / 2
			e.thr[0][g][sfb] = thrMS
			e.thr[1][g][sfb] = thrMS
			lo, hi := int(swb[sfb]), int(swb[sfb+1])
			for w := 0; w < L; w++ {
				base := (winBase + w) * 128
				for k := lo; k < hi; k++ {
					l, r := e.spec[0][base+k], e.spec[1][base+k]
					e.spec[0][base+k] = (l + r) / 2
					e.spec[1][base+k] = (l - r) / 2
				}
			}
		}
		winBase += L
	}
	if all {
		return 2
	}
	return 1
}

// demandOf is the perceptual bit demand of one band: information above
// the masking threshold.
func demandOf(energy, thr, width float64) float64 {
	if thr <= 0 || energy <= thr {
		return 0
	}
	return width * math.Log2(energy/thr)
}

// overheadBits counts every non-spectral bit of the frame so the rate
// loop budgets exactly: element headers, ics_info, the M/S mask, TNS
// presence and data, and the END element with byte alignment slack.
func (e *Encoder) overheadBits(seq, maxSfb, msMask, groups int) int {
	ics := 1 + 2 + 1 // reserved + sequence + shape
	if seq == eightShort {
		ics += 4 + 7
	} else {
		ics += 6 + 1
	}
	perChan := 8 + 1 + 1 + 1 // global_gain + pulse + tns present + gain control
	total := 3 + 4           // element id + instance tag
	if e.channels == 2 {
		total += 1 + ics + 2 // common_window + shared ics_info + ms_mask_present
		if msMask == 1 {
			total += groups * maxSfb
		}
		total += 2 * perChan
	} else {
		total += ics + perChan
	}
	for c := 0; c < e.channels; c++ {
		total += e.tns[c].sideBits()
	}
	total += 3 + 7 // END + worst-case alignment
	return total
}

// writeICSBody emits one channel's individual_channel_stream:
// global_gain, then ics_info when the window is not shared (SCE), then
// sections, scalefactors, pulse/TNS/gain flags, and spectra. The order
// matches decodeChannelData: global_gain comes FIRST.
func (e *Encoder) writeICSBody(c int, info func()) {
	cq := &e.cq[c]
	w := &e.w
	w.writeBits(8, uint64(cq.globalGain))
	if info != nil {
		info()
	}
	// section_data
	lenEsc := uint64(1)<<uint(cq.lenBits) - 1
	for g := 0; g < cq.nGroups; g++ {
		k := 0
		for k < cq.maxSfb {
			cb := cq.bands[g*cq.maxSfb+k].cb
			run := 1
			for k+run < cq.maxSfb && cq.bands[g*cq.maxSfb+k+run].cb == cb {
				run++
			}
			w.writeBits(4, uint64(cb))
			l := run
			for l >= int(lenEsc) {
				w.writeBits(uint(cq.lenBits), lenEsc)
				l -= int(lenEsc)
			}
			w.writeBits(uint(cq.lenBits), uint64(l))
			k += run
		}
	}
	// scale_factor_data
	prev := cq.globalGain
	for g := 0; g < cq.nGroups; g++ {
		for k := 0; k < cq.maxSfb; k++ {
			b := &cq.bands[g*cq.maxSfb+k]
			if b.cb == 0 {
				continue
			}
			e.w.writeSFDelta(b.sf - prev)
			prev = b.sf
		}
	}
	w.writeBits(1, 0) // pulse_data_present
	if e.tns[c].present {
		w.writeBits(1, 1)
		e.tns[c].write(w)
	} else {
		w.writeBits(1, 0)
	}
	w.writeBits(1, 0) // gain_control_data_present
	// spectral_data: per group, tuples across each equal-codebook section.
	var vbuf [1024]int
	for g := 0; g < cq.nGroups; g++ {
		for k := 0; k < cq.maxSfb; {
			b := &cq.bands[g*cq.maxSfb+k]
			run := 1
			for k+run < cq.maxSfb && cq.bands[g*cq.maxSfb+k+run].cb == b.cb {
				run++
			}
			if b.cb != 0 {
				n := 0
				for j := 0; j < run; j++ {
					bb := &cq.bands[g*cq.maxSfb+k+j]
					for i := 0; i < bb.n; i++ {
						vbuf[n] = cq.q[bb.off+i]
						n++
					}
				}
				w.writeSpecRun(b.cb, vbuf[:n])
			}
			k += run
		}
	}
}

// writeICSInfo emits ics_info for the frame's window configuration.
func (e *Encoder) writeICSInfo(seq, maxSfb int, groupLen []int) {
	w := &e.w
	w.writeBits(1, 0) // ics_reserved
	w.writeBits(2, uint64(seq))
	w.writeBits(1, shapeSine)
	if seq == eightShort {
		w.writeBits(4, uint64(maxSfb))
		// scale_factor_grouping: bit i set means window i+1 shares
		// window i's group.
		bits := uint64(0)
		win := 0
		for _, L := range groupLen {
			for j := 1; j < L; j++ {
				bits |= 1 << uint(6-(win+j-1))
			}
			win += L
		}
		w.writeBits(7, bits)
	} else {
		w.writeBits(6, uint64(maxSfb))
		w.writeBits(1, 0) // predictor_data_present
	}
}

func (e *Encoder) writeSCE(seq int, groupLen []int, maxSfb int) {
	e.w.writeBits(3, elSCE)
	e.w.writeBits(4, 0) // element_instance_tag
	e.writeICSBody(0, func() { e.writeICSInfo(seq, maxSfb, groupLen) })
}

func (e *Encoder) writeCPE(seq int, groupLen []int, maxSfb, msMask int) {
	e.w.writeBits(3, elCPE)
	e.w.writeBits(4, 0)
	e.w.writeBits(1, 1) // common_window
	e.writeICSInfo(seq, maxSfb, groupLen)
	e.w.writeBits(2, uint64(msMask))
	if msMask == 1 {
		for g := range groupLen {
			for sfb := 0; sfb < maxSfb; sfb++ {
				v := uint64(0)
				if e.msUse[g][sfb] {
					v = 1
				}
				e.w.writeBits(1, v)
			}
		}
	}
	e.writeICSBody(0, nil)
	e.writeICSBody(1, nil)
}

// Finish pads the tail to a whole block, encodes it, then pushes two
// final zero blocks: the first is the block that covers every real
// sample with two overlapping windows, the second only flushes the
// deferred AU and enters no emitted window. The trailer is unchanged by
// the deferral: the AU sequence is identical, one push later.
func (e *Encoder) Finish(emit func(codec.Packet) error) (codec.Trailer, error) {
	if n := len(e.pending[0]); n > 0 {
		for c := 0; c < e.channels; c++ {
			e.pending[c] = append(e.pending[c], make([]float32, frameLen-n)...)
		}
		if err := e.pushBlock(emit); err != nil {
			return codec.Trailer{}, err
		}
	}
	for i := 0; i < 2; i++ {
		for c := 0; c < e.channels; c++ {
			if cap(e.pending[c]) < frameLen {
				e.pending[c] = make([]float32, frameLen)
			} else {
				e.pending[c] = e.pending[c][:frameLen]
				clear(e.pending[c])
			}
		}
		if err := e.pushBlock(emit); err != nil {
			return codec.Trailer{}, err
		}
	}
	for c := 0; c < e.channels; c++ {
		e.pending[c] = e.pending[c][:0]
	}
	delay := int64(EncoderDelay)
	padding := e.outFrames*frameLen - e.inSamples - delay
	if padding < 0 {
		padding = 0
	}
	return codec.Trailer{Samples: e.inSamples, Delay: delay, Padding: padding}, nil
}
