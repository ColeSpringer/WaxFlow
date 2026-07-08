package alac

// bitWriter assembles MSB-first bit fields into a growing byte slice, the
// mirror of bitReader. Whole bytes spill from the cache as they fill, so
// the buffer always holds finished bytes plus at most 7 pending bits; align
// pads the tail with zeros, which is what frame assembly needs (a frame ends
// on a byte boundary so the muxer can size the packet).
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

// bitLen reports how many bits have been written, finished bytes plus the
// pending cache. Frame assembly compares the compressed and verbatim sizes
// through it.
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

// writeOnes appends count 1-bits with no terminator. The adaptive-Golomb
// prefix codes are runs of ones (the reference counts leading ones), so a
// terminating zero is written separately when the code calls for one.
func (w *bitWriter) writeOnes(count int) {
	for count >= 32 {
		w.writeBits(32, 0xFFFFFFFF)
		count -= 32
	}
	if count > 0 {
		w.writeBits(uint(count), 1<<uint(count)-1)
	}
}

// align pads with zeros to the next byte boundary.
func (w *bitWriter) align() {
	if w.n > 0 {
		w.writeBits(8-w.n, 0)
	}
}
