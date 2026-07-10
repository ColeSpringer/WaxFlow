package opus

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
)

// TestEncoderComplexityResolution pins how EncoderOptions.Complexity maps to
// the encoder's analysis depth: the zero value keeps the default, -1 spells
// complexity 0 (the lowest setting is otherwise unreachable behind the zero
// value), 1..10 pass through, and anything else is rejected.
func TestEncoderComplexityResolution(t *testing.T) {
	f := audio.Format{Rate: SampleRate, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	for _, tc := range []struct {
		in      int
		want    int
		wantErr bool
	}{
		{0, DefaultComplexity, false},
		{-1, 0, false},
		{1, 1, false},
		{10, 10, false},
		{-2, 0, true},
		{11, 0, true},
	} {
		enc, err := NewEncoder(f, &EncoderOptions{Complexity: tc.in})
		if tc.wantErr {
			if err == nil {
				t.Errorf("Complexity=%d: want an error, got complexity %d", tc.in, enc.celt.complexity)
			}
			continue
		}
		if err != nil {
			t.Fatalf("Complexity=%d: %v", tc.in, err)
		}
		if enc.celt.complexity != tc.want {
			t.Errorf("Complexity=%d: encoder runs at %d, want %d", tc.in, enc.celt.complexity, tc.want)
		}
	}
}

// TestComputeEquivRate pins compute_equiv_rate's branch arithmetic against
// hand-computed reference values. The mode-unknown case matters most: the
// reference passes 0 as "no mode yet" (its MODE_* constants start at 1000),
// while our modeSILK is 0, so the unknown-mode sentinel here must be modeNone.
// At complexity < 2 the SILK branch applies an extra 4/5 penalty that the
// unknown branch must not, or the mode decision itself would shift.
func TestComputeEquivRate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		bitrate    int32
		channels   int
		frameRate  int
		vbr        bool
		mode       int
		complexity int
		loss       int
		want       int32
	}{
		{"unknown-c0", 64000, 1, 50, true, modeNone, 0, 0, 57600},
		{"silk-c0-penalty", 64000, 1, 50, true, modeSILK, 0, 0, 46080},
		{"unknown-c1", 64000, 1, 50, true, modeNone, 1, 0, 58240},
		{"silk-c1-penalty", 64000, 1, 50, true, modeSILK, 1, 0, 46592},
		{"silk-c2-no-penalty", 64000, 1, 50, true, modeSILK, 2, 0, 58880},
		{"celt-c4-pf-penalty", 64000, 1, 50, true, modeCELT, 4, 0, 54144},
		{"celt-c5", 64000, 1, 50, true, modeCELT, 5, 0, 60800},
		{"silk-c5-loss20", 64000, 1, 50, true, modeSILK, 5, 20, 51447},
		{"unknown-c5-loss20", 64000, 1, 50, true, modeNone, 5, 20, 56124},
		{"cbr-unknown-c10", 64000, 1, 50, false, modeNone, 10, 0, 58667},
		{"fr100-stereo-overhead", 64000, 2, 100, true, modeNone, 10, 0, 59000},
	} {
		got := computeEquivRate(tc.bitrate, tc.channels, tc.frameRate, tc.vbr, tc.mode, tc.complexity, tc.loss)
		if got != tc.want {
			t.Errorf("%s: computeEquivRate = %d, want %d", tc.name, got, tc.want)
		}
	}
}
