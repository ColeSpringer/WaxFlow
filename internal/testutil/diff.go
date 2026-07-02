package testutil

import (
	"fmt"
	"math"

	"github.com/colespringer/waxflow/audio"
)

// DiffI32 compares int sample slices exactly. It returns the index of the
// first mismatch, or -1 when equal (length mismatch counts as a mismatch
// at the shorter length).
func DiffI32(a, b []int32) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}

// FloatDiff summarizes the difference between two float sample slices.
type FloatDiff struct {
	N      int     // samples compared
	RMS    float64 // root mean square of the differences
	MaxAbs float64 // largest absolute difference
	MaxAt  int     // index of the largest difference
}

func (d FloatDiff) String() string {
	return fmt.Sprintf("n=%d rms=%.3g max=%.3g@%d", d.N, d.RMS, d.MaxAbs, d.MaxAt)
}

// CompareF32 measures a and b, which must be the same length.
func CompareF32(a, b []float32) FloatDiff {
	if len(a) != len(b) {
		return FloatDiff{N: -1, RMS: math.Inf(1), MaxAbs: math.Inf(1)}
	}
	d := FloatDiff{N: len(a)}
	var sum float64
	for i := range a {
		diff := math.Abs(float64(a[i]) - float64(b[i]))
		sum += diff * diff
		if diff > d.MaxAbs {
			d.MaxAbs, d.MaxAt = diff, i
		}
	}
	if d.N > 0 {
		d.RMS = math.Sqrt(sum / float64(d.N))
	}
	return d
}

// Interleave flattens a planar buffer to interleaved int32 samples,
// left-shifted to 32-bit like ffmpeg's s32le output, for direct
// comparison against FFmpegDecodeS32.
func Interleave(b *audio.Buffer) []int32 {
	ch := b.Fmt.Channels
	out := make([]int32, b.N*ch)
	shift := 32 - b.Fmt.BitDepth
	for c := 0; c < ch; c++ {
		s := b.ChanI(c)
		for i, v := range s {
			out[i*ch+c] = v << shift
		}
	}
	return out
}

// InterleaveF flattens a planar float buffer to interleaved float32.
func InterleaveF(b *audio.Buffer) []float32 {
	ch := b.Fmt.Channels
	out := make([]float32, b.N*ch)
	for c := 0; c < ch; c++ {
		s := b.ChanF(c)
		for i, v := range s {
			out[i*ch+c] = v
		}
	}
	return out
}
