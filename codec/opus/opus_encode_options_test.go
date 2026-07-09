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
