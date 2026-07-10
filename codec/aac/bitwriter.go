package aac

// bitWriter assembles MSB-first bit fields into a growing byte slice, the
// mirror of bitReader (the same shape codec/alac uses; the nested-module
// extraction rules out sharing it across codec packages). Whole bytes
// spill from the cache as they fill; align pads the tail with zeros,
// which is how a raw_data_block ends after the END element.
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

// bitLen reports how many bits have been written so far.
func (w *bitWriter) bitLen() int { return len(w.buf)*8 + int(w.n) }

// writeBits appends the low k bits of v, MSB-first, k <= 57.
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

// align pads with zeros to the next byte boundary.
func (w *bitWriter) align() {
	if w.n > 0 {
		w.writeBits(8-w.n, 0)
	}
}
