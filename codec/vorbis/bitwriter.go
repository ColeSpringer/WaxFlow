package vorbis

// bitWriter assembles Vorbis's little-endian bit packing, the exact mirror of
// bitReader: the first bit of a field is the least-significant bit of the byte
// and multi-bit fields fill from the low bit up. This is the OPPOSITE of the
// MSB-first writer in codec/flac/bitwriter.go, so it mirrors bitReader rather
// than copying FLAC's. Whole bytes spill from the low end of the cache as they
// fill, so the buffer always holds finished bytes plus at most 7 pending bits.
type bitWriter struct {
	buf   []byte
	cache uint64 // low n bits are pending; the next bit out is the LSB
	n     uint   // pending bit count, 0..7 between writes
	total int64  // total bits written, for diagnostics and rate control
}

// reset drops all content but keeps the buffer capacity.
func (w *bitWriter) reset() {
	w.buf = w.buf[:0]
	w.cache = 0
	w.n = 0
	w.total = 0
}

// writeBits appends the low k bits of v LSB-first, k <= 32 (the widest Vorbis
// field, a packed float or a 32-bit codeword). The invariant is that cache
// holds only its low n pending bits with everything above zero, so ORing the
// next field in at position n never collides.
func (w *bitWriter) writeBits(k uint, v uint32) {
	if k == 0 {
		return
	}
	w.cache |= uint64(v&mask32(k)) << w.n
	w.n += k
	w.total += int64(k)
	for w.n >= 8 {
		w.buf = append(w.buf, byte(w.cache))
		w.cache >>= 8
		w.n -= 8
	}
}

// writeBit appends a single bit (the low bit of v).
func (w *bitWriter) writeBit(v uint32) { w.writeBits(1, v) }

// mask32 returns the low-k-bit mask, handling k==32 (where 1<<32 overflows a
// 32-bit shift) without a special case at the call sites.
func mask32(k uint) uint32 {
	if k >= 32 {
		return 0xffffffff
	}
	return 1<<k - 1
}

// bits returns the number of bits written so far, including the pending
// fraction. Rate control and the residue partitioner budget in whole bits.
func (w *bitWriter) bits() int64 { return w.total }

// bytes finalizes the packet: it flushes the final partial byte (its unused
// high bits zero) and returns the buffer. Trailing zero bits are inert, the
// decoder reads only the fields a well-formed packet defines. The returned
// slice aliases the writer's buffer, so it is valid until the next reset.
//
// Clearing the pending bits makes bytes idempotent (a second call flushes
// nothing), but it also ends the packet: the writer must be reset before it is
// written to again. Writing more bits after bytes without a reset would restart
// at a byte boundary (the pending byte is already committed), not continue the
// interrupted byte, so callers reset-then-write per packet and never write after
// bytes. This is deliberately not a defensive copy; the zero-copy aliasing is the
// hot-path contract, and reset reuses the buffer for the next packet.
func (w *bitWriter) bytes() []byte {
	if w.n > 0 {
		w.buf = append(w.buf, byte(w.cache))
		w.cache = 0
		w.n = 0
	}
	return w.buf
}
