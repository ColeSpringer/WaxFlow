package aac

import (
	"math"
	"testing"
)

// qmfTestSignal is a deterministic multi-sine test signal, bandlimited to
// keep energy off the bank edges. eval returns the value at a possibly
// fractional sample position, which is what lets the upsampling test
// compare against an exact reference at half-sample phases.
type qmfTestSignal struct{ freqs, phases, amps []float64 }

func newQMFTestSignal() *qmfTestSignal {
	s := &qmfTestSignal{}
	// Frequencies in radians per sample, spread across the band but inside
	// 0.9*pi so prototype rolloff at the top edge does not dominate the SNR.
	for i := 1; i <= 12; i++ {
		s.freqs = append(s.freqs, 0.07*float64(i)*math.Pi)
		s.phases = append(s.phases, float64(i)*1.7)
		s.amps = append(s.amps, 0.5/float64(i))
	}
	return s
}

func (s *qmfTestSignal) eval(pos float64) float64 {
	var v float64
	for i := range s.freqs {
		v += s.amps[i] * math.Sin(s.freqs[i]*pos+s.phases[i])
	}
	return v
}

// snrAgainst returns the SNR in dB of got against the signal evaluated at
// (n - delay) * step, skipping a settling head.
func snrAgainst(sig *qmfTestSignal, got []float32, delay float64, step float64) float64 {
	var sigPow, errPow float64
	for n := 1500; n < len(got); n++ {
		want := sig.eval((float64(n) - delay) * step)
		e := float64(got[n]) - want
		sigPow += want * want
		errPow += e * e
	}
	if errPow == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(sigPow/errPow)
}

// TestQMFUpsampling is the decoder shape: 32-band analysis at the core
// rate, zero high bands, 64-band synthesis at the doubled rate. The output
// must be the 2x-interpolated input at the pinned integer chain delay of
// 578 output samples (ffmpeg's, verified tap-for-tap): the SNR collapses
// if the delay moves, so checking at the pin covers both.
func TestQMFUpsampling(t *testing.T) {
	sig := newQMFTestSignal()
	const frames = 300
	in := make([]float32, 32*frames)
	for n := range in {
		in[n] = float32(sig.eval(float64(n)))
	}
	var ana qmfAnalyzer32
	var syn qmfSynthesizer64
	out := make([]float32, 64*frames)
	var re32, im32 [32]float32
	var re, im [64]float32
	for s := 0; s < frames; s++ {
		ana.analyze(in[32*s:32*s+32], re32[:], im32[:])
		clear(re[:])
		clear(im[:])
		copy(re[:32], re32[:])
		copy(im[:32], im32[:])
		syn.synthesize(re[:], im[:], out[64*s:64*s+64])
	}
	// The reference advances half an input sample per output sample. The
	// spec's restart-modulation pair keeps the mixed-rate chain alias-free,
	// which is what holds the round trip near 60 dB.
	snr := snrAgainst(sig, out, 578, 0.5)
	t.Logf("upsample snr=%.1f dB at the pinned 578-sample delay", snr)
	if snr < 55 {
		t.Errorf("upsampling SNR %.1f dB at delay 578, want >= 55", snr)
	}
}
