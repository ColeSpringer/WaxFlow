package wmalossless

// The bit layer. Everything in the coded stream is read most significant bit
// first, bytes in file order; the little-endian fields are confined to the
// WAVEFORMATEX of wmalossless.go.
//
// Two things here are not in codec/wma's reader, and both are forced by the
// format rather than chosen. Fields run to 32 bits (the Golomb escape reads a
// width of up to 32, and a remainder width is the ceiling log of a running
// average that can approach 2^31), so the window is 64 bits wide rather than
// 32. And a frame is not bounded by a packet in either direction, so the
// carry has to be a bit string with its own bit length: a frame resumes
// bit-continuously with no padding and no realignment, and a carry stored as
// bytes plus an offset would resume up to seven bits off.
//
// The reader is a copy of codec/wma's shape rather than an import. The two
// packages must not couple: they are different codecs with different
// revision lifecycles, and a change made for one must not be able to move the
// other's decoded samples.

// bitReader reads a bit string most significant bit first. This format has no
// sync word and no in-band framing, so nothing re-anchors a reader that has
// drifted: an overrun is the only signal a walk has gone wrong, and it has to
// reach the caller rather than return zeroes. The first overrun is latched and
// reported through err.
type bitReader struct {
	buf []byte
	n   int // valid bits, which is not len(buf)*8 for a carry that stops mid-byte
	pos int // bits consumed
	err error
}

// resetBits reads the first n bits of b and treats the rest as absent. The
// padding zeroes to the next byte boundary at the end of a carry are not
// payload, and reading them back as payload lets a walk that should have
// overrun finish instead.
func (r *bitReader) resetBits(b []byte, n int) {
	r.buf, r.n, r.pos, r.err = b, n, 0, nil
}

func (r *bitReader) bitLen() int    { return r.n }
func (r *bitReader) left() int      { return r.n - r.pos }
func (r *bitReader) failed() bool   { return r.err != nil }
func (r *bitReader) errored() error { return r.err }

// overrun latches a read past the end. The first one wins, so the message
// names where the walk actually left the stream.
func (r *bitReader) overrun() uint32 {
	if r.err == nil {
		r.err = malformed("the frame reads past the end of its %d-bit run", r.n)
	}
	return 0
}

// maxBits is the widest field this reader returns. The window assembles eight
// bytes from the containing byte, so a field starting seven bits into it has
// 57 left and 32 is comfortably inside.
const maxBits = 32

// bits reads n bits, 0 <= n <= maxBits.
func (r *bitReader) bits(n int) uint32 {
	if n == 0 {
		return 0
	}
	if n < 0 || n > maxBits || r.pos+n > r.n {
		if n >= 0 && n <= maxBits {
			r.pos = r.n
		}
		return r.overrun()
	}
	i := r.pos >> 3
	off := uint(r.pos & 7)
	var acc uint64
	if i+8 <= len(r.buf) {
		b := r.buf[i : i+8 : i+8]
		acc = uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
			uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
	} else {
		// The tail is short of eight bytes at the very end, so it assembles
		// byte by byte there.
		for j := 0; j < 8; j++ {
			acc <<= 8
			if i+j < len(r.buf) {
				acc |= uint64(r.buf[i+j])
			}
		}
	}
	r.pos += n
	return uint32((acc << off) >> (64 - uint(n)))
}

// signed reads n bits as a two's-complement signed value.
func (r *bitReader) signed(n int) int32 {
	if n <= 0 || n > maxBits {
		if n != 0 {
			r.overrun()
		}
		return 0
	}
	v := r.bits(n)
	if n == 32 {
		return int32(v)
	}
	sh := uint(32 - n)
	return int32(v<<sh) >> sh
}

// bit reads one bit.
func (r *bitReader) bit() uint32 {
	if r.pos >= r.n {
		r.pos = r.n
		return r.overrun()
	}
	b := (r.buf[r.pos>>3] >> (7 - uint(r.pos&7))) & 1
	r.pos++
	return uint32(b)
}

// unary counts 1 bits until a 0, consuming the 0, and stops at limit. The
// format's residual coder escapes at 32, so a longer run is something no
// encoder writes; without the bound a stream of 1 bits spins until the buffer
// is exhausted. Returns the count and whether the terminating 0 was reached.
func (r *bitReader) unary(limit int) (int, bool) {
	for n := 0; n <= limit; n++ {
		if r.pos >= r.n {
			r.overrun()
			return n, false
		}
		if r.bit() == 0 {
			return n, true
		}
	}
	return limit + 1, false
}

// skip advances n bits, latching an overrun rather than moving past the end.
func (r *bitReader) skip(n int) {
	if n < 0 || r.pos+n > r.n {
		r.pos = r.n
		r.overrun()
		return
	}
	r.pos += n
}

// bitAppender packs a bit run into a byte slice aligned at bit 0. It is the
// cross-packet carry: what a packet leaves unread is not byte aligned.
type bitAppender struct {
	buf  []byte
	bits int
}

func (w *bitAppender) reset() { w.buf, w.bits = w.buf[:0], 0 }

// appendFrom copies n bits out of src starting at bit position at.
//
// A frame spans packets and a packet is twelve kilobytes, so this runs over
// nearly every byte of the stream and its shape is worth more than it looks:
// the byte-at-a-time loop below was a quarter of the decode. When the two bit
// offsets line up, which is most of a packet after the first byte, the middle
// is a plain copy.
func (w *bitAppender) appendFrom(src []byte, at, n int) {
	// Fill to a destination byte boundary first, which is what makes the
	// aligned case below reachable at all.
	if off := w.bits & 7; off != 0 {
		take := min(8-off, n)
		w.buf[w.bits>>3] |= byte(peekByteBits(src, at, take) << uint(8-off-take))
		w.bits += take
		at += take
		n -= take
	}
	if n == 0 {
		return
	}
	if at&7 == 0 {
		// Both sides are byte aligned: copy whole bytes and leave the tail.
		whole := n >> 3
		i := at >> 3
		w.buf = append(w.buf, src[i:i+whole]...)
		w.bits += whole << 3
		at += whole << 3
		n -= whole << 3
	}
	// Unaligned: shift whole bytes across, which is two loads and an append
	// rather than the branchy general read below.
	if sh := uint(at & 7); sh != 0 {
		i := at >> 3
		for n >= 8 && i+1 < len(src) {
			w.buf = append(w.buf, src[i]<<sh|src[i+1]>>(8-sh))
			w.bits += 8
			i++
			at += 8
			n -= 8
		}
	}
	for n > 0 {
		if w.bits&7 == 0 {
			w.buf = append(w.buf, 0)
		}
		free := 8 - w.bits&7
		take := min(free, n)
		w.buf[w.bits>>3] |= byte(peekByteBits(src, at, take) << uint(free-take))
		w.bits += take
		at += take
		n -= take
	}
}

// peekByteBits reads up to 8 bits from src at bit position at, right aligned,
// padding with zeroes past the end.
func peekByteBits(src []byte, at, n int) uint32 {
	i := at >> 3
	off := uint(at & 7)
	var acc uint32
	if i < len(src) {
		acc = uint32(src[i]) << 8
	}
	if i+1 < len(src) {
		acc |= uint32(src[i+1])
	}
	return (acc >> (16 - off - uint(n))) & (1<<uint(n) - 1)
}
