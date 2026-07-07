package mp3

// bitWriter assembles MSB-first bit fields into a growing byte slice, the
// mirror of bitReader. Layer III main data is a continuous bitstream that
// the frame assembler later slices into physical main-data slots, so the
// writer tracks nothing but bits: whole bytes spill from the cache as they
// fill, and align pads the tail with zeros to a byte boundary (each
// frame's main data is byte-aligned so main_data_begin stays byte-granular).
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
func (w *bitWriter) writeBits(k uint, v uint32) {
	if k == 0 {
		return
	}
	w.cache = w.cache<<k | uint64(v)&(1<<k-1)
	w.n += k
	for w.n >= 8 {
		w.n -= 8
		w.buf = append(w.buf, byte(w.cache>>w.n))
	}
}

// bitLen is the number of bits written so far.
func (w *bitWriter) bitLen() int { return len(w.buf)*8 + int(w.n) }

// align pads with zeros to the next byte boundary.
func (w *bitWriter) align() {
	if w.n > 0 {
		w.writeBits(8-w.n, 0)
	}
}
