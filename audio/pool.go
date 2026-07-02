package audio

import (
	"math/bits"
	"sync"
)

// StandardChunk is the pipeline's standard chunk size in frames. Stages
// re-chunk to encoder-native sizes downstream; everything upstream deals
// in this.
const StandardChunk = 4096

// Buffers are pooled whole (struct plus backing array) in power-of-two
// size classes of total sample count, one pool set per domain. Pooling
// the *Buffer rather than boxed slice headers keeps Put allocation-free.
// Requests above the top class allocate directly and are dropped on Put;
// requests below the bottom class round up (the waste is trivial).
const (
	minClassBits = 8  // 256 samples
	maxClassBits = 22 // 4 Mi samples = 16 MiB int32/float32
)

var (
	intPools   [maxClassBits + 1]sync.Pool // *Buffer with I backing, cap in [1<<k, 1<<(k+1))
	floatPools [maxClassBits + 1]sync.Pool // *Buffer with F backing, likewise
)

// classBits returns the pool class for a total sample count, or -1 when
// the request is too large to pool.
func classBits(total int) int {
	if total <= 1<<minClassBits {
		return minClassBits
	}
	b := bits.Len(uint(total - 1)) // ceil(log2(total))
	if b > maxClassBits {
		return -1
	}
	return b
}

// Get returns a Buffer for f with capacity for at least frames frames per
// channel, drawn from the pool. N is 0, Pos is 0, Discont is false, and
// sample contents are unspecified. Release with Put. The format must be
// valid and frames positive; Get panics otherwise, because a bad format
// here is a programming error, not input-dependent.
func Get(f Format, frames int) *Buffer {
	if err := f.Valid(); err != nil {
		panic("audio: Get with invalid format: " + err.Error())
	}
	if frames <= 0 {
		panic("audio: Get with non-positive frame count")
	}
	total := frames * f.Channels
	class := classBits(total)

	var b *Buffer
	if class >= 0 {
		if f.Type == Int {
			b, _ = intPools[class].Get().(*Buffer)
		} else {
			b, _ = floatPools[class].Get().(*Buffer)
		}
	}
	if b == nil {
		alloc := total
		if class >= 0 {
			alloc = 1 << class
		}
		b = &Buffer{}
		if f.Type == Int {
			b.I = make([]int32, alloc)
		} else {
			b.F = make([]float32, alloc)
		}
	}
	if f.Type == Int {
		b.I = b.I[:total]
	} else {
		b.F = b.F[:total]
	}
	b.Fmt = f
	b.Stride = frames
	b.N = 0
	b.Pos = 0
	b.Discont = false
	return b
}

// Put returns b, backing array included, to the pool. b must not be used
// after Put. Buffers that were not obtained from Get are accepted too;
// oversized or undersized backing arrays make the whole buffer fall
// through to the garbage collector.
func Put(b *Buffer) {
	if b == nil {
		return
	}
	switch {
	case b.I != nil:
		s := b.I[:cap(b.I)]
		if len(s) >= 1<<minClassBits {
			// Index by floor(log2(cap)): every buffer in class k has
			// capacity of at least 1<<k, which Get relies on.
			if k := bits.Len(uint(len(s))) - 1; k <= maxClassBits {
				*b = Buffer{I: s}
				intPools[k].Put(b)
			}
		}
	case b.F != nil:
		s := b.F[:cap(b.F)]
		if len(s) >= 1<<minClassBits {
			if k := bits.Len(uint(len(s))) - 1; k <= maxClassBits {
				*b = Buffer{F: s}
				floatPools[k].Put(b)
			}
		}
	}
}
