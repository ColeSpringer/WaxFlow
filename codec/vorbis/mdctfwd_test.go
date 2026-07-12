package vorbis

import (
	"math"
	"math/rand"
	"testing"
)

// TestMDCTForwardInverseTDAC drives the forward MDCT into the decoder's own
// inverse and confirms time-domain aliasing cancellation: three overlapping
// all-long blocks, analysis-windowed and forward-transformed, then inverse-
// transformed, synthesis-windowed, and overlap-added, reconstruct the input in
// the fully overlapped interior. This validates the transform, the fwdScale
// normalization, and window agreement together.
func TestMDCTForwardInverseTDAC(t *testing.T) {
	for _, n := range []int{256, 2048, 4096} {
		t.Run("", func(t *testing.T) {
			hop := n / 2
			blocks := 4
			total := (blocks + 1) * hop
			rng := rand.New(rand.NewSource(42))
			x := make([]float32, total)
			for i := range x {
				// A mix of tones plus noise: broadband content stresses every bin.
				x[i] = float32(0.3*math.Sin(2*math.Pi*float64(i)*5/float64(n)) +
					0.2*math.Sin(2*math.Pi*float64(i)*37.5/float64(n)) +
					0.1*(rng.Float64()*2-1))
			}
			win := fullWindow(n)
			fwd := newMDCTForward(n)
			plan := getPlan(n)

			recon := make([]float64, total)
			wbuf := make([]float32, n)
			spec := make([]float32, n/2)
			tbuf := make([]float64, n)
			cr := make([]float64, n)
			ci := make([]float64, n)
			for b := 0; b < blocks; b++ {
				off := b * hop
				for i := 0; i < n; i++ {
					wbuf[i] = x[off+i] * win[i]
				}
				fwd.forward(wbuf, spec)
				plan.imdct(spec, tbuf, cr, ci)
				// Synthesis window (all-long: both neighbours size n).
				for i := 0; i < n; i++ {
					recon[off+i] += tbuf[i] * float64(win[i])
				}
			}

			// The interior [hop, blocks*hop) is covered by two blocks and must
			// reconstruct to full precision.
			var maxErr float64
			for i := hop; i < blocks*hop; i++ {
				e := math.Abs(recon[i] - float64(x[i]))
				if e > maxErr {
					maxErr = e
				}
			}
			if maxErr > 1e-5 {
				t.Fatalf("n=%d TDAC max reconstruction error %.3g exceeds 1e-5", n, maxErr)
			}
		})
	}
}
