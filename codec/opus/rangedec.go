package opus

import "math/bits"

// rangeDecoder is Opus's entropy decoder (RFC 6716 section 4.1), a range coder
// that reads range-coded symbols from the front of the packet and raw bits from
// the back. It is a clean-room port of libopus entdec.c;
// the arithmetic is bit-exact with the reference, which the RFC requires.
type rangeDecoder struct {
	buf     []byte
	storage int
	offs    int    // next byte from the front
	endOffs int    // bytes consumed from the back
	endWin  uint32 // raw-bit window (from the back)
	nEnd    int    // valid bits in endWin
	nbits   int    // total bits consumed, for ec_tell
	rng     uint32
	val     uint32
	ext     uint32 // scratch shared by decode/update
	rem     int    // last byte read from the front (normalize straddle)
}

// Range coder constants (RFC 6716 section 4.1 / libopus).
const (
	ecSymBits  = 8
	ecCodeBits = 32
	ecSymMax   = 0xFF
	ecCodeTop  = 1 << 31
	ecCodeBot  = ecCodeTop >> 8               // 1<<23
	ecCodeXtra = (ecCodeBits-2)%ecSymBits + 1 // 7
	ecWindow   = 32
	ecUintBits = 8
)

func newRangeDecoder(buf []byte) *rangeDecoder {
	d := &rangeDecoder{buf: buf, storage: len(buf)}
	d.nbits = ecCodeBits + 1 - ((ecCodeBits-ecCodeXtra)/ecSymBits)*ecSymBits
	d.rng = 1 << ecCodeXtra
	rem := d.readByte()
	d.val = d.rng - 1 - uint32(rem>>(ecSymBits-ecCodeXtra))
	d.rem = rem
	d.normalize()
	return d
}

func (d *rangeDecoder) readByte() int {
	if d.offs < d.storage {
		b := int(d.buf[d.offs])
		d.offs++
		return b
	}
	return 0
}

func (d *rangeDecoder) readByteFromEnd() int {
	if d.endOffs < d.storage {
		d.endOffs++
		return int(d.buf[d.storage-d.endOffs])
	}
	return 0
}

func (d *rangeDecoder) normalize() {
	for d.rng <= ecCodeBot {
		sym := d.rem
		d.rem = d.readByte()
		sym = (sym<<ecSymBits | d.rem) >> (ecSymBits - ecCodeXtra)
		d.val = ((d.val << ecSymBits) + uint32(ecSymMax & ^sym)) & (ecCodeTop - 1)
		d.rng <<= ecSymBits
		d.nbits += ecSymBits
	}
}

// decode returns the current cumulative frequency in [0, ft).
func (d *rangeDecoder) decode(ft uint32) uint32 {
	d.ext = d.rng / ft
	s := d.val / d.ext
	if s+1 < ft {
		return ft - (s + 1)
	}
	return 0
}

// decodeBin is decode with ft == 1<<bits (RFC 6716 4.1.3.1).
func (d *rangeDecoder) decodeBin(bits uint) uint32 {
	d.ext = d.rng >> bits
	s := d.val / d.ext
	ft := uint32(1) << bits
	if s+1 < ft {
		return ft - (s + 1)
	}
	return 0
}

// update advances the decoder after a symbol with cumulative range [fl, fh) of
// total ft (RFC 6716 4.1.2).
func (d *rangeDecoder) update(fl, fh, ft uint32) {
	s := d.ext * (ft - fh)
	d.val -= s
	if fl > 0 {
		d.rng = d.ext * (fh - fl)
	} else {
		d.rng -= s
	}
	d.normalize()
}

// decodeBitLogp decodes one bit whose probability of being zero is
// 1 - 2^-logp (RFC 6716 4.1.3.2).
func (d *rangeDecoder) decodeBitLogp(logp uint) int {
	r := d.rng
	s := r >> logp
	var ret int
	if d.val < s {
		ret = 1
	} else {
		d.val -= s
	}
	if ret == 1 {
		d.rng = s
	} else {
		d.rng = r - s
	}
	d.normalize()
	return ret
}

// decodeICDF decodes a symbol from an inverse cumulative distribution table
// scaled to 2^ftb (RFC 6716 4.1.3.3). icdf is non-increasing and ends in 0.
func (d *rangeDecoder) decodeICDF(icdf []byte, ftb uint) int {
	r := d.rng >> ftb
	ret := -1
	s := d.rng
	var t uint32
	for {
		t = s
		ret++
		s = r * uint32(icdf[ret])
		if d.val >= s {
			break
		}
	}
	d.val -= s
	d.rng = t - s
	d.normalize()
	return ret
}

// decodeRawBits reads bits raw from the back of the packet (RFC 6716 4.1.4).
func (d *rangeDecoder) decodeRawBits(bits uint) uint32 {
	window := d.endWin
	available := d.nEnd
	if uint(available) < bits {
		for {
			window |= uint32(d.readByteFromEnd()) << available
			available += ecSymBits
			if available > ecWindow-ecSymBits {
				break
			}
		}
	}
	ret := window & ((1 << bits) - 1)
	window >>= bits
	available -= int(bits)
	d.endWin = window
	d.nEnd = available
	// Raw bits consumed from the back count toward the total, exactly like the
	// front reads in normalize (libopus ec_dec_bits: nbits_total += _bits).
	// tell()/tellFrac() must not under-report, or CELT bit allocation desyncs.
	d.nbits += int(bits)
	return ret
}

// decodeUint decodes a uniformly distributed integer in [0, ft) (RFC 6716
// 4.1.5). ft must be at least 1.
func (d *rangeDecoder) decodeUint(ft uint32) uint32 {
	ft--
	ftb := ilog(ft)
	if ftb > ecUintBits {
		ftb -= ecUintBits
		t := (ft >> uint(ftb)) + 1
		s := d.decode(t)
		d.update(s, s+1, t)
		v := s<<uint(ftb) | d.decodeRawBits(uint(ftb))
		if v <= ft {
			return v
		}
		// Out-of-range raw tail: the packet is corrupt. Clamp and keep
		// decoding, matching the reference (ec_dec_uint returns ft and the
		// decode carries on); a decoder is robust, not validating, and the
		// bounded output on hostile input is what the fuzzer asserts.
		return ft
	}
	ft++
	s := d.decode(ft)
	d.update(s, s+1, ft)
	return s
}

// tell returns the number of bits consumed so far (ceil), used by CELT bit
// allocation (RFC 6716 4.1.6).
func (d *rangeDecoder) tell() int {
	return d.nbits - ilog(d.rng)
}

// tellFrac returns bits consumed in eighth-bit units (RFC 6716 4.1.6).
func (d *rangeDecoder) tellFrac() int {
	return ecTellFrac(d.nbits, d.rng)
}

// ecTellFrac computes bits used in eighth-bit units from a coder's bit count
// and range register (RFC 6716 section 4.1.6): rng is corrected to 15
// significant bits, then the fractional part comes from the table. The
// encoder and decoder share it because bit allocation must see identical
// accounting on both sides.
func ecTellFrac(nbits int, rng uint32) int {
	n := nbits << 3
	l := ilog(rng)
	r := rng >> uint(l-16)
	b := (r >> 12) - 8
	if r > correctionThresh[b] {
		b++
	}
	l = l*8 + int(b)
	return n - l
}

// correctionThresh is the tell_frac fractional-bit correction table (libopus).
var correctionThresh = [8]uint32{35733, 38967, 42495, 46340, 50535, 55109, 60097, 65535}

// ilog returns floor(log2(x)) + 1 for x > 0, and 0 for x == 0.
func ilog(x uint32) int { return bits.Len32(x) }
