package opus

import (
	"math"
	"testing"
)

// TestMDCTForwardBackwardRoundTrip drives a signal through the forward MDCT and
// then the inverse (with the decoder's overlap-add arrangement) and requires the
// reconstruction to match the input at the transform's inherent delay. This is
// the analysis/synthesis identity the CELT encoder relies on: a matched pair
// means encode(forward) then decode(backward) recovers the audio.
func TestMDCTForwardBackwardRoundTrip(t *testing.T) {
	for _, LM := range []int{0, 1, 2, 3} {
		N := 120 << LM
		n := 2 * N
		const overlap = 120
		plan := newMDCTPlan(n)
		window := celtWindow(overlap)
		scr := newMDCTScratch(n / 4)

		frames := 40
		total := frames * N
		sig := make([]float32, total)
		var seed uint32 = 0x2545f491
		for i := range sig {
			seed = seed*1664525 + 1013904223
			sig[i] = float32(int32(seed)) / float32(1<<31)
		}

		// Analysis: frame k covers signal [k*N-overlap, (k+1)*N), the encoder's
		// [previous overlap history | N new] input buffer (zeros before 0).
		coeffs := make([][]float32, frames)
		inMem := make([]float32, N+overlap)
		for k := 0; k < frames; k++ {
			for j := 0; j < N+overlap; j++ {
				si := k*N - overlap + j
				if si >= 0 && si < len(sig) {
					inMem[j] = sig[si]
				} else {
					inMem[j] = 0
				}
			}
			out := make([]float32, N)
			plan.forward(inMem, out, 1, window, overlap, scr)
			coeffs[k] = out
		}

		// Synthesis: the decoder's sliding overlap-add buffer.
		const bufSize = 2048
		decodeMem := make([]float32, bufSize+overlap)
		recon := make([]float32, 0, total)
		for k := 0; k < frames; k++ {
			keep := bufSize - N + overlap
			copy(decodeMem[:keep], decodeMem[N:N+keep])
			base := bufSize - N
			plan.backward(coeffs[k], 1, decodeMem[base:], window, overlap, scr)
			recon = append(recon, decodeMem[base:base+N]...)
		}

		// Find the delay that best aligns reconstruction with input, then score.
		bestSNR, bestD := math.Inf(-1), 0
		for d := 0; d <= 2*overlap; d++ {
			var sigE, errE float64
			cnt := 0
			for i := 4 * N; i < total-2*overlap; i++ {
				if i+d >= len(recon) {
					break
				}
				s := float64(sig[i])
				r := float64(recon[i+d])
				sigE += s * s
				e := s - r
				errE += e * e
				cnt++
			}
			if cnt == 0 || errE == 0 {
				continue
			}
			snr := 10 * math.Log10(sigE/errE)
			if snr > bestSNR {
				bestSNR, bestD = snr, d
			}
		}
		t.Logf("LM=%d N=%d: reconstruction SNR %.1f dB at delay %d", LM, N, bestSNR, bestD)
		if bestSNR < 100 {
			t.Errorf("LM=%d: MDCT round-trip SNR %.1f dB too low (want >=100)", LM, bestSNR)
		}
	}
}
