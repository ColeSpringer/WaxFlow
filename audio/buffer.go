package audio

// Buffer is a chunk of planar PCM. Exactly one of I and F is populated,
// selected by Fmt.Type. All channels share one flat backing array: channel
// c occupies the Stride-sized region starting at c*Stride, of which the
// first N frames are meaningful. One allocation per buffer, channels
// adjacent in memory, and each channel view is a contiguous slice whose
// bounds checks hoist out of DSP kernels.
//
// Pos is the position of the first frame in the current stage's timeline,
// in samples at Fmt.Rate. format.Media stamps it, rate-changing DSP nodes
// rescale it, and nothing else touches it (ADR-0006). Discont marks the
// first buffer after a seek or splice.
//
// Contents beyond N frames per channel are unspecified.
type Buffer struct {
	Fmt Format

	// I holds int-domain samples, right-justified at Fmt.BitDepth.
	I []int32
	// F holds float-domain samples, nominal range [-1, 1].
	F []float32

	// Stride is the per-channel capacity in frames within the backing
	// array. Frame capacity for the whole buffer, too: Cap() == Stride.
	Stride int
	// N is the number of valid frames in each channel, 0 <= N <= Stride.
	N int

	// Pos is the sample position of frame 0 in this stage's timeline.
	Pos int64
	// Discont marks the first buffer after a seek or splice point.
	Discont bool
}

// Cap returns the buffer's frame capacity per channel.
func (b *Buffer) Cap() int { return b.Stride }

// ChanI returns channel c's valid frames as a contiguous int32 slice.
// The slice capacity is bounded at Stride, so appending past it can never
// clobber the next channel. Panics if the buffer is not int-domain.
func (b *Buffer) ChanI(c int) []int32 {
	if b.I == nil {
		panic("audio: ChanI on a float-domain buffer")
	}
	return b.I[c*b.Stride : c*b.Stride+b.N : c*b.Stride+b.Stride]
}

// ChanF returns channel c's valid frames as a contiguous float32 slice.
// The slice capacity is bounded at Stride, so appending past it can never
// clobber the next channel. Panics if the buffer is not float-domain.
func (b *Buffer) ChanF(c int) []float32 {
	if b.F == nil {
		panic("audio: ChanF on an int-domain buffer")
	}
	return b.F[c*b.Stride : c*b.Stride+b.N : c*b.Stride+b.Stride]
}

// CopyFrames copies n frames from src starting at frame srcOff into dst
// starting at frame dstOff, channel by channel. Neither buffer's N is
// consulted or updated; offsets are bounded by Stride. Overlapping copies
// within one buffer are safe (compaction uses this). Formats must match;
// a mismatch panics, because it is a pipeline wiring bug, never
// input-dependent.
func CopyFrames(dst *Buffer, dstOff int, src *Buffer, srcOff, n int) {
	if n <= 0 {
		return
	}
	if dst.Fmt != src.Fmt {
		panic("audio: CopyFrames between mismatched formats " + dst.Fmt.String() + " and " + src.Fmt.String())
	}
	for c := 0; c < dst.Fmt.Channels; c++ {
		if dst.Fmt.Type == Float {
			copy(dst.F[c*dst.Stride+dstOff:c*dst.Stride+dstOff+n],
				src.F[c*src.Stride+srcOff:c*src.Stride+srcOff+n])
		} else {
			copy(dst.I[c*dst.Stride+dstOff:c*dst.Stride+dstOff+n],
				src.I[c*src.Stride+srcOff:c*src.Stride+srcOff+n])
		}
	}
}
