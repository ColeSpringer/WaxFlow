package vorbis

import (
	"math"

	"github.com/colespringer/waxflow/dsp/fft"
)

// The forward (analysis) MDCT is the transpose of the decoder's inverse
// (mdct.go), so it uses the identical cosine kernel
//
//	X[k] = fwdScale * Σ_{n=0}^{N-1} xw[n] cos((2π/N)(n+n0)(k+1/2)), n0 = (N/2+1)/2
//
// with xw the analysis-windowed block. Writing cos as a real part and
// factoring the (n+n0)(k+1/2) product gives a pre-twiddle, one length-N DFT,
// and a post-twiddle:
//
//	h[n] = xw[n] * exp(-iπn/N)
//	H    = DFT_-[h]                       (the dsp/fft forward, e^{-2πi jk/N})
//	X[k] = fwdScale * Re{ C[k] * conj(H[k]) }, C[k] = exp(i(πn0/N)(2k+1))
//	     = fwdScale * (cRe[k]*H.re[k] + cIm[k]*H.im[k])
//
// The decoder computes its inverse with a private +sign FFT; the encoder uses
// dsp/fft here on purpose (the plan's "second FFT path"): its fixed float32
// op order with no FMA makes the forward transform a pure function of its
// input, which the deterministic-mode/golden gate needs. Unifying the decoder
// onto dsp/fft is optional later cleanup, out of scope here.

// fwdScale is the analysis normalization. The decoder's inverse carries no 1/N
// (vorbisScale == 1), so the forward holds it all. The single-block operator
// D·((C)Dᵀ) works out to (C·N/4)·(xw[m] ± aliased reflection), so C = 4/N makes
// the IMDCT produce the standard MDCT form xw[m] ± alias whose reflected copy
// the overlap-add cancels: with the sine window applied on both analysis and
// synthesis (Princen-Bradley, w[i]²+w[i+N/2]²==1) and 50% overlap, forward(4/N)
// + windowed inverse + overlap-add is the identity. Verified by the TDAC
// round-trip in mdctfwd_test.go.
func fwdScale(n int) float64 { return 4.0 / float64(n) }

// mdctForward is one block size's analysis transform. Each Encoder owns its own
// (single-threaded), so the FFT scratch lives here rather than being passed per
// call the way the shared decoder plans take it.
type mdctForward struct {
	n            int
	plan         *fft.Plan
	preRe, preIm []float32 // pre-twiddle exp(-iπn/N), length n
	cRe, cIm     []float32 // post-twiddle C[k]*fwdScale, length n/2
	inRe, inIm   []float32 // FFT input scratch, length n
	outRe, outIm []float32 // FFT output scratch, length n
}

func newMDCTForward(n int) *mdctForward {
	m := &mdctForward{
		n:     n,
		plan:  fft.NewPlan(n),
		preRe: make([]float32, n),
		preIm: make([]float32, n),
		cRe:   make([]float32, n/2),
		cIm:   make([]float32, n/2),
		inRe:  make([]float32, n),
		inIm:  make([]float32, n),
		outRe: make([]float32, n),
		outIm: make([]float32, n),
	}
	for i := 0; i < n; i++ {
		a := math.Pi * float64(i) / float64(n)
		m.preRe[i] = float32(math.Cos(a))
		m.preIm[i] = float32(-math.Sin(a))
	}
	n0 := (float64(n)/2 + 1) / 2
	s := fwdScale(n)
	for k := 0; k < n/2; k++ {
		a := math.Pi * n0 / float64(n) * float64(2*k+1)
		m.cRe[k] = float32(s * math.Cos(a))
		m.cIm[k] = float32(s * math.Sin(a))
	}
	return m
}

// fullWindow is the symmetric analysis/synthesis window for an all-long block:
// the plan's rising half (from the shared plan) followed by its mirror,
// matching what the decoder's applyWindow produces when both neighbours are the
// same size. Block switching (4c) uses neighbour-aware windows instead.
func fullWindow(n int) []float32 {
	rise := getPlan(n).window // length n/2
	w := make([]float32, n)
	for i := 0; i < n/2; i++ {
		w[i] = rise[i]
		w[n-1-i] = rise[i]
	}
	return w
}

// forward transforms the analysis-windowed block (length n) into spec (length
// n/2). The caller applies the window; keeping it out mirrors the decoder's
// split of imdct from applyWindow and lets block switching pick the window.
func (m *mdctForward) forward(windowed []float32, spec []float32) {
	n := m.n
	for i := 0; i < n; i++ {
		x := windowed[i]
		m.inRe[i] = x * m.preRe[i]
		m.inIm[i] = x * m.preIm[i]
	}
	m.plan.Transform(m.outRe, m.outIm, m.inRe, m.inIm)
	for k := 0; k < n/2; k++ {
		spec[k] = m.cRe[k]*m.outRe[k] + m.cIm[k]*m.outIm[k]
	}
}
