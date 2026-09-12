package asf

import (
	"math"
	"testing"
)

// TestTakeRefusesALengthThatWraps pins the packet cursor's bounds check
// against a file-derived length near the top of the range. take's n comes from
// a variable-width field in the packet, so p+n wraps negative where int is 32
// bits and a negative total passes any check written as a sum, which then
// slices past the end. Caught on 386 (make test-386); everywhere else it is a
// plain out-of-range case.
func TestTakeRefusesALengthThatWraps(t *testing.T) {
	for _, p := range []int{0, 2, 8} {
		r := br{b: make([]byte, 16), p: p, ok: true}
		if got := r.take(math.MaxInt32 - 1); got != nil {
			t.Errorf("p=%d: take returned %d bytes", p, len(got))
		}
		if r.ok {
			t.Errorf("p=%d: take left the cursor ok", p)
		}
	}
}
