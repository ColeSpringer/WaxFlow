package aac

import (
	"math"
	"testing"
)

// imdctDirect is the reference O(N²) inverse MDCT, kept as the oracle for
// the fast FFT-based transform.
func imdctDirect(spec, out []float64) {
	n := len(out)
	n0 := (float64(n)/2 + 1) / 2
	for i := 0; i < n; i++ {
		var sum float64
		for k := 0; k < n/2; k++ {
			sum += spec[k] * math.Cos(2*math.Pi/float64(n)*(float64(i)+n0)*(float64(k)+0.5))
		}
		out[i] = 2.0 / float64(n) * sum
	}
}

// TestIMDCTFastMatchesDirect checks the FFT-based IMDCT reproduces the
// direct transform for both sizes on pseudo-random spectra.
func TestIMDCTFastMatchesDirect(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan *imdctPlan
	}{
		{"long", planLong},
		{"short", planShort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := tc.plan.n
			spec := make([]float64, n/2)
			state := uint32(0x12345)
			for i := range spec {
				state = state*1664525 + 1013904223
				spec[i] = float64(int32(state)) / (1 << 24)
			}
			want := make([]float64, n)
			got := make([]float64, n)
			imdctDirect(spec, want)
			tc.plan.imdct(spec, got)
			var maxErr float64
			for i := range want {
				if e := math.Abs(got[i] - want[i]); e > maxErr {
					maxErr = e
				}
			}
			if maxErr > 1e-9 {
				t.Fatalf("%s IMDCT max error %g exceeds 1e-9", tc.name, maxErr)
			}
		})
	}
}
