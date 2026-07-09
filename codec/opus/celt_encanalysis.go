package opus

import "math"

// CELT encoder analysis stages: transient detection, time-frequency resolution,
// spreading, allocation trim, dynamic allocation, and the stereo decisions.
// Clean-room ports of libopus celt_encoder.c (float build), which drive the
// side-information the decoder reads back. The tone-detection inputs come from
// toneDetect (celt_encpitch.go); the weak-transient refinement is a hybrid
// low-rate feature out of scope for CELT music mode, so its input stays at the
// neutral value that disables it.

// transientInvTable is 6*64/x, trained to minimize average error (libopus).
var transientInvTable = [128]byte{
	255, 255, 156, 110, 86, 70, 59, 51, 45, 40, 37, 33, 31, 28, 26, 25,
	23, 22, 21, 20, 19, 18, 17, 16, 16, 15, 15, 14, 13, 13, 12, 12,
	12, 12, 11, 11, 11, 10, 10, 10, 9, 9, 9, 9, 9, 9, 8, 8,
	8, 8, 8, 7, 7, 7, 7, 7, 7, 6, 6, 6, 6, 6, 6, 6,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 5, 5, 5, 5, 5, 5, 5,
	5, 5, 5, 5, 5, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4,
	4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 3, 3,
	3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 2,
}

// transientAnalysis decides whether a frame uses short blocks (returns 1) and
// reports tfEstimate (VBR/tf_analysis input) and the channel that drove the
// decision. in holds the pre-emphasized signal per channel, each length n. The
// metric is scale-invariant. Clean-room port of libopus transient_analysis.
// toneFreq/toneishness (tone_detect) keep the partial cycle of a very low
// frequency tone from reading as a transient.
func transientAnalysis(in [][]float32, n, C int, tfEstimate *float32, tfChan *int, toneFreq, toneishness float32) int {
	const forwardDecay = float32(0.0625)
	len2 := n / 2
	tmp := make([]float32, n)
	maskMetric := 0
	*tfChan = 0
	for c := 0; c < C; c++ {
		var mem0, mem1 float32
		// High-pass filter: (1 - 2z^-1 + z^-2) / (1 - z^-1 + .5z^-2).
		for i := 0; i < n; i++ {
			x := in[c][i]
			y := mem0 + x
			mem00 := mem0
			mem0 = mem0 - x + 0.5*mem1
			mem1 = x - mem00
			tmp[i] = y
		}
		// The first few samples are unreliable without propagated memory.
		for i := 0; i < 12 && i < n; i++ {
			tmp[i] = 0
		}
		var mean float32
		mem0 = 0
		// Forward pass: post-echo threshold, grouped by two.
		for i := 0; i < len2; i++ {
			x2 := tmp[2*i]*tmp[2*i] + tmp[2*i+1]*tmp[2*i+1]
			mean += x2
			mem0 = x2 + (1-forwardDecay)*mem0
			tmp[i] = forwardDecay * mem0
		}
		mem0 = 0
		var maxE float32
		// Backward pass: pre-echo threshold (13.9 dB/ms).
		for i := len2 - 1; i >= 0; i-- {
			mem0 = tmp[i] + 0.875*mem0
			tmp[i] = 0.125 * mem0
			maxE = max(maxE, 0.125*mem0)
		}
		// Frame energy is the geometric mean of the energy and half the max.
		mean = float32(math.Sqrt(float64(mean) * float64(maxE) * 0.5 * float64(len2)))
		norm := float32(len2) / (1e-15 + mean)
		unmask := 0
		// Harmonic mean over 1/4 of the samples, discarding the boundaries.
		for i := 12; i < len2-5; i += 4 {
			id := int(math.Floor(64 * float64(norm) * float64(tmp[i]+1e-15)))
			if id < 0 {
				id = 0
			} else if id > 127 {
				id = 127
			}
			unmask += int(transientInvTable[id])
		}
		if len2 > 17 {
			unmask = 64 * unmask * 4 / (6 * (len2 - 17))
		}
		if unmask > maskMetric {
			*tfChan = c
			maskMetric = unmask
		}
	}
	isTransient := 0
	if maskMetric > 200 {
		isTransient = 1
	}
	// Prevent the transient detector from confusing the partial cycle of a
	// very low frequency tone with a transient.
	if toneishness > 0.98 && toneFreq < 0.026 {
		isTransient = 0
		maskMetric = 0
	}
	// Arbitrary metric for VBR boost / tf_analysis.
	tfMax := float32(math.Sqrt(27*float64(maskMetric))) - 42
	if tfMax < 0 {
		tfMax = 0
	}
	inner := 0.0069*float32(math.Min(163, float64(tfMax))) - 0.139
	if inner < 0 {
		inner = 0
	}
	*tfEstimate = float32(math.Sqrt(float64(inner)))
	return isTransient
}

func absf(x float32) float32 {
	if x < 0 {
		return -x
	}
	return x
}

// patchTransientDecision is the encoder's last chance to catch a transient the
// time-domain analysis missed: it aggressively spreads the previous frame's
// quantized energy (-6 dB/Bark, both directions) and reports whether the mean
// energy increase over it exceeds 1 dB. Clean-room port of libopus
// patch_transient_decision (float build).
func patchTransientDecision(newE, oldE []float32, start, end, C int) bool {
	nb := celtNBands
	spreadOld := make([]float32, 26)
	if C == 1 {
		spreadOld[start] = oldE[start]
		for i := start + 1; i < end; i++ {
			spreadOld[i] = max(spreadOld[i-1]-1.0, oldE[i])
		}
	} else {
		spreadOld[start] = max(oldE[start], oldE[start+nb])
		for i := start + 1; i < end; i++ {
			spreadOld[i] = max(spreadOld[i-1]-1.0, max(oldE[i], oldE[i+nb]))
		}
	}
	for i := end - 2; i >= start; i-- {
		spreadOld[i] = max(spreadOld[i], spreadOld[i+1]-1.0)
	}
	i0 := max(2, start)
	var meanDiff float32
	for c := 0; c < C; c++ {
		for i := i0; i < end-1; i++ {
			x1 := max(0, newE[i+c*nb])
			x2 := max(0, spreadOld[i])
			meanDiff += max(0, x1-x2)
		}
	}
	meanDiff /= float32(C * (end - 1 - i0))
	return meanDiff > 1.0
}

// l1Metric is the L1 norm of a band, biased toward good frequency resolution
// (libopus l1_metric, float build). The bias grows with LM so the Viterbi in
// tfAnalysis prefers coarser time resolution when in doubt.
func l1Metric(tmp []float32, N, LM int, bias float32) float32 {
	var L1 float32
	for i := 0; i < N; i++ {
		L1 += absf(tmp[i])
	}
	return L1 + (float32(LM)*bias)*L1
}

// tfAnalysis chooses each band's time-frequency resolution and the tf_select
// index by minimising an L1-metric Viterbi cost weighted by importance[].
// Clean-room port of libopus tf_analysis (float build); X is the normalised MDCT
// (channel tfChan of N0 samples), effEnd the last analysed band. It writes tfRes
// (per band) and returns tf_select, both consumed by tfEncode.
func tfAnalysis(effEnd, isTransient int, tfRes []int, lambda int, X []float32, N0, LM int, tfEstimate float32, tfChan int, importance []int) int {
	length := effEnd
	metric := make([]int, length)
	path0 := make([]int, length)
	path1 := make([]int, length)
	bias := 0.04 * max(-0.25, 0.5-tfEstimate)
	maxWidth := (int(celtEBands[length]) - int(celtEBands[length-1])) << LM
	tmp := make([]float32, maxWidth)
	tmp1 := make([]float32, maxWidth)

	for i := 0; i < length; i++ {
		N := (int(celtEBands[i+1]) - int(celtEBands[i])) << LM
		narrow := (int(celtEBands[i+1]) - int(celtEBands[i])) == 1
		copy(tmp[:N], X[tfChan*N0+(int(celtEBands[i])<<LM):tfChan*N0+(int(celtEBands[i])<<LM)+N])
		lm0 := 0
		if isTransient != 0 {
			lm0 = LM
		}
		L1 := l1Metric(tmp[:N], N, lm0, bias)
		bestL1 := L1
		bestLevel := 0
		// Check the -1 (extra split) case for transients.
		if isTransient != 0 && !narrow {
			copy(tmp1[:N], tmp[:N])
			haar1(tmp1[:N], N>>LM, 1<<LM)
			L1 = l1Metric(tmp1[:N], N, LM+1, bias)
			if L1 < bestL1 {
				bestL1 = L1
				bestLevel = -1
			}
		}
		kmax := LM
		if isTransient == 0 && !narrow {
			kmax = LM + 1
		}
		for k := 0; k < kmax; k++ {
			var B int
			if isTransient != 0 {
				B = LM - k - 1
			} else {
				B = k + 1
			}
			haar1(tmp[:N], N>>uint(k), 1<<uint(k))
			L1 = l1Metric(tmp[:N], N, B, bias)
			if L1 < bestL1 {
				bestL1 = L1
				bestLevel = k + 1
			}
		}
		// metric is in Q1 so narrow bands can pick the -0.5 mid-point.
		if isTransient != 0 {
			metric[i] = 2 * bestLevel
		} else {
			metric[i] = -2 * bestLevel
		}
		if narrow && (metric[i] == 0 || metric[i] == -2*LM) {
			metric[i] -= 1
		}
	}

	// Search for the optimal tf resolution, including tf_select.
	sc := func(sel, j int) int { return 2 * int(tfSelectTable[LM][4*isTransient+2*sel+j]) }
	var selcost [2]int
	transientLambda := func() int {
		if isTransient != 0 {
			return 0
		}
		return lambda
	}
	for sel := 0; sel < 2; sel++ {
		cost0 := importance[0] * iabs(metric[0]-sc(sel, 0))
		cost1 := importance[0]*iabs(metric[0]-sc(sel, 1)) + transientLambda()
		for i := 1; i < length; i++ {
			curr0 := min(cost0, cost1+lambda)
			curr1 := min(cost0+lambda, cost1)
			cost0 = curr0 + importance[i]*iabs(metric[i]-sc(sel, 0))
			cost1 = curr1 + importance[i]*iabs(metric[i]-sc(sel, 1))
		}
		selcost[sel] = min(cost0, cost1)
	}
	// Conservatively only allow tf_select=1 for transients.
	tfSelect := 0
	if selcost[1] < selcost[0] && isTransient != 0 {
		tfSelect = 1
	}
	cost0 := importance[0] * iabs(metric[0]-sc(tfSelect, 0))
	cost1 := importance[0]*iabs(metric[0]-sc(tfSelect, 1)) + transientLambda()
	// Viterbi forward pass.
	for i := 1; i < length; i++ {
		from0 := cost0
		from1 := cost1 + lambda
		var curr0 int
		if from0 < from1 {
			curr0 = from0
			path0[i] = 0
		} else {
			curr0 = from1
			path0[i] = 1
		}
		from0 = cost0 + lambda
		from1 = cost1
		var curr1 int
		if from0 < from1 {
			curr1 = from0
			path1[i] = 0
		} else {
			curr1 = from1
			path1[i] = 1
		}
		cost0 = curr0 + importance[i]*iabs(metric[i]-sc(tfSelect, 0))
		cost1 = curr1 + importance[i]*iabs(metric[i]-sc(tfSelect, 1))
	}
	if cost0 < cost1 {
		tfRes[length-1] = 0
	} else {
		tfRes[length-1] = 1
	}
	// Viterbi backward pass to confirm the decisions.
	for i := length - 2; i >= 0; i-- {
		if tfRes[i+1] == 1 {
			tfRes[i] = path1[i+1]
		} else {
			tfRes[i] = path0[i+1]
		}
	}
	return tfSelect
}

// allocTrimAnalysis picks the allocation trim (0..10), which tilts the bit
// allocation toward low or high frequencies. Clean-room port of libopus
// alloc_trim_analysis (float build): the base level tracks equiv_rate, then the
// spectral tilt, transient estimate, and (stereo) inter-channel correlation
// nudge it. The stereo-saving output feeds VBR only and is not tracked here;
// surround trim and the tonality analysis are out of scope.
func allocTrimAnalysis(X, bandLogE []float32, end, LM, C, N0 int, tfEstimate float32, intensity, equivRate int, stereoSaving *float32) int {
	trim := float32(5.0)
	if equivRate < 64000 {
		trim = 4.0
	} else if equivRate < 80000 {
		trim = 4.0 + (1.0/16.0)*float32((equivRate-64000)>>10)
	}
	if C == 2 {
		var sum float32
		for i := 0; i < 8; i++ {
			lo := int(celtEBands[i]) << LM
			w := (int(celtEBands[i+1]) - int(celtEBands[i])) << LM
			sum += innerProd(X[lo:], X[N0+lo:], w)
		}
		sum = min(1.0, absf(sum*(1.0/8.0)))
		minXC := sum
		for i := 8; i < intensity; i++ {
			lo := int(celtEBands[i]) << LM
			w := (int(celtEBands[i+1]) - int(celtEBands[i])) << LM
			minXC = min(minXC, absf(innerProd(X[lo:], X[N0+lo:], w)))
		}
		minXC = min(1.0, absf(minXC))
		// mid-side savings estimates: logXC from the LF average, logXC2 from the
		// minimum correlation. stereoSaving persists as the VBR rate-control input.
		logXC := float32(math.Log2(float64(1.001 - sum*sum)))
		logXC2 := max(0.5*logXC, float32(math.Log2(float64(1.001-minXC*minXC))))
		trim += max(-4.0, 0.75*logXC)
		*stereoSaving = min(*stereoSaving+0.25, -0.5*logXC2)
	}
	// Spectral tilt.
	var diff float32
	for c := 0; c < C; c++ {
		for i := 0; i < end-1; i++ {
			diff += bandLogE[i+c*celtNBands] * float32(2+2*i-end)
		}
	}
	diff /= float32(C * (end - 1))
	trim -= max(-2.0, min(2.0, (diff+1.0)/6.0))
	trim -= 2 * tfEstimate
	trimIndex := int(math.Floor(float64(0.5 + trim)))
	if trimIndex < 0 {
		trimIndex = 0
	} else if trimIndex > 10 {
		trimIndex = 10
	}
	return trimIndex
}

func innerProd(a, b []float32, n int) float32 {
	var s float32
	for i := 0; i < n; i++ {
		s += a[i] * b[i]
	}
	return s
}

func median3(x []float32) float32 {
	var t0, t1 float32
	if x[0] > x[1] {
		t0, t1 = x[1], x[0]
	} else {
		t0, t1 = x[0], x[1]
	}
	t2 := x[2]
	if t1 < t2 {
		return t1
	} else if t0 < t2 {
		return t2
	}
	return t0
}

func median5(x []float32) float32 {
	t2 := x[2]
	var t0, t1, t3, t4 float32
	if x[0] > x[1] {
		t0, t1 = x[1], x[0]
	} else {
		t0, t1 = x[0], x[1]
	}
	if x[3] > x[4] {
		t3, t4 = x[4], x[3]
	} else {
		t3, t4 = x[3], x[4]
	}
	if t0 > t3 {
		t0, t3 = t3, t0
		t1, t4 = t4, t1
	}
	if t2 > t1 {
		if t1 < t3 {
			return min(t2, t3)
		}
		return min(t4, t1)
	}
	if t2 < t3 {
		return min(t1, t3)
	}
	return min(t2, t4)
}

// hysteresisDecision maps a value to a threshold bucket, biased toward the
// previous decision by the per-bucket hysteresis so borderline frames don't
// toggle every packet (libopus hysteresis_decision). Used for the intensity and
// (disabled here) tapset decisions.
func hysteresisDecision(val float32, thresholds, hysteresis []float32, N, prev int) int {
	i := 0
	for ; i < N; i++ {
		if val < thresholds[i] {
			break
		}
	}
	if i > prev && val < thresholds[prev]+hysteresis[prev] {
		i = prev
	}
	if i < prev && val > thresholds[prev-1]-hysteresis[prev-1] {
		i = prev
	}
	return i
}

// spreadingDecision picks the PVQ rotation (spreading) amount for the frame from
// a rough CDF of the normalised coefficients, weighted by the masking-derived
// spreadWeight and smoothed with a running average plus hysteresis. Clean-room
// port of libopus spreading_decision (float build). updateHF (pf_on &&
// !shortBlocks) would also drive the tapset decision, but it is always false
// here since the pitch pre-filter is disabled.
func spreadingDecision(X []float32, average *int, lastDecision int, hfAverage, tapsetDecision *int, updateHF bool, end, C, M int, spreadWeight []int) int {
	N0 := M * celtShortMDCTSize
	if M*(int(celtEBands[end])-int(celtEBands[end-1])) <= 8 {
		return spreadNone
	}
	sum, nbBands, hfSum := 0, 0, 0
	for c := 0; c < C; c++ {
		for i := 0; i < end; i++ {
			N := M * (int(celtEBands[i+1]) - int(celtEBands[i]))
			if N <= 8 {
				continue
			}
			x := X[M*int(celtEBands[i])+c*N0:]
			var tcount [3]int
			for j := 0; j < N; j++ {
				x2N := x[j] * x[j] * float32(N)
				if x2N < 0.25 {
					tcount[0]++
				}
				if x2N < 0.0625 {
					tcount[1]++
				}
				if x2N < 0.015625 {
					tcount[2]++
				}
			}
			// Only include the four last bands (8 kHz and up) for the HF sum.
			if i > celtNBands-4 {
				hfSum += 32 * (tcount[1] + tcount[0]) / N
			}
			tmp := b2i(2*tcount[2] >= N) + b2i(2*tcount[1] >= N) + b2i(2*tcount[0] >= N)
			sum += tmp * spreadWeight[i]
			nbBands += spreadWeight[i]
		}
	}
	if updateHF {
		if hfSum != 0 {
			hfSum = hfSum / (C * (4 - celtNBands + end))
		}
		*hfAverage = (*hfAverage + hfSum) >> 1
		hfSum = *hfAverage
		if *tapsetDecision == 2 {
			hfSum += 4
		} else if *tapsetDecision == 0 {
			hfSum -= 4
		}
		if hfSum > 22 {
			*tapsetDecision = 2
		} else if hfSum > 18 {
			*tapsetDecision = 1
		} else {
			*tapsetDecision = 0
		}
	}
	sum = (sum << 8) / nbBands
	// Recursive averaging, then hysteresis around the previous decision.
	sum = (sum + *average) >> 1
	*average = sum
	sum = (3*sum + (((3 - lastDecision) << 7) + 64) + 2) >> 2
	switch {
	case sum < 80:
		return spreadAggr
	case sum < 256:
		return spreadNormal
	case sum < 384:
		return spreadLight
	default:
		return spreadNone
	}
}

// stereoAnalysis decides whether the frame codes bands as dual (independent
// L/R) stereo rather than mid/side, by comparing the L1 norm (an entropy proxy)
// of the L/R signal against the M/S signal over the low bands. Clean-room port
// of libopus stereo_analysis (float build). Returns 1 for dual stereo.
func stereoAnalysis(X []float32, LM, N0 int) int {
	sumLR := float32(1e-15)
	sumMS := float32(1e-15)
	for i := 0; i < 13; i++ {
		for j := int(celtEBands[i]) << LM; j < int(celtEBands[i+1])<<LM; j++ {
			L := X[j]
			R := X[N0+j]
			m := L + R
			s := L - R
			sumLR += absf(L) + absf(R)
			sumMS += absf(m) + absf(s)
		}
	}
	sumMS *= 0.707107
	thetas := 13
	// We don't need thetas for the lower bands with LM<=1.
	if LM <= 1 {
		thetas -= 8
	}
	lhs := float32(int(celtEBands[13])<<(LM+1)+thetas) * sumMS
	rhs := float32(int(celtEBands[13])<<(LM+1)) * sumLR
	return b2i(lhs > rhs)
}

// dynallocOffsets computes the per-band dynamic-allocation boosts (extra bits
// for spectral peaks the flat allocation under-serves, especially tones) plus
// the two side products the later analysis stages consume: importance[] (how
// much each band matters, feeding tf_analysis's Viterbi weights) and
// spreadWeight[] (a rough masking model feeding spreading_decision). It returns
// maxDepth (the peak band energy above the noise floor, feeding compute_vbr's
// floor) and writes tot_boost into *totBoost in 1/8-bit units. Clean-room port
// of libopus dynalloc_analysis (float build); the leak/surround/LFE inputs
// stay at the neutral values that disable them, while toneFreq/toneishness
// (toneDetect) compensate for the under-allocation on tones. bandLogE2 is the per-frame log
// energy (a copy of bandLogE unless the complexity>=8 second MDCT ran); offsets
// are consumed by the dynalloc coding loop, so a suboptimal value only costs
// quality, never a decodable stream.
func dynallocOffsets(bandLogE, bandLogE2, oldBandE []float32, start, end, C, LM, effectiveBytes, isTransient int, vbr, constrainedVBR bool, offsets, importance, spreadWeight []int, totBoost *int, toneFreq, toneishness float32) float32 {
	nb := celtNBands
	for i := 0; i < end; i++ {
		offsets[i] = 0
	}
	const lsbDepth = 24
	noiseFloor := make([]float32, end)
	for i := 0; i < end; i++ {
		noiseFloor[i] = 0.0625*float32(celtLogN[i]) + 0.5 + float32(9-lsbDepth) -
			celtEMeans[i] + 0.0062*float32((i+5)*(i+5))
	}
	maxDepth := float32(-31.9)
	for c := 0; c < C; c++ {
		for i := 0; i < end; i++ {
			maxDepth = max(maxDepth, bandLogE[c*nb+i]-noiseFloor[i])
		}
	}
	// A simple masking model so the spreading decision can ignore bands that are
	// fully masked. spreadWeight is computed even below the dynalloc threshold.
	mask := make([]float32, end)
	sig := make([]float32, end)
	for i := 0; i < end; i++ {
		mask[i] = bandLogE[i] - noiseFloor[i]
	}
	if C == 2 {
		for i := 0; i < end; i++ {
			mask[i] = max(mask[i], bandLogE[nb+i]-noiseFloor[i])
		}
	}
	copy(sig, mask)
	for i := 1; i < end; i++ {
		mask[i] = max(mask[i], mask[i-1]-2.0)
	}
	for i := end - 2; i >= 0; i-- {
		mask[i] = max(mask[i], mask[i+1]-3.0)
	}
	for i := 0; i < end; i++ {
		// SMR: never more than 72 dB below the peak, never below the noise floor.
		smr := sig[i] - max(max(0, maxDepth-12.0), mask[i])
		shift := int(-math.Floor(float64(0.5 + smr)))
		if shift < 0 {
			shift = 0
		} else if shift > 5 {
			shift = 5
		}
		spreadWeight[i] = 32 >> uint(shift)
	}

	if effectiveBytes < 30+5*LM {
		for i := start; i < end; i++ {
			importance[i] = 13
		}
		*totBoost = 0
		return maxDepth
	}

	follower := make([]float32, C*nb)
	bandLogE3 := make([]float32, end)
	last := 0
	for c := 0; c < C; c++ {
		copy(bandLogE3, bandLogE2[c*nb:c*nb+end])
		if LM == 0 {
			for i := 0; i < min(8, end); i++ {
				bandLogE3[i] = max(bandLogE2[c*nb+i], oldBandE[c*nb+i])
			}
		}
		f := follower[c*nb:]
		f[0] = bandLogE3[0]
		for i := 1; i < end; i++ {
			if bandLogE3[i] > bandLogE3[i-1]+0.5 {
				last = i
			}
			f[i] = min(f[i-1]+1.5, bandLogE3[i])
		}
		for i := last - 1; i >= 0; i-- {
			f[i] = min(f[i], min(f[i+1]+2.0, bandLogE3[i]))
		}
		const offset = float32(1.0)
		for i := 2; i < end-2; i++ {
			f[i] = max(f[i], median5(bandLogE3[i-2:])-offset)
		}
		tmp := median3(bandLogE3[0:]) - offset
		f[0] = max(f[0], tmp)
		f[1] = max(f[1], tmp)
		tmp = median3(bandLogE3[end-3:]) - offset
		f[end-2] = max(f[end-2], tmp)
		f[end-1] = max(f[end-1], tmp)
		for i := 0; i < end; i++ {
			f[i] = max(f[i], noiseFloor[i])
		}
	}
	if C == 2 {
		for i := start; i < end; i++ {
			follower[nb+i] = max(follower[nb+i], follower[i]-4.0)
			follower[i] = max(follower[i], follower[nb+i]-4.0)
			follower[i] = 0.5 * (max(0, bandLogE[i]-follower[i]) + max(0, bandLogE[nb+i]-follower[nb+i]))
		}
	} else {
		for i := start; i < end; i++ {
			follower[i] = max(0, bandLogE[i]-follower[i])
		}
	}
	// (surround_dynalloc is zero here.) importance is read from the SMR-domain
	// follower before the CBR halving and band weighting below.
	for i := start; i < end; i++ {
		importance[i] = int(math.Floor(float64(0.5 + 13*float32(math.Exp2(float64(min(follower[i], 4.0)))))))
	}
	// For non-transient CBR/constrained-VBR frames, halve the dynalloc contribution.
	if (!vbr || constrainedVBR) && isTransient == 0 {
		for i := start; i < end; i++ {
			follower[i] *= 0.5
		}
	}
	for i := start; i < end; i++ {
		if i < 8 {
			follower[i] *= 2
		}
		if i >= 12 {
			follower[i] *= 0.5
		}
	}
	// Compensate for Opus' under-allocation on tones.
	if toneishness > 0.98 {
		freqBin := int(math.Floor(0.5 + float64(toneFreq)*120/math.Pi))
		for i := start; i < end; i++ {
			if freqBin >= int(celtEBands[i]) && freqBin <= int(celtEBands[i+1]) {
				follower[i] += 2
			}
			if freqBin >= int(celtEBands[i])-1 && freqBin <= int(celtEBands[i+1])+1 {
				follower[i] += 1
			}
			if freqBin >= int(celtEBands[i])-2 && freqBin <= int(celtEBands[i+1])+2 {
				follower[i] += 1
			}
			if freqBin >= int(celtEBands[i])-3 && freqBin <= int(celtEBands[i+1])+3 {
				follower[i] += 0.5
			}
		}
		if freqBin >= int(celtEBands[end]) {
			follower[end-1] += 2
			follower[end-2] += 1
		}
	}
	boostTotal := 0
	for i := start; i < end; i++ {
		follower[i] = min(follower[i], 4)
		width := C * (int(celtEBands[i+1]) - int(celtEBands[i])) << LM
		var boost, boostBits int
		switch {
		case width < 6:
			boost = int(follower[i])
			boostBits = boost * width << bitRes
		case width > 48:
			boost = int(follower[i] * 8)
			boostBits = (boost * width << bitRes) / 8
		default:
			boost = int(follower[i] * float32(width) / 6)
			boostBits = boost * 6 << bitRes
		}
		// For CBR and non-transient constrained VBR, limit dynalloc to 2/3 of the bits.
		if (!vbr || (constrainedVBR && isTransient == 0)) &&
			(boostTotal+boostBits)>>bitRes>>3 > 2*effectiveBytes/3 {
			cap := (2 * effectiveBytes / 3) << bitRes << 3
			offsets[i] = cap - boostTotal
			boostTotal = cap
			break
		}
		offsets[i] = boost
		boostTotal += boostBits
	}
	*totBoost = boostTotal
	return maxDepth
}

// computeVBR derives the VBR target rate (in 1/8-bit units per frame) from the
// base target and the frame's analysis: stereo savings, dynalloc boost, the
// transient estimate, an energy floor (maxDepth), and a temporal-VBR nudge.
// Clean-room port of libopus compute_vbr (float build) for the CELT-music path:
// the tonality/activity/surround terms depend on the tone analyser and energy
// mask, which are out of scope, so their neutral (disabled) values apply.
func computeVBR(baseTarget, LM, bitrate, lastCodedBands, C, intensity int, constrainedVBR bool,
	stereoSaving float32, totBoost int, tfEstimate float32, maxDepth, temporalVBR float32) int {
	nb := celtNBands
	codedBands := lastCodedBands
	if codedBands == 0 {
		codedBands = nb
	}
	codedBins := int(celtEBands[codedBands]) << LM
	if C == 2 {
		codedBins += int(celtEBands[min(intensity, codedBands)]) << LM
	}
	target := baseTarget
	// Stereo savings: fewer bits when the channels are correlated.
	if C == 2 {
		codedStereoBands := min(intensity, codedBands)
		codedStereoDof := (int(celtEBands[codedStereoBands]) << LM) - codedStereoBands
		maxFrac := 0.8 * float32(codedStereoDof) / float32(codedBins)
		ss := min(stereoSaving, 1.0)
		sub := min(maxFrac*float32(target), (ss-0.1)*float32(codedStereoDof<<bitRes))
		target -= int(sub)
	}
	// Boost per dynalloc (minus the average, for calibration).
	target += totBoost - (19 << LM)
	// Transient boost (compensating for the average). SHL32 is a float no-op.
	const tfCalibration = 0.044
	target += int((tfEstimate - tfCalibration) * float32(target))
	// Energy floor so we never starve a loud frame.
	bins := int(celtEBands[nb-2]) << LM
	floorDepth := int(float32(C*bins<<bitRes) * maxDepth)
	floorDepth = max(floorDepth, target>>2)
	target = min(target, floorDepth)
	// Constrained VBR can't hold a higher rate for long, so damp the excursion.
	if constrainedVBR {
		target = baseTarget + int(0.67*float32(target-baseTarget))
	}
	// Temporal VBR (only when not surround-masked).
	if tfEstimate < 0.2 {
		amount := 0.0000031 * float32(max(0, min(32000, 96000-bitrate)))
		tvbrFactor := temporalVBR * amount
		target += int(tvbrFactor * float32(target))
	}
	// Never more than double the base target.
	return min(2*baseTarget, target)
}
