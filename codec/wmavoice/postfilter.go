//go:build !wmavoicetablesgen

package wmavoice

import "math"

// The postfilter (note 10). It runs twice per frame, over 80 samples each
// time, and each half uses its own LPC set: the frame-centre set for the first
// and the frame-end set for the second.
//
// Everything here is float32 unless a comment says otherwise. The denoise
// filter's normalisation is the one place in this codec where getting the
// arithmetic wrong does not look like noise, it looks like a broken decoder.

const (
	// pfWindow is the length the denoise filter convolves at.
	pfWindow = dftLen
	// pfCacheLen bounds the overlap-add cache.
	pfCacheLen = 2 * halfFrameSamples
	// agcAlpha is the adaptive gain control's one-pole coefficient. The
	// control is deliberately slow: it tracks the level, not the envelope.
	agcAlpha = float32(0.99)
	// energyStep continues the magnitude table's geometric series past its
	// last row, and energyScale is 1/log10(energyStep), which is what puts an
	// index on that series.
	energyStep  = 1.0331663
	energyScale = 70.570526123
	// energyBias is subtracted before the index is taken.
	energyBias = 0.0295
	// denoiseHard and denoiseSoft weight the gain by whether the frame's fixed
	// codebook is the hardcoded one.
	denoiseHard = 5.0 / 13.0
	denoiseSoft = 5.0 / 14.7
	// specFloor keeps log10 off zero. The LPC vector always carries a leading
	// 1.0, so no real stream reaches it; a crafted one whose coefficients sum
	// to exactly -1 would otherwise put -Inf into every downstream term.
	specFloor = 1e-30
)

// The DC removal section (note 10.7), a high-pass with its zero pair
// essentially on the unit circle at DC.
const (
	dcZero0 = float32(-1.99997)
	dcZero1 = float32(1.0)
	dcPole0 = float32(-1.9330735188)
	dcPole1 = float32(0.93589198496)
	dcGain  = float32(0.93980580475)
)

// postfilter runs the chain over one 80-sample half-frame and writes the
// result into out. at is the half-frame's offset within the superframe.
func (d *Decoder) postfilter(out []float32, at int, lpc []float32, fcb fcbKind, pitch int) {
	size := halfFrameSamples
	syn := d.synth[d.synthBase+at:]

	// 10.2 Zero synthesis, into a buffer that persists across superframes: an
	// excitation-like signal recovered from the synthesis output. It reads
	// synth history rather than its own, so it is FIR.
	zAt := d.zeroBase + at
	allZero(d.zero, d.synth, zAt, d.synthBase+at, size, lpc)

	// 10.3 Kalman smoothing, for the two pulse-coded fixed codebooks only.
	src := d.zero[zAt : zAt+size]
	if fcb == fcbWindowPulses || fcb == fcbInnovation {
		if d.smooth(d.smoothed[:size], zAt, size, pitch) {
			src = d.smoothed[:size]
			d.paths.KalmanOK++
		} else {
			d.paths.KalmanFail++
		}
	}

	// 10.4 Re-synthesis, into a buffer with L samples of its own history. The
	// history is saved BEFORE the denoise filter runs, so what carries over is
	// the clean re-synthesis and not the denoised signal.
	l := d.lsps
	copy(d.resynth[l:l+size], src)
	allPole(d.resynth, l, size, lpc)
	resyn := d.resynth[l : l+size]
	// The history for the next half-frame, taken now because it is the clean
	// re-synthesis that carries over and not the denoised signal. It reads the
	// tail of what was just written and writes below it, so it does not
	// disturb resyn.
	copy(d.resynth[:l], d.resynth[size:size+l])

	// 10.5 The Wiener denoise filter, and its overlap-add cache. The filter
	// skips a silence frame; the cache merge does not, which is how the tail
	// of the last denoised frame decays into a silent passage instead of
	// clicking off.
	work := &d.pfWork
	if fcb != fcbSilence {
		d.denoise(work, resyn, lpc, fcb == fcbHardcoded, size)
		d.paths.DenoiseRuns++
	} else {
		clear(work[:])
		copy(work[:size], resyn)
	}
	d.mergeCache(work, size, fcb == fcbSilence)

	// 10.6 Adaptive gain control, restoring the pre-postfilter energy.
	var speech, post float32
	for m := range size {
		speech += absF(syn[m])
		post += absF(work[m])
	}
	var g float32
	if post != 0 {
		g = float32(float64(1-agcAlpha) * float64(speech) / float64(post))
	}
	for m := range size {
		d.agcMem = agcAlpha*d.agcMem + g
		out[m] = work[m] * d.agcMem
	}

	// 10.7 DC removal, which no real format selects.
	if d.cfg.DCLevel() > dcRemovalLevel {
		d.removeDC(out[:size])
		d.paths.DCRemoval++
	}
}

// smooth searches the zero-synthesis history for the best-matching earlier
// segment and blends toward it, at most 37.5 percent of the way (note 10.3).
// It reports false when no lag beats a zero correlation, which happens in
// near-silent passages where the history is all zeros.
func (d *Decoder) smooth(dst []float32, at, size, pitch int) bool {
	lo := max(d.g.minPitch, pitch-3)
	hi := min(d.g.maxPitch, pitch+3)
	cur := d.zero[at : at+size]
	best, bestDot := -1, float32(0)
	for lag := lo; lag <= hi; lag++ {
		var acc float32
		past := d.zero[at-lag : at-lag+size]
		for m := range size {
			acc += cur[m] * past[m]
		}
		if acc > bestDot {
			bestDot, best = acc, lag
		}
	}
	if best < 0 || bestDot <= 0 {
		return false
	}
	past := d.zero[at-best : at-best+size]
	var den float32
	for m := range size {
		den += past[m] * past[m]
	}
	if den <= 0 {
		return false
	}
	f := float32(0.625)
	if den > bestDot {
		f = den / (den + 0.6*bestDot)
	}
	for m := range size {
		dst[m] = past[m] + f*(cur[m]-past[m])
	}
	return true
}

// denoise builds a Wiener impulse response from the LPC power spectrum and
// convolves the half-frame with it, leaving 128 samples in work: the first
// size of them belong to this half-frame and the tail overlaps into the next.
func (d *Decoder) denoise(work *[pfWindow]float32, in, lpc []float32, hardcoded bool, size int) {
	t := d.tf
	l := len(lpc)

	// Step 1: tilt the LPCs into a 128-entry vector of 1.0, the coefficients,
	// then zeros.
	v := &d.pfSpec
	clear(v[:])
	v[0] = 1
	copy(v[1:], lpc)
	tiltFilter(0.7*tiltFactor(lpc), v[:l+2])

	// Step 2: the LPC power spectrum, as base-10 logarithms, and the range
	// they span. Index 64 reads bin 32's real part where the Nyquist bin would
	// sit under a packed layout the reference no longer uses; nothing in reach
	// can say what Microsoft's decoder reads there, and matching the reference
	// is what the gate is written against (note 10.5).
	spec := &d.pfLPCSpec
	t.forwardDFT(spec, v)
	g := &d.pfGain
	g[0] = log10f(spec[0] * spec[0])
	for n := 1; n < dftBins-1; n++ {
		g[n] = log10f(spec[2*n]*spec[2*n] + spec[2*n+1]*spec[2*n+1])
	}
	g[dftBins-1] = log10f(spec[64] * spec[64])
	lo, hi := g[0], g[0]
	for _, x := range g[:dftBins] {
		lo, hi = min(lo, x), max(hi, x)
	}

	// Step 3: a phase and a magnitude per point.
	rng := hi - lo
	irange := float32(0)
	if rng > 0 {
		irange = trigLen / rng
	}
	weight := float32(denoiseSoft)
	if hardcoded {
		weight = denoiseHard
	}
	gainMul := rng * weight
	angleMul := float32(float64(gainMul) * (8 * math.Ln10 / math.Pi))
	mag := &d.pfMag
	pwrRow := &denoisePower[d.cfg.DenoiseStrength()]
	for n := range dftBins {
		i := clip(int(math.RoundToEven(float64((hi-g[n])*irange-1))), 0, trigLen-1)
		pwr := pwrRow[i]
		g[n] = angleMul * pwr
		x := max((float64(pwr*gainMul)-energyBias)*energyScale, 0)
		// The floor is repeated on the integer: max passes a NaN through, and
		// int(NaN) is the most negative value on amd64, which would index the
		// table from below. Nothing in reach makes the product NaN; the guard
		// is here because the alternative is a panic.
		j := max(int(x), 0)
		if j > len(energyTable)-1 {
			mag[n] = energyTable[len(energyTable)-1] * float32(math.Pow(energyStep, float64(j-(len(energyTable)-1))))
		} else {
			mag[n] = energyTable[j]
		}
	}

	// Step 4: the Hilbert-style phase shift.
	var dct, h [trigLen]float32
	copy(dct[:], g[:trigLen])
	var s [trigLen]float32
	t.dctI(&s, &dct)
	t.dstI(&h, &s)
	h64 := t.phaseRef65(&s)

	// Step 5: the coefficient spectrum, walked downward because the higher
	// magnitudes must still be readable when the lower points write into
	// indices 2n and 2n+1.
	pt := t.phase
	c := &d.pfCoefs
	last := mag[dftBins-1] * pt[phaseIndex(h64-2*h[trigLen-1])][0]
	for n := trigLen - 1; n >= 1; n-- {
		p := h64 - 2*h[n-1]
		if n&1 != 0 {
			p = -h64 - 2*h[n-1]
		}
		a := phaseIndex(p)
		c[2*n+1] = mag[n] * pt[a][1]
		c[2*n] = mag[n] * pt[a][0]
	}
	c[0] = mag[0] * pt[phaseIndex(h64)][0]
	// The 65th magnitude lands on bin 32's real part, which the n = 32 step
	// already wrote: the same array-position confusion as g[64] above, seen
	// from the other side, and reproduced for the same reason.
	c[trigLen] = last
	// The Nyquist bin is zero, and the DC bin's imaginary part takes its value
	// from it, which is why it is zero too.
	c[1] = 0
	c[dftCoefs-2] = 0
	c[dftCoefs-1] = 0

	// Step 6: back to the time domain, truncated to the impulse response's
	// length and normalised to a fixed energy.
	ir := &d.pfIR
	t.inverseDFT(ir, c)
	rem := min(pfWindow-1-size, size-1)
	clear(ir[rem:])
	if d.cfg.DenoiseTilt() {
		ir[rem-1] = 0
		tiltFilter(-1.8*tiltFactor(ir[:rem-1]), ir[:rem])
	}
	var energy float32
	for _, x := range ir[:rem] {
		energy += x * x
	}
	// An identically zero impulse response cannot come out of a magnitude
	// table whose every entry is positive, but the scale below would be an
	// infinity if one did, and every sample after it a NaN.
	if energy > 0 {
		sq := float32(1.0 / trigLen * math.Sqrt(1/float64(energy)))
		for i := range rem {
			ir[i] *= sq
		}
	}

	// Step 7: convolve, through the transform pair whose round trip supplies
	// the other factor of 128.
	sig := &d.pfSpec
	clear(sig[:])
	copy(sig[:size], in)
	sigSpec := &d.pfSigSpec
	irSpec := &d.pfIRSpec
	t.forwardDFT(sigSpec, sig)
	t.forwardDFT(irSpec, ir)
	// Bin 0's parts multiply separately; its imaginary part is zero on both
	// sides, so that product is zero. Every other bin is a complex product,
	// and bin 64's imaginary parts are zero on both sides too.
	sigSpec[0] *= irSpec[0]
	sigSpec[1] = 0
	for k := 1; k < dftBins; k++ {
		ar, ai := sigSpec[2*k], sigSpec[2*k+1]
		br, bi := irSpec[2*k], irSpec[2*k+1]
		sigSpec[2*k] = ar*br - ai*bi
		sigSpec[2*k+1] = ar*bi + ai*br
	}
	t.inverseDFT(work, sigSpec)
}

// mergeCache is the overlap-add of note 10.5's step 8: what the previous
// half-frame's impulse response left hanging past its own 80 samples is added
// here, and this one's tail is kept for the next.
func (d *Decoder) mergeCache(work *[pfWindow]float32, size int, silence bool) {
	if d.cacheLen > 0 {
		lim := min(d.cacheLen, size)
		for m := range lim {
			work[m] += d.cache[m]
		}
		d.cacheLen -= lim
		copy(d.cache[:], d.cache[size:])
		clear(d.cache[pfCacheLen-size:])
	}
	if silence {
		return
	}
	rem := min(pfWindow-1-size, size-1)
	lim := min(rem, d.cacheLen)
	for m := range lim {
		d.cache[m] += work[size+m]
	}
	if lim < rem {
		copy(d.cache[lim:rem], work[size+lim:size+rem])
		d.cacheLen = rem
	}
}

// removeDC is the second-order section of note 10.7, applied in place with
// memory that persists.
//
// DEFICIT: the DC noise level is 1 to 4 on all seven formats Windows' encoder
// offers and this filter needs more than 8, so it never runs on anything this
// tree can reach.
func (d *Decoder) removeDC(v []float32) {
	for m := range v {
		t := dcGain*v[m] - dcPole0*d.dcMem[0] - dcPole1*d.dcMem[1]
		v[m] = t + dcZero0*d.dcMem[0] + dcZero1*d.dcMem[1]
		d.dcMem[1], d.dcMem[0] = d.dcMem[0], t
	}
}

// tiltFactor is note 10.5's tilt: the first-order correlation of a coefficient
// run against its own energy.
func tiltFactor(a []float32) float32 {
	num := a[0]
	for i := 0; i < len(a)-1; i++ {
		num += a[i] * a[i+1]
	}
	den := float32(1)
	for _, x := range a {
		den += x * x
	}
	return num / den
}

// tiltFilter is the one-tap filter that applies it, in place, from a zero
// memory, which is how both callers run it.
func tiltFilter(t float32, s []float32) {
	for i := len(s) - 1; i >= 1; i-- {
		s[i] -= t * s[i-1]
	}
}

func log10f(v float32) float32 {
	return float32(math.Log10(math.Max(float64(v), specFloor)))
}

func absF(x float32) float32 {
	if x < 0 {
		return -x
	}
	return x
}
