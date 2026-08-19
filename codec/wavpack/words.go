package wavpack

import (
	"encoding/binary"
	"math/bits"
)

// The entropy coder. WavPack does not use plain Rice codes: it splits the
// value range at three running medians (roughly 5/7, 10/49, and 20/343 of the
// samples), sends the zone as a unary count, and codes the offset within the
// zone with a minimum-length binary code. The medians move after every sample,
// so encoder and decoder stay in step with no side information beyond the
// three starting values each block carries.
//
// Two coder details are load-bearing for bit-exactness and easy to lose in a
// port. The unary count is shared between adjacent samples: an odd count
// leaves a "holding one" that the next sample's count starts from, and an even
// one leaves a "holding zero" that skips the next sample's unary field
// entirely. And a run of samples whose medians have all collapsed to zero is
// coded as an Elias-gamma run length instead of as individual zeros.

// limitOnes is the longest unary zone count sent literally; at that many ones
// the count escapes to an Elias-gamma field.
const limitOnes = 16

// Median update steps. Each zone's median rises by 5/N of itself when a
// sample lands at or above it and falls by 2/N when it lands below, with N the
// zone's sample share.
const (
	div0 = 128 // 5/7 of samples
	div1 = 64  // 10/49 of samples
	div2 = 32  // 20/343 of samples
)

// entropyChan is one channel's three medians. They are stored 16 times their
// nominal value so the update steps stay integral.
type entropyChan struct {
	median [3]uint32
}

func (c *entropyChan) med(i int) uint32 { return (c.median[i] >> 4) + 1 }

func (c *entropyChan) incMed0() { c.median[0] += ((c.median[0] + div0) / div0) * 5 }
func (c *entropyChan) decMed0() { c.median[0] -= ((c.median[0] + (div0 - 2)) / div0) * 2 }
func (c *entropyChan) incMed1() { c.median[1] += ((c.median[1] + div1) / div1) * 5 }
func (c *entropyChan) decMed1() { c.median[1] -= ((c.median[1] + (div1 - 2)) / div1) * 2 }
func (c *entropyChan) incMed2() { c.median[2] += ((c.median[2] + div2) / div2) * 5 }
func (c *entropyChan) decMed2() { c.median[2] -= ((c.median[2] + (div2 - 2)) / div2) * 2 }

// bitReader reads a WavPack sub-block bitstream, least significant bit first.
// The reference reads 16-bit words on little-endian hosts, which for LSB-first
// packing is the same bit order as reading bytes; bytes are simpler and never
// read further ahead than the reference does.
//
// Reads past the end return zero bits and latch over, which the block decode
// reports as a truncation. The reference instead wraps to the start of the
// sub-block and flags an error, which decodes to different garbage; either way
// the stream is rejected, and one that never overruns decodes identically.
type bitReader struct {
	data []byte
	pos  int
	sr   uint64 // shift register, next bit in the low position
	bc   int    // valid bits in sr
	over bool
}

func (b *bitReader) reset(data []byte) {
	*b = bitReader{data: data}
}

// open reports whether the reader has a sub-block behind it.
func (b *bitReader) open() bool { return b.data != nil }

// fill makes at least n bits available, n <= 32. Callers only ever ask with
// bc < n <= 32, so bc is at most 31 on entry and the four-byte refill leaves
// at most 63 bits in the register. Loading four little-endian bytes at once is
// the same bit order as four single-byte loads, so the fast path is a pure
// speedup, worth about 2.5% of decode on the noise benchmark.
func (b *bitReader) fill(n int) {
	for b.bc < n {
		if b.bc <= 31 && b.pos+4 <= len(b.data) {
			b.sr |= uint64(binary.LittleEndian.Uint32(b.data[b.pos:])) << uint(b.bc)
			b.pos += 4
			b.bc += 32
			continue
		}
		var v uint64
		if b.pos < len(b.data) {
			v = uint64(b.data[b.pos])
			b.pos++
		} else {
			b.over = true
		}
		b.sr |= v << uint(b.bc)
		b.bc += 8
	}
}

func (b *bitReader) getBit() uint32 {
	b.fill(1)
	bit := uint32(b.sr) & 1
	b.sr >>= 1
	b.bc--
	return bit
}

func (b *bitReader) getBits(n int) uint32 {
	if n <= 0 {
		return 0
	}
	b.fill(n)
	v := uint32(b.sr) & uint32((1<<uint(n))-1)
	b.sr >>= uint(n)
	b.bc -= n
	return v
}

// readCode reads one value in 0..maxcode using the fewest whole bits that can
// carry it: the low codes take one bit less than the high ones, so a range
// that is not a power of two wastes no more than the leftover.
func (b *bitReader) readCode(maxcode uint32) uint32 {
	if maxcode < 2 {
		if maxcode == 0 {
			return 0
		}
		return b.getBit()
	}
	n := bits.Len32(maxcode)
	extras := uint32((uint64(1) << uint(n)) - uint64(maxcode) - 1)
	b.fill(n)
	code := uint32(b.sr) & uint32((1<<uint(n-1))-1)
	if code >= extras {
		code = (code << 1) - extras + (uint32(b.sr>>uint(n-1)) & 1)
		b.sr >>= uint(n)
		b.bc -= n
	} else {
		b.sr >>= uint(n - 1)
		b.bc -= n - 1
	}
	return code
}

// readElias reads the escaped count form: a unary length, then that many
// bits below an implicit leading one.
func (b *bitReader) readElias() (uint32, bool) {
	cbits := 0
	for cbits < 33 && b.getBit() != 0 {
		cbits++
	}
	if cbits == 33 {
		return 0, false
	}
	if cbits < 2 {
		return uint32(cbits), true
	}
	var v, mask uint32 = 0, 1
	for k := 1; k < cbits; k++ {
		if b.getBit() != 0 {
			v |= mask
		}
		mask <<= 1
	}
	return v | mask, true
}

// wordCoder is the entropy decoder's state for one block: two channels of
// medians plus the shared unary carry and zero-run accumulator.
type wordCoder struct {
	c           [2]entropyChan
	holdingOne  bool
	holdingZero bool
	zerosAcc    uint32
}

// getWordsLossless decodes n frames into buf, interleaved for stereo. It
// returns the number of frames decoded, short only when the bitstream ran out
// mid-stream. This is the lossless-only path: the hybrid coder's error limit
// is always zero here, so every value comes from a plain readCode.
func (w *wordCoder) getWordsLossless(bs *bitReader, buf []int32, n int, mono bool) int {
	total := n
	if !mono {
		total *= 2
	}
	chanOf := func(i int) *entropyChan {
		if mono {
			return &w.c[0]
		}
		return &w.c[i&1]
	}
	i := 0
	for i < total {
		c := chanOf(i)
		// A held zero means the previous sample's unary count was even, so
		// this sample's zone is the lowest one with no field of its own.
		if w.holdingZero {
			w.holdingZero = false
			low := bs.readCode(c.med(0) - 1)
			c.decMed0()
			buf[i] = signed(bs.getBit(), low)
			i++
			if i == total {
				break
			}
			c = chanOf(i)
		}
		// Collapsed medians on both channels mean silence or near-silence,
		// which is coded as a run length rather than sample by sample.
		if w.c[0].median[0] < 2 && !w.holdingOne && w.c[1].median[0] < 2 {
			if w.zerosAcc != 0 {
				if w.zerosAcc--; w.zerosAcc != 0 {
					buf[i] = 0
					i++
					continue
				}
			} else {
				run, ok := bs.readElias()
				if !ok {
					break
				}
				w.zerosAcc = run
				if run != 0 {
					w.c[0].median = [3]uint32{}
					w.c[1].median = [3]uint32{}
					buf[i] = 0
					i++
					continue
				}
			}
		}
		// The zone count is unary, and it is shared with the next sample:
		// its low bit is that sample's carry, so an odd count leaves a held
		// one and an even one a held zero. Counts stay uint32 the whole way
		// because the escape form can reach 2^31 and the arithmetic below is
		// meant to wrap exactly as the reference's does.
		var ones uint32
		for ones < limitOnes+1 && bs.getBit() != 0 {
			ones++
		}
		if ones >= limitOnes {
			if ones == limitOnes+1 {
				break
			}
			esc, ok := bs.readElias()
			if !ok {
				break
			}
			ones = esc + limitOnes
		}
		var carry uint32
		if w.holdingOne {
			carry = 1
		}
		w.holdingOne = ones&1 != 0
		w.holdingZero = !w.holdingOne
		ones = ones>>1 + carry

		var low, high uint32
		switch {
		case ones == 0:
			high = c.med(0) - 1
			c.decMed0()
		case ones == 1:
			low = c.med(0)
			c.incMed0()
			high = low + c.med(1) - 1
			c.decMed1()
		case ones == 2:
			low = c.med(0)
			c.incMed0()
			low += c.med(1)
			c.incMed1()
			high = low + c.med(2) - 1
			c.decMed2()
		default:
			low = c.med(0)
			c.incMed0()
			low += c.med(1)
			c.incMed1()
			low += (ones - 2) * c.med(2)
			high = low + c.med(2) - 1
			c.incMed2()
		}
		low += bs.readCode(high - low)
		buf[i] = signed(bs.getBit(), low)
		i++
	}
	if mono {
		return i
	}
	return i / 2
}

// signed applies the trailing sign bit: a set bit means the ones complement
// of the magnitude, which is how negative values ride the same code space.
func signed(sign, mag uint32) int32 {
	if sign != 0 {
		return ^int32(mag)
	}
	return int32(mag)
}

// exp2Table is the reference's 256-entry mantissa table for its fixed-point
// base-2 exponential. The format stores medians and decorrelation history as
// log values so a 16-bit field spans the whole 32-bit range; only the
// exponential is needed to decode.
var exp2Table = [256]byte{
	0x00, 0x01, 0x01, 0x02, 0x03, 0x03, 0x04, 0x05, 0x06, 0x06, 0x07, 0x08, 0x08, 0x09, 0x0a, 0x0b,
	0x0b, 0x0c, 0x0d, 0x0e, 0x0e, 0x0f, 0x10, 0x10, 0x11, 0x12, 0x13, 0x13, 0x14, 0x15, 0x16, 0x16,
	0x17, 0x18, 0x19, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1d, 0x1e, 0x1f, 0x20, 0x20, 0x21, 0x22, 0x23,
	0x24, 0x24, 0x25, 0x26, 0x27, 0x28, 0x28, 0x29, 0x2a, 0x2b, 0x2c, 0x2c, 0x2d, 0x2e, 0x2f, 0x30,
	0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x3a, 0x3b, 0x3c, 0x3d,
	0x3e, 0x3f, 0x40, 0x41, 0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, 0x48, 0x49, 0x4a, 0x4b,
	0x4c, 0x4d, 0x4e, 0x4f, 0x50, 0x51, 0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a,
	0x5b, 0x5c, 0x5d, 0x5e, 0x5e, 0x5f, 0x60, 0x61, 0x62, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68, 0x69,
	0x6a, 0x6b, 0x6c, 0x6d, 0x6e, 0x6f, 0x70, 0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79,
	0x7a, 0x7b, 0x7c, 0x7d, 0x7e, 0x7f, 0x80, 0x81, 0x82, 0x83, 0x84, 0x85, 0x87, 0x88, 0x89, 0x8a,
	0x8b, 0x8c, 0x8d, 0x8e, 0x8f, 0x90, 0x91, 0x92, 0x93, 0x95, 0x96, 0x97, 0x98, 0x99, 0x9a, 0x9b,
	0x9c, 0x9d, 0x9f, 0xa0, 0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa8, 0xa9, 0xaa, 0xab, 0xac, 0xad,
	0xaf, 0xb0, 0xb1, 0xb2, 0xb3, 0xb4, 0xb6, 0xb7, 0xb8, 0xb9, 0xba, 0xbc, 0xbd, 0xbe, 0xbf, 0xc0,
	0xc2, 0xc3, 0xc4, 0xc5, 0xc6, 0xc8, 0xc9, 0xca, 0xcb, 0xcd, 0xce, 0xcf, 0xd0, 0xd2, 0xd3, 0xd4,
	0xd6, 0xd7, 0xd8, 0xd9, 0xdb, 0xdc, 0xdd, 0xde, 0xe0, 0xe1, 0xe2, 0xe4, 0xe5, 0xe6, 0xe8, 0xe9,
	0xea, 0xec, 0xed, 0xee, 0xf0, 0xf1, 0xf2, 0xf4, 0xf5, 0xf6, 0xf8, 0xf9, 0xfa, 0xfc, 0xfd, 0xff,
}

// exp2s turns a stored log value back into the integer it came from: the low
// byte indexes the mantissa table, the rest is the exponent.
func exp2s(log int) int32 {
	if log < 0 {
		return -exp2s(-log)
	}
	value := uint32(exp2Table[log&0xff]) | 0x100
	if log >>= 8; log <= 9 {
		return int32(value >> uint(9-log))
	}
	return int32(value << uint((log-9)&0x1f))
}

// restoreWeight expands a stored decorrelation weight back to its internal
// range of about plus or minus 1024. The rounding correction is the reference's
// and is not symmetric: it undoes the bias the encoder's store applies.
func restoreWeight(w int8) int32 {
	r := int32(w) * 8
	if r > 0 {
		r += (r + 64) >> 7
	}
	return r
}
