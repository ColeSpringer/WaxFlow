package mp3

import (
	"math"
	"testing"
)

// TestAnalysisRoundTrip feeds PCM through the encoder analysis stage and
// back through the decoder's reconstruction (antialias, IMDCT overlap-add,
// synthesis filterbank), then checks that the output matches the input at
// the filterbank's fixed latency. A correct transform pair reconstructs to
// near machine precision (the only loss is float32 rounding); a sign, scale,
// or layout error blows the error up at every lag.
func TestAnalysisRoundTrip(t *testing.T) {
	const grans = 24
	const n = grans * 576
	in := make([]float32, n+576)
	for i := range in {
		// A couple of tones plus a slow sweep, so every subband sees energy.
		x := float64(i)
		in[i] = float32(0.3*math.Sin(2*math.Pi*440*x/44100) +
			0.2*math.Sin(2*math.Pi*3000*x/44100) +
			0.1*math.Sin(2*math.Pi*11000*x/44100))
	}

	var a analyzer
	var dec Decoder
	gi := grInfo{blockType: blockNormal}
	out := make([]float32, 0, n)
	for gr := 0; gr < grans; gr++ {
		var xr [576]float32
		a.granuleMDCT(in[gr*576:gr*576+576], &xr)

		var g granule
		g.spec[0] = xr
		antialias(&g, 0, 31)
		dec.hybrid(&gi, &g, 0, 0)
		var pcm [576]float32
		dec.synth(&g, 0, pcm[:])
		out = append(out, pcm[:]...)
	}

	// Find the lag (filterbank + MDCT latency) that best aligns output to
	// input, then report the reconstruction error there. The total latency
	// is the 481-sample polyphase group delay plus the MDCT's one-granule
	// (576-sample) overlap delay, so search past 1057.
	bestLag, bestRMS := -1, math.Inf(1)
	for lag := 0; lag < 1200; lag++ {
		var sum float64
		cnt := 0
		for i := 3 * 576; i+lag < n; i++ { // skip startup transient
			d := float64(out[i+lag]) - float64(in[i])
			sum += d * d
			cnt++
		}
		if cnt == 0 {
			continue
		}
		if rms := math.Sqrt(sum / float64(cnt)); rms < bestRMS {
			bestRMS, bestLag = rms, lag
		}
	}
	t.Logf("best lag = %d samples, RMS = %g", bestLag, bestRMS)
	// The transform pair reconstructs to float32 rounding (~1e-5); a scale or
	// sign slip lands orders of magnitude above this, so the tight bound is
	// what makes the check meaningful (a 0.1% scale error alone is ~1e-3).
	if bestRMS > 1e-5 {
		t.Fatalf("analysis/synthesis round-trip RMS %g too high (lag %d): transform pair is off", bestRMS, bestLag)
	}
}
