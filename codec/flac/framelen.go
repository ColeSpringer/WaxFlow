package flac

// minFrameBytes is less than any frame takes: a header, a subframe per channel
// and the CRC-16.
const minFrameBytes = 8

// MaxSamplesIn is the most samples n bytes of FLAC frames can hold. A frame
// number, or a gap in the numbers, past it is a claim the bytes refute.
func MaxSamplesIn(n int64) int64 { return (max(n, 0)/minFrameBytes + 1) * MaxBlockSize }

// FrameLen returns the length in bytes of the frame that begins b, header and
// CRC-16 included, by parsing its subframes to where they end; the CRC-16 must
// check there. It finds a frame's end without the next frame's header, which
// is what a reader needs when that header is damaged. scratch, when not nil,
// is reused for the residual across calls.
func FrameLen(b []byte, si StreamInfo, scratch *[]int64) (int, error) {
	fi, err := ParseFrameHeader(b)
	if err != nil {
		return 0, err
	}
	bits := fi.Bits
	if bits == 0 {
		bits = si.Bits
	}
	var res []int64
	if scratch != nil {
		if cap(*scratch) < fi.BlockSize {
			*scratch = make([]int64, fi.BlockSize)
		}
		res = (*scratch)[:fi.BlockSize]
	} else {
		res = make([]int64, fi.BlockSize)
	}
	r := &bitReader{data: b, pos: fi.hdrLen}
	for c := 0; c < fi.Channels; c++ {
		if err := decodeSubframe(r, res, fi.subframeBits(c, bits)); err != nil {
			return 0, err
		}
	}
	r.align()
	if r.err {
		return 0, malformed("frame overruns its data")
	}
	end := r.consumed() + 2
	if end > len(b) {
		return 0, malformed("frame truncated")
	}
	if !crcCloses(b[:end]) {
		return 0, malformed("frame CRC-16 mismatch")
	}
	return end, nil
}

// SilentFrame returns a frame of n samples of digital silence shaped like
// template, a frame of the same stream: its blocking strategy and channels,
// with the rate and depth left to STREAMINFO. Its number is 0; a copy numbers
// it with the rest (Numberer).
func SilentFrame(template []byte, si StreamInfo, n int) ([]byte, error) {
	fi, err := ParseFrameHeader(template)
	if err != nil {
		return nil, err
	}
	if n < 1 || n > MaxBlockSize {
		return nil, malformed("a silent frame of %d samples", n)
	}
	b := []byte{0xFF, 0xF8, 0x70, byte(fi.Channels-1) << 4}
	if fi.Variable {
		b[1] |= 1
	}
	b = appendCoded(b, 0)
	b = append(b, byte((n-1)>>8), byte(n-1))
	b = append(b, crc8(b))
	// A CONSTANT subframe per channel: its header byte, then a zero.
	b = append(b, make([]byte, (fi.Channels*(8+si.Bits)+7)/8)...)
	crc := CRC16(b)
	return append(b, byte(crc>>8), byte(crc)), nil
}
