// Package ogg demuxes Ogg streams (RFC 3533): page parsing with CRC
// verification, packet reassembly across pages, and per-mapping content
// handling. The only mapping wired so far is Ogg-FLAC (the Xiph FLAC-in-
// Ogg mapping, version 1); Vorbis and Opus land with their codec
// milestones. Other mappings are still recognized by name so errors can
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

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func le64(b []byte) uint64 {
	return uint64(le32(b)) | uint64(le32(b[4:]))<<32
}
