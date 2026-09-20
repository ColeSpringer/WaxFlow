package mpa

// The LAME extension's two checksums are CRC-16/ARC: polynomial
// x^16 + x^15 + x^2 + 1 (0x8005) taken least-significant bit first, which
// is the reflected table constant 0xA001, starting at 0 with no final
// inversion. It is not FLAC's CRC-16, which runs the same polynomial
// unreflected, so the table lives here rather than being shared.

var crc16Table = func() (t [256]uint16) {
	for i := range t {
		c := uint16(i)
		for range 8 {
			if c&1 != 0 {
				c = c>>1 ^ 0xA001
			} else {
				c >>= 1
			}
		}
		t[i] = c
	}
	return
}()

// updateCRC16 extends a running CRC-16/ARC with b, for incremental use
// over the audio frames as they are written.
func updateCRC16(crc uint16, b []byte) uint16 {
	for _, v := range b {
		crc = crc>>8 ^ crc16Table[uint8(crc)^v]
	}
	return crc
}
