package testutil

import (
	"math"
	"math/rand/v2"

	"github.com/colespringer/waxflow/audio"
)

// Signal synthesis for tests. Everything here is deterministic: seeded
// PRNG for noise, closed-form values elsewhere, so goldens and
// round-trip assertions never flake. Callers own the returned buffers
// (release with audio.Put).

// Sine synthesizes a full-scale-scaled sine per channel (each channel's
// phase offset by its index, so channel swaps are detectable).
func Sine(f audio.Format, frames int, freq, amp float64) *audio.Buffer {
	b := audio.Get(f, frames)
	b.N = frames
	scale := float64(int64(1)<<(f.BitDepth-1) - 1)
	for c := 0; c < f.Channels; c++ {
		phase := float64(c) * 0.1
		if f.Type == audio.Int {
			s := b.ChanI(c)
			for i := range s {
				v := amp * math.Sin(2*math.Pi*freq*float64(i)/float64(f.Rate)+phase)
				s[i] = int32(math.RoundToEven(v * scale))
			}
		} else {
			s := b.ChanF(c)
			for i := range s {
				s[i] = float32(amp * math.Sin(2*math.Pi*freq*float64(i)/float64(f.Rate)+phase))
			}
		}
	}
	return b
}

// Noise synthesizes seeded uniform noise spanning the full sample range,
// with the range extremes pinned to the first frames of every channel so
// clipping and sign bugs cannot hide.
func Noise(f audio.Format, frames int, seed uint64) *audio.Buffer {
	b := audio.Get(f, frames)
	b.N = frames
	rng := rand.New(rand.NewPCG(seed, seed))
	for c := 0; c < f.Channels; c++ {
		if f.Type == audio.Int {
			s := b.ChanI(c)
			lo := int32(-1) << (f.BitDepth - 1)
			hi := -(lo + 1)
			// uint64(lo) sign-extends, so hi-lo is computed modulo 2^64;
			// the wrapped difference is exactly the span (2^depth - 1),
			// including at depth 32.
			for i := range s {
				s[i] = lo + int32(rng.Uint64N(uint64(hi)-uint64(lo)+1))
			}
			if len(s) >= 3 {
				s[0], s[1], s[2] = lo, hi, 0
			}
		} else {
			s := b.ChanF(c)
			for i := range s {
				s[i] = float32(rng.Float64()*2 - 1)
			}
			if len(s) >= 3 {
				s[0], s[1], s[2] = -1, 1, 0
			}
		}
	}
	return b
}

// Ramp synthesizes a per-channel counter (offset by channel) that walks
// the full range, making positions identifiable by value: sample i of
// channel c is predictable, which seek tests rely on.
func Ramp(f audio.Format, frames int) *audio.Buffer {
	b := audio.Get(f, frames)
	b.N = frames
	for c := 0; c < f.Channels; c++ {
		if f.Type == audio.Int {
			s := b.ChanI(c)
			for i := range s {
				s[i] = RampAtI(f, c, int64(i))
			}
		} else {
			s := b.ChanF(c)
			for i := range s {
				s[i] = RampAtF(c, int64(i))
			}
		}
	}
	return b
}

// RampAtI is the closed form of Ramp for int formats, letting tests
// verify any position without holding the whole signal.
func RampAtI(f audio.Format, channel int, pos int64) int32 {
	span := int64(1) << f.BitDepth
	lo := int64(-1) << (f.BitDepth - 1)
	return int32(lo + (pos*7+int64(channel)*13)%span)
}

// RampAtF is the closed form of Ramp for float formats.
func RampAtF(channel int, pos int64) float32 {
	return float32((pos*7+int64(channel)*13)%20001-10000) / 10000
}
