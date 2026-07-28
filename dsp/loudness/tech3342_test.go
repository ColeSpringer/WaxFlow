package loudness

import (
	"math"
	"testing"
)

// steppedSine synthesizes one continuous 997 Hz sine whose amplitude
// steps between the given loudness levels, each held for secs seconds.
// The phase runs unbroken across the steps so a level change is an
// amplitude step and not a discontinuity in the waveform.
//
// The level calibration is the anchor TestIntegratedAnchors pins: the
// same 997 Hz tone in both channels of a stereo meter reads its own dBFS
// amplitude in LUFS, so amplitude 10^(L/20) measures L LUFS.
func steppedSine(rate, secs int, levels []float64) []float32 {
	out := make([]float32, 0, len(levels)*secs*rate)
	w := 2 * math.Pi * 997 / float64(rate)
	i := 0
	for _, l := range levels {
		amp := math.Pow(10, l/20)
		for range secs * rate {
			out = append(out, float32(amp*math.Sin(w*float64(i))))
			i++
		}
	}
	return out
}

// TestEBUTech3342Vectors measures the four EBU Tech 3342 loudness-range
// cases and checks them against the document's stated results at its own
// tolerance, +-1 LU. This is the conformance question, which is the bar
// the finding behind it actually asks about: agreeing with another
// implementation to a tenth of an LU is not what a loudness range is
// specified to.
//
// Each case is a sequence of 20 s segments at the levels below. Case 4 is
// the one that earns its place: its -50 LUFS segments sit below the 20 LU
// relative gate and must be excluded, leaving the -35 and -20 plateaus to
// set the range. A test built only from two-level cases would pass with
// the relative gate deleted.
//
// The segment structure here is the published form of the test material
// rather than a reading of the document itself, which is not to hand. The
// expected results are the document's, and every case reproduces them.
func TestEBUTech3342Vectors(t *testing.T) {
	const rate, segSecs = 48000, 20
	for _, tc := range []struct {
		name   string
		levels []float64
		want   float64
	}{
		{"3342-1", []float64{-20, -30}, 10},
		{"3342-2", []float64{-20, -15}, 5},
		{"3342-3", []float64{-40, -20}, 20},
		{"3342-4", []float64{-50, -35, -20, -35, -50}, 15},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := steppedSine(rate, segSecs, tc.levels)
			m := measure(t, rate, 0, 4096, s, s)
			got := m.Range()
			t.Logf("LRA %.3f LU (want %g +-1), integrated %.3f LUFS", got, tc.want, m.Integrated())
			if math.Abs(got-tc.want) > 1 {
				t.Errorf("loudness range = %.3f LU, want %g +-1", got, tc.want)
			}
		})
	}
}

// TestRangeNeedsTwoBlocks pins what Range answers when there is not
// enough audio to have a range: zero, not a negative number or a NaN out
// of a percentile lookup on an empty slice. Anything under 3 s produces
// no short-term window at all, and one window has no spread.
func TestRangeNeedsTwoBlocks(t *testing.T) {
	const rate = 48000
	amp := math.Pow(10, -23.0/20)
	for _, secs := range []float64{0.5, 2.9, 3.0} {
		n := int(secs * rate)
		s := sine(rate, n, 997, amp, 0)
		if got := measure(t, rate, 0, 4096, s, s).Range(); got != 0 {
			t.Errorf("%.1f s of audio: Range = %v, want 0", secs, got)
		}
	}
	// Steady material long enough to fill the distribution has a range,
	// and it is a small one rather than zero by luck.
	s := sine(rate, 10*rate, 997, amp, 0)
	if got := measure(t, rate, 0, 4096, s, s).Range(); got < 0 || got > 0.1 {
		t.Errorf("10 s of steady tone: Range = %.4f LU, want a small non-negative value", got)
	}
}
