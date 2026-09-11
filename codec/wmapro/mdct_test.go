//go:build !wmaprotablesgen

package wmapro

import (
	"math"
	"math/rand/v2"
	"testing"
)

// dct4 is the definition in section 13.3 of docs/notes/wma-pro-bitstream.md,
// written out with no factorization, so the fast path has something to be
// right against.
func dct4(x []float64) []float64 {
	n := len(x)
	out := make([]float64, n)
	for m := range out {
		var s float64
		for k := 0; k < n; k++ {
			s += x[k] * math.Cos(math.Pi*float64(2*k+1)*float64(2*m+1)/float64(4*n))
		}
		out[m] = s
	}
	return out
}

// textbookIMDCT is the 2L-point inverse MDCT the notes state the half-output
// against.
func textbookIMDCT(x []float64) []float64 {
	n := len(x)
	out := make([]float64, 2*n)
	for i := range out {
		var s float64
		for k := 0; k < n; k++ {
			s += x[k] * math.Cos(math.Pi/float64(n)*(float64(i)+0.5+float64(n)/2)*(float64(k)+0.5))
		}
		out[i] = s
	}
	return out
}

// TestIMDCTMatchesAllThreeStatements scores the fast transform against every
// form the notes give it in, rather than against any one of them. A sign error
// or a factor of two cannot survive all three, and the notes say so precisely
// because those are the two failures that hide.
func TestIMDCTMatchesAllThreeStatements(t *testing.T) {
	for _, length := range []int{64, 128, 256, 1024, 2048} {
		rng := rand.New(rand.NewPCG(1, uint64(length)))
		spec := make([]float32, length)
		x := make([]float64, length)
		for i := range spec {
			spec[i] = float32(rng.Float64()*2 - 1)
			x[i] = float64(spec[i])
		}
		const scale = 0.25
		got := IMDCTForTest(spec, scale)

		c := dct4(x)
		y := textbookIMDCT(x)
		var worstA, worstB float64
		for i := range got {
			// As a reversed DCT-IV.
			worstA = max(worstA, math.Abs(float64(got[i])-scale*c[length-1-i]))
			// As the NEGATED middle half of the textbook inverse transform.
			worstB = max(worstB, math.Abs(float64(got[i])+scale*y[length/2+i]))
		}
		// The tolerance is float32 rounding over a transform of this length,
		// not a slack bound: a sign error scores twice the peak here.
		tol := 2e-5 * float64(length)
		if worstA > tol || worstB > tol {
			t.Errorf("length %d: reversed DCT-IV off by %g, negated textbook half off by %g (tol %g)",
				length, worstA, worstB, tol)
		}
	}
}

// forwardButterfly is the inverse of overlapButterfly. The pair is an
// orthogonal rotation, so the analysis direction is the transpose; writing it
// out is what lets the reconstruction test below exist with no encoder.
func forwardButterfly(region []float32, w []float32) {
	n := len(region)
	for r := 0; r < n/2; r++ {
		a, b := region[r], region[n-1-r]
		wa, wb := w[r], w[n-1-r]
		region[r] = a*wb + b*wa
		region[n-1-r] = -a*wa + b*wb
	}
}

// forwardMDCT inverts the transform of section 13.3: with h the folded half
// and X the coefficients, h = scale * reverse(DCT-IV(X)) inverts to
// X = DCT-IV(reverse(h)) / scale, because DCT-IV is its own inverse up to L/2.
func forwardMDCT(h []float32, scale float64) []float32 {
	n := len(h)
	rev := make([]float64, n)
	for i := range rev {
		rev[i] = float64(h[n-1-i])
	}
	c := dct4(rev)
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(c[i] * 2 / float64(n) / scale)
	}
	return out
}

// TestTransformReconstructsThroughMixedBlockSizes is the property that makes
// the window transition rule checkable with no oracle and no encoder: analysis
// then synthesis returns the signal, across subframes of DIFFERENT lengths.
//
// It is the one test in this package that would catch a wrong overlap length,
// a window centred on the wrong sample, or a butterfly whose two signs are
// swapped, none of which a differential can separate from a coefficient bug.
func TestTransformReconstructsThroughMixedBlockSizes(t *testing.T) {
	const frameLen = 512
	tilings := [][]int{
		{512},
		{256, 256},
		{128, 64, 64, 128, 128},
		{64, 64, 128, 256},
		{512},
	}
	rng := rand.New(rand.NewPCG(7, 9))
	total := len(tilings) * frameLen
	src := make([]float32, total+2*frameLen)
	for i := range src {
		src[i] = float32(rng.Float64()*2 - 1)
	}

	// Analysis: fold at every subframe boundary, then transform each subframe.
	work := append([]float32(nil), src...)
	coefs := make([][]float32, 0, 16)
	lens := make([]int, 0, 16)
	prev := frameLen
	at := frameLen // leave a frame of lead-in, as a decode does
	for _, tiling := range tilings {
		for _, length := range tiling {
			ov := min(prev, length)
			forwardButterfly(work[at-ov/2:at+ov/2], planFor(ov).window)
			prev = length
			at += length
		}
	}
	at = frameLen
	prev = frameLen
	for _, tiling := range tilings {
		for _, length := range tiling {
			coefs = append(coefs, forwardMDCT(work[at:at+length], 1))
			lens = append(lens, length)
			at += length
		}
	}

	// Synthesis: the decoder's own path.
	out := make([]float32, len(src))
	scratch := newIMDCTScratch(frameLen)
	at, prev = frameLen, frameLen
	for i, length := range lens {
		planFor(length).imdct(coefs[i], out[at:at+length], 1, scratch)
		ov := min(prev, length)
		overlapButterfly(out[at-ov/2:at+ov/2], planFor(ov).window)
		prev = length
		at += length
	}

	// The first and last half-frames are lead-in and lead-out and never
	// reconstruct; everything between must.
	lo, hi := frameLen+frameLen/2, at-frameLen/2
	var worst float64
	for i := lo; i < hi; i++ {
		worst = max(worst, math.Abs(float64(out[i])-float64(src[i])))
	}
	if worst > 1e-4 {
		t.Errorf("reconstruction off by %g over [%d,%d)", worst, lo, hi)
	}
}
