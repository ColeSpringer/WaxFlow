package aac

import (
	"math"
	"testing"
)

// TestQMFEncoderAnalysisReconstruction is the encoder shape: 64-band
// analysis at the full rate straight into the decoder's 64-band synthesis.
// Near-perfect reconstruction at the pinned integer delay is what makes
// the encoder's measured subband energies live on the decoder's scale (a
// wrong gain or a wrong modulation family collapses the SNR).
func TestQMFEncoderAnalysisReconstruction(t *testing.T) {
	sig := newQMFTestSignal()
	const frames = 300
	in := make([]float32, 64*frames)
	for n := range in {
		in[n] = float32(sig.eval(float64(n)))
	}
	var ana qmfAnalyzer64
	var syn qmfSynthesizer64
	out := make([]float32, 64*frames)
	var re, im [64]float32
	for s := 0; s < frames; s++ {
		ana.analyze(in[64*s:64*s+64], re[:], im[:])
		syn.synthesize(re[:], im[:], out[64*s:64*s+64])
	}
	best, bestSNR := 0, math.Inf(-1)
	for d := 500; d <= 800; d++ {
		if snr := snrAgainst(sig, out, float64(d), 1); snr > bestSNR {
			best, bestSNR = d, snr
		}
	}
	t.Logf("encoder analysis+synthesis: best snr=%.1f dB at delay %d", bestSNR, best)
	snr := snrAgainst(sig, out, qmfEncPairDelay, 1)
	if snr < 55 {
		t.Errorf("reconstruction SNR %.1f dB at the pinned delay %d, want >= 55 (best %.1f at %d)",
			snr, qmfEncPairDelay, bestSNR, best)
	}
}
