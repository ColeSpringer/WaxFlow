package ogg

import (
	"bytes"
	"encoding/binary"
)

// FindStart returns the offset of the first page in window that begins a
// logical stream and whose checksum holds. The checksum is what makes a page
// behind stray bytes certain where the capture pattern alone is four letters.
func FindStart(window []byte) (int, bool) {
	for i := 0; ; i++ {
		j := bytes.Index(window[i:], []byte("OggS"))
		if j < 0 {
			return 0, false
		}
		i += j
		b := window[i:]
		if len(b) < headerLen || b[4] != 0 || b[5]&flagBOS == 0 {
			continue
		}
		size := headerLen + int(b[26])
		if len(b) < size {
			continue
		}
		for _, lace := range b[headerLen:size] {
			size += int(lace)
		}
		if len(b) < size {
			continue
		}
		// The checksum is taken with its own field read as zero.
		crc := crc32(0, b[:22])
		crc = crc32(crc, []byte{0, 0, 0, 0})
		if crc32(crc, b[26:size]) == binary.LittleEndian.Uint32(b[22:]) {
			return i, true
		}
	}
}
