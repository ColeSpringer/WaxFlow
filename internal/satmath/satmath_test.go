package satmath_test

import (
	"math"
	"testing"

	"github.com/colespringer/waxflow/internal/satmath"
)

func TestAdd(t *testing.T) {
	for _, c := range []struct{ a, b, want int64 }{
		{2, 3, 5},
		{0, 0, 0},
		{math.MaxInt64, 0, math.MaxInt64},
		{math.MaxInt64, 1, math.MaxInt64},
		{math.MaxInt64, math.MaxInt64, math.MaxInt64},
		{math.MaxInt64 - 5, 5, math.MaxInt64},
		{math.MaxInt64 - 5, 4, math.MaxInt64 - 1},
	} {
		if got := satmath.Add(c.a, c.b); got != c.want {
			t.Errorf("Add(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestMulDiv(t *testing.T) {
	for _, c := range []struct{ a, b, c, want int64 }{
		{10, 20, 4, 50},                     // exact
		{7, 1, 2, 3},                        // rounds down
		{1000, int64(1e9), 600, 1666666666}, // chapter ns, no overflow
		{math.MaxInt64, math.MaxInt64, 1, math.MaxInt64}, // product overflows -> saturate
		{math.MaxInt64, 2, 1, math.MaxInt64},             // quotient overflows int64 -> saturate
		{1 << 40, 1 << 40, 1 << 40, 1 << 40},             // 128-bit intermediate, exact
		{-5, 10, 2, 0},                                   // non-positive operand
		{5, 10, 0, 0},                                    // non-positive divisor
	} {
		if got := satmath.MulDiv(c.a, c.b, c.c); got != c.want {
			t.Errorf("MulDiv(%d, %d, %d) = %d, want %d", c.a, c.b, c.c, got, c.want)
		}
	}
}

// TestMulDivRound: a count crosses a rate change to the nearest sample, and
// one a header made too large to scale stays the largest count there is
// rather than wrapping.
func TestMulDivRound(t *testing.T) {
	for _, c := range []struct{ a, b, c, want int64 }{
		{4096, 1, 1, 4096},
		{4096, 160, 147, 4458}, // 44.1 kHz to 48 kHz: 4458.23
		{4096, 147, 160, 3763}, // and back: 3763.2
		{3, 1, 2, 2},           // a half rounds up
		{0, 160, 147, 0},
		{-4, 160, 147, 0},
		{4096, 160, 0, 0},
		{math.MaxInt64, 160, 147, math.MaxInt64},
		{math.MaxInt64, 147, 160, 8473973058860325273},
		{math.MaxInt64 / 2, 3, 1, math.MaxInt64},
	} {
		if got := satmath.MulDivRound(c.a, c.b, c.c); got != c.want {
			t.Errorf("MulDivRound(%d, %d, %d) = %d, want %d", c.a, c.b, c.c, got, c.want)
		}
	}
}
