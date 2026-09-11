//go:build !wmaprotablesgen

package wmapro

import (
	"math"
	"sync"

	"github.com/colespringer/waxflow/dsp/fft"
)

// The inverse transform, the sine windows, and the overlap butterfly.
//
// This codec's inverse transform takes L coefficients to L time samples, which
// is the non-redundant half of a 2L-point inverse MDCT. Written as a reversed
// DCT-IV, with
//
//	C[m] = sum over k<L of X[k] cos( pi (2k+1)(2m+1) / (4L) )
//
// the output is
//
//	h[i] = scale * C[L-1-i]
//
// and that half is computed here through an L/2-point complex DFT, which is a
// quarter of the transform length. The reduction is the standard one:
// pairing X[2j] with X[L-1-2j] and rotating by exp(-2*pi*i*(j+1/8)/(2L)) makes
// one complex DFT output carry C[2q] in its real part and C[L-1-2q] in its
// imaginary part. codec/wma/mdct.go derives the same factorization at length
// for the full 2L output; this needs only the half, so the outer-quarter
// symmetries that file also uses do not appear.
//
// The SIGN is the thing to get wrong. Against the textbook inverse MDCT
//
//	y[n] = sum_k X[k] cos( pi/L (n + 1/2 + L/2)(k + 1/2) ),  n in 0..2L-1
//
// this half is h[j] = -scale * y[L/2 + j], the NEGATED middle. The overlap
// butterfly below carries a compensating minus sign, so the pair is consistent
// and only the pair is. TestIMDCTMatchesAllThreeStatements scores the
// implementation against all three forms rather than against either alone.
//
// The sum is left unnormalised by the DFT, and the whole normalisation arrives
// as one scale factor: (2/L) * 2^-(bitsPerSample-1). The second term is what
// puts the output at a nominal full scale of 1.0 at both depths, and it pairs
// with the quantisation base scaling with the depth; getting only one of the
// two right is about 48 dB of error.

// imdctPlan holds the read-only rotation table and DFT plan for one transform
// length. Plans are immutable after construction and shared across sessions.
type imdctPlan struct {
	length int // L, the number of coefficients and of output samples
	m      int // L/2, the DFT length
	// tw is exp(-2*pi*i*(j + 1/8)/(2L)), the rotation both the pre- and the
	// post-pass apply.
	twRe, twIm []float64
	fp         *fft.Plan
	// window is the rising half of the sine window of length L, used when L is
	// the shorter of two adjacent subframes. The falling half is the same
	// entries read backwards.
	window []float32
}

var (
	planMu    sync.Mutex
	planCache = map[int]*imdctPlan{}
)

func planFor(length int) *imdctPlan {
	planMu.Lock()
	defer planMu.Unlock()
	if p, ok := planCache[length]; ok {
		return p
	}
	p := newIMDCTPlan(length)
	planCache[length] = p
	return p
}

func newIMDCTPlan(length int) *imdctPlan {
	m := length / 2
	p := &imdctPlan{length: length, m: m, fp: fft.NewPlan(m)}
	p.twRe = make([]float64, m)
	p.twIm = make([]float64, m)
	for j := range m {
		sn, c := math.Sincos(2 * math.Pi * (float64(j) + 0.125) / float64(2*length))
		p.twRe[j], p.twIm[j] = c, sn
	}
	// The half-sample sine. Only this form satisfies the Princen-Bradley
	// condition w[i]^2 + w[n-1-i]^2 = 1, so the plain sin(pi*i/n) breaks
	// time-domain alias cancellation quietly rather than loudly.
	p.window = make([]float32, length)
	for i := range length {
		p.window[i] = float32(math.Sin((float64(i) + 0.5) * math.Pi / float64(2*length)))
	}
	return p
}

// imdctScratch is caller-owned working memory, sized to the largest transform
// the stream can ask for, so the hot path never allocates.
type imdctScratch struct {
	cr, ci, dr, di []float32
}

func newIMDCTScratch(maxLen int) *imdctScratch {
	m := maxLen / 2
	return &imdctScratch{
		cr: make([]float32, m),
		ci: make([]float32, m),
		dr: make([]float32, m),
		di: make([]float32, m),
	}
}

// imdct transforms the plan's L coefficients in spec into the L time samples
// of out, scaled by scale.
//
// The rotations stay in float64 against a float32 DFT: they are O(L) beside
// the transform's O(L log L), so the precision is close to free.
func (p *imdctPlan) imdct(spec, out []float32, scale float64, s *imdctScratch) {
	m, length := p.m, p.length
	cr, ci := s.cr[:m], s.ci[:m]
	dr, di := s.dr[:m], s.di[:m]

	// Pre-rotation: fold the coefficients from both ends into L/2 complex
	// samples, X[2j] against X[L-1-2j], and rotate.
	for j := range m {
		x1 := float64(spec[2*j])
		x2 := float64(spec[length-1-2*j])
		c, sn := p.twRe[j], p.twIm[j]
		cr[j] = float32(x1*c + x2*sn)
		ci[j] = float32(x2*c - x1*sn)
	}

	p.fp.Transform(dr, di, cr, ci)

	// Post-rotation. One complex value carries two DCT-IV outputs, C[2q] in
	// its real part and C[L-1-2q] in its imaginary part; reversed by
	// h[i] = C[L-1-i] they land at opposite ends of the output and walk
	// towards each other.
	for q := range m {
		re, im := float64(dr[q]), float64(di[q])
		c, sn := p.twRe[q], p.twIm[q]
		out[2*q] = float32(scale * (re*sn - im*c))
		out[length-1-2*q] = float32(scale * (re*c + im*sn))
	}
}

// overlapButterfly folds the boundary between the previous subframe's tail and
// this one's head, in place, over a region of `overlap` samples centred on the
// boundary.
//
// There is no long-start or long-stop window shape in this codec and no
// special case for a size change: the overlap is the shorter of the two
// subframe lengths, it is always centred exactly on the boundary, and the
// window is the sine window of that length. The minus sign on the first output
// line is the one that pairs with the transform's sign.
func overlapButterfly(region []float32, w []float32) {
	n := len(region)
	for r := 0; r < n/2; r++ {
		a, b := region[r], region[n-1-r]
		wa, wb := w[r], w[n-1-r]
		region[r] = a*wb - b*wa
		region[n-1-r] = a*wa + b*wb
	}
}
