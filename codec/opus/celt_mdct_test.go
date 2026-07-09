package opus

import (
	"math"
	"testing"
)

// TestMDCTLinearity checks the inverse MDCT is linear and index-safe across
// every CELT block size. The transform is a faithful port; end-to-end CELT
// decode against ffmpeg is the conformance oracle. This guards the porting
// mechanics (indexing, the incremental DFT modulo, the TDAC fold).
func TestMDCTLinearity(t *testing.T) {
	for _, n := range []int{240, 480, 960, 1920} {
		plan := newMDCTPlan(n)
		n2 := n / 2
		overlap := 120
		window := celtWindow(overlap)
		scr := newMDCTScratch(n / 4)
		outLen := overlap/2 + n2 + 8

		// Two random spectra and their weighted sum.
		x1 := make([]float32, n2)
		x2 := make([]float32, n2)
		var seed uint32 = 0x9e3779b9
		rnd := func() float32 {
			seed = seed*1664525 + 1013904223
			return float32(int32(seed))/float32(1<<31) - 0
		}
		for i := 0; i < n2; i++ {
			x1[i] = rnd()
			x2[i] = rnd()
		}
		const a, b = 0.3, -1.7
		sum := make([]float32, n2)
		for i := range sum {
			sum[i] = a*x1[i] + b*x2[i]
		}

		run := func(x []float32) []float32 {
			out := make([]float32, outLen)
			plan.backward(x, 1, out, window, overlap, scr)
			return out
		}
		o1 := run(x1)
		o2 := run(x2)
		os := run(sum)

		var maxErr float64
		for i := 0; i < outLen; i++ {
			want := a*float64(o1[i]) + b*float64(o2[i])
			got := float64(os[i])
			if math.IsNaN(got) || math.IsInf(got, 0) {
				t.Fatalf("n=%d idx=%d produced non-finite %v", n, i, got)
			}
			if e := math.Abs(want - got); e > maxErr {
				maxErr = e
			}
		}
		if maxErr > 1e-3 {
			t.Errorf("n=%d: inverse MDCT not linear, max error %g", n, maxErr)
		}
	}
}
