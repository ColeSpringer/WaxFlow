package vorbis

import (
	"math"
	"testing"
)

// imdctDirect is the reference O(N^2) inverse MDCT, the definition the fast
// transform has to agree with. Vorbis had no such oracle: its inverse MDCT was
// pinned only through the decode differential and through the forward
// transform's round trip, both of which score the transform against something
// downstream of it rather than against what it is supposed to compute.
func imdctDirect(spec []float32, out []float64) {
	n := len(out)
	n0 := (float64(n)/2 + 1) / 2
	for i := range out {
		var sum float64
		for k := 0; k < n/2; k++ {
			sum += float64(spec[k]) *
				math.Cos(2*math.Pi/float64(n)*(float64(i)+n0)*(float64(k)+0.5))
		}
		out[i] = vorbisScale * sum
	}
}

// TestIMDCTMatchesDirect checks the fast transform against the direct sum at
// every block size the decoder builds a plan for.
func TestIMDCTMatchesDirect(t *testing.T) {
	for _, n := range []int{64, 128, 256, 512, 1024, 2048, 4096, 8192} {
		spec := make([]float32, n/2)
		state := uint32(0x12345)
		for i := range spec {
			state = state*1664525 + 1013904223
			spec[i] = float32(float64(int32(state)) / (1 << 24))
		}
		want := make([]float64, n)
		got := make([]float64, n)
		imdctDirect(spec, want)
		cr := make([]float64, n)
		ci := make([]float64, n)
		getPlan(n).imdct(spec, got, cr, ci)

		var peak, worst float64
		for i := range want {
			peak = max(peak, math.Abs(want[i]))
			worst = max(worst, math.Abs(got[i]-want[i]))
		}
		// The direct sum's own rounding grows with N; a part in 1e-11 of the
		// transform's peak is ~11 digits at N=8192.
		if worst > peak*1e-11 {
			t.Errorf("n=%d: worst deviation %g against peak %g", n, worst, peak)
		}
	}
}

// mdctDirect is the reference O(N^2) forward MDCT.
func mdctDirect(windowed []float32, spec []float64) {
	n := len(windowed)
	n0 := (float64(n)/2 + 1) / 2
	s := fwdScale(n)
	for k := range spec {
		var sum float64
		for i := 0; i < n; i++ {
			sum += float64(windowed[i]) *
				math.Cos(2*math.Pi/float64(n)*(float64(i)+n0)*(float64(k)+0.5))
		}
		spec[k] = s * sum
	}
}

// TestMDCTForwardMatchesDirect scores the analysis transform against the
// direct sum. The round-trip test beside it scores the PAIR, which a matched
// error in both directions passes; this scores the forward alone.
func TestMDCTForwardMatchesDirect(t *testing.T) {
	for _, n := range []int{256, 2048} {
		x := make([]float32, n)
		state := uint32(0x9e3779b9)
		for i := range x {
			state = state*1664525 + 1013904223
			x[i] = float32(float64(int32(state)) / (1 << 24))
		}
		want := make([]float64, n/2)
		got := make([]float32, n/2)
		mdctDirect(x, want)
		newMDCTForward(n).forward(x, got)

		var peak, worst float64
		for i := range want {
			peak = max(peak, math.Abs(want[i]))
			worst = max(worst, math.Abs(float64(got[i])-want[i]))
		}
		// float32 twiddles and a float32 DFT against a float64 reference sum.
		if worst > peak*1e-5 {
			t.Errorf("n=%d: worst deviation %g against peak %g", n, worst, peak)
		}
	}
}
