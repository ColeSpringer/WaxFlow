//go:build !wmatablesgen

package wma

import (
	"math"
	"math/rand/v2"
	"testing"
)

// naiveIMDCT is the transform written out, the definition the fast one has to
// agree with, leading minus and all.
func naiveIMDCT(spec []float32) []float32 {
	m := len(spec)
	out := make([]float32, 2*m)
	for n := range out {
		var sum float64
		for k := range m {
			sum += float64(spec[k]) *
				math.Cos(math.Pi/float64(m)*(float64(n)+0.5+float64(m)/2)*(float64(k)+0.5))
		}
		out[n] = float32(-sum)
	}
	return out
}

func TestIMDCTMatchesTheNaiveTransform(t *testing.T) {
	for _, n := range []int{128, 256, 512, 1024} {
		spec := make([]float32, n)
		rng := rand.New(rand.NewPCG(1, uint64(n)))
		for i := range spec {
			spec[i] = float32(rng.NormFloat64())
		}
		want := naiveIMDCT(spec)
		got := make([]float32, 2*n)
		planFor(n).imdct(spec, got, newIMDCTScratch(2*n))
		var peak float64
		for i := range want {
			peak = max(peak, math.Abs(float64(want[i])))
		}
		var worst float64
		for i := range got {
			worst = max(worst, math.Abs(float64(got[i]-want[i])))
		}
		// float32 twiddles against a float64 reference sum: the residual is
		// rounding, six decades below the transform's own peak.
		if worst > peak*1e-5 {
			t.Errorf("n=%d: worst deviation %g against peak %g", n, worst, peak)
		}
	}
}

// TestIMDCTReconstructsThroughOverlap is what pins the normalisation the
// format leaves to the implementation. Three constants stack -- the
// transform's own convention, the 1/32768, and mdctNorm -- and which part
// belongs to the transform depends on how the transform is defined. This runs
// a forward MDCT written from the definition, the decoder's own inverse and
// overlap-add, and asserts perfect reconstruction, which holds for exactly one
// scale: the 1/n4 that mdctNorm carries. Getting it wrong by any factor fails
// here rather than as a level error in a differential.
func TestIMDCTReconstructsThroughOverlap(t *testing.T) {
	const n = 256
	rng := rand.New(rand.NewPCG(7, 7))
	src := make([]float64, 6*n)
	for i := range src {
		src[i] = rng.NormFloat64()
	}
	plan := planFor(n)
	w := plan.window
	scratch := newIMDCTScratch(2 * n)
	acc := make([]float32, len(src)+2*n)
	block := make([]float32, 2*n)
	spec := make([]float32, n)
	// Blocks hop by n and overlap by n, the same 50% lapping a frame's blocks
	// have.
	for b := 0; b+2*n <= len(src); b += n {
		for k := range n {
			var sum float64
			for i := range 2 * n {
				var win float64
				if i < n {
					win = float64(w[i])
				} else {
					win = float64(w[2*n-1-i])
				}
				sum += src[b+i] * win *
					math.Cos(math.Pi/float64(n)*(float64(i)+0.5+float64(n)/2)*(float64(k)+0.5))
			}
			// The decoder's scale, applied where the decoder applies it. The
			// forward transform carries the same leading minus the inverse
			// does, which is why the pair reconstructs rather than inverting.
			spec[k] = float32(-sum * mdctNorm(n, true))
		}
		plan.imdct(spec, block, scratch)
		overlapAdd(acc[b:b+2*n], block, n, n, n, plan, plan, plan)
	}
	// The first and last blocks have no partner, so only the fully lapped
	// interior reconstructs.
	var worst float64
	for i := n; i < len(src)-n; i++ {
		worst = max(worst, math.Abs(float64(acc[i])-src[i]))
	}
	if worst > 1e-4 {
		t.Fatalf("worst reconstruction error %g; the transform normalisation is off", worst)
	}
}

// A window that does not satisfy the Princen-Bradley condition breaks
// time-domain alias cancellation quietly rather than loudly, so the half
// sample offset is asserted rather than assumed.
func TestWindowSatisfiesPrincenBradley(t *testing.T) {
	for _, n := range []int{128, 512, 2048} {
		w := planFor(n).window
		if len(w) != n {
			t.Fatalf("n=%d: window has %d entries", n, len(w))
		}
		for i := range n {
			// w[i]^2 + w[n-1-i]^2 == 1, the second half being the first
			// reversed.
			if s := float64(w[i])*float64(w[i]) + float64(w[n-1-i])*float64(w[n-1-i]); math.Abs(s-1) > 1e-6 {
				t.Fatalf("n=%d i=%d: w^2 sum %v", n, i, s)
			}
		}
		// The plain sin(pi*i/n) would start at exactly 0; this one does not.
		if w[0] == 0 {
			t.Errorf("n=%d: window starts at 0, which is the whole-sample form", n)
		}
	}
}

// analysisWindow is the window an encoder pairs with overlapAdd's synthesis
// side for a block of length b between neighbours of length prev and next: the
// long-start/long-stop shape, flat where the neighbour is shorter and zero
// where the neighbour has already finalised the samples.
func analysisWindow(b, prev, next int) []float64 {
	w := make([]float64, 2*b)
	wb := planFor(b).window
	if b <= prev {
		for i := range b {
			w[i] = float64(wb[i])
		}
	} else {
		m := (b - prev) / 2
		pw := planFor(prev).window
		for i := range prev {
			w[m+i] = float64(pw[i])
		}
		for i := m + prev; i < b; i++ {
			w[i] = 1
		}
	}
	if b <= next {
		for i := range b {
			w[b+i] = float64(wb[b-1-i])
		}
	} else {
		m := (b - next) / 2
		nw := planFor(next).window
		for i := range m {
			w[b+i] = 1
		}
		for i := range next {
			w[b+m+i] = float64(nw[next-1-i])
		}
	}
	return w
}

// TestOverlapAddReconstructsAcrossBlockSizeChanges is the only test the
// asymmetric window will ever get. Neither ffmpeg encoder sets the
// variable-block-length bit, so no generated corpus contains a frame with more
// than one block size in it, and the differential cannot reach a transition at
// all.
//
// What holds without an oracle is time-domain alias cancellation: analysis and
// synthesis together must return the signal. That is a sharp test of exactly
// the part that is easy to get wrong. The left half's leading region must be
// LEFT ALONE rather than written -- the previous block already finalised those
// samples -- its middle must be added under the PREVIOUS block's window rather
// than this one's, and its trailing region stored rather than added; the right
// half is the mirror, stored throughout, with its tail zeroed. Any one of
// those wrong leaves uncancelled alias at every transition, which is quiet
// enough to pass a listening test and loud enough to fail this one.
func TestOverlapAddReconstructsAcrossBlockSizeChanges(t *testing.T) {
	const frameLen = 2048
	// Every transition: steady long, shrinking, growing, and frames that
	// change size twice in both directions. The 2048 -> 512 -> 128 run is
	// there deliberately: the right half's trailing region is only reachable
	// when the next block is more than three times shorter, so a sequence that
	// never drops by more than half leaves the zeroing of it untested.
	frames := [][]int{
		{2048},
		{1024, 512, 512},
		{512, 512, 1024},
		{512, 256, 256, 512, 512},
		{2048},
		{512, 128, 128, 128, 128, 512, 512},
		{2048},
		{2048},
	}
	var flat []int
	for _, f := range frames {
		for _, b := range f {
			if b > frameLen {
				t.Fatalf("block %d longer than the frame", b)
			}
			flat = append(flat, b)
		}
	}
	rng := rand.New(rand.NewPCG(11, 13))
	src := make([]float64, (len(frames)+1)*frameLen)
	for i := range src {
		src[i] = rng.NormFloat64()
	}

	acc := make([]float32, 2*frameLen)
	out := make([]float32, 0, len(src))
	scratch := newIMDCTScratch(2 * frameLen)
	block := make([]float32, 2*frameLen)
	spec := make([]float32, frameLen)
	bi := 0
	for f, frame := range frames {
		blockPos := 0
		if n := sum(frame); n != frameLen {
			t.Fatalf("frame %d blocks sum to %d", f, n)
		}
		for _, b := range frame {
			prev, next := flat[max(bi-1, 0)], flat[min(bi+1, len(flat)-1)]
			at := frameLen/2 + blockPos - b/2
			abs := f*frameLen + at
			w := analysisWindow(b, prev, next)
			for k := range b {
				var s float64
				for i := range 2 * b {
					s += src[abs+i] * w[i] *
						math.Cos(math.Pi/float64(b)*(float64(i)+0.5+float64(b)/2)*(float64(k)+0.5))
				}
				spec[k] = float32(-s * mdctNorm(b, true))
			}
			planFor(b).imdct(spec[:b], block[:2*b], scratch)
			overlapAdd(acc[at:at+2*b], block[:2*b], b, prev, next,
				planFor(b), planFor(prev), planFor(next))
			blockPos += b
			bi++
		}
		out = append(out, acc[:frameLen]...)
		copy(acc[:frameLen], acc[frameLen:])
		clear(acc[frameLen:])
	}
	// The opening and closing frames have no partner on one side, so only the
	// fully lapped interior reconstructs.
	var worst float64
	var worstAt int
	for i := frameLen; i < (len(frames)-1)*frameLen; i++ {
		if d := math.Abs(float64(out[i]) - src[i]); d > worst {
			worst, worstAt = d, i
		}
	}
	if worst > 1e-4 {
		t.Fatalf("worst reconstruction error %g at %d (frame %d)", worst, worstAt, worstAt/frameLen)
	}
}

func sum(v []int) int {
	n := 0
	for _, x := range v {
		n += x
	}
	return n
}
