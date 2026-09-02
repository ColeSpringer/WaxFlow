package musepack

import "math/bits"

// bitReader reads MSB-first bit fields from a byte slice at an absolute bit
// position, bounded by a bit length that need not be a whole number of bytes:
// an SV7 frame states its length in bits, and the pad bits behind it are not
// payload. A read past the end latches over and returns zeros; every parse
// loop checks it where a wrong answer would otherwise pass as an odd stream.
//
// Both stream versions read through this one reader. SV7 is written as
// little-endian 32-bit words read MSB-first, and the container realigns each
// frame into a plain MSB-first byte run before it reaches here, so there is no
// word-swapped reader.
type bitReader struct {
	data []byte
	pos  int // next bit, absolute from the start of data
	end  int // valid bits
	over bool
}

func (r *bitReader) reset(data []byte, bitLen int) {
	r.data, r.pos, r.end, r.over = data, 0, bitLen, false
}

// remaining is the unread bit count.
func (r *bitReader) remaining() int { return r.end - r.pos }

// overrun latches a read past the end.
func (r *bitReader) overrun() uint32 {
	r.over = true
	r.pos = r.end
	return 0
}

// window loads eight bytes from the byte holding pos, zero-padded past the
// slice, so a field of up to 32 bits at any bit offset fits in one shift.
func (r *bitReader) window() uint64 {
	i := r.pos >> 3
	if i+8 <= len(r.data) {
		return uint64(r.data[i])<<56 | uint64(r.data[i+1])<<48 | uint64(r.data[i+2])<<40 | uint64(r.data[i+3])<<32 |
			uint64(r.data[i+4])<<24 | uint64(r.data[i+5])<<16 | uint64(r.data[i+6])<<8 | uint64(r.data[i+7])
	}
	var acc uint64
	for j := 0; j < 8; j++ {
		acc <<= 8
		if i+j < len(r.data) {
			acc |= uint64(r.data[i+j])
		}
	}
	return acc
}

// bits reads n bits, 0 <= n <= 32.
func (r *bitReader) bits(n int) uint32 {
	if n == 0 {
		return 0
	}
	if r.pos+n > r.end {
		return r.overrun()
	}
	v := uint32(r.window() << (r.pos & 7) >> (64 - n))
	r.pos += n
	return v
}

// bit reads one bit.
func (r *bitReader) bit() uint32 {
	if r.pos >= r.end {
		return r.overrun()
	}
	b := r.data[r.pos>>3] >> (7 - r.pos&7) & 1
	r.pos++
	return uint32(b)
}

// peek returns the next n bits without consuming them, n <= 32, zero-padded
// past the end. The Huffman walk needs a fixed window and settles the overrun
// itself when it consumes.
func (r *bitReader) peek(n int) uint32 {
	return uint32(r.window() << (r.pos & 7) >> (64 - n))
}

// MaxVarintBytes bounds a variable-length size: nine bytes carry 63 bits,
// which is every size a stream could state. The container reads packet sizes
// against the same bound.
const MaxVarintBytes = 9

const maxVarintBytes = MaxVarintBytes

// varint reads the SV8 variable-length size (mpc_bits_get_size): seven bits
// per byte, most significant first, the high bit continuing.
func (r *bitReader) varint() (uint64, bool) {
	var v uint64
	for i := 0; i < maxVarintBytes; i++ {
		b := r.bits(8)
		v = v<<7 | uint64(b&0x7F)
		if b&0x80 == 0 {
			return v, !r.over
		}
	}
	return 0, false
}

// golomb reads a Rice/Golomb code with k remainder bits (mpc_bits_golomb_dec):
// a unary quotient, then k bits. The quotient is bounded by the data left, so
// a run of zeros at the end of a packet ends the read rather than the process.
func (r *bitReader) golomb(k int) (int32, bool) {
	q := 0
	for r.bit() == 0 {
		if r.over {
			return 0, false
		}
		q++
		if q > 31-k {
			return 0, false
		}
	}
	v := int32(q)<<k | int32(r.bits(k))
	return v, !r.over
}

// logDec reads a value in 0..max as the reference's truncated binary code
// (mpc_bits_log_dec): with V = max+1 values and L = ceil(log2 V), the first
// 2^L - V values take L-1 bits and the rest L. The reference's log2 and
// log2_lost tables are exactly this arithmetic, so they are computed rather
// than transcribed.
func (r *bitReader) logDec(max int) int {
	if max <= 0 {
		return 0
	}
	return int(r.truncated(uint32(max + 1)))
}

// truncated reads one of v values (v >= 2) in truncated binary.
func (r *bitReader) truncated(v uint32) uint32 {
	l := bits.Len32(v - 1)
	lost := uint32(1)<<l - v
	code := r.bits(l - 1)
	if code >= lost {
		code = (code<<1 | r.bit()) - lost
	}
	return code
}

// maxEnum is the widest enumeration the format codes: an 18-sample half for
// the res-1 quantiser and one mid/side flag per band, 32 at most.
const maxEnum = 32

// binomial is C(n, k) for n <= maxEnum, k <= maxEnum/2: the reference's Cnk
// table, and the source of its Cnk_len and Cnk_lost tables too.
var binomial = func() (c [maxEnum + 1][maxEnum/2 + 1]uint32) {
	for n := 0; n <= maxEnum; n++ {
		c[n][0] = 1
		for k := 1; k <= maxEnum/2 && k <= n; k++ {
			c[n][k] = c[n-1][k-1] + c[n-1][k]
		}
	}
	return c
}()

// enumDec reads which k of n positions are set (mpc_bits_enum_dec): the index
// of the combination in truncated binary over C(n, k) values, then the
// combinatorial number system walked from the top position down. 1 <= k <=
// n/2 and n <= maxEnum, which the callers guarantee.
func (r *bitReader) enumDec(k, n int) uint32 {
	code := r.truncated(binomial[n][k])
	var set uint32
	for k > 0 && n > 0 {
		n--
		if c := binomial[n][k]; code >= c {
			set |= 1 << n
			code -= c
			k--
		}
	}
	return set
}

// BitReader is the reader the container uses on SV8 header packets: the
// stream header's fields, the seek table's Golomb codes and the variable-length
// sizes are the codec's syntax, so they are decoded by the codec's reader
// rather than restated.
type BitReader struct{ r bitReader }

// NewBitReader reads b from its first bit.
func NewBitReader(b []byte) *BitReader {
	br := &BitReader{}
	br.r.reset(b, len(b)*8)
	return br
}

// Bits reads n bits, n <= 32.
func (b *BitReader) Bits(n int) uint32 { return b.r.bits(n) }

// Varint reads a variable-length size.
func (b *BitReader) Varint() (uint64, bool) { return b.r.varint() }

// Golomb reads a Golomb code with k remainder bits.
func (b *BitReader) Golomb(k int) (int32, bool) { return b.r.golomb(k) }

// Pos is the next bit's position.
func (b *BitReader) Pos() int { return b.r.pos }

// Overrun reports a read past the end.
func (b *BitReader) Overrun() bool { return b.r.over }

// ReadVarint reads a variable-length size off the front of b and returns it
// with the byte count it took, or ok false when b ends inside it or it runs
// past maxVarintBytes.
func ReadVarint(b []byte) (v uint64, n int, ok bool) {
	for n < len(b) && n < maxVarintBytes {
		c := b[n]
		n++
		v = v<<7 | uint64(c&0x7F)
		if c&0x80 == 0 {
			return v, n, true
		}
	}
	return 0, n, false
}
