package alac

import (
	"encoding/binary"
	"math/bits"
)

// bitReader reads MSB-first bit fields from a packet. Byte accesses past
// the end return zero so a 32-bit peek near the tail never panics; callers
// bound real reads with validBits. The adaptive-Golomb routines address the
// underlying bytes directly through peek32 and streamBits, mirroring the
// reference decoder's byte+bit-offset model.
type bitReader struct {
	data      []byte
	pos       int // next bit, absolute from the start of data
	validBits int // len(payload)*8; reads past this are damage
}

// byteAt returns data[i] or zero past the end.
func (r *bitReader) byteAt(i int) uint32 {
	if i < len(r.data) {
		return uint32(r.data[i])
	}
	return 0
}

// read returns the next n bits (n <= 32), MSB-first, advancing the cursor.
func (r *bitReader) read(n uint) uint32 {
	if n == 0 {
		return 0
	}
	byteOff := r.pos >> 3
	bitOff := uint(r.pos & 7)
	var acc uint64
	if byteOff+8 <= len(r.data) {
		acc = binary.BigEndian.Uint64(r.data[byteOff:])
	} else {
		for i := 0; i < 8; i++ {
			acc = acc<<8 | uint64(r.byteAt(byteOff+i))
		}
	}
	r.pos += int(n)
	return uint32((acc >> (64 - bitOff - n)) & (1<<n - 1))
}

// one reads a single bit.
func (r *bitReader) one() uint32 { return r.read(1) }

// advance skips n bits.
func (r *bitReader) advance(n int) { r.pos += n }

// byteAlign rounds the cursor up to the next byte boundary.
func (r *bitReader) byteAlign() {
	if rem := r.pos & 7; rem != 0 {
		r.pos += 8 - rem
	}
}

// overrun reports whether the cursor has passed the valid payload.
func (r *bitReader) overrun() bool { return r.pos > r.validBits }

// read32 returns the four bytes at byte offset i as a big-endian word.
func (r *bitReader) read32(i int) uint32 {
	if i >= 0 && i+4 <= len(r.data) {
		return binary.BigEndian.Uint32(r.data[i:])
	}
	return r.byteAt(i)<<24 | r.byteAt(i+1)<<16 | r.byteAt(i+2)<<8 | r.byteAt(i+3)
}

// peek32 loads the 32-bit window at bit position p, left-justified so the
// next bit is the MSB (the reference's read32bit(in+p/8) << (p&7)).
func (r *bitReader) peek32(p int) uint32 {
	return r.read32(p>>3) << uint(p&7)
}

// streamBits reads numbits (<= 32) at absolute bit position p without
// moving the cursor, spanning a fifth byte when the field straddles the
// window (the reference getstreambits).
func (r *bitReader) streamBits(p int, numbits uint) uint32 {
	load1 := r.read32(p >> 3)
	bo := uint(p & 7)
	var result uint32
	if numbits+bo > 32 {
		result = load1 << bo
		load2 := r.byteAt((p >> 3) + 4)
		load2 >>= 8 - (numbits + bo - 32)
		result >>= 32 - numbits
		result |= load2
	} else {
		result = load1 >> (32 - numbits - bo)
	}
	if numbits != 32 {
		result &= ^(^uint32(0) << numbits)
	}
	return result
}

// Adaptive-Golomb constants (Apple aglib.h).
const (
	qbShift           = 9
	qb                = 1 << qbShift
	mmulShift         = 2
	mDenShift         = qbShift - mmulShift - 1 // 6
	mOff              = 1 << (mDenShift - 2)    // 16
	bitOff            = 24
	maxPrefix16       = 9
	maxPrefix32       = 9
	maxDataTypeBits16 = 16
	nMaxMeanClamp     = 0xFFFF
	nMeanClampVal     = 0xFFFF
)

// lead counts the leading 0 bits of x (the reference lead()); the Golomb
// prefix decoders pass ^stream to count leading 1 bits.
func lead(x uint32) uint32 { return uint32(bits.LeadingZeros32(x)) }

// lg3a returns floor(log2(x+3)).
func lg3a(x uint32) uint32 { return 31 - uint32(bits.LeadingZeros32(x+3)) }

// dynGet32 decodes one adaptive-Golomb value with the 32-bit escape,
// advancing p (the reference dyn_get_32bit).
func (r *bitReader) dynGet32(p *int, m, k, maxbits uint32) uint32 {
	stream := r.peek32(*p)
	result := uint32(bits.LeadingZeros32(^stream))
	if result >= maxPrefix32 {
		result = r.streamBits(*p+maxPrefix32, uint(maxbits))
		*p += maxPrefix32 + int(maxbits)
		return result
	}
	*p += int(result) + 1
	if k != 1 {
		stream <<= result + 1
		v := stream >> (32 - k)
		*p += int(k) - 1
		result *= m
		if v >= 2 {
			result += v - 1
			*p++
		}
	}
	return result
}

// dynGet16 decodes one adaptive-Golomb value with the 16-bit escape (the
// zero-run length), advancing p (the reference dyn_get).
func (r *bitReader) dynGet16(p *int, m, k uint32) uint32 {
	stream := r.peek32(*p)
	pre := uint32(bits.LeadingZeros32(^stream))
	if pre >= maxPrefix16 {
		pre = maxPrefix16
		*p += int(pre)
		stream <<= pre
		result := stream >> (32 - maxDataTypeBits16)
		*p += maxDataTypeBits16
		return result
	}
	*p += int(pre) + 1
	stream <<= pre + 1
	v := stream >> (32 - k)
	*p += int(k)
	result := pre*m + v - 1
	if v < 2 {
		result -= v - 1
		*p--
	}
	return result
}
