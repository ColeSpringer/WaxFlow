package aac

import "math"

// Forward MDCT for the encoder, the exact inverse of imdct.go's transform:
//
//	X[k] = 2 Σ_{n=0}^{N-1} x[n] cos((2π/N)(n+n0)(k+1/2)), k < N/2, n0 = (N/2+1)/2
//
// The forward scale of 2 complements the decoder's (2/N)-scaled inverse:
// with the Princen-Bradley window pairs in tables.go the pair then
// reconstructs perfectly under overlap-add (mdct_encode_test.go pins the
// round trip; unit forward scale reconstructs at exactly half).
//
// The sum factors through the same radix-2 FFT the inverse plan carries:
// X[k] = Re{ E_k · DFT(x[n]·exp(-iπn/N))[k] }, E_k = exp(-i2πn0(k+1/2)/N),
// and the forward DFT runs on the inverse kernel via conjugation.
type mdctPlan struct {
	inv            *imdctPlan // borrowed FFT kernel and twiddles
	preRe, preIm   []float64  // conj pre-twiddle exp(+iπn/N)
	postRe, postIm []float64  // E_k, imaginary part negated into the sum
}

var (
	mdctLong  = newMDCTPlan(planLong)
	mdctShort = newMDCTPlan(planShort)
)

func newMDCTPlan(inv *imdctPlan) *mdctPlan {
	n := inv.n
	p := &mdctPlan{inv: inv,
		preRe: make([]float64, n), preIm: make([]float64, n),
		postRe: make([]float64, n/2), postIm: make([]float64, n/2)}
	for m := 0; m < n; m++ {
		a := math.Pi * float64(m) / float64(n)
		p.preRe[m], p.preIm[m] = math.Cos(a), math.Sin(a)
	}
	n0 := (float64(n)/2 + 1) / 2
	for k := 0; k < n/2; k++ {
		a := 2 * math.Pi * n0 * (float64(k) + 0.5) / float64(n)
		// The forward scale of 2 folds into the post-rotation.
		p.postRe[k], p.postIm[k] = 2*math.Cos(a), -2*math.Sin(a)
	}
	return p
}

// mdct computes spec[k] (length n/2) from the windowed block x (length n).
// The scratch is stack-sized per transform length: the plans are shared
// package singletons, so plan-owned scratch would race across concurrent
// encoders, and one 2048 buffer would zero 32 KiB for each of the eight
// 256-point short transforms a transient frame runs per channel.
func (p *mdctPlan) mdct(x, spec []float64) {
	n := p.inv.n
	if n <= 256 {
		var reBuf, imBuf [256]float64
		p.run(reBuf[:n], imBuf[:n], x, spec)
		return
	}
	var reBuf, imBuf [2048]float64
	p.run(reBuf[:n], imBuf[:n], x, spec)
}

func (p *mdctPlan) run(cr, ci, x, spec []float64) {
	n := p.inv.n
	// The inverse kernel computes exp(+i); feeding conj(c) and conjugating
	// the output yields the forward DFT of c[m] = x[m]·exp(-iπm/N).
	for m := 0; m < n; m++ {
		cr[m] = x[m] * p.preRe[m]
		ci[m] = x[m] * p.preIm[m]
	}
	p.inv.fft(cr, ci)
	for k := 0; k < n/2; k++ {
		// Re{E_k · conj((cr,ci))} with E's imaginary part precomputed
		// negated: postRe·cr + postIm·ci.
		spec[k] = p.postRe[k]*cr[k] + p.postIm[k]*ci[k]
	}
}

// windowedLong fills w2048 with the analysis-windowed block for a long
// window sequence (sine shape; the encoder writes window_shape 0). The
// left half of LONG_STOP and the right half of LONG_START carry the
// short-window tapers at the offsets the decoder's longWindowApply uses.
func windowedLong(t *[2048]float64, seq int, out *[2048]float64) {
	wl := &longWindow[shapeSine]
	ws := &shortWindow[shapeSine]
	if seq == longStop {
		for n := 0; n < 448; n++ {
			out[n] = 0
		}
		for n := 0; n < 128; n++ {
			out[448+n] = t[448+n] * ws[n]
		}
		for n := 576; n < 1024; n++ {
			out[n] = t[n]
		}
	} else {
		for n := 0; n < 1024; n++ {
			out[n] = t[n] * wl[n]
		}
	}
	if seq == longStart {
		for n := 1024; n < 1472; n++ {
			out[n] = t[n]
		}
		for n := 0; n < 128; n++ {
			out[1472+n] = t[1472+n] * ws[128+n]
		}
		for n := 1600; n < 2048; n++ {
			out[n] = 0
		}
	} else {
		for n := 1024; n < 2048; n++ {
			out[n] = t[n] * wl[n]
		}
	}
}

// mdctFrame transforms one 2048-sample time block into the 1024-line
// spectrum for the window sequence: one long transform, or eight short
// transforms at 128-sample hops starting at offset 448 (the decoder's
// shortFilterbank layout), window-major in spec.
func mdctFrame(t *[2048]float64, seq int, spec *[1024]float64) {
	if seq != eightShort {
		var w [2048]float64
		windowedLong(t, seq, &w)
		mdctLong.mdct(w[:], spec[:1024])
		return
	}
	ws := &shortWindow[shapeSine]
	var w [256]float64
	for i := 0; i < 8; i++ {
		off := 448 + i*128
		for n := 0; n < 256; n++ {
			w[n] = t[off+n] * ws[n]
		}
		mdctShort.mdct(w[:], spec[i*128:i*128+128])
	}
}
