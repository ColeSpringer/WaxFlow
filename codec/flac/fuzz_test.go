package flac_test

import (
	"bytes"
	"testing"

	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/internal/testutil"
)

// FuzzRenumberFrame: arbitrary bytes and an arbitrary number never panic, and
// a frame RenumberFrame accepts re-parses with the new number, checks, and
// keeps everything but its number. The bytes are also sealed into a frame that
// checks, so arbitrary headers reach the rewrite, and such a frame is never
// refused for a number that fits unless it holds a second frame.
func FuzzRenumberFrame(f *testing.F) {
	frames := encodedFrames(f, 0)
	f.Add(frames[0], uint64(1000))
	f.Add(frames[1], uint64(0))
	f.Add(frames[len(frames)-1], uint64(1<<31-1))
	f.Add(encodedFrames(f, 2000)[5], uint64(5))
	f.Add(testutil.SetFLACFrameNumber(f, frames[2], true, 1<<35), uint64(8192))
	f.Add(frames[2][:6], uint64(0))
	f.Add([]byte{0xFF, 0xF8, 0xC9, 0x18, 0x00}, uint64(1<<36))
	f.Add([]byte{0xFF, 0xF9, 0x7D, 0x18, 0xFE, 0x81, 0x80, 0x80, 0x80, 0x80, 0x80, 0x12, 0x34, 0x56, 0x78, 0, 0, 0}, uint64(7))
	f.Fuzz(func(t *testing.T, frame []byte, coded uint64) {
		requireRenumberFrame(t, frame, coded)
		if sealed, ok := seal(frame); ok {
			if accepted := requireRenumberFrame(t, sealed, coded); !accepted && coded < 1<<36 && !holdsTwo(t, sealed) {
				t.Fatalf("refused a frame that checks: % x", sealed[:min(len(sealed), 16)])
			}
		}
	})
}

// requireRenumberFrame renumbers frame to coded and, when RenumberFrame
// accepts it, holds the result to the frame it came from.
func requireRenumberFrame(t *testing.T, frame []byte, coded uint64) bool {
	t.Helper()
	in := bytes.Clone(frame)
	out, err := flac.RenumberFrame(nil, frame, coded)
	if !bytes.Equal(frame, in) {
		t.Fatal("the input frame was written to")
	}
	if err != nil {
		return false
	}
	hdr := headerLen(t, frame)
	before, err := flac.ParseFrameHeader(frame)
	if err != nil {
		t.Fatalf("accepted a frame whose header does not parse: %v", err)
	}
	fi, err := flac.ParseFrameHeader(out)
	if err != nil {
		t.Fatalf("the renumbered frame does not parse: %v", err)
	}
	if fi.Coded != coded {
		t.Fatalf("the renumbered frame carries %d, want %d", fi.Coded, coded)
	}
	if fi.Variable != before.Variable || fi.BlockSize != before.BlockSize || fi.Rate != before.Rate ||
		fi.Channels != before.Channels || fi.Bits != before.Bits {
		t.Fatalf("the header changed beyond its number: %+v, was %+v", fi, before)
	}
	n := len(out) - 2
	if n < 0 || flac.CRC16(out[:n]) != uint16(out[n])<<8|uint16(out[n+1]) {
		t.Fatal("the renumbered frame fails its CRC-16")
	}
	// The audio follows the header unchanged, so the two frames end alike.
	if tail := len(frame) - 2 - hdr; !bytes.Equal(out[n-tail:n], frame[hdr:len(frame)-2]) {
		t.Fatal("the audio changed")
	}
	return true
}

// headerLen is the length of the frame header at the start of b, which must
// parse: the shortest prefix ParseFrameHeader reads it from.
func headerLen(t *testing.T, b []byte) int {
	t.Helper()
	for n := 5; n <= min(len(b), flac.MaxFrameHeaderLen); n++ {
		if _, err := flac.ParseFrameHeader(b[:n]); err == nil {
			return n
		}
	}
	t.Fatal("no header length parses")
	return 0
}

// holdsTwo reports whether a second frame header starts inside b past its
// own: at a point where the CRC-16 of everything before it closes to zero.
func holdsTwo(t *testing.T, b []byte) bool {
	t.Helper()
	hdr := headerLen(t, b)
	var crc uint16
	for k := range b {
		if k > hdr+2 && crc == 0 {
			if _, err := flac.ParseFrameHeader(b[k:]); err == nil {
				return true
			}
		}
		crc = flac.UpdateCRC16(crc, b[k:k+1])
	}
	return false
}

// seal returns b with the header at its start given its CRC-8 and the whole
// given a CRC-16, a frame that checks, when a header that parses can be made
// of it.
func seal(b []byte) ([]byte, bool) {
	for n := 5; n <= min(len(b), flac.MaxFrameHeaderLen); n++ {
		h := bytes.Clone(b[:n])
		h[n-1] = crc8(h[:n-1])
		if _, err := flac.ParseFrameHeader(h); err != nil {
			continue
		}
		out := append(h, b[n:]...)
		crc := flac.CRC16(out)
		return append(out, byte(crc>>8), byte(crc)), true
	}
	return nil, false
}

// crc8 is the frame header checksum (polynomial 0x07, zero initial value),
// written out here rather than taken from the package under test.
func crc8(b []byte) byte {
	var c byte
	for _, v := range b {
		c ^= v
		for range 8 {
			if c&0x80 != 0 {
				c = c<<1 ^ 0x07
			} else {
				c <<= 1
			}
		}
	}
	return c
}
