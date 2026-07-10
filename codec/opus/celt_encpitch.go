package opus

import "math"

// Encoder-side pitch analysis and the pitch pre-filter (the comb filter whose
// inverse is the decoder's post-filter). Clean-room ports of libopus pitch.c,
// celt_lpc.c, and the run_prefilter/tone_detect parts of celt_encoder.c
// (float build). The pre-filter attenuates the harmonic structure of pitched
// signals before the MDCT so coding noise stays shaped along the harmonics,
// and the decoder's post-filter restores it.

// toneLPC fits a 2-tap LPC filter with a least-squares fit over both forward
// and backward prediction (libopus tone_lpc, float build). It reports
// failure when the covariance system is near-singular.
func toneLPC(x []float32, length, delay int, lpc *[2]float32) bool {
	var r00, r01, r11, r02, r12, r22, edges float32
	for i := 0; i < length-2*delay; i++ {
		r00 += x[i] * x[i]
		r01 += x[i] * x[i+delay]
		r02 += x[i] * x[i+2*delay]
	}
	edges = 0
	for i := 0; i < delay; i++ {
		edges += x[length+i-2*delay]*x[length+i-2*delay] - x[i]*x[i]
	}
	r11 = r00 + edges
	edges = 0
	for i := 0; i < delay; i++ {
		edges += x[length+i-delay]*x[length+i-delay] - x[i+delay]*x[i+delay]
	}
	r22 = r11 + edges
	edges = 0
	for i := 0; i < delay; i++ {
		edges += x[length+i-2*delay]*x[length+i-delay] - x[i]*x[i+delay]
	}
	r12 = r01 + edges
	// Reverse and sum to get the backward contribution.
	R00 := r00 + r22
	R01 := r01 + r12
	R11 := 2 * r11
	R02 := 2 * r02
	R12 := r12 + r01
	r00, r01, r11, r02, r12 = R00, R01, R11, R02, R12
	// Solve A*x=b, where A=[r00, r01; r01, r11] and b=[r02; r12].
	den := r00*r11 - r01*r01
	if den < 0.001*(r00*r11) {
		return true
	}
	num1 := r02*r11 - r01*r12
	switch {
	case num1 >= den:
		lpc[1] = 1
	case num1 <= -den:
		lpc[1] = -1
	default:
		lpc[1] = num1 / den
	}
	num0 := r00*r12 - r02*r01
	switch {
	case 0.5*num0 >= den:
		lpc[0] = 1.999999
	case 0.5*num0 <= -den:
		lpc[0] = -1.999999
	default:
		lpc[0] = num0 / den
	}
	return false
}

// toneDetect detects pure or nearly pure tones so the encoder can keep them
// from destabilizing the pitch estimator, the transient detector, and the
// allocation (libopus tone_detect, float build). It returns the tone's
// normalized angular frequency (or -1) and sets toneishness to the squared
// pole radius of the fitted resonator.
func toneDetect(in [][]float32, C, n int, toneishness *float32) float32 {
	delay := 1
	x := make([]float32, n)
	if C == 2 {
		for i := 0; i < n; i++ {
			x[i] = in[0][i] + in[1][i]
		}
	} else {
		copy(x, in[0][:n])
	}
	var lpc [2]float32
	fail := toneLPC(x, n, delay, &lpc)
	// If the LPC filter resonates too close to DC, retry with down-sampling.
	for delay <= SampleRate/3000 && (fail || (lpc[0] > 1 && lpc[1] < 0)) {
		delay *= 2
		fail = toneLPC(x, n, delay, &lpc)
	}
	// Check that the filter has complex roots.
	if !fail && lpc[0]*lpc[0]+3.999999*lpc[1] < 0 {
		*toneishness = -lpc[1]
		return float32(math.Acos(0.5*float64(lpc[0]))) / float32(delay)
	}
	*toneishness = 0
	return -1
}

// celtAutocorr computes lag+1 autocorrelation values of x (libopus
// _celt_autocorr, float build, rectangular window path).
func celtAutocorr(x []float32, ac []float32, lag, n int) {
	for k := 0; k <= lag; k++ {
		var d float32
		for i := k; i < n; i++ {
			d += x[i] * x[i-k]
		}
		ac[k] = d
	}
}

// celtLPC computes LPC coefficients from autocorrelations by Levinson-Durbin
// (libopus _celt_lpc, float build).
func celtLPC(lpc []float32, ac []float32, p int) {
	clear(lpc[:p])
	errE := ac[0]
	if ac[0] > 1e-10 {
		for i := 0; i < p; i++ {
			var rr float32
			for j := 0; j < i; j++ {
				rr += lpc[j] * ac[i-j]
			}
			rr += ac[i+1]
			r := -rr / errE
			lpc[i] = r
			for j := 0; j < (i+1)>>1; j++ {
				tmp1, tmp2 := lpc[j], lpc[i-1-j]
				lpc[j] = tmp1 + r*tmp2
				lpc[i-1-j] = tmp2 + r*tmp1
			}
			errE = errE - r*r*errE
			// Bail out once we get 30 dB gain.
			if errE <= 0.001*ac[0] {
				break
			}
		}
	}
}

// celtFIR5 applies a 5-tap FIR in place (libopus celt_fir5, float build).
func celtFIR5(x []float32, num *[5]float32, n int) {
	var mem0, mem1, mem2, mem3, mem4 float32
	for i := 0; i < n; i++ {
		sum := x[i] +
			num[0]*mem0 + num[1]*mem1 + num[2]*mem2 + num[3]*mem3 + num[4]*mem4
		mem4, mem3, mem2, mem1, mem0 = mem3, mem2, mem1, mem0, x[i]
		x[i] = sum
	}
}

// pitchDownsample half-rates the summed channels and whitens the result with
// a modified LPC filter, preparing the pitch search input (libopus
// pitch_downsample, float build, factor 2).
func pitchDownsample(x [][]float32, xLP []float32, length, C int) {
	const factor = 2
	offset := factor / 2
	for i := 1; i < length; i++ {
		xLP[i] = 0.25*x[0][factor*i-offset] + 0.25*x[0][factor*i+offset] + 0.5*x[0][factor*i]
	}
	xLP[0] = 0.25*x[0][offset] + 0.5*x[0][0]
	if C == 2 {
		for i := 1; i < length; i++ {
			xLP[i] += 0.25*x[1][factor*i-offset] + 0.25*x[1][factor*i+offset] + 0.5*x[1][factor*i]
		}
		xLP[0] += 0.25*x[1][offset] + 0.5*x[1][0]
	}
	var ac [5]float32
	celtAutocorr(xLP, ac[:], 4, length)
	// Noise floor -40 dB.
	ac[0] *= 1.0001
	// Lag windowing.
	for i := 1; i <= 4; i++ {
		ac[i] -= ac[i] * (0.008 * float32(i)) * (0.008 * float32(i))
	}
	var lpc [4]float32
	celtLPC(lpc[:], ac[:], 4)
	tmp := float32(1.0)
	for i := 0; i < 4; i++ {
		tmp *= 0.9
		lpc[i] *= tmp
	}
	// Add a zero.
	const c1 = float32(0.8)
	num := [5]float32{
		lpc[0] + 0.8,
		lpc[1] + c1*lpc[0],
		lpc[2] + c1*lpc[1],
		lpc[3] + c1*lpc[2],
		c1 * lpc[3],
	}
	celtFIR5(xLP, &num, length)
}

// pitchXcorr computes the raw cross-correlations for every candidate lag
// (libopus celt_pitch_xcorr, plain C path).
func pitchXcorr(x, y []float32, xcorr []float32, length, maxPitch int) {
	for i := 0; i < maxPitch; i++ {
		xcorr[i] = innerProd(x, y[i:], length)
	}
}

// findBestPitch keeps the two lags with the best normalized correlation
// (libopus find_best_pitch, float build).
func findBestPitch(xcorr, y []float32, length, maxPitch int, bestPitch *[2]int) {
	syy := float32(1)
	bestNum := [2]float32{-1, -1}
	bestDen := [2]float32{}
	bestPitch[0], bestPitch[1] = 0, 1
	for j := 0; j < length; j++ {
		syy += y[j] * y[j]
	}
	for i := 0; i < maxPitch; i++ {
		if xcorr[i] > 0 {
			// The scaling avoids both underflow and overflow when squaring.
			xcorr16 := xcorr[i] * 1e-12
			num := xcorr16 * xcorr16
			if num*bestDen[1] > bestNum[1]*syy {
				if num*bestDen[0] > bestNum[0]*syy {
					bestNum[1], bestDen[1], bestPitch[1] = bestNum[0], bestDen[0], bestPitch[0]
					bestNum[0], bestDen[0], bestPitch[0] = num, syy, i
				} else {
					bestNum[1], bestDen[1], bestPitch[1] = num, syy, i
				}
			}
		}
		syy += y[i+length]*y[i+length] - y[i]*y[i]
		syy = max(1, syy)
	}
}

// pitchSearch finds the best pitch lag with a coarse 4x-decimated
// cross-correlation refined at 2x and by pseudo-interpolation (libopus
// pitch_search, float build). xLP is the current frame in the half-rate
// domain and y the same buffer including maxPitch samples of history before
// it.
func pitchSearch(xLP, y []float32, length, maxPitch int) int {
	lag := length + maxPitch
	xLP4 := make([]float32, length>>2)
	yLP4 := make([]float32, lag>>2)
	xcorr := make([]float32, maxPitch>>1)
	// Downsample by 2 again.
	for j := range xLP4 {
		xLP4[j] = xLP[2*j]
	}
	for j := range yLP4 {
		yLP4[j] = y[2*j]
	}
	// Coarse search with 4x decimation.
	var bestPitch [2]int
	pitchXcorr(xLP4, yLP4, xcorr, length>>2, maxPitch>>2)
	findBestPitch(xcorr[:maxPitch>>2], yLP4, length>>2, maxPitch>>2, &bestPitch)
	// Finer search with 2x decimation.
	for i := 0; i < maxPitch>>1; i++ {
		xcorr[i] = 0
		if iabs(i-2*bestPitch[0]) > 2 && iabs(i-2*bestPitch[1]) > 2 {
			continue
		}
		xcorr[i] = max(-1, innerProd(xLP, y[i:], length>>1))
	}
	findBestPitch(xcorr, y, length>>1, maxPitch>>1, &bestPitch)
	// Refine by pseudo-interpolation.
	offset := 0
	if bestPitch[0] > 0 && bestPitch[0] < (maxPitch>>1)-1 {
		a := xcorr[bestPitch[0]-1]
		b := xcorr[bestPitch[0]]
		c := xcorr[bestPitch[0]+1]
		if c-a > 0.7*(b-a) {
			offset = 1
		} else if a-c > 0.7*(b-c) {
			offset = -1
		}
	}
	// The subtraction reproduces the reference exactly even though the sign
	// looks inverted (a peak drifting toward larger i means a smaller lag,
	// yet the caller's maxPeriod-result grows by offset). Any period codes a
	// valid bitstream, so nothing breaks either way, but this value is only
	// the seed for removeDoubling, which re-derives its own interpolation
	// offset in the lag domain; keeping the reference behavior is what keeps
	// our per-frame decisions identical to libopus's (the prefilter
	// differential test pins that agreement).
	return 2*bestPitch[0] - offset
}

func computePitchGain(xy, xx, yy float32) float32 {
	return xy / float32(math.Sqrt(float64(1+xx*yy)))
}

var secondCheck = [16]int{0, 0, 3, 2, 3, 2, 5, 2, 3, 2, 3, 2, 5, 2, 3, 2}

// removeDoubling checks whether the detected period is a multiple of the true
// pitch and returns the pitch gain (libopus remove_doubling, float build).
// x is the half-rate whitened buffer whose first maxPeriod samples are
// history; periods are in full-rate samples.
func removeDoubling(x []float32, maxPeriod, minPeriod, N int, T0ptr *int, prevPeriod int, prevGain float32) float32 {
	minPeriod0 := minPeriod
	maxPeriod /= 2
	minPeriod /= 2
	*T0ptr /= 2
	prevPeriod /= 2
	N /= 2
	xo := maxPeriod // x[xo:] is the current frame
	if *T0ptr >= maxPeriod {
		*T0ptr = maxPeriod - 1
	}
	T := *T0ptr
	T0 := T
	yyLookup := make([]float32, maxPeriod+1)
	xx := innerProd(x[xo:], x[xo:], N)
	xy := innerProd(x[xo:], x[xo-T0:], N)
	yyLookup[0] = xx
	yy := xx
	for i := 1; i <= maxPeriod; i++ {
		yy = yy + x[xo-i]*x[xo-i] - x[xo+N-i]*x[xo+N-i]
		yyLookup[i] = max(0, yy)
	}
	yy = yyLookup[T0]
	bestXY, bestYY := xy, yy
	g := computePitchGain(xy, xx, yy)
	g0 := g
	// Look for any pitch at T/k.
	for k := 2; k <= 15; k++ {
		T1 := (2*T0 + k) / (2 * k)
		if T1 < minPeriod {
			break
		}
		// Look for another strong correlation at T1b.
		var T1b int
		if k == 2 {
			if T1+T0 > maxPeriod {
				T1b = T0
			} else {
				T1b = T0 + T1
			}
		} else {
			T1b = (2*secondCheck[k]*T0 + k) / (2 * k)
		}
		xy = innerProd(x[xo:], x[xo-T1:], N)
		xy2 := innerProd(x[xo:], x[xo-T1b:], N)
		xy = 0.5 * (xy + xy2)
		yy = 0.5 * (yyLookup[T1] + yyLookup[T1b])
		g1 := computePitchGain(xy, xx, yy)
		var cont float32
		switch {
		case iabs(T1-prevPeriod) <= 1:
			cont = prevGain
		case iabs(T1-prevPeriod) <= 2 && 5*k*k < T0:
			cont = 0.5 * prevGain
		}
		thresh := max(0.3, 0.7*g0-cont)
		// Bias against very high pitch (very short period) to avoid
		// false-positives due to short-term correlation.
		if T1 < 3*minPeriod {
			thresh = max(0.4, 0.85*g0-cont)
		} else if T1 < 2*minPeriod {
			thresh = max(0.5, 0.9*g0-cont)
		}
		if g1 > thresh {
			bestXY, bestYY = xy, yy
			T = T1
			g = g1
		}
	}
	bestXY = max(0, bestXY)
	var pg float32
	if bestYY <= bestXY {
		pg = 1
	} else {
		pg = bestXY / (bestYY + 1)
	}
	var xcorr [3]float32
	for k := 0; k < 3; k++ {
		xcorr[k] = innerProd(x[xo:], x[xo-(T+k-1):], N)
	}
	offset := 0
	if xcorr[2]-xcorr[0] > 0.7*(xcorr[1]-xcorr[0]) {
		offset = 1
	} else if xcorr[0]-xcorr[2] > 0.7*(xcorr[1]-xcorr[2]) {
		offset = -1
	}
	if pg > g {
		pg = g
	}
	*T0ptr = 2*T + offset
	if *T0ptr < minPeriod0 {
		*T0ptr = minPeriod0
	}
	return pg
}

// combFilter runs the two-window comb filter from x into y, transitioning
// from (T0, g0, tapset0) to (T1, g1, tapset1) over the first overlap samples
// (libopus celt.c comb_filter, float build, shared by both directions like
// the reference). y[yo+i] is written from x[xo+i-T] history, so x must carry
// at least T1+2 samples before xo. The decoder applies it in place (y == x)
// as the post-filter; the encoder calls it with negative gains, making the
// pre-filter the exact inverse.
func combFilter(y []float32, yo int, x []float32, xo int, T0, T1, N int,
	g0, g1 float32, tapset0, tapset1 int, window []float64, overlap int) {
	if g0 == 0 && g1 == 0 {
		if yo != xo || &y[0] != &x[0] {
			copy(y[yo:yo+N], x[xo:xo+N])
		}
		return
	}
	// When the gain is zero, T0 and/or T1 is set to zero. We need them to be
	// at least 2 to avoid processing garbage data.
	T0 = max(T0, combMinPeriod)
	T1 = max(T1, combMinPeriod)
	g00 := g0 * combGains[tapset0][0]
	g01 := g0 * combGains[tapset0][1]
	g02 := g0 * combGains[tapset0][2]
	g10 := g1 * combGains[tapset1][0]
	g11 := g1 * combGains[tapset1][1]
	g12 := g1 * combGains[tapset1][2]
	x1 := x[xo-T1+1]
	x2 := x[xo-T1]
	x3 := x[xo-T1-1]
	x4 := x[xo-T1-2]
	// If the filter didn't change, we don't need the overlap.
	if g0 == g1 && T0 == T1 && tapset0 == tapset1 {
		overlap = 0
	}
	i := 0
	for ; i < overlap; i++ {
		x0 := x[xo+i-T1+2]
		f := float32(window[i] * window[i])
		y[yo+i] = x[xo+i] +
			(1-f)*g00*x[xo+i-T0] +
			(1-f)*g01*(x[xo+i-T0+1]+x[xo+i-T0-1]) +
			(1-f)*g02*(x[xo+i-T0+2]+x[xo+i-T0-2]) +
			f*g10*x2 +
			f*g11*(x1+x3) +
			f*g12*(x0+x4)
		x4, x3, x2, x1 = x3, x2, x1, x0
	}
	if g1 == 0 {
		if yo != xo || &y[0] != &x[0] {
			copy(y[yo+overlap:yo+N], x[xo+overlap:xo+N])
		}
		return
	}
	// The constant-filter tail with T1.
	for ; i < N; i++ {
		x0 := x[xo+i-T1+2]
		y[yo+i] = x[xo+i] + g10*x2 + g11*(x1+x3) + g12*(x0+x4)
		x4, x3, x2, x1 = x3, x2, x1, x0
	}
}

// runPrefilter searches the frame for a pitch, decides whether the pre-filter
// pays for itself, applies it in place to in[c][overlap:overlap+N], and
// updates the pre-filter memories (libopus run_prefilter, float build). It
// returns pf_on, the pitch lag, the quantized gain, and its 3-bit index.
// in[c] holds [overlap history | N new] pre-emphasized samples; on entry the
// history is the unfiltered tail (for the pitch search continuity), and this
// replaces it with the filtered tail the MDCT windows need.
func (e *celtEncoder) runPrefilter(in [][]float32, C, N int, prefilterTapset int,
	enabled bool, tfEstimate float32, nbAvailableBytes int, toneFreq, toneishness float32) (pfOn, pitchIndex int, gain float32, qgain int) {

	const maxPeriod = combMaxPeriod
	const minPeriod = combMinPeriod
	overlap := e.overlap
	pre := make([][]float32, C)
	for c := 0; c < C; c++ {
		pre[c] = make([]float32, N+maxPeriod)
		copy(pre[c][:maxPeriod], e.prefilterMem[c])
		copy(pre[c][maxPeriod:], in[c][overlap:overlap+N])
	}

	var gain1 float32
	switch {
	case enabled && toneishness > 0.99:
		// The signal is dominated by a single tone: the standard pitch
		// estimator becomes unreliable, so derive the period from the tone
		// frequency directly.
		multiple := 1
		for toneFreq >= float32(multiple)*0.39 {
			multiple++
		}
		if toneFreq > 0.006148 {
			pitchIndex = min(int(math.Floor(0.5+2*math.Pi*float64(multiple)/float64(toneFreq))), maxPeriod-2)
		} else {
			// For a pitch too low for the post-filter, a very high pitch
			// still helps through the filter's DC component.
			pitchIndex = minPeriod
		}
		gain1 = 0.75
	case enabled && e.complexity >= 5:
		pitchBuf := make([]float32, (maxPeriod+N)>>1)
		pitchDownsample(pre, pitchBuf, (maxPeriod+N)>>1, C)
		// Don't search the last 1.5 octaves of the range: too many
		// false-positives from short-term correlation.
		pitchIndex = maxPeriod - pitchSearch(pitchBuf[maxPeriod>>1:], pitchBuf, N, maxPeriod-3*minPeriod)
		gain1 = removeDoubling(pitchBuf, maxPeriod, minPeriod, N, &pitchIndex, e.prefilterPeriod, e.prefilterGain)
		if pitchIndex > maxPeriod-2 {
			pitchIndex = maxPeriod - 2
		}
		gain1 = 0.7 * gain1
	default:
		gain1 = 0
		pitchIndex = minPeriod
	}
	// The analyser damps the gain when the signal is not actually pitched
	// (max_pitch_ratio near 0 on noise, near 1 on clean pitch).
	if e.analysis.valid {
		gain1 *= e.analysis.maxPitchRatio
	}

	// Gain threshold for enabling the prefilter/postfilter, adjusted by rate
	// and continuity.
	pfThreshold := float32(0.2)
	if iabs(pitchIndex-e.prefilterPeriod)*10 > pitchIndex {
		pfThreshold += 0.2
		// Completely disable the prefilter on strong transients without
		// continuity.
		if tfEstimate > 0.98 {
			gain1 = 0
		}
	}
	if nbAvailableBytes < 25 {
		pfThreshold += 0.1
	}
	if nbAvailableBytes < 35 {
		pfThreshold += 0.1
	}
	if e.prefilterGain > 0.4 {
		pfThreshold -= 0.1
	}
	if e.prefilterGain > 0.55 {
		pfThreshold -= 0.1
	}
	pfThreshold = max(pfThreshold, 0.2)

	qg := 0
	if gain1 < pfThreshold {
		gain1 = 0
		pfOn = 0
	} else {
		if absf(gain1-e.prefilterGain) < 0.1 {
			gain1 = e.prefilterGain
		}
		qg = int(math.Floor(0.5+float64(gain1)*32/3)) - 1
		qg = max(0, min(7, qg))
		gain1 = 0.09375 * float32(qg+1)
		pfOn = 1
	}

	// Apply the filter in place, transitioning from the previous frame's
	// filter, and compare loudness before and after.
	var before, after [2]float32
	offset := celtShortMDCTSize - overlap
	e.prefilterPeriod = max(e.prefilterPeriod, minPeriod)
	for c := 0; c < C; c++ {
		copy(in[c][:overlap], e.preHistory[c])
		for i := 0; i < N; i++ {
			before[c] += absf(in[c][overlap+i])
		}
		if offset != 0 {
			combFilter(in[c], overlap, pre[c], maxPeriod,
				e.prefilterPeriod, e.prefilterPeriod, offset, -e.prefilterGain, -e.prefilterGain,
				e.prefilterTapset, e.prefilterTapset, nil, 0)
		}
		combFilter(in[c], overlap+offset, pre[c], maxPeriod+offset,
			e.prefilterPeriod, pitchIndex, N-offset, -e.prefilterGain, -gain1,
			e.prefilterTapset, prefilterTapset, e.window, overlap)
		for i := 0; i < N; i++ {
			after[c] += absf(in[c][overlap+i])
		}
	}
	cancelPitch := false
	if C == 2 {
		thresh0 := 0.25*gain1*before[0] + 0.01*before[1]
		thresh1 := 0.25*gain1*before[1] + 0.01*before[0]
		// Don't use the filter if one channel gets significantly worse.
		if after[0]-before[0] > thresh0 || after[1]-before[1] > thresh1 {
			cancelPitch = true
		}
		// Use the filter only if at least one channel gets significantly
		// better.
		if before[0]-after[0] < thresh0 && before[1]-after[1] < thresh1 {
			cancelPitch = true
		}
	} else if after[0] > before[0] {
		// Check that the mono channel actually got better.
		cancelPitch = true
	}
	if cancelPitch {
		// Revert to a gain of zero, fading the previous frame's filter out.
		for c := 0; c < C; c++ {
			copy(in[c][overlap:overlap+N], pre[c][maxPeriod:maxPeriod+N])
			combFilter(in[c], overlap+offset, pre[c], maxPeriod+offset,
				e.prefilterPeriod, pitchIndex, overlap, -e.prefilterGain, 0,
				e.prefilterTapset, prefilterTapset, e.window, overlap)
		}
		gain1 = 0
		pfOn = 0
		qg = 0
	}

	for c := 0; c < C; c++ {
		// The filtered tail becomes the next frame's MDCT history; the
		// caller copies it from in[c] after the frame is fully assembled.
		// The unfiltered signal slides into the pitch-search history.
		if N > maxPeriod {
			copy(e.prefilterMem[c], pre[c][N:N+maxPeriod])
		} else {
			copy(e.prefilterMem[c][:maxPeriod-N], e.prefilterMem[c][N:])
			copy(e.prefilterMem[c][maxPeriod-N:], pre[c][maxPeriod:maxPeriod+N])
		}
	}
	return pfOn, pitchIndex, gain1, qg
}
