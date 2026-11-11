// Package satmath is integer arithmetic on sample counts and durations that
// holds at math.MaxInt64 rather than wrapping. A header can declare any
// length and any timescale, so a product or a sum of what two headers say can
// pass 2^63 on a file a few kilobytes long, and a wrapped count is a negative
// length somewhere downstream.
package satmath

import (
	"math"
	"math/bits"
)

// Add is a + b for two counts, held at math.MaxInt64.
func Add(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// MulDiv computes a*b/c in 128 bits, rounding down and saturating at
// math.MaxInt64. A non-positive operand yields 0.
func MulDiv(a, b, c int64) int64 {
	if a <= 0 || b <= 0 || c <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi >= uint64(c) {
		return math.MaxInt64 // quotient would exceed 64 bits
	}
	q, _ := bits.Div64(hi, lo, uint64(c))
	if q > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(q)
}

// MulDivRound is MulDiv rounding to nearest rather than down. It is for a
// rescale between two grids that do not divide each other, where rounding
// down is a bias and not just a rounding: a count moved from 44.1 kHz to
// 48 kHz and back should come back the same.
func MulDivRound(a, b, c int64) int64 {
	if a <= 0 || b <= 0 || c <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	// a and b are int64, so the product is below 2^126 and hi below 2^62: the
	// half-divisor carry cannot overflow hi.
	lo, carry := bits.Add64(lo, uint64(c)/2, 0)
	hi += carry
	if hi >= uint64(c) {
		return math.MaxInt64 // quotient would exceed 64 bits
	}
	q, _ := bits.Div64(hi, lo, uint64(c))
	if q > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(q)
}
