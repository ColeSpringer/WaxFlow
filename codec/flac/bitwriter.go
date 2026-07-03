package flac

// bitWriter assembles MSB-first bit fields into a growing byte slice, the
// mirror of bitReader. Whole bytes spill from the cache as they fill, so
// the buffer always holds finished bytes plus at most 7 pending bits;
// align pads the tail with zeros, which is what frame assembly needs
// (subframes end on a zero-padded byte boundary before the CRC-16).
type bitWriter struct {
	buf   []byte
	cache uint64 // low n bits are pending, next bit out is their MSB
	n     uint
}

// reset drops all content but keeps the buffer capacity.
func (w *bitWriter) reset() {
	w.buf = w.buf[:0]
	w.cache = 0
	w.n = 0
}

// writeBits appends the low k bits of v, k <= 57.
func (w *bitWriter) writeBits(k uint, v uint64) {
	if k == 0 {
		return
	}
	w.cache = w.cache<<k | v&(1<<k-1)
	w.n += k
	for w.n >= 8 {
		w.n -= 8
		w.buf = append(w.buf, byte(w.cache>>w.n))
	}
}

// writeSigned appends v as a k-bit two's-complement field, 1 <= k <= 57.
// The value must fit: -(1<<(k-1)) <= v < 1<<(k-1).
func (w *bitWriter) writeSigned(k uint, v int64) {
	w.writeBits(k, uint64(v))
}

// writeUnary appends q zeros and a terminating one. Unsigned by type:
// a negative count has no meaning here, and a conversion slip would
// otherwise turn into a wild writeBits shift instead of an error.
func (w *bitWriter) writeUnary(q uint64) {
	for q >= 32 {
		w.writeBits(32, 0)
		q -= 32
	}
	w.writeBits(uint(q)+1, 1)
}

// align pads with zeros to the next byte boundary.
func (w *bitWriter) align() {
	if w.n > 0 {
		w.writeBits(8-w.n, 0)
	}
}
