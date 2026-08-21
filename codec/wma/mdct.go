//go:build !wmatablesgen

package wma

import (
	"math"
	"sync"

	"github.com/colespringer/waxflow/dsp/fft"
)

// The inverse MDCT and the block windows.
//
// WMA's filterbank is the plain TDAC MDCT: blockLen coefficients become
// 2*blockLen time samples,
//
//	y[m] = -Σ_{k<M} X[k] cos((π/M)(m + 1/2 + M/2)(k + 1/2)),  M = blockLen
//
// computed here through an M/2-point complex DFT, which is N/4 of the output
// length. The reduction is the standard one and it falls out of two steps.
//
// First, the middle half of the output is a DCT-IV of the coefficients, read
// backwards. Substituting n = M/2 + h into the definition splits the phase
// into (π/M)(h + 1/2)(k + 1/2) plus π(k + 1/2), and the second term turns
// every cosine into ±sine, which leaves a DST-IV of (-1)^k·X[k]; that DST-IV
// is the DCT-IV of X itself, reversed, and its sign cancels the transform's
// leading minus, so
//
//	y[M/2 + h] = C[M - 1 - h],  C = DCT-IV(X)
//
// Second, the outer quarters are not computed at all. The transform's own
// symmetries give them for free: the first half is antisymmetric about its
// centre and the second half symmetric about its centre, so
//
//	y[M/2 - 1 - q] = -y[M/2 + q]      and      y[3M/2 + r] = y[3M/2 - 1 - r]
//
// Third, the DCT-IV of length M is one M/2-point complex DFT between two
// rotations by exp(-2πi(j + 1/8)/N), pairing X[2j] with X[M-1-2j]: the pair's
// two phases differ by π(2n + 1/2), so one complex output carries C[2n] in its
// real part and C[M-1-2n] in its imaginary part. codec/opus/celt_mdct.go
// factors CELT's MDCT the same way and tabulates the same angle.
//
// The transform this replaced was a 2M-point complex DFT, half of whose input
// was zero padding: eight times the transform length for the same answer.
// TestIMDCTMatchesTheNaiveTransform is what says the two agree, since it
// scores against the definition written out rather than against either.
//
// The leading minus is the format's convention, not a slip. The encoder's
// forward transform carries the same sign, so the pair still reconstructs; a
// decoder without it inverts the polarity of everything, and because the
// inversion is downstream of the reconstruction it takes the generated noise
// with it. That is how it was pinned: an exact -1.000000 correlation against
// the oracle on all eighteen cells of the analysis corpus, ten of them
// noise-coded, where a mis-read coefficient sign bit (the other way to invert
// a decode) would have left the noise alone and scored short of -1.
//
// The sum is left unnormalised: the 4/N that makes it the inverse of the
// unnormalised forward transform is 1/n4, which section 9 of the format folds
// into the coefficient scale along with the 1/32768 and v1's extra sqrt(n4),
// so the whole normalisation arrives in one multiply before the transform
// rather than in three places around it.
// TestIMDCTReconstructsThroughOverlap pins that convention without an oracle.

// imdctPlan holds the read-only rotation table and DFT plan for one block
// size. Plans are immutable after construction and shared across sessions.
type imdctPlan struct {
	blockLen int // M, the number of coefficients
	m        int // M/2, the DFT length
	// tw is exp(-2*pi*i*(j + 1/8)/(2M)), the rotation both the pre- and the
	// post-pass apply. One table serves both: the two passes index it by the
	// DFT input and output position, which run over the same M/2 values.
	twRe, twIm []float64
	fp         *fft.Plan
	// window is the rising half-sample sine, blockLen entries. The falling
	// half is this reversed, which is why the right half of a block is
	// windowed backwards rather than from a second table.
	window []float32
}

var (
	planMu    sync.Mutex
	planCache = map[int]*imdctPlan{}
)

// planFor returns the plan for a block of blockLen coefficients.
func planFor(blockLen int) *imdctPlan {
	planMu.Lock()
	defer planMu.Unlock()
	if p, ok := planCache[blockLen]; ok {
		return p
	}
	p := newIMDCTPlan(blockLen)
	planCache[blockLen] = p
	return p
}

func newIMDCTPlan(blockLen int) *imdctPlan {
	m := blockLen / 2
	p := &imdctPlan{blockLen: blockLen, m: m, fp: fft.NewPlan(m)}
	p.twRe = make([]float64, m)
	p.twIm = make([]float64, m)
	for j := range m {
		sn, c := math.Sincos(2 * math.Pi * (float64(j) + 0.125) / float64(2*blockLen))
		p.twRe[j], p.twIm[j] = c, sn
	}
	// The half-sample sine. Only this form satisfies the Princen-Bradley
	// condition w[i]^2 + w[n-1-i]^2 = 1, so the plain sin(pi*i/n) breaks
	// time-domain alias cancellation quietly rather than loudly.
	p.window = make([]float32, blockLen)
	for i := range blockLen {
		p.window[i] = float32(math.Sin((float64(i) + 0.5) * math.Pi / float64(2*blockLen)))
	}
	return p
}

// imdctScratch is caller-owned working memory, sized to the largest transform
// the stream can ask for, so the hot path never allocates. maxN is the largest
// TIME length (2*blockLen); the buffers hold a quarter of it, which is the DFT
// length.
type imdctScratch struct {
	cr, ci, dr, di []float32
}

func newIMDCTScratch(maxN int) *imdctScratch {
	n4 := maxN / 4
	return &imdctScratch{
		cr: make([]float32, n4),
		ci: make([]float32, n4),
		dr: make([]float32, n4),
		di: make([]float32, n4),
	}
}

// imdct transforms the blockLen coefficients in spec into the 2*blockLen time
// samples of out.
//
// The rotations stay in float64 against a float32 DFT: they are O(M) beside
// the transform's O(M log M), so the precision is close to free.
func (p *imdctPlan) imdct(spec, out []float32, s *imdctScratch) {
	m, blockLen := p.m, p.blockLen
	cr, ci := s.cr[:m], s.ci[:m]
	dr, di := s.dr[:m], s.di[:m]

	// Pre-rotation: fold the coefficients from both ends into M/2 complex
	// samples, X[2j] against X[M-1-2j], and rotate.
	for j := range m {
		x1 := float64(spec[2*j])
		x2 := float64(spec[blockLen-1-2*j])
		c, sn := p.twRe[j], p.twIm[j]
		cr[j] = float32(x1*c + x2*sn)
		ci[j] = float32(x2*c - x1*sn)
	}

	p.fp.Transform(dr, di, cr, ci)

	// Post-rotation, straight into the middle half of the output. One complex
	// value carries two DCT-IV outputs, C[2q] in its real part and C[M-1-2q]
	// in its imaginary part; reversed by the line above, they land at opposite
	// ends of that half and walk towards each other.
	for q := range m {
		re, im := float64(dr[q]), float64(di[q])
		c, sn := p.twRe[q], p.twIm[q]
		out[m+2*q] = float32(re*sn - im*c)
		out[3*m-1-2*q] = float32(re*c + im*sn)
	}

	// The outer quarters, by the transform's own symmetries. Both read only
	// from the middle half, which is now complete.
	for q := range m {
		out[m-1-q] = -out[m+q]
	}
	for r := range m {
		out[3*m+r] = out[3*m-1-r]
	}
}

// overlapAdd folds one block's 2*blockLen transform samples into the frame
// accumulator at dst, which must have 2*blockLen samples of room.
//
// The two halves are asymmetric, and that asymmetry is what makes variable
// block lengths work without a start/stop window signal: the left half's
// leading edge is untouched (the previous block already finalised those
// samples, and writing there destroys finished output) and its trailing edge
// is copied, while the right half's leading edge is copied and its trailing
// edge is zeroed.
func overlapAdd(dst, block []float32, blockLen, prevLen, nextLen int, plan, prevPlan, nextPlan *imdctPlan) {
	w := plan.window

	// Left half, added into what the previous block left behind.
	if blockLen <= prevLen {
		for i := range blockLen {
			dst[i] += block[i] * w[i]
		}
	} else {
		m := (blockLen - prevLen) / 2
		pw := prevPlan.window
		for i := range prevLen {
			dst[m+i] += block[m+i] * pw[i]
		}
		copy(dst[m+prevLen:blockLen], block[m+prevLen:blockLen])
	}

	// Right half, stored rather than added, since nothing has written there.
	right := dst[blockLen : 2*blockLen]
	src := block[blockLen : 2*blockLen]
	if blockLen <= nextLen {
		for i := range blockLen {
			right[i] = src[i] * w[blockLen-1-i]
		}
	} else {
		m := (blockLen - nextLen) / 2
		nw := nextPlan.window
		copy(right[:m], src[:m])
		for i := range nextLen {
			right[m+i] = src[m+i] * nw[nextLen-1-i]
		}
		// The tail the next block's window will not reach. Nothing has written
		// there either, so this restates the accumulator's invariant rather
		// than enforcing it; see decodeBlock's uncoded-block note.
		clear(right[m+nextLen:])
	}
}
