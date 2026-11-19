package testutil

import (
	"math"
	"testing"
)

// Fixtures and measures for what an encoder does in its first moments, at
// the head of a stream and behind a restart. Both inputs are 48 kHz and
// periodic, so a stretch one period on holds the same samples in the same
// 20 ms frames, coded in steady state: the stream is its own reference.

// StartupPeriod is the period of StartupTone and StartupPlucked in samples:
// 3 s, a whole number of 20 ms frames.
const StartupPeriod = 3 * 48000

// StartupTone is a mix of three tones, the top one amplitude modulated at
// 1 Hz, with a phase offset between channels.
func StartupTone(frames, chans int) [][]float32 {
	out := make([][]float32, chans)
	for c := range out {
		out[c] = make([]float32, frames)
		for i := range out[c] {
			x := float64(i%StartupPeriod) / 48000
			out[c][i] = float32(0.25*math.Sin(2*math.Pi*220*x) + 0.2*math.Sin(2*math.Pi*1337*x+float64(c)) +
				0.1*math.Sin(2*math.Pi*5201*x)*(0.5+0.5*math.Sin(2*math.Pi*x)))
		}
	}
	return out
}

// StartupPlucked is a run of decaying six-harmonic notes, one every 375 ms,
// over a faint noise floor. It starts a third of the way into a note: an
// onset on a stream's very first frame is coded with no history by any
// encoder, which is not what these fixtures measure.
func StartupPlucked(frames, chans int) [][]float32 {
	notes := [...]float64{130.81, 164.81, 196.0, 261.63, 329.63, 392.0, 220.0, 293.66}
	const spacing = StartupPeriod / len(notes)
	out := make([][]float32, chans)
	seed := uint32(12345)
	for c := range out {
		period := make([]float32, StartupPeriod)
		for i := range period {
			k := i / spacing
			x := float64(i-k*spacing) / 48000
			f := notes[(k*3+c)%len(notes)]
			var v float64
			for h := 1; h <= 6; h++ {
				v += math.Exp(-4*x) * 0.22 / float64(h) * math.Sin(2*math.Pi*f*float64(h)*x)
			}
			seed = seed*1664525 + 1013904223
			period[i] = float32(v + 0.01*(float64(seed>>8)/(1<<23)-1))
		}
		out[c] = make([]float32, frames)
		for i := range out[c] {
			out[c][i] = period[(i+spacing/3)%StartupPeriod]
		}
	}
	return out
}

// ErrorLevels are the rms and the largest of the 5 ms window errors over a
// stretch of a decode.
type ErrorLevels struct{ RMS, Peak float64 }

// ErrorLevelsOf measures dec against src over [from, to) in 5 ms windows at
// 48 kHz, all channels pooled. Sample i of dec is sample i of src. A
// stretch with no whole window has no levels: they come back NaN, which
// HoldToSteadyState fails.
func ErrorLevelsOf(src, dec [][]float32, from, to int) ErrorLevels {
	const window = 240
	var l ErrorLevels
	n := 0
	for i := from; i+window <= to; i += window {
		var e float64
		for c := range src {
			for j := i; j < i+window; j++ {
				d := float64(dec[c][j]) - float64(src[c][j])
				e += d * d
			}
		}
		e /= float64(len(src) * window)
		l.RMS += e
		l.Peak = max(l.Peak, math.Sqrt(e))
		n++
	}
	l.RMS = math.Sqrt(l.RMS / float64(n))
	return l
}

// Bounds on a stretch that follows an encoder start, against the same
// samples coded in steady state.
const (
	startupRMSBound  = 1.25
	startupPeakBound = 2.0
)

// HoldToSteadyState fails t when a stretch behind an encoder start is coded
// worse than its steady state: its rms error or its largest window over the
// steady figure by more than the bound.
func HoldToSteadyState(t testing.TB, what string, got, steady ErrorLevels) {
	t.Helper()
	// Written so that a NaN on either side fails.
	if !(got.RMS <= startupRMSBound*steady.RMS) {
		t.Errorf("%s: rms error %.4f is %.2f times the steady state's %.4f, over %.2f",
			what, got.RMS, got.RMS/steady.RMS, steady.RMS, startupRMSBound)
	}
	if !(got.Peak <= startupPeakBound*steady.Peak) || !(steady.RMS > 0) {
		t.Errorf("%s: largest 5 ms error %.4f is %.2f times the steady state's %.4f, over %.2f",
			what, got.Peak, got.Peak/steady.Peak, steady.Peak, startupPeakBound)
	}
	if testing.Verbose() {
		t.Logf("%s: rms %.4f (%.2fx) peak %.4f (%.2fx); steady rms %.4f peak %.4f",
			what, got.RMS, got.RMS/steady.RMS, got.Peak, got.Peak/steady.Peak, steady.RMS, steady.Peak)
	}
}
