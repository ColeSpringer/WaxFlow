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
// with xw the analysis-windowed block. It runs through the same N/4-point
// DCT-IV the decoder's inverse uses (codec/aac/imdct.go carries the
// derivation): the kernel's symmetries fold the N windowed samples onto N/2 --
// the time-domain aliasing the window pairs cancel on overlap-add -- and what
// is left is X[k] = fwdScale * DCT-IV(u)[k], one N/4-point complex DFT between
// two rotations by exp(-2*pi*i*(j + 1/8)/N).
//
// The decoder computes its inverse with a private float64 FFT; the encoder uses
// dsp/fft here on purpose (the plan's "second FFT path"): its fixed float32
// op order with no FMA makes the forward transform a pure function of its
// input, which the deterministic-mode/golden gate needs. The rotations and the
// fold below are plain float32 multiplies and adds for the same reason.

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
	n    int // the time-domain length
	m    int // n/4, the DFT length
	n2   int // n/2, the DCT-IV length and the coefficient count
	plan *fft.Plan
	// rotRe/rotIm is exp(-2*pi*i*(j + 1/8)/n), the rotation the DCT-IV applies
	// before and after its FFT; one table serves both passes.
	rotRe, rotIm []float32
	scale        float32
	u            []float32 // the folded block, length n/2
	inRe, inIm   []float32 // FFT input scratch, length n/4
	outRe, outIm []float32 // FFT output scratch, length n/4
}

func newMDCTForward(n int) *mdctForward {
	m := &mdctForward{
		n: n, m: n / 4, n2: n / 2,
		plan:  fft.NewPlan(n / 4),
		rotRe: make([]float32, n/4),
		rotIm: make([]float32, n/4),
		scale: float32(fwdScale(n)),
		u:     make([]float32, n/2),
		inRe:  make([]float32, n/4),
		inIm:  make([]float32, n/4),
		outRe: make([]float32, n/4),
		outIm: make([]float32, n/4),
	}
	for j := 0; j < n/4; j++ {
		a := 2 * math.Pi * (float64(j) + 0.125) / float64(n)
		m.rotRe[j], m.rotIm[j] = float32(math.Cos(a)), float32(-math.Sin(a))
	}
	return m
}

// fullWindow is the symmetric analysis/synthesis window for an all-long block:
// the plan's rising half (from the shared plan) followed by its mirror,
// matching what the decoder's applyWindow produces when both neighbours are the
// same size. Block switching uses neighbour-aware windows instead.
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
	n2, q := m.n2, m.m
	// The TDAC fold: every input position outside [0, n/2) lands back inside
	// it with a sign, which is the aliasing the window pairs cancel.
	u := m.u
	for j := 0; j < q; j++ {
		u[j] = -windowed[3*q-1-j] - windowed[j+3*q]
	}
	for j := q; j < n2; j++ {
		u[j] = windowed[j-q] - windowed[3*q-1-j]
	}
	// Pre-rotation, the n/4-point DFT, then post-rotation: one complex output
	// carries C[2k] in its real part and C[n2-1-2k] in its imaginary part.
	for j := 0; j < q; j++ {
		x1, x2 := u[2*j], u[n2-1-2*j]
		c, s := m.rotRe[j], m.rotIm[j]
		m.inRe[j] = x1*c - x2*s
		m.inIm[j] = x2*c + x1*s
	}
	m.plan.Transform(m.outRe, m.outIm, m.inRe, m.inIm)
	for k := 0; k < q; k++ {
		re, im := m.outRe[k], m.outIm[k]
		c, s := m.rotRe[k], m.rotIm[k]
		spec[2*k] = m.scale * (re*c - im*s)
		spec[n2-1-2*k] = -m.scale * (im*c + re*s)
	}
}
