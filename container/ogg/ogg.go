// Package ogg demuxes Ogg streams (RFC 3533): page parsing with CRC
// verification, packet reassembly across pages, and per-mapping content
// handling. The only mapping wired so far is Ogg-FLAC (the Xiph FLAC-in-
// Ogg mapping, version 1); Vorbis and Opus land with their codec
// codecs. Other mappings are still recognized by name so errors can
// say what was found.
//
// Seeking bisects on page granule positions to get near the target, then
// lands exactly using the positions FLAC frame headers carry themselves;
// the demuxer hands format.Media the frame containing the target and
// Media pre-rolls to the sample.
package ogg

// Page framing constants (RFC 3533 section 6).
const (
	headerLen = 27
	// maxPageSize bounds one page: header, 255 lacing values, and 255
	// segments of 255 bytes.
	maxPageSize = headerLen + 255 + 255*255
)

// Header type flags.
const (
	flagContinued = 0x01
	flagBOS       = 0x02
	flagEOS       = 0x04
)

// Match reports whether head begins with an Ogg page capture pattern. It
// is the format sniff-table entry.
func Match(head []byte) bool {
	return len(head) >= 4 && string(head[:4]) == "OggS"
}

// crcTable is the Ogg page checksum table: polynomial 0x04C11DB7,
// unreflected, zero initial value and no final XOR (RFC 3533 section 6).
var crcTable = func() (t [256]uint32) {
	for i := range t {
		c := uint32(i) << 24
		for range 8 {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04C11DB7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return
}()

func crc32(crc uint32, b []byte) uint32 {
	for _, v := range b {
		crc = crc<<8 ^ crcTable[uint8(crc>>24)^v]
	}
	return crc
}

// crcPow[k] is x^(8*2^k) modulo the polynomial: a register of 1 advanced
// over 2^k zero bytes.
var crcPow = func() (t [64]uint32) {
	t[0] = crc32(1, []byte{0})
	for k := 1; k < len(t); k++ {
		t[k] = crcMul(t[k-1], t[k-1])
	}
	return
}()

// crcMul is a times b modulo the checksum polynomial, each a register's
// polynomial with bit 31 its x^31 term.
func crcMul(a, b uint32) uint32 {
	var r uint32
	for ; a != 0; a >>= 1 {
		if a&1 != 0 {
			r ^= b
		}
		if b&0x80000000 != 0 {
			b = b<<1 ^ 0x04C11DB7
		} else {
			b <<= 1
		}
	}
	return r
}

// crcShift is crc advanced over n zero bytes, which is what a checksum's
// leading part contributes once n more bytes follow it.
func crcShift(crc uint32, n int64) uint32 {
	for k := 0; n > 0; k, n = k+1, n>>1 {
		if n&1 != 0 {
			crc = crcMul(crc, crcPow[k])
		}
	}
	return crc
}
