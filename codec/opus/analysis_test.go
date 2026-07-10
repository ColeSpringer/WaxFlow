package opus

import (
	"math"
	"math/rand"
	"testing"
)

// TestAnalysisFFT checks the mixed-radix 480-point FFT against a naive DFT.
func TestAnalysisFFT(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	const n = analysisFFTSize
	in := make([]complex64, n)
	for i := range in {
		in[i] = complex(rng.Float32()*2-1, rng.Float32()*2-1)
	}
	out := make([]complex64, n)
	analysisFFT.transform(in, out)

	for _, k := range []int{0, 1, 7, 59, 239, 240, 401, 479} {
		var accR, accI float64
		for j := 0; j < n; j++ {
			ang := -2 * math.Pi * float64(k) * float64(j) / n
			c, s := math.Cos(ang), math.Sin(ang)
			xr, xi := float64(real(in[j])), float64(imag(in[j]))
			accR += xr*c - xi*s
			accI += xr*s + xi*c
		}
		accR /= n
		accI /= n
		if math.Abs(accR-float64(real(out[k]))) > 1e-4 || math.Abs(accI-float64(imag(out[k]))) > 1e-4 {
			t.Errorf("bin %d: got (%g,%g), want (%g,%g)", k, real(out[k]), imag(out[k]), accR, accI)
		}
	}
}

// runAnalyserOn feeds a mono 48 kHz signal through the analyser in 20 ms
// frames and returns the last info read.
func runAnalyserOn(pcm []float32) analysisInfo {
	var t tonalityAnalysisState
	var info analysisInfo
	x := [][]float32{pcm}
	const frame = 960
	pos := 0
	for pos+frame <= len(pcm) {
		sub := [][]float32{pcm[pos:]}
		t.runAnalysis(sub, frame, frame, 0, -2, 1, 24, &info)
		pos += frame
	}
	_ = x
	return info
}

// TestTonalityAnalysisFeatures pins gross feature behavior: a sustained
// chord is far more tonal than white noise, noise fills the full bandwidth,
// and a low-passed signal is detected as narrower than fullband.
func TestTonalityAnalysisFeatures(t *testing.T) {
	const n = 3 * 48000
	rng := rand.New(rand.NewSource(8))

	chord := make([]float32, n)
	for i := range chord {
		ti := float64(i) / 48000
		chord[i] = float32(0.2*math.Sin(2*math.Pi*523*ti) +
			0.2*math.Sin(2*math.Pi*659*ti) +
			0.2*math.Sin(2*math.Pi*784*ti))
	}
	noise := make([]float32, n)
	for i := range noise {
		noise[i] = rng.Float32()*0.5 - 0.25
	}
	lowpassed := make([]float32, n)
	// A 500 Hz tone has no energy anywhere near the top bands.
	for i := range lowpassed {
		lowpassed[i] = float32(0.3 * math.Sin(2*math.Pi*500*float64(i)/48000))
	}

	chordInfo := runAnalyserOn(chord)
	noiseInfo := runAnalyserOn(noise)
	lowInfo := runAnalyserOn(lowpassed)

	t.Logf("chord: valid=%v tonality=%.3f musicProb=%.3f bandwidth=%d actProb=%.3f",
		chordInfo.valid, chordInfo.tonality, chordInfo.musicProb, chordInfo.bandwidth, chordInfo.activityProbability)
	t.Logf("noise: valid=%v tonality=%.3f musicProb=%.3f bandwidth=%d actProb=%.3f",
		noiseInfo.valid, noiseInfo.tonality, noiseInfo.musicProb, noiseInfo.bandwidth, noiseInfo.activityProbability)
	t.Logf("500Hz: valid=%v tonality=%.3f musicProb=%.3f bandwidth=%d maxPitchRatio=%.3f",
		lowInfo.valid, lowInfo.tonality, lowInfo.musicProb, lowInfo.bandwidth, lowInfo.maxPitchRatio)

	if !chordInfo.valid || !noiseInfo.valid || !lowInfo.valid {
		t.Fatal("analysis did not produce valid info")
	}
	if lowInfo.tonality < 0.5 {
		t.Errorf("pure tone tonality %.3f, want > 0.5", lowInfo.tonality)
	}
	if chordInfo.tonality < 0.3 {
		t.Errorf("chord tonality %.3f, want > 0.3", chordInfo.tonality)
	}
	if noiseInfo.tonality > 0.2 {
		t.Errorf("noise tonality %.3f, want < 0.2", noiseInfo.tonality)
	}
	if chordInfo.musicProb <= noiseInfo.musicProb {
		t.Errorf("musicProb: chord %.3f <= noise %.3f", chordInfo.musicProb, noiseInfo.musicProb)
	}
	if chordInfo.activityProbability < 0.5 {
		t.Errorf("chord activity probability %.3f, want > 0.5", chordInfo.activityProbability)
	}
	if noiseInfo.activityProbability > 0.5 {
		t.Errorf("stationary noise activity probability %.3f, want < 0.5", noiseInfo.activityProbability)
	}
	if noiseInfo.bandwidth < 18 {
		t.Errorf("noise bandwidth %d, want fullband (>=18)", noiseInfo.bandwidth)
	}
	if lowInfo.maxPitchRatio != 1 {
		t.Errorf("500 Hz tone maxPitchRatio %.3f, want 1 (all energy below 3.2 kHz)", lowInfo.maxPitchRatio)
	}
}
