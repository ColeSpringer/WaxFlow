package opus

// CELT decode main loop (RFC 6716 section 4.3). Ported from libopus
// celt_decoder.c and celt.c. Ties the entropy, energy,
// allocation, and band-shape stages together, runs the inverse MDCT with
// overlap-add through a sliding history buffer, applies the pitch post-filter
// and de-emphasis, and carries the inter-frame energy/post-filter state a
// packet's prediction needs.

const (
	celtDecodeBufferSize = 2048 // DECODE_BUFFER_SIZE
	combMinPeriod        = 15   // COMBFILTER_MINPERIOD
	combMaxPeriod        = 1024 // COMBFILTER_MAXPERIOD
	celtSigScale         = 32768.0
	celtPreemph          = 0.85000610 // mode->preemph[0] for the 48 kHz mode
)

// tfSelectTable picks the per-band time-frequency resolution (libopus celt.c).
var tfSelectTable = [4][8]int8{
	{0, -1, 0, -1, 0, -1, 0, -1},
	{0, -1, 0, -2, 1, 0, 1, -1},
	{0, -2, 0, -3, 2, 0, 1, -1},
	{0, -2, 0, -3, 3, 0, 1, -1},
}

// combGains are the 3-tap post-filter gains per tapset (libopus celt.c).
var combGains = [3][3]float32{
	{0.3066406250, 0.2170410156, 0.1296386719},
	{0.4638671875, 0.2680664062, 0.0},
	{0.7998046875, 0.1000976562, 0.0},
}

// celtDecoder holds the persistent CELT decode state for one stream.
type celtDecoder struct {
	channels int // output channel count (CC)
	overlap  int

	decodeMem [][]float32 // per channel, celtDecodeBufferSize+overlap
	// Energy history, each 2*nbEBands: current, previous, two-ago, background.
	oldBandE, oldLogE, oldLogE2, backgroundLogE []float32

	postPeriod, postPeriodOld int
	postGain, postGainOld     float32
	postTapset, postTapsetOld int
	preemphMemD               [2]float32
	rng                       uint32
	prefilterAndFold          bool

	// Per-frame scratch. Only the large buffers are pooled here; the small
	// per-frame temporaries (band vectors sized celtNBands, SILK frame
	// buffers) deliberately still allocate, ~20-30 allocs/packet measured as
	// immaterial next to decode cost. The full scratch sweep is deferred to
	// the performance milestone alongside the MDCT FFT.
	window  []float64
	mdctScr *mdctScratch
	mdctPlanCache
	freq  []float32
	freq2 []float32 // second channel for the stereo->mono downmix
	xnorm []float32
	iy    []int
	u     []uint32
}

func newCELTDecoder(channels int) *celtDecoder {
	d := &celtDecoder{
		channels:       channels,
		overlap:        celtOverlap,
		oldBandE:       make([]float32, 2*celtNBands),
		oldLogE:        make([]float32, 2*celtNBands),
		oldLogE2:       make([]float32, 2*celtNBands),
		backgroundLogE: make([]float32, 2*celtNBands),
		window:         celtWindow(celtOverlap),
		mdctScr:        newMDCTScratch(480),
		freq:           make([]float32, celtShortMDCTSize<<celtMaxLM),
		freq2:          make([]float32, celtShortMDCTSize<<celtMaxLM),
		xnorm:          make([]float32, 2*(celtShortMDCTSize<<celtMaxLM)),
		iy:             make([]int, celtShortMDCTSize<<celtMaxLM),
		u:              make([]uint32, celtMaxPulses+2),
	}
	d.decodeMem = make([][]float32, channels)
	for c := range d.decodeMem {
		d.decodeMem[c] = make([]float32, celtDecodeBufferSize+d.overlap)
	}
	for i := range d.oldLogE {
		d.oldLogE[i] = -28
		d.oldLogE2[i] = -28
	}
	return d
}

const celtMaxPulses = 128

// Reset clears the inter-frame state after a seek or mode switch
// (OPUS_RESET_STATE: everything zeroed, then oldLogE/oldLogE2 to -28;
// backgroundLogE stays 0, matching the reference).
func (d *celtDecoder) Reset() {
	for c := range d.decodeMem {
		clear(d.decodeMem[c])
	}
	clear(d.oldBandE)
	clear(d.backgroundLogE)
	for i := range d.oldLogE {
		d.oldLogE[i] = -28
		d.oldLogE2[i] = -28
	}
	d.postPeriod, d.postPeriodOld, d.postGain, d.postGainOld = 0, 0, 0, 0
	d.postTapset, d.postTapsetOld = 0, 0
	d.preemphMemD = [2]float32{}
	d.rng = 0
	d.prefilterAndFold = false
}

// tfDecode reads the per-band time-frequency change flags (libopus).
func tfDecode(start, end, isTransient int, tfRes []int, LM int, dec *rangeDecoder) {
	budget := dec.storage * 8
	tell := dec.tell()
	logp := 4
	if isTransient != 0 {
		logp = 2
	}
	tfSelectRsv := 0
	if LM > 0 && tell+logp+1 <= budget {
		tfSelectRsv = 1
	}
	budget -= tfSelectRsv
	curr, tfChanged := 0, 0
	for i := start; i < end; i++ {
		if tell+logp <= budget {
			curr ^= dec.decodeBitLogp(uint(logp))
			tell = dec.tell()
			tfChanged |= curr
		}
		tfRes[i] = curr
		if isTransient != 0 {
			logp = 4
		} else {
			logp = 5
		}
	}
	tfSelect := 0
	if tfSelectRsv != 0 &&
		tfSelectTable[LM][4*isTransient+0+tfChanged] != tfSelectTable[LM][4*isTransient+2+tfChanged] {
		tfSelect = dec.decodeBitLogp(1)
	}
	for i := start; i < end; i++ {
		tfRes[i] = int(tfSelectTable[LM][4*isTransient+2*tfSelect+tfRes[i]])
	}
}

// celtDecode decodes one CELT-only frame from data into the CC output channels
// out[0..CC-1], each of length N = shortMdctSize<<LM. C is the number of coded
// channels (1 or 2); start/end are the coded band range.
func (d *celtDecoder) celtDecode(data []byte, LM, C, start, end int, out [][]float32) error {
	return d.celtDecodeInner(newRangeDecoder(data), data, LM, C, start, end, out)
}

// celtDecodeInner is the CELT decode body. dec is the entropy decoder (its own
// for CELT-only, or the frame's shared decoder positioned past the SILK data
// for hybrid); data is the whole frame's bytes (for the bit budget). out is
// always overwritten; in a hybrid frame the caller sums the SILK low band on
// top afterward (the float build's opus_decode_frame order).
func (d *celtDecoder) celtDecodeInner(dec *rangeDecoder, data []byte, LM, C, start, end int, out [][]float32) error {
	CC := d.channels
	M := 1 << LM
	N := M * celtShortMDCTSize
	overlap := d.overlap
	nb := celtNBands
	effEnd := min(end, celtNBands)

	if len(data) <= 1 {
		// Treat as silence (a minimal loss concealment).
		d.combShiftSilence(N, CC, out)
		return nil
	}

	if C == 1 {
		for i := 0; i < nb; i++ {
			d.oldBandE[i] = max(d.oldBandE[i], d.oldBandE[nb+i])
		}
	}
	totalBits := len(data) * 8
	tell := dec.tell()

	silence := 0
	if tell >= totalBits {
		silence = 1
	} else if tell == 1 {
		silence = dec.decodeBitLogp(15)
	}
	if silence != 0 {
		tell = len(data) * 8
		dec.nbits += tell - dec.tell()
	}

	// Post-filter (pitch) parameters.
	postGain := float32(0)
	postPitch := 0
	postTapset := 0
	if start == 0 && tell+16 <= totalBits {
		if dec.decodeBitLogp(1) != 0 {
			octave := int(dec.decodeUint(6))
			postPitch = (16 << uint(octave)) + int(dec.decodeRawBits(uint(4+octave))) - 1
			qg := int(dec.decodeRawBits(3))
			if dec.tell()+2 <= totalBits {
				postTapset = dec.decodeICDF(celtTapsetICDF, 2)
			}
			postGain = 0.09375 * float32(qg+1)
		}
		tell = dec.tell()
	}

	isTransient := 0
	if LM > 0 && tell+3 <= totalBits {
		isTransient = dec.decodeBitLogp(3)
		tell = dec.tell()
	}
	shortBlocks := 0
	if isTransient != 0 {
		shortBlocks = M
	}

	intraEner := 0
	if tell+3 <= totalBits {
		intraEner = dec.decodeBitLogp(3)
	}
	unquantCoarseEnergy(d.oldBandE, nb, start, end, intraEner != 0, dec, C, LM)

	tfRes := make([]int, nb)
	tfDecode(start, end, isTransient, tfRes, LM, dec)

	tell = dec.tell()
	spread := spreadNormal
	if tell+4 <= totalBits {
		spread = dec.decodeICDF(celtSpreadICDF, 5)
	}

	cap := make([]int, nb)
	initCaps(cap, LM, C)

	offsets := make([]int, nb)
	dynallocLogp := 6
	totalBitsFrac := totalBits << bitRes
	tell = dec.tellFrac()
	for i := start; i < end; i++ {
		width := C * (int(celtEBands[i+1]) - int(celtEBands[i])) << LM
		quanta := min(width<<bitRes, max(6<<bitRes, width))
		loopLogp := dynallocLogp
		boost := 0
		for tell+(loopLogp<<bitRes) < totalBitsFrac && boost < cap[i] {
			flag := dec.decodeBitLogp(uint(loopLogp))
			tell = dec.tellFrac()
			if flag == 0 {
				break
			}
			boost += quanta
			totalBitsFrac -= quanta
			loopLogp = 1
		}
		offsets[i] = boost
		if boost > 0 {
			dynallocLogp = max(2, dynallocLogp-1)
		}
	}

	fineQuant := make([]int, nb)
	allocTrim := 5
	if tell+(6<<bitRes) <= totalBitsFrac {
		allocTrim = dec.decodeICDF(celtTrimICDF, 7)
	}

	bits := (len(data)*8)<<bitRes - dec.tellFrac() - 1
	antiCollapseRsv := 0
	if isTransient != 0 && LM >= 2 && bits >= (LM+2)<<bitRes {
		antiCollapseRsv = 1 << bitRes
	}
	bits -= antiCollapseRsv

	pulses := make([]int, nb)
	finePriority := make([]int, nb)
	intensity, dualStereo, balance := 0, 0, 0
	codedBands := cltComputeAllocation(start, end, offsets, cap, allocTrim, &intensity, &dualStereo,
		bits, &balance, pulses, fineQuant, finePriority, C, LM, nil, dec, false, 0, 0)

	unquantFineEnergy(d.oldBandE, nb, start, end, fineQuant, dec, C)

	// Slide the decode history left by N so the new frame's synthesis region
	// keeps the previous frame's overlap tail.
	keep := celtDecodeBufferSize - N + overlap
	for c := 0; c < CC; c++ {
		copy(d.decodeMem[c][:keep], d.decodeMem[c][N:N+keep])
	}

	X := d.xnorm[:C*N]
	clear(X)
	collapseMasks := make([]byte, C*nb)
	var Y []float32
	if C == 2 {
		Y = X[N:]
	}
	// The noise-fill/folding PRNG seed is decoder state: it persists across
	// frames and is refreshed from the range coder's final register below
	// (libopus st->rng), so a primed state reproduces the reference noise.
	disableInv := 0
	if CC == 1 {
		disableInv = 1
	}
	quantAllBands(start, end, X, Y, collapseMasks, d.oldBandE, pulses, shortBlocks, spread,
		dualStereo, intensity, tfRes, len(data)*(8<<bitRes)-antiCollapseRsv, balance, nil, dec,
		LM, codedBands, &d.rng, 0, disableInv, d.iy, d.u)

	antiCollapseOn := 0
	if antiCollapseRsv > 0 {
		antiCollapseOn = int(dec.decodeRawBits(1))
	}
	unquantEnergyFinalise(d.oldBandE, nb, start, end, fineQuant, finePriority, len(data)*8-dec.tell(), dec, C)
	if antiCollapseOn != 0 {
		antiCollapse(X, collapseMasks, LM, C, N, start, end, d.oldBandE, d.oldLogE, d.oldLogE2, pulses, d.rng)
	}
	if silence != 0 {
		for i := 0; i < C*nb; i++ {
			d.oldBandE[i] = -28
		}
	}

	d.synthesis(X, LM, C, CC, isTransient, start, effEnd, N, silence != 0)

	// Post-filter each output channel across the frame.
	for c := 0; c < CC; c++ {
		base := celtDecodeBufferSize - N
		d.postPeriod = max(d.postPeriod, combMinPeriod)
		d.postPeriodOld = max(d.postPeriodOld, combMinPeriod)
		combFilter(d.decodeMem[c], base, d.decodeMem[c], base, d.postPeriodOld, d.postPeriod, celtShortMDCTSize,
			d.postGainOld, d.postGain, d.postTapsetOld, d.postTapset, d.window, overlap)
		if LM != 0 {
			combFilter(d.decodeMem[c], base+celtShortMDCTSize, d.decodeMem[c], base+celtShortMDCTSize, d.postPeriod, postPitch, N-celtShortMDCTSize,
				d.postGain, postGain, d.postTapset, postTapset, d.window, overlap)
		}
	}
	d.postPeriodOld = d.postPeriod
	d.postGainOld = d.postGain
	d.postTapsetOld = d.postTapset
	d.postPeriod = postPitch
	d.postGain = postGain
	d.postTapset = postTapset
	if LM != 0 {
		d.postPeriodOld = d.postPeriod
		d.postGainOld = d.postGain
		d.postTapsetOld = d.postTapset
	}

	d.updateEnergyState(C, isTransient, M, start, end)
	d.rng = dec.rng

	d.deemphasis(out, CC, N)
	return nil
}

// deemphasis runs the de-emphasis filter over the freshly synthesized region
// of the decode buffer into out, planar and scaled to [-1,1] (libopus
// deemphasis, the 48 kHz float case).
func (d *celtDecoder) deemphasis(out [][]float32, CC, N int) {
	base := celtDecodeBufferSize - N
	for c := 0; c < CC; c++ {
		m := d.preemphMemD[c]
		src := d.decodeMem[c]
		dst := out[c]
		for j := 0; j < N; j++ {
			tmp := src[base+j] + m
			m = celtPreemph * tmp
			dst[j] = tmp * (1.0 / celtSigScale)
		}
		d.preemphMemD[c] = m
	}
}

// synthesis denormalizes the bands and runs the inverse MDCT with overlap-add
// into the decode buffer's synthesis region (libopus celt_synthesis; the mono,
// stereo, and mono→stereo cases).
func (d *celtDecoder) synthesis(X []float32, LM, C, CC, isTransient, start, effEnd, N int, silence bool) {
	M := 1 << LM
	var B, NB, mdctN int
	if isTransient != 0 {
		B = M
		NB = celtShortMDCTSize
		mdctN = 2 * celtShortMDCTSize
	} else {
		B = 1
		NB = celtShortMDCTSize << LM
		mdctN = 2 * (celtShortMDCTSize << LM)
	}
	plan := d.planFor(mdctN)
	overlap := d.overlap

	do := func(cSrc, cOut int) {
		base := celtDecodeBufferSize - N
		denormaliseBands(X[cSrc*N:], d.freq, d.oldBandE[cSrc*celtNBands:], start, effEnd, M, 1, silence)
		for b := 0; b < B; b++ {
			plan.backward(d.freq[b:], B, d.decodeMem[cOut][base+NB*b:], d.window, overlap, d.mdctScr)
		}
	}

	if CC == 2 && C == 1 {
		do(0, 0)
		do(0, 1)
	} else if CC == 1 && C == 2 {
		// Downmixing a stereo stream to mono: average the two coded channels
		// in the signal (denormalised) domain, then one inverse MDCT.
		base := celtDecodeBufferSize - N
		denormaliseBands(X, d.freq, d.oldBandE, start, effEnd, M, 1, silence)
		denormaliseBands(X[N:], d.freq2, d.oldBandE[celtNBands:], start, effEnd, M, 1, silence)
		for i := 0; i < N; i++ {
			d.freq[i] = 0.5*d.freq[i] + 0.5*d.freq2[i]
		}
		for b := 0; b < B; b++ {
			plan.backward(d.freq[b:], B, d.decodeMem[0][base+NB*b:], d.window, overlap, d.mdctScr)
		}
	} else {
		for c := 0; c < CC; c++ {
			do(c, c)
		}
	}
}

// updateEnergyState carries the log-energy history forward for the next frame's
// prediction and anti-collapse (libopus celt_decoder.c).
func (d *celtDecoder) updateEnergyState(C, isTransient, M, start, end int) {
	nb := celtNBands
	if C == 1 {
		copy(d.oldBandE[nb:2*nb], d.oldBandE[:nb])
	}
	if isTransient == 0 {
		copy(d.oldLogE2, d.oldLogE[:2*nb])
		copy(d.oldLogE, d.oldBandE[:2*nb])
	} else {
		for i := 0; i < 2*nb; i++ {
			d.oldLogE[i] = min(d.oldLogE[i], d.oldBandE[i])
		}
	}
	maxInc := float32(min(160, M)) * 0.001
	for i := 0; i < 2*nb; i++ {
		d.backgroundLogE[i] = min(d.backgroundLogE[i]+maxInc, d.oldBandE[i])
	}
	for c := 0; c < 2; c++ {
		for i := 0; i < start; i++ {
			d.oldBandE[c*nb+i] = 0
			d.oldLogE[c*nb+i] = -28
			d.oldLogE2[c*nb+i] = -28
		}
		for i := end; i < nb; i++ {
			d.oldBandE[c*nb+i] = 0
			d.oldLogE[c*nb+i] = -28
			d.oldLogE2[c*nb+i] = -28
		}
	}
}

// combShiftSilence advances the decode buffer and emits silence for an empty
// packet (a minimal concealment; full PLC is out of scope, RFC 6716 file
// mode). The PRNG seed and post-filter state deliberately stay put: the
// reference would run PLC here and the next frame diverges from it wholesale
// either way, so there is no reference state to track through this path.
func (d *celtDecoder) combShiftSilence(N, CC int, out [][]float32) {
	keep := celtDecodeBufferSize - N + d.overlap
	for c := 0; c < CC; c++ {
		copy(d.decodeMem[c][:keep], d.decodeMem[c][N:N+keep])
		clear(d.decodeMem[c][celtDecodeBufferSize-N : celtDecodeBufferSize])
	}
	d.deemphasis(out, CC, N)
}
