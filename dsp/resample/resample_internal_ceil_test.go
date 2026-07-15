package resample

import "testing"

// TestCeilDivWholeRange pins ceil over negatives as well as positives.
//
// The negative half is not decoration: a span pre-rolling into the audio
// ahead of its own sample 0 anchors the resampler at a negative position,
// and the bias form this replaced was off by one exactly there, which would
// put the pre-roll's output one sample away from the grid the kept stream
// lands on.
func TestCeilDivWholeRange(t *testing.T) {
	for _, tc := range []struct{ a, b, want int64 }{
		{0, 147, 0},
		{1, 147, 1},
		{146, 147, 1},
		{147, 147, 1},
		{148, 147, 2},
		{15854, 147, 108},
		// Negatives: ceil(-107.85) is -107, not -106.
		{-15854, 147, -107},
		{-1, 147, 0},
		{-146, 147, 0},
		{-147, 147, -1},
		{-148, 147, -1},
	} {
		if got := ceilDiv(tc.a, tc.b); got != tc.want {
			t.Errorf("ceilDiv(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestOffsetForNegativePhase pins the property OffsetFor exists for, on the
// negative half: the phase offset must stay a legal grid phase in [0, m),
// or a pre-rolled segment's output would not land on the same grid the kept
// stream does.
func TestOffsetForNegativePhase(t *testing.T) {
	r, err := New(44100, 48000, 2, HQ)
	if err != nil {
		t.Fatal(err)
	}
	l, m := int64(r.bank.l), int64(r.bank.m)
	for pos := int64(-20000); pos <= 20000; pos += 337 {
		outPos, phase := r.OffsetFor(pos)
		if int64(phase) < 0 || int64(phase) >= m {
			t.Fatalf("OffsetFor(%d) phase %d outside [0, %d)", pos, phase, m)
		}
		if got := outPos*m - pos*l; got != int64(phase) {
			t.Fatalf("OffsetFor(%d) anchor is inconsistent: %d != %d", pos, got, phase)
		}
	}
}
