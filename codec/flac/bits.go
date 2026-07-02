package flac

import "math/bits"

// bitReader reads MSB-first bit fields from a byte slice. Reads past the
// end set err and return zeros instead of failing per call; decode loops
// are bounded by parsed counts (block size, predictor order), so the
// caller checks err once per frame instead of plumbing an error through
// every field read. This is the decoder's hottest path.
type bitReader struct {
	data  []byte
	pos   int    // next byte to load into the cache
	cache uint64 // low n bits are valid, next bit out is the MSB of those
	n     uint
	err   bool
}

// ensure tops the cache up to at least k valid bits, k <= 57.
func (r *bitReader) ensure(k uint) bool {
	for r.n < k {
		if r.pos >= len(r.data) {
			r.err = true
			return false
		}
		r.cache = r.cache<<8 | uint64(r.data[r.pos])
		r.pos++
		r.n += 8
	}
	return true
}

// u reads k unsigned bits, k <= 57.
func (r *bitReader) u(k uint) uint64 {
	if k == 0 {
		return 0
	}
	if !r.ensure(k) {
		return 0
	}
	r.n -= k
	return r.cache >> r.n & (1<<k - 1)
}

// s reads k bits as a sign-extended two's-complement value, 1 <= k <= 57.
func (r *bitReader) s(k uint) int64 {
	v := r.u(k)
	return int64(v<<(64-k)) >> (64 - k)
}

// unary reads zeros up to a terminating one and returns the zero count.
func (r *bitReader) unary() int {
	n := 0
	for {
		if r.n == 0 && !r.ensure(1) {
			return n
		}
		window := r.cache << (64 - r.n)
		lz := uint(bits.LeadingZeros64(window))
		if lz >= r.n {
			n += int(r.n)
			r.n = 0
			continue
		}
		r.n -= lz + 1
		return n + int(lz)
	}
}

// align discards bits up to the next byte boundary.
func (r *bitReader) align() {
	r.n -= r.n % 8
}

// consumed returns the number of whole bytes consumed so far. Only valid
// on a byte boundary.
func (r *bitReader) consumed() int {
	return r.pos - int(r.n)/8
}
