package flac

import (
	"encoding/binary"
	"fmt"
	"math/bits"

	"github.com/colespringer/waxflow/waxerr"
)

// RenumberFrame appends frame, one whole frame, to dst with its coded number
// set to coded: the frame number under the fixed blocking strategy, the first
// sample's number under the variable one (RFC 9639 section 9.1.5). A copy
// that starts mid-stream uses it to number its frames from 0.
//
// The number's coded length can change, so the frame's can. The header's
// CRC-8 and the frame's CRC-16, which covers the header, are recomputed. A
// frame whose header does not parse or whose CRC-16 does not check is refused,
// since a recomputed checksum would hide the damage. So is a packet holding a
// second frame: its CRC-16 checks as a whole, and the inner frame would keep
// its number and the outer one a CRC-16 that no longer checks. dst must not
// overlap frame.
func RenumberFrame(dst, frame []byte, coded uint64) ([]byte, error) {
	fi, err := ParseFrameHeader(frame)
	if err != nil {
		return dst, err
	}
	end := len(frame) - 2 // where the CRC-16 starts
	if end < fi.hdrLen {
		return dst, malformed("frame truncated")
	}
	// A frame ends where the running CRC-16 closes to zero, so a header
	// there starts a second one.
	var crc uint16
	for k, v := range frame[:end] {
		if crc == 0 && k > fi.hdrLen+2 && SyncOK(frame[k:]) {
			if _, err := ParseFrameHeader(frame[k:]); err == nil {
				return dst, malformed("packet holds a second frame at byte %d", k)
			}
		}
		crc = crc<<8 ^ crc16Table[uint8(crc>>8)^v]
	}
	if crc != uint16(frame[end])<<8|uint16(frame[end+1]) {
		return dst, malformed("frame CRC-16 mismatch")
	}
	if coded >= 1<<36 {
		return dst, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("flac: coded number %d does not fit 36 bits", coded))
	}
	width := 1 // the coded number's bytes, from its first
	if frame[4] >= 0x80 {
		width = bits.LeadingZeros8(^frame[4])
	}
	start := len(dst)
	dst = append(dst, frame[:4]...)
	dst = appendCoded(dst, coded)
	dst = append(dst, frame[4+width:fi.hdrLen-1]...) // block size and rate extensions
	dst = append(dst, crc8(dst[start:]))
	dst = append(dst, frame[fi.hdrLen:end]...)
	crc = CRC16(dst[start:])
	return append(dst, byte(crc>>8), byte(crc)), nil
}

// appendCoded appends v in the UTF-8-like form a frame header carries: one
// byte below 0x80, else n bytes holding 5n+1 bits, and 36 bits at seven.
func appendCoded(dst []byte, v uint64) []byte {
	if v < 0x80 {
		return append(dst, byte(v))
	}
	n := 2
	for n < 7 && v >= 1<<(5*n+1) {
		n++
	}
	dst = append(dst, ^byte(0xFF>>n)|byte(v>>(6*(n-1))))
	for i := n - 2; i >= 0; i-- {
		dst = append(dst, 0x80|byte(v>>(6*i))&0x3F)
	}
	return dst
}

// Numberer numbers a stream's frames as they pass, from a position on: the
// frame number under the fixed blocking strategy, the first sample's number
// under the variable one, as RFC 9639 counts them from a stream's first frame.
// A copy numbers its frames from 0 with it, and a run restarted mid-stream
// numbers them where they are.
type Numberer struct {
	si      StreamInfo
	num     Numbering
	latched bool
	next    int64  // the next frame's first sample
	block   int64  // the fixed strategy's block size, for a run from past 0
	frame   uint64 // the next frame's number, under the fixed strategy
	buf     []byte
}

// NewNumberer returns a Numberer for a stream that si describes, whose next
// frame starts at sample from. A run from past 0 under the fixed strategy
// numbers its first frame from/block, where block is the stream's block size:
// its first frame's, which STREAMINFO's bounds need not match.
func NewNumberer(si StreamInfo, from int64, block int) *Numberer {
	return &Numberer{si: si, next: from, block: int64(block)}
}

// Number returns frame numbered for its place, and the sample it starts at.
// The result is frame itself when it is numbered so already, and otherwise a
// buffer the next call overwrites.
func (n *Numberer) Number(frame []byte) ([]byte, int64, error) {
	fi, err := ParseFrameHeader(frame)
	if err != nil {
		return nil, 0, err
	}
	if !n.latched {
		n.num = n.si.Numbering(fi)
		if !n.num.SampleCoded && n.next > 0 {
			// The stream's block size, not this frame's: a run may start on
			// the short final frame.
			if n.block <= 0 || n.next%n.block != 0 {
				return nil, 0, malformed("a fixed-blocksize frame cannot start at sample %d", n.next)
			}
			n.frame = uint64(n.next / n.block)
		}
		n.latched = true
	}
	coded := n.frame
	if n.num.SampleCoded {
		coded = uint64(n.next)
	}
	out := frame
	if fi.Coded != coded {
		if n.buf, err = RenumberFrame(n.buf[:0], frame, coded); err != nil {
			return nil, 0, err
		}
		out = n.buf
	}
	start := n.next
	n.next += int64(fi.BlockSize)
	n.frame++
	return out, start, nil
}

// RestateStreamInfo returns a copy of si, a STREAMINFO block body, stating
// samples and no MD5, and the rest as it was. A reader whose frames disagree
// with the header it found hands this on instead: a header whose total does
// not describe the frames has a signature that cannot either. A total past
// the field's 36 bits is stated as 0, unknown.
func RestateStreamInfo(si []byte, samples int64) ([]byte, error) {
	out, err := ClearMD5(si)
	if err != nil {
		return nil, err
	}
	if samples < 0 || samples >= 1<<36 {
		samples = 0
	}
	out[13] = out[13]&0xF0 | byte(samples>>32)&0x0F
	binary.BigEndian.PutUint32(out[14:18], uint32(samples))
	return out, nil
}

// ClearMD5 returns a copy of si, a STREAMINFO block body, whose MD5 signature
// is all zero, RFC 9639's "not computed". A copy of a stream that starts past
// frame 0 writes it in place of the signature it inherits, which describes
// the whole stream the copied one was cut from.
func ClearMD5(si []byte) ([]byte, error) {
	if len(si) != StreamInfoLen {
		return nil, malformed("STREAMINFO of %d bytes, want %d", len(si), StreamInfoLen)
	}
	out := append([]byte(nil), si...)
	clear(out[18:])
	return out, nil
}
