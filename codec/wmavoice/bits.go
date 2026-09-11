//go:build !wmavoicetablesgen

package wmavoice

// The bit layer. WMA Voice is read most significant bit first, bytes in file
// order, and like every codec in this family it carries no sync word: nothing
// inside a packet re-anchors a reader that has drifted, so an overrun is the
// only signal a walk has gone wrong and every one of them has to reach the
// caller rather than return zeroes. The reader latches the first overrun and
// reports it through err.
//
// The shape is codec/wmapro's, copied rather than imported: the two packages
// must not couple, since a change made for one codec's bitstream must not
// silently alter the other's.

type bitReader struct {
	buf []byte
	n   int // valid bits, which is not len(buf)*8 for the carry
	pos int // bits consumed
	err error
}

func (r *bitReader) reset(b []byte) { r.resetBits(b, len(b)*8) }

// resetBits reads the first n bits of b and treats the rest as absent. The
// cross-packet carry needs it: a carry stops mid-byte, and the bits between
// there and the next boundary are not payload.
func (r *bitReader) resetBits(b []byte, n int) {
	r.buf, r.n, r.pos, r.err = b, n, 0, nil
}

// left is how many bits are unread.
func (r *bitReader) left() int { return r.n - r.pos }

// overrun latches a read past the end. The first one wins, so the message
// names where the walk actually left the packet.
func (r *bitReader) overrun() uint64 {
	if r.err == nil {
		r.err = malformed("the walk reads past the end of its %d-bit payload", r.n)
	}
	return 0
}

// maxBits is the widest field this reader returns. The window is eight bytes
// read from the containing byte, so a field starting seven bits into it has 57
// left; the bound is asserted at every call rather than left to a comment,
// because an over-wide unsigned shift in Go is zero rather than a panic.
const maxBits = 57

// window assembles the eight bytes containing the current position, or as many
// as remain, with the rest zero.
func (r *bitReader) window() uint64 {
	i := r.pos >> 3
	if i+8 <= len(r.buf) {
		b := r.buf[i : i+8 : i+8]
		return uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
			uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
	}
	var acc uint64
	for j := range 8 {
		acc <<= 8
		if i+j < len(r.buf) {
			acc |= uint64(r.buf[i+j])
		}
	}
	return acc
}

// bits reads n bits, 0 <= n <= maxBits. A width of zero reads nothing and
// returns zero, which is a meaning the format uses rather than a convenience:
// several field widths are computed and a computed zero means read no bits.
func (r *bitReader) bits(n int) uint64 {
	if n == 0 {
		return 0
	}
	if n < 0 || n > maxBits {
		panic("wmavoice: bit field width out of range")
	}
	if r.pos+n > r.n {
		r.pos = r.n
		return r.overrun()
	}
	v := (r.window() << (r.pos & 7)) >> (64 - n)
	r.pos += n
	return v
}

// bit reads one bit.
func (r *bitReader) bit() uint32 {
	if r.pos >= r.n {
		r.pos = r.n
		r.overrun()
		return 0
	}
	b := (r.buf[r.pos>>3] >> (7 - r.pos&7)) & 1
	r.pos++
	return uint32(b)
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

// seek moves to an absolute bit position, latching an overrun past the end.
func (r *bitReader) seek(at int) {
	if at < 0 || at > r.n {
		r.pos = r.n
		r.overrun()
		return
	}
	r.pos = at
}

// bitAppender packs a bit run into a fresh byte slice aligned at bit 0. The
// cross-packet carry needs it: what a packet leaves unread is not byte
// aligned, so a carry stored as raw bytes plus an offset would resume the next
// superframe up to seven bits off.
type bitAppender struct {
	buf  []byte
	bits int
}

func (w *bitAppender) reset() { w.buf, w.bits = w.buf[:0], 0 }

// appendFrom copies n bits out of src starting at bit position at.
//
// The head is filled to a byte boundary, the middle is copied a byte at a time
// with one shift, and only the tail takes the general read. A carry here runs
// to kilobits on every low-bit-rate passage, so the byte-wise middle is the
// difference between a copy and a per-bit loop over most of the stream.
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
	if at&7 == 0 {
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
