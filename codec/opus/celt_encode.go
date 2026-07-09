package opus

// CELT encode main loop (RFC 6716 section 4.3, encode direction). Ported from
// libopus celt_encoder.c celt_encode_with_ec, float build, CELT-only music mode.
// It runs pre-emphasis, tone detection, the pitch pre-filter, the forward MDCT,
// band-energy analysis and coarse/fine quantization, the full side-information
// analysis (transient/patch-transient detection, tf_analysis, spreading,
// dynamic allocation, alloc-trim, stereo intensity/dual-stereo), CBR or
// (constrained) VBR rate control, and the PVQ band coding (with the theta RDO
// search at high complexity) into one CELT payload. The analysis depth is gated
// by the complexity knob (see EncoderOptions.Complexity).
//
// Phase-1 scope: the tonality analyser (analysis->valid) stays off, so its
// activity/tonality boosts and the pre-filter gain damping are not taken; it
// lands with the phase-2 speech/music decision. The bitstream is fully
// standards-compliant and decodes with both our decoder and libopus.

// celtEncoder holds the persistent CELT encode state for one stream.
type celtEncoder struct {
	channels int
	overlap  int

	// Encode configuration (survives Reset, like libopus's fields outside
	// ENCODER_RESET_START). complexity gates the analysis depth (0..10);
	// vbr/constrainedVBR pick the rate-control mode; bitrate is the target b/s.
	complexity     int
	vbr            bool
	constrainedVBR bool
	bitrate        int

	preemphMem [2]float32  // pre-emphasis filter memory per channel
	preHistory [][]float32 // per channel: overlap samples of prefiltered history (libopus in_mem)

	// Pitch pre-filter state: the sliding unfiltered pre-emphasized history
	// the pitch search and comb filter read, plus the previous frame's filter
	// parameters for the crossfade and continuity decisions.
	prefilterMem    [][]float32 // per channel, combMaxPeriod samples
	prefilterPeriod int
	prefilterGain   float32
	prefilterTapset int

	// Energy prediction/finalisation state (the quantized log energies the next
	// frame predicts from, matching the decoder's oldBandE/oldLogE).
	oldBandE    []float32
	oldLogE     []float32
	oldLogE2    []float32
	energyError []float32

	delayedIntra    float32
	consecTransient int
	spreadDecision  int
	tonalAverage    int
	hfAverage       int
	tapsetDecision  int
	intensity       int
	stereoSaving    float32
	specAvg         float32
	lastCodedBands  int
	rng             uint32

	// Constrained-VBR rate control: the bit reservoir bounding short-term
	// overshoot and the drift correction steering the long-term average onto
	// the target (libopus vbr_reservoir/vbr_drift/vbr_offset, 1/8-bit units).
	vbrReservoir int
	vbrDrift     int
	vbrOffset    int
	vbrCount     int

	// Per-frame scratch.
	window  []float64
	mdctScr *mdctScratch
	mdctPlanCache
	iy []int
	u  []uint32
}

func newCELTEncoder(channels int) *celtEncoder {
	e := &celtEncoder{
		channels:       channels,
		overlap:        celtOverlap,
		complexity:     5,
		oldBandE:       make([]float32, channels*celtNBands),
		oldLogE:        make([]float32, channels*celtNBands),
		oldLogE2:       make([]float32, channels*celtNBands),
		energyError:    make([]float32, channels*celtNBands),
		spreadDecision: spreadNormal,
		tonalAverage:   256,
		window:         celtWindow(celtOverlap),
		mdctScr:        newMDCTScratch(480),
		iy:             make([]int, (celtShortMDCTSize<<celtMaxLM)+3),
		u:              make([]uint32, celtMaxPulses+2),
	}
	e.preHistory = make([][]float32, channels)
	e.prefilterMem = make([][]float32, channels)
	for c := range e.preHistory {
		e.preHistory[c] = make([]float32, celtOverlap)
		e.prefilterMem[c] = make([]float32, combMaxPeriod)
	}
	for i := range e.oldLogE {
		e.oldLogE[i] = -28
		e.oldLogE2[i] = -28
	}
	return e
}

// Reset clears the inter-frame state (matching the decoder's OPUS_RESET_STATE
// parity: energies to -28, everything else zeroed).
func (e *celtEncoder) Reset() {
	e.preemphMem = [2]float32{}
	for c := range e.preHistory {
		clear(e.preHistory[c])
		clear(e.prefilterMem[c])
	}
	e.prefilterPeriod = 0
	e.prefilterGain = 0
	e.prefilterTapset = 0
	clear(e.oldBandE)
	clear(e.energyError)
	for i := range e.oldLogE {
		e.oldLogE[i] = -28
		e.oldLogE2[i] = -28
	}
	e.delayedIntra = 0
	e.consecTransient = 0
	e.spreadDecision = spreadNormal
	e.tonalAverage = 256
	e.hfAverage = 0
	e.tapsetDecision = 0
	e.intensity = 0
	e.stereoSaving = 0
	e.specAvg = 0
	e.lastCodedBands = 0
	e.rng = 0
	e.vbrReservoir = 0
	e.vbrDrift = 0
	e.vbrOffset = 0
	e.vbrCount = 0
}

// celtPreemphasis applies the CELT pre-emphasis filter to one channel of input
// [-1,1] PCM, scaling to the internal signal domain (libopus celt_preemphasis,
// float 48 kHz fast path). It is the exact inverse of the decoder's deemphasis.
func (e *celtEncoder) celtPreemphasis(pcm, inp []float32, N int, mem *float32) {
	coef0 := float32(celtPreemph)
	m := *mem
	for i := 0; i < N; i++ {
		x := pcm[i] * celtSigScale
		inp[i] = x - m
		m = coef0 * x
	}
	*mem = m
}

// computeMDCTs runs the forward MDCT of every coded channel into freq (C*N,
// short blocks interleaved with stride B), libopus compute_mdcts.
func (e *celtEncoder) computeMDCTs(shortBlocks int, in [][]float32, freq []float32, C, LM int) {
	var B, N int
	if shortBlocks != 0 {
		B = shortBlocks
		N = celtShortMDCTSize
	} else {
		B = 1
		N = celtShortMDCTSize << LM
	}
	plan := e.planFor(2 * N)
	frameN := celtShortMDCTSize << LM
	for c := 0; c < C; c++ {
		for b := 0; b < B; b++ {
			plan.forward(in[c][b*N:], freq[c*frameN+b:], B, e.window, e.overlap, e.mdctScr)
		}
	}
}

// tfEncode codes the per-band time-frequency change flags and tf_select, then
// rewrites tfRes to the resolved resolution the band coder uses (libopus
// tf_encode, the inverse of tfDecode).
func tfEncode(start, end, isTransient int, tfRes []int, LM, tfSelect int, e *rangeEncoder) {
	budget := e.storage * 8
	tell := e.tell()
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
			e.encodeBitLogp(tfRes[i]^curr, uint(logp))
			tell = e.tell()
			curr = tfRes[i]
			tfChanged |= curr
		} else {
			tfRes[i] = curr
		}
		if isTransient != 0 {
			logp = 4
		} else {
			logp = 5
		}
	}
	if tfSelectRsv != 0 &&
		tfSelectTable[LM][4*isTransient+tfChanged] != tfSelectTable[LM][4*isTransient+2+tfChanged] {
		e.encodeBitLogp(tfSelect, 1)
	} else {
		tfSelect = 0
	}
	for i := start; i < end; i++ {
		tfRes[i] = int(tfSelectTable[LM][4*isTransient+2*tfSelect+tfRes[i]])
	}
}

// celtEncode encodes one CELT-only frame. pcm holds C channels of N samples of
// 48 kHz float PCM in [-1,1]; start/end are the coded band range; nbBytes is the
// CELT payload size: the fixed size in CBR, or the buffer maximum in VBR (the
// frame is then shrunk to its computed size). It returns the range-coded payload,
// whose length equals nbBytes in CBR and is <= nbBytes in VBR.
func (e *celtEncoder) celtEncode(pcm [][]float32, N, LM, C, start, end, nbBytes int) []byte {
	M := 1 << LM
	overlap := e.overlap
	nb := celtNBands
	effEnd := min(end, nb)

	// Rate control. In CBR, nbCompressedBytes is the fixed payload size and
	// effectiveBytes equals it. In VBR, the buffer starts at the passed maximum
	// (nbBytes), effectiveBytes tracks the target rate, and the frame is shrunk
	// to its computed size after the analysis (libopus vbr_rate/effectiveBytes).
	vbrRate := 0
	effectiveBytes := nbBytes
	if e.vbr && e.bitrate > 0 {
		vbrRate = (e.bitrate / 50) << bitRes
		effectiveBytes = vbrRate >> (3 + bitRes)
	}
	nbCompressedBytes := nbBytes
	totalBits := nbCompressedBytes * 8

	// Equivalent bitrate for the frame, clamped to the configured target. Feeds
	// the intensity-stereo and allocation-trim decisions (libopus equiv_rate).
	equivRate := nbCompressedBytes*8*50<<(3-LM) - (40*C+20)*((400>>LM)-50)
	if e.bitrate > 0 {
		equivRate = min(equivRate, e.bitrate-(40*C+20)*((400>>LM)-50))
	}

	buf := make([]byte, nbCompressedBytes)
	enc := newRangeEncoder(buf)

	// Constrained-VBR bust prevention: cap this frame's buffer so the
	// short-term rate can never violate the reservoir bound, before any
	// analysis reads the available bytes (libopus celt_encode_with_ec).
	if vbrRate > 0 && e.constrainedVBR {
		vbrBound := vbrRate
		maxAllowed := min(max(2, (vbrRate+vbrBound-e.vbrReservoir)>>(bitRes+3)), nbCompressedBytes)
		if maxAllowed < nbCompressedBytes {
			nbCompressedBytes = maxAllowed
			totalBits = nbCompressedBytes * 8
			enc.shrink(nbCompressedBytes)
		}
	}

	// Pre-emphasis into per-channel [history(overlap) | new N] buffers. The
	// history starts as the unfiltered pre-filter memory tail so the tone and
	// transient analyses see a continuous unfiltered signal; runPrefilter
	// swaps in the filtered tail the MDCT windows need.
	in := make([][]float32, C)
	var sampleMax float32
	for c := 0; c < C; c++ {
		in[c] = make([]float32, N+overlap)
		copy(in[c][:overlap], e.prefilterMem[c][combMaxPeriod-overlap:])
		e.celtPreemphasis(pcm[c], in[c][overlap:], N, &e.preemphMem[c])
		for i := 0; i < N; i++ {
			if a := absf(pcm[c][i]); a > sampleMax {
				sampleMax = a
			}
		}
	}

	// Silence flag (tell==1 at the start of a CELT-only frame).
	silence := 0
	if sampleMax <= 1.0/(1<<24) {
		silence = 1
	}
	enc.encodeBitLogp(silence, 15)
	if silence != 0 {
		tell := nbCompressedBytes * 8
		enc.nbits += tell - enc.tell()
	}

	// Tone detection guards the pitch estimator, the transient detector, and
	// the dynalloc boost against pure tones.
	var toneishness float32
	toneFreq := toneDetect(in, C, N+overlap, &toneishness)

	// Transient analysis (complexity >= 1).
	isTransient := 0
	shortBlocks := 0
	var tfEstimate float32
	tfChan := 0
	if silence == 0 {
		isTransient = transientAnalysis(in, N+overlap, C, &tfEstimate, &tfChan, toneFreq, toneishness)
	}
	toneishness = min(toneishness, 1-tfEstimate)

	// Pitch pre-filter: search, decide, apply, and code the post-filter
	// parameters the decoder reads back (the filter pair is exactly inverse).
	nbAvailableBytes := nbCompressedBytes
	{
		enabled := silence == 0 && nbAvailableBytes > 12*C && enc.tell()+16 <= totalBits
		prefilterTapset := e.tapsetDecision
		pfOn, pitchIndex, gain1, qg := e.runPrefilter(in, C, N, prefilterTapset,
			enabled, tfEstimate, nbAvailableBytes, toneFreq, toneishness)
		if pfOn == 0 {
			if enc.tell()+16 <= totalBits {
				enc.encodeBitLogp(0, 1)
			}
		} else {
			enc.encodeBitLogp(1, 1)
			pitchIndex++
			octave := ilog(uint32(pitchIndex)) - 5
			enc.encodeUint(uint32(octave), 6)
			enc.encodeRawBits(uint32(pitchIndex-(16<<octave)), uint(4+octave))
			pitchIndex--
			enc.encodeRawBits(uint32(qg), 3)
			enc.encodeICDF(prefilterTapset, celtTapsetICDF, 2)
		}
		e.prefilterPeriod = pitchIndex
		e.prefilterGain = gain1
		e.prefilterTapset = prefilterTapset
	}

	if LM > 0 && enc.tell()+3 <= totalBits {
		if isTransient != 0 {
			shortBlocks = M
		}
	} else {
		isTransient = 0
	}

	// Forward MDCT, band energies, log energies. bandLogE2 is the per-frame log
	// energy dynalloc analyses; on a transient frame at complexity >= 8 it is
	// recomputed from a second long-block MDCT (better frequency resolution),
	// otherwise it is a copy of bandLogE.
	freq := make([]float32, C*N)
	bandE := make([]float32, nb*C)
	bandLogE := make([]float32, nb*C)
	bandLogE2 := make([]float32, C*nb)
	secondMdct := shortBlocks != 0 && e.complexity >= 8
	if secondMdct {
		e.computeMDCTs(0, in, freq, C, LM)
		computeBandEnergies(freq, bandE, effEnd, C, LM)
		amp2Log2(effEnd, end, bandE, bandLogE2, C)
		for c := 0; c < C; c++ {
			for i := 0; i < end; i++ {
				bandLogE2[nb*c+i] += 0.5 * float32(LM)
			}
		}
	}
	e.computeMDCTs(shortBlocks, in, freq, C, LM)
	computeBandEnergies(freq, bandE, effEnd, C, LM)
	amp2Log2(effEnd, end, bandE, bandLogE, C)
	if !secondMdct {
		copy(bandLogE2, bandLogE)
	}

	// Temporal VBR: how much this frame's peak-tracking band energy exceeds the
	// running spectral average, used to nudge the VBR target. spec_avg is updated
	// every frame regardless of the rate-control mode (libopus temporal_vbr).
	var temporalVBR float32
	{
		follow := float32(-10)
		var frameAvg, offset float32
		if shortBlocks != 0 {
			offset = 0.5 * float32(LM)
		}
		for i := start; i < end; i++ {
			follow = max(follow-1, bandLogE[i]-offset)
			if C == 2 {
				follow = max(follow, bandLogE[i+nb]-offset)
			}
			frameAvg += follow
		}
		frameAvg /= float32(end - start)
		temporalVBR = min(3.0, max(-1.5, frameAvg-e.specAvg))
		e.specAvg += 0.02 * temporalVBR
	}

	// Last chance to catch a transient the time-domain analysis missed
	// (complexity >= 5): if found, redo the MDCT in short blocks.
	if LM > 0 && enc.tell()+3 <= totalBits && isTransient == 0 && e.complexity >= 5 {
		if patchTransientDecision(bandLogE, e.oldBandE, start, end, C) {
			isTransient = 1
			shortBlocks = M
			e.computeMDCTs(shortBlocks, in, freq, C, LM)
			computeBandEnergies(freq, bandE, effEnd, C, LM)
			amp2Log2(effEnd, end, bandE, bandLogE, C)
			for c := 0; c < C; c++ {
				for i := 0; i < end; i++ {
					bandLogE2[nb*c+i] += 0.5 * float32(LM)
				}
			}
			tfEstimate = 0.2
		}
	}

	if LM > 0 && enc.tell()+3 <= totalBits {
		enc.encodeBitLogp(isTransient, 3)
	}

	// Normalise bands to unit-norm shapes.
	X := make([]float32, C*N)
	normaliseBands(freq, X, bandE, effEnd, C, M)

	// Dynamic allocation boosts plus the importance/spread-weight side products
	// and maxDepth, computed before the energy bias from the previous frame's
	// quantized energies (libopus order).
	offsets := make([]int, nb)
	importance := make([]int, nb)
	spreadWeight := make([]int, nb)
	totBoost := 0
	maxDepth := dynallocOffsets(bandLogE, bandLogE2, e.oldBandE, start, end, C, LM, effectiveBytes, isTransient,
		e.vbr, e.constrainedVBR, offsets, importance, spreadWeight, &totBoost, toneFreq, toneishness)

	// Time-frequency resolution analysis (complexity >= 2), consuming importance
	// and producing per-band tf_res plus tf_select. Below the threshold or at low
	// complexity, resolution follows the transient flag (libopus fallback).
	tfRes := make([]int, nb)
	tfSelect := 0
	if effectiveBytes >= 15*C && e.complexity >= 2 {
		lambda := max(80, 20480/effectiveBytes+2)
		tfSelect = tfAnalysis(effEnd, isTransient, tfRes, lambda, X, N, LM, tfEstimate, tfChan, importance)
		for i := effEnd; i < end; i++ {
			tfRes[i] = tfRes[effEnd-1]
		}
	} else {
		for i := 0; i < end; i++ {
			tfRes[i] = isTransient
		}
	}

	// Stabilize energy by biasing toward the previous frame's error.
	for c := 0; c < C; c++ {
		for i := start; i < end; i++ {
			if absf(bandLogE[i+c*nb]-e.oldBandE[i+c*nb]) < 2 {
				bandLogE[i+c*nb] -= 0.25 * e.energyError[i+c*nb]
			}
		}
	}

	errorArr := make([]float32, C*nb)
	quantCoarseEnergy(start, end, effEnd, bandLogE, e.oldBandE, totalBits, errorArr, enc,
		C, LM, nbCompressedBytes, false, &e.delayedIntra, e.complexity >= 4)

	tfEncode(start, end, isTransient, tfRes, LM, tfSelect, enc)

	// Spread decision (PVQ rotation amount). At low complexity/bitrate or on
	// short blocks libopus forces NORMAL/NONE; otherwise spreadingDecision drives
	// it from the coefficient CDF. e.spreadDecision persists as the hysteresis
	// anchor for the next frame.
	spread := spreadNormal
	if enc.tell()+4 <= totalBits {
		if shortBlocks != 0 || e.complexity < 3 || nbCompressedBytes < 10*C {
			if e.complexity == 0 {
				spread = spreadNone
			}
		} else {
			spread = spreadingDecision(X, &e.tonalAverage, e.spreadDecision,
				&e.hfAverage, &e.tapsetDecision, false, effEnd, C, M, spreadWeight)
		}
		e.spreadDecision = spread
		enc.encodeICDF(spread, celtSpreadICDF, 5)
	} else {
		e.spreadDecision = spreadNormal
	}

	// Caps and dynamic allocation coding (offsets computed by dynalloc above).
	caps := make([]int, nb)
	initCaps(caps, LM, C)
	dynallocLogp := 6
	totalBitsFrac := totalBits << bitRes
	totalBoost := 0
	tellF := enc.tellFrac()
	for i := start; i < end; i++ {
		width := C * (int(celtEBands[i+1]) - int(celtEBands[i])) << LM
		quanta := min(width<<bitRes, max(6<<bitRes, width))
		loopLogp := dynallocLogp
		boost := 0
		j := 0
		for tellF+(loopLogp<<bitRes) < totalBitsFrac-totalBoost && boost < caps[i] {
			flag := 0
			if j < offsets[i] {
				flag = 1
			}
			enc.encodeBitLogp(flag, uint(loopLogp))
			tellF = enc.tellFrac()
			if flag == 0 {
				break
			}
			boost += quanta
			totalBoost += quanta
			loopLogp = 1
			j++
		}
		if j != 0 {
			dynallocLogp = max(2, dynallocLogp-1)
		}
		offsets[i] = boost
	}

	// Stereo decisions: dual-stereo (per-band L/R vs M/S) from stereo_analysis,
	// and the intensity-stereo boundary from a rate-driven hysteresis decision.
	dualStereo := 0
	if C == 2 {
		if LM != 0 {
			dualStereo = stereoAnalysis(X, LM, N)
		}
		e.intensity = hysteresisDecision(float32(equivRate/1000),
			celtIntensityThresholds, celtIntensityHysteresis, 21, e.intensity)
		e.intensity = min(end, max(start, e.intensity))
	}

	// Allocation trim (tilts allocation toward low or high frequencies) and the
	// stereo-saving estimate the VBR rate control reads.
	allocTrim := 5
	if tellF+(6<<bitRes) <= totalBitsFrac-totalBoost {
		allocTrim = allocTrimAnalysis(X, bandLogE, end, LM, C, N, tfEstimate, e.intensity, equivRate, &e.stereoSaving)
		enc.encodeICDF(allocTrim, celtTrimICDF, 7)
		tellF = enc.tellFrac()
	}

	// Variable bitrate: size the frame from the analysis, then shrink the range
	// coder to the chosen size (libopus compute_vbr + the VBR target loop). The
	// 2-byte margin keeps the decoder's bust-prevention from ever triggering.
	// In constrained VBR the reservoir tracks short-term overshoot (bounded by
	// the up-front bust prevention) and the drift loop steers the long-term
	// average onto the target.
	if vbrRate > 0 {
		lmDiff := celtMaxLM - LM
		minAllowed := ((tellF + totalBoost + (1 << (bitRes + 3)) - 1) >> (bitRes + 3)) + 2
		nbCompressedBytes = min(nbCompressedBytes, maxFrameBytes) // packet_size_cap (LM==3: >>0)
		baseTarget := vbrRate - ((40*C + 20) << bitRes)
		if e.constrainedVBR {
			baseTarget += e.vbrOffset >> lmDiff
		}
		target := computeVBR(baseTarget, LM, e.bitrate, e.lastCodedBands, C, e.intensity,
			e.constrainedVBR, e.stereoSaving, totBoost, tfEstimate, maxDepth, temporalVBR)
		target += tellF
		nbAvailableBytes := (target + (1 << (bitRes + 2))) >> (bitRes + 3)
		nbAvailableBytes = max(minAllowed, nbAvailableBytes)
		nbAvailableBytes = min(nbCompressedBytes, nbAvailableBytes)
		// By how much did we "miss" the target on this frame.
		delta := target - vbrRate
		target = nbAvailableBytes << (bitRes + 3)
		// A silent frame doesn't adjust the drift (or the encoder would shoot
		// to very high rates after a span of silence), but the reservoir
		// still refills.
		if silence != 0 {
			nbAvailableBytes = 2
			target = 2 * 8 << bitRes
			delta = 0
		}
		var alpha float32
		if e.vbrCount < 970 {
			e.vbrCount++
			alpha = 1 / float32(e.vbrCount+20)
		} else {
			alpha = 0.001
		}
		if e.constrainedVBR {
			// How many bits we used in excess of what we're allowed.
			e.vbrReservoir += target - vbrRate
			// The offset needed to reach the target on average.
			e.vbrDrift += int(alpha * float32((delta<<lmDiff)-e.vbrOffset-e.vbrDrift))
			e.vbrOffset = -e.vbrDrift
			if e.vbrReservoir < 0 {
				// Under the min value: increase the rate, unless just coding
				// silence.
				adjust := -e.vbrReservoir / (8 << bitRes)
				if silence == 0 {
					nbAvailableBytes += adjust
				}
				e.vbrReservoir = 0
			}
		}
		nbCompressedBytes = min(nbCompressedBytes, nbAvailableBytes)
		enc.shrink(nbCompressedBytes)
	}
	totalBits = nbCompressedBytes * 8

	// Bit allocation.
	bits := (nbCompressedBytes*8)<<bitRes - enc.tellFrac() - 1
	antiCollapseRsv := 0
	if isTransient != 0 && LM >= 2 && bits >= (LM+2)<<bitRes {
		antiCollapseRsv = 1 << bitRes
	}
	bits -= antiCollapseRsv

	pulses := make([]int, nb)
	fineQuant := make([]int, nb)
	finePriority := make([]int, nb)
	balance := 0
	signalBandwidth := end - 1
	codedBands := cltComputeAllocation(start, end, offsets, caps, allocTrim, &e.intensity, &dualStereo,
		bits, &balance, pulses, fineQuant, finePriority, C, LM, enc, nil, true, e.lastCodedBands, signalBandwidth)
	if e.lastCodedBands != 0 {
		e.lastCodedBands = min(e.lastCodedBands+1, max(e.lastCodedBands-1, codedBands))
	} else {
		e.lastCodedBands = codedBands
	}

	quantFineEnergy(start, end, e.oldBandE, errorArr, fineQuant, enc, C)
	clear(e.energyError)

	// Residual (shape) quantization.
	collapseMasks := make([]byte, C*nb)
	var Y []float32
	if C == 2 {
		Y = X[N:]
	}
	quantAllBands(start, end, X, Y, collapseMasks, bandE, pulses, shortBlocks, spread,
		dualStereo, e.intensity, tfRes, nbCompressedBytes*(8<<bitRes)-antiCollapseRsv, balance, enc, nil,
		LM, codedBands, &e.rng, e.complexity, b2i(C == 1), e.iy, e.u)

	antiCollapseOn := 0
	if antiCollapseRsv > 0 {
		if e.consecTransient < 2 {
			antiCollapseOn = 1
		}
		enc.encodeRawBits(uint32(antiCollapseOn), 1)
	}
	quantEnergyFinalise(start, end, e.oldBandE, errorArr, fineQuant, finePriority,
		nbCompressedBytes*8-enc.tell(), enc, C)

	for c := 0; c < C; c++ {
		for i := start; i < end; i++ {
			e.energyError[i+c*nb] = max(-0.5, min(0.5, errorArr[i+c*nb]))
		}
	}
	if silence != 0 {
		for i := 0; i < C*nb; i++ {
			e.oldBandE[i] = -28
		}
	}

	e.updateEnergyState(C, isTransient, start, end)
	e.rng = enc.rng

	// Carry the tail of this frame's prefiltered signal as the next frame's
	// MDCT overlap history (libopus st->in_mem).
	for c := 0; c < C; c++ {
		copy(e.preHistory[c], in[c][N:N+overlap])
	}

	enc.done()
	return enc.payload()
}

// updateEnergyState carries the quantized log energies forward for the next
// frame's prediction (libopus celt_encode_with_ec tail; the decoder's
// backgroundLogE is PLC-only and not tracked here).
func (e *celtEncoder) updateEnergyState(C, isTransient, start, end int) {
	nb := celtNBands
	if isTransient == 0 {
		copy(e.oldLogE2, e.oldLogE[:C*nb])
		copy(e.oldLogE, e.oldBandE[:C*nb])
	} else {
		for i := 0; i < C*nb; i++ {
			e.oldLogE[i] = min(e.oldLogE[i], e.oldBandE[i])
		}
	}
	for c := 0; c < C; c++ {
		for i := 0; i < start; i++ {
			e.oldBandE[c*nb+i] = 0
			e.oldLogE[c*nb+i] = -28
			e.oldLogE2[c*nb+i] = -28
		}
		for i := end; i < nb; i++ {
			e.oldBandE[c*nb+i] = 0
			e.oldLogE[c*nb+i] = -28
			e.oldLogE2[c*nb+i] = -28
		}
	}
	if isTransient != 0 {
		e.consecTransient++
	} else {
		e.consecTransient = 0
	}
}
