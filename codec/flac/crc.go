package flac

// FLAC's two frame checksums (RFC 9639 section 9.1.8, 9.3): CRC-8 with
// polynomial x^8 + x^2 + x + 1 (0x07) over the frame header, and CRC-16
// with polynomial x^16 + x^15 + x^2 + 1 (0x8005) over the whole frame up
// to the checksum itself. Both start at 0 and are unreflected.

var crc8Table = func() (t [256]uint8) {
	for i := range t {
		c := uint8(i)
		for range 8 {
			if c&0x80 != 0 {
				c = c<<1 ^ 0x07
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return
}()

var crc16Table = func() (t [256]uint16) {
	for i := range t {
		c := uint16(i) << 8
		for range 8 {
			if c&0x8000 != 0 {
				c = c<<1 ^ 0x8005
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return
}()

func crc8(b []byte) uint8 {
	var c uint8
	for _, v := range b {
		c = crc8Table[c^v]
	}
	return c
}

// CRC16 returns the FLAC frame checksum of b. Exported for containers:
// flacn confirms frame boundaries by checking that the bytes before a
// sync candidate checksum the span from the previous frame start.
func CRC16(b []byte) uint16 {
	return UpdateCRC16(0, b)
}

// UpdateCRC16 extends a running CRC16 with b, for incremental use over a
// growing span.
func UpdateCRC16(crc uint16, b []byte) uint16 {
	for _, v := range b {
		crc = crc<<8 ^ crc16Table[uint8(crc>>8)^v]
	}
	return crc
}
