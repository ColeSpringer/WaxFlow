package mp3

// bitReader reads MSB-first bit fields from a byte slice at an
// addressable bit position. Layer III needs the address: part_2_3_length
// delimits each granule's scalefactor and Huffman data, so decode loops
// are bounded by positions, not by data exhaustion. Reads past the end
// set err and return zeros; the caller checks once per granule and
// treats the frame as damaged (ADR-0005: errors instead of garbage, and
// for a lossy stream the honest error unit is a silent frame).
type bitReader struct {
	data []byte
	pos  int // next bit, absolute from the start of data
	err  bool
}

// bitLen is the total addressable bit count.
func (r *bitReader) bitLen() int { return len(r.data) * 8 }

// bit reads one bit.
func (r *bitReader) bit() uint32 {
	if r.pos >= len(r.data)*8 {
		r.err = true
		return 0
	}
	b := r.data[r.pos>>3] >> (7 - r.pos&7) & 1
	r.pos++
	return uint32(b)
}

// bits reads k bits, k <= 24.
func (r *bitReader) bits(k uint) uint32 {
	if k == 0 {
		return 0
	}
	if r.pos+int(k) > len(r.data)*8 {
		r.err = true
		r.pos = len(r.data) * 8
		return 0
	}
	i := r.pos >> 3
	off := uint(r.pos & 7)
	// Load 32 bits starting at the current byte; off + k <= 31 always
	// holds for k <= 24, so four bytes suffice.
	var v uint32
	for j := 0; j < 4; j++ {
		v <<= 8
		if i+j < len(r.data) {
			v |= uint32(r.data[i+j])
		}
	}
	r.pos += int(k)
	return v << off >> (32 - k)
}

// bitPos is the current absolute bit position.
func (r *bitReader) bitPos() int { return r.pos }

// setPos jumps to an absolute bit position (granule boundaries). Past
// the end marks the reader failed.
func (r *bitReader) setPos(p int) {
	if p > len(r.data)*8 {
		r.err = true
		p = len(r.data) * 8
	}
	r.pos = p
}
