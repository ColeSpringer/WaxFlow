//go:build !wmatablesgen

package wma

// bitReader reads a WMA superframe most-significant-bit first. WMA has no sync
// word and no in-band framing, so nothing in a packet re-anchors a reader that
// has drifted: overruns are the only signal a walk has gone wrong, and every
// one of them has to reach the caller rather than return zeroes. The reader
// latches the first overrun and reports it through err; readers check it at the
// points where a wrong answer would otherwise be indistinguishable from a
// legitimately odd stream (the gain ladder, the coefficient loop).
type bitReader struct {
	buf []byte
	n   int // valid bits, which is not len(buf)*8 for the carry
	pos int // bits consumed
	err error
}

func (r *bitReader) reset(b []byte) {
	r.resetBits(b, len(b)*8)
}

// resetBits reads the first n bits of b and treats the rest as absent. The
// reservoir carry needs it: a carry stops mid-byte, and the padding zeroes to
// the next boundary are not payload. Reading them back as payload lets a walk
// that should have overrun finish instead, which is the one failure this
// reader exists to prevent.
func (r *bitReader) resetBits(b []byte, n int) {
	r.buf, r.n, r.pos, r.err = b, n, 0, nil
}

// bitLen is the reader's length in bits.
func (r *bitReader) bitLen() int { return r.n }

// overrun latches a read past the end. The first one wins, so the message
// names where the walk actually left the packet.
func (r *bitReader) overrun() uint32 {
	if r.err == nil {
		r.err = malformed("the frame reads past the end of its %d-byte packet", len(r.buf))
	}
	return 0
}

// maxBits is the widest field this reader can return, and it is a property of
// the reader rather than of the format: bits and peek assemble a four-byte
// window from the containing byte, so a field starting seven bits into it has
// 25 left. Past that the shift zero-fills the top of the answer at seven of
// every eight bit positions -- no panic and no error, since Go defines an
// over-wide unsigned shift as zero -- so the bound is asserted below rather
// than left to a comment.
const maxBits = 25

// Every width fed to bits is a constant or bounded by maxOffsetBits, so the
// bound is checked once here: raising maxOffsetBits past the window breaks the
// build instead of silently truncating the superframe's bit offset. The books
// fed to peek are checked by TestBookCodewordsFitTheReaderWindow.
const _ = uint(maxBits - maxOffsetBits)

// bits reads n bits, 0 <= n <= maxBits. Wider fields do not exist in this
// format; the superframe's bit offset is the widest at maxOffsetBits.
func (r *bitReader) bits(n int) uint32 {
	if n == 0 {
		return 0
	}
	if r.pos+n > r.bitLen() {
		r.pos = r.bitLen()
		return r.overrun()
	}
	// Read four bytes from the containing byte and shift the field down. The
	// tail is short of four bytes at the very end of a packet, so it assembles
	// byte by byte there.
	i := r.pos >> 3
	off := r.pos & 7
	var acc uint32
	if i+4 <= len(r.buf) {
		acc = uint32(r.buf[i])<<24 | uint32(r.buf[i+1])<<16 | uint32(r.buf[i+2])<<8 | uint32(r.buf[i+3])
	} else {
		for j := range 4 {
			acc <<= 8
			if i+j < len(r.buf) {
				acc |= uint32(r.buf[i+j])
			}
		}
	}
	r.pos += n
	return (acc << off) >> (32 - n)
}

// bit reads one bit.
func (r *bitReader) bit() uint32 {
	if r.pos >= r.bitLen() {
		r.pos = r.bitLen()
		return r.overrun()
	}
	b := (r.buf[r.pos>>3] >> (7 - r.pos&7)) & 1
	r.pos++
	return uint32(b)
}

// peek returns the next n bits without consuming them, n <= maxBits, padded
// past the end of the buffer. The VLC walk needs a fixed-width window and
// settles the overrun itself: a codeword that only decodes because of the
// padding is caught when the walk consumes more bits than remain.
func (r *bitReader) peek(n int) uint32 {
	i := r.pos >> 3
	off := r.pos & 7
	var acc uint32
	for j := range 4 {
		acc <<= 8
		if i+j < len(r.buf) {
			acc |= uint32(r.buf[i+j])
		}
	}
	return (acc << off) >> (32 - n)
}

// skip advances n bits, latching an overrun rather than moving past the end.
func (r *bitReader) skip(n int) {
	if r.pos+n > r.bitLen() {
		r.pos = r.bitLen()
		r.overrun()
		return
	}
	r.pos += n
}

// align advances to the next byte boundary. v1 does this after each channel
// slot of a stereo block; v2 never does.
func (r *bitReader) align() {
	if n := r.pos & 7; n != 0 {
		r.skip(8 - n)
	}
}

// bitAppender packs a bit run into a fresh byte slice aligned at bit 0. The
// reservoir carry needs it: what a packet leaves unread is not byte aligned,
// so a carry stored as raw bytes plus an offset would resume the next frame up
// to seven bits off.
type bitAppender struct {
	buf  []byte
	bits int
}

func (w *bitAppender) reset() { w.buf, w.bits = w.buf[:0], 0 }

// appendFrom copies n bits out of src starting at bit position at; a bit past
// the end of src reads as zero.
//
// What a packet leaves unread is most of a packet, so this is filled by
// bytes: the head to a byte boundary, the middle a byte at a time with one
// shift, and only the tail by the general read.
func (w *bitAppender) appendFrom(src []byte, at, n int) {
	if n <= 0 {
		return
	}
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
	// A run that starts past the source has no whole byte to copy; the padded
	// read below takes it.
	if at&7 == 0 && at>>3 < len(src) {
		whole := min(n>>3, len(src)-at>>3)
		i := at >> 3
		w.buf = append(w.buf, src[i:i+whole]...)
		w.bits += whole << 3
		at += whole << 3
		n -= whole << 3
	}
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
