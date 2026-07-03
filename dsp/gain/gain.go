// Package gain scales PCM level: a plain scalar gain kernel, plus a
// look-ahead true-peak limiter the chain inserts whenever the level path
// can clip (net positive gain, or a downmix whose worst-case matrix gain
// exceeds unity). Kernels follow the DSP slice
// convention: per-channel []float32, no buffers, no strides.
package gain

import "math"

// Version is the scalar gain node's revision for cache keys (plan
// section 10).
const Version = "gain-1"

// FromDB converts decibels to a linear amplitude factor.
func FromDB(db float64) float64 {
	return math.Pow(10, db/20)
}

// Apply scales one channel in place. A factor of exactly 1 is the
// identity, bit for bit.
func Apply(x []float32, g float32) {
	if g == 1 {
		return
	}
	for i := range x {
		x[i] *= g
	}
}
