// Package gain scales PCM level: a plain scalar gain kernel, plus a
// look-ahead true-peak limiter the chain inserts whenever the level path
// can clip (net positive gain, or a downmix whose worst-case matrix gain
// exceeds unity). Kernels follow the DSP slice
// convention: per-channel []float32, no buffers, no strides.
package gain

import (
	"fmt"
	"math"
)

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

// checkFrames enforces the slice convention the kernels share and returns
// the frame count: one slice per channel, all of the same length, since they
// are the channels of one buffer. The chain always satisfies it (pumpStage
// slices one audio.Buffer by its stride), but these kernels are public, so
// the contract is checked rather than assumed.
//
// The comparison is equality, not "at least n". A channel longer than its
// siblings is the same wiring bug as one shorter, and it fails more quietly:
// the kernel would process the shortest channel's worth of frames and
// silently ignore the tail of the others. Only the shorter direction crashes
// on its own, so the longer one is exactly the case a guard has to add.
//
// Note it does not compare dst against src. Those legitimately differ: a
// caller may offer more room than it has input, and the pump does.
//
// It panics rather than returning an error, matching the channel-count
// checks: a ragged call is a caller bug at the wiring level, not a runtime
// condition, and the alternative is an index-out-of-range panic several
// frames into a hot loop that says nothing about what was wrong. The cost is
// one pass over the channel slices per chunk, not per sample.
func checkFrames(what string, slices [][]float32) int {
	n := len(slices[0])
	for _, s := range slices[1:] {
		if len(s) != n {
			panic(fmt.Sprintf("gain: %s channel slices differ in length (%d vs %d); "+
				"every channel must cover the same frames", what, len(s), n))
		}
	}
	return n
}
