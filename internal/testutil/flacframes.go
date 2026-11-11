package testutil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// RenumberFLACFrames returns raw, a native FLAC stream of fixed block size as
// this tree's encoder writes one, with every frame's coded number moved on by
// frames: the stream an `ffmpeg -ss X -c copy` cut leaves, whose first frame is
// not frame 0. Each frame header's CRC-8 and each frame's CRC-16 are
// recomputed. STREAMINFO's frame-size bounds, which a longer coded number can
// break, are cleared to 0, its "unknown".
func RenumberFLACFrames(t testing.TB, raw []byte, frames int64) []byte {
	t.Helper()
	return rewriteFLACFrames(t, raw, func(coded, _ uint64) (bool, uint64) {
		return false, coded + uint64(frames)
	})
}

// VariableFLACFrames returns raw, a native FLAC stream of fixed block size as
// this tree's encoder writes one, with every frame moved to the variable
// blocking strategy, whose coded number is the frame's first sample, and
// numbered from sample from. Checksums and STREAMINFO are as RenumberFLACFrames
// leaves them.
func VariableFLACFrames(t testing.TB, raw []byte, from int64) []byte {
	t.Helper()
	return rewriteFLACFrames(t, raw, func(_, sample uint64) (bool, uint64) {
		return true, sample + uint64(from)
	})
}

// rewriteFLACFrames returns raw, a native FLAC stream of fixed block size, with
// each frame renumbered by number, which is handed the frame's coded number and
// its first sample and returns the blocking strategy and coded number to write.
func rewriteFLACFrames(t testing.TB, raw []byte, number func(coded, sample uint64) (variable bool, to uint64)) []byte {
	t.Helper()
	if !bytes.HasPrefix(raw, []byte("fLaC")) {
		t.Fatal("not a native FLAC stream")
	}
	out := append([]byte(nil), raw...)
	off := 4
	for last := false; !last; {
		if off+4 > len(out) {
			t.Fatal("the metadata runs past the stream")
		}
		last = out[off]&0x80 != 0
		length := int(out[off+1])<<16 | int(out[off+2])<<8 | int(out[off+3])
		if out[off]&0x7F == 0 && length >= 10 {
			clear(out[off+4+4 : off+4+10]) // minimum and maximum frame size
		}
		off += 4 + length
	}
	out = out[:off]
	sample := uint64(0)
	for pos := off; pos < len(raw); {
		hdr := flacHeaderSize(raw, pos)
		if hdr == 0 || raw[pos+1] != 0xF8 {
			t.Fatalf("no fixed-block-size frame header that checks at %d", pos)
		}
		end := flacFrameEnd(t, raw, pos, hdr)
		coded, _ := flacCoded(t, raw[pos+4:])
		variable, to := number(coded, sample)
		out = append(out, SetFLACFrameNumber(t, raw[pos:end], variable, to)...)
		sample += uint64(flacBlockSize(t, raw[pos:pos+hdr]))
		pos = end
	}
	return out
}

// SetFLACFrameNumber returns frame, one whole FLAC frame that checks, under
// the fixed or the variable blocking strategy and carrying coded as its coded
// number: the frame number under the fixed one, the first sample's under the
// variable one. Its header's CRC-8 and its CRC-16 are recomputed.
func SetFLACFrameNumber(t testing.TB, frame []byte, variable bool, coded uint64) []byte {
	t.Helper()
	hdr := flacHeaderSize(frame, 0)
	if hdr == 0 || len(frame) < hdr+2 || flacCRC16(0, frame) != 0 {
		t.Fatalf("not a whole FLAC frame that checks: % x", frame[:min(len(frame), 16)])
	}
	_, n := flacCoded(t, frame[4:])
	h := append([]byte(nil), frame[:4]...)
	h[1] = 0xF8
	if variable {
		h[1] = 0xF9
	}
	h = appendFLACCoded(h, coded)
	h = append(h, frame[4+n:hdr-1]...) // block size and rate extensions
	h = append(h, flacCRC8(h))
	out := append(h, frame[hdr:len(frame)-2]...)
	crc := flacCRC16(0, out)
	return append(out, byte(crc>>8), byte(crc))
}

// flacBlockSize is the block size a frame header states (RFC 9639 section
// 9.1.1).
func flacBlockSize(t testing.TB, h []byte) int {
	t.Helper()
	at := 4 + flacCodedLen(h[4]) // where an uncommon block size is stored
	switch c := int(h[2] >> 4); {
	case c == 1:
		return 192
	case c >= 2 && c <= 5:
		return 576 << (c - 2)
	case c == 6:
		return int(h[at]) + 1
	case c == 7:
		return (int(h[at])<<8 | int(h[at+1])) + 1
	case c >= 8:
		return 256 << (c - 8)
	}
	t.Fatalf("reserved block size code in % x", h)
	return 0
}

// flacHeaderSize is the length of the frame header at pos, CRC-8 included, or 0
// when no header that checks starts there.
func flacHeaderSize(raw []byte, pos int) int {
	if pos+5 > len(raw) || raw[pos] != 0xFF || raw[pos+1]&0xFE != 0xF8 {
		return 0
	}
	n := flacCodedLen(raw[pos+4])
	if n == 0 {
		return 0
	}
	size := 4 + n + 1
	switch raw[pos+2] >> 4 {
	case 6:
		size++
	case 7:
		size += 2
	}
	switch raw[pos+2] & 0x0F {
	case 12:
		size++
	case 13, 14:
		size += 2
	}
	if pos+size > len(raw) || flacCRC8(raw[pos:pos+size-1]) != raw[pos+size-1] {
		return 0
	}
	return size
}

// flacFrameEnd finds where the frame at pos ends: the first frame header after
// it, or the end of the stream, where the bytes since pos checksum to zero,
// which is a frame with its own CRC-16 at its end.
func flacFrameEnd(t testing.TB, raw []byte, pos, hdr int) int {
	t.Helper()
	crc, at := uint16(0), pos
	for c := pos + hdr; c+1 < len(raw); c++ {
		if raw[c] != 0xFF || raw[c+1] != 0xF8 {
			continue
		}
		crc, at = flacCRC16(crc, raw[at:c]), c
		if crc == 0 && flacHeaderSize(raw, c) > 0 {
			return c
		}
	}
	if flacCRC16(crc, raw[at:]) != 0 {
		t.Fatalf("the frame at %d does not checksum", pos)
	}
	return len(raw)
}

// flacCodedLen is the byte length of a coded number from its first byte, 0
// for a byte no coded number starts with.
func flacCodedLen(b byte) int {
	switch {
	case b < 0x80:
		return 1
	case b&0xE0 == 0xC0:
		return 2
	case b&0xF0 == 0xE0:
		return 3
	case b&0xF8 == 0xF0:
		return 4
	case b&0xFC == 0xF8:
		return 5
	case b&0xFE == 0xFC:
		return 6
	case b == 0xFE:
		return 7
	}
	return 0
}

func flacCoded(t testing.TB, b []byte) (uint64, int) {
	t.Helper()
	n := flacCodedLen(b[0])
	if n == 0 || n > len(b) {
		t.Fatalf("no coded number in % x", b[:min(len(b), 7)])
	}
	if n == 1 {
		return uint64(b[0]), 1
	}
	v := uint64(b[0] & (0x7F >> n))
	for _, c := range b[1:n] {
		v = v<<6 | uint64(c&0x3F)
	}
	return v, n
}

// appendFLACCoded encodes v the way a frame header carries it: UTF-8 widened
// to 36 bits (RFC 9639 section 9.1.5).
func appendFLACCoded(out []byte, v uint64) []byte {
	if v < 0x80 {
		return append(out, byte(v))
	}
	n := 2
	for v >= 1<<(5*n+1) && n < 7 {
		n++
	}
	out = append(out, byte(uint16(0xFF00)>>n)|byte(v>>(6*(n-1))))
	for i := n - 2; i >= 0; i-- {
		out = append(out, 0x80|byte(v>>(6*i))&0x3F)
	}
	return out
}

// flacCRC8 is the frame header checksum: polynomial 0x07, zero initial value.
func flacCRC8(b []byte) byte {
	var crc byte
	for _, v := range b {
		crc ^= v
		for range 8 {
			if crc&0x80 != 0 {
				crc = crc<<1 ^ 0x07
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// flacCRC16 is the frame checksum: polynomial 0x8005, zero initial value.
func flacCRC16(crc uint16, b []byte) uint16 {
	for _, v := range b {
		crc ^= uint16(v) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x8005
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// FLACFrame is where one frame of a native FLAC stream sits and what it
// holds: its bytes, [Off, End), and its samples, Dur of them from Pos.
type FLACFrame struct {
	Off, End int
	Pos, Dur int64
}

// FLACFrames walks raw, a native FLAC stream of fixed block size as this
// tree's encoder and the reference one write it, and returns its frames in
// order. It is how a test finds a frame to damage or a frame boundary to cut
// at without opening the demuxer it is testing.
func FLACFrames(t testing.TB, raw []byte) []FLACFrame {
	t.Helper()
	if !bytes.HasPrefix(raw, []byte("fLaC")) {
		t.Fatal("not a native FLAC stream")
	}
	off := 4
	for last := false; !last; {
		if off+4 > len(raw) {
			t.Fatal("the metadata runs past the stream")
		}
		last = raw[off]&0x80 != 0
		off += 4 + (int(raw[off+1])<<16 | int(raw[off+2])<<8 | int(raw[off+3]))
	}
	var frames []FLACFrame
	var sample int64
	for pos := off; pos < len(raw); {
		hdr := flacHeaderSize(raw, pos)
		if hdr == 0 || raw[pos+1] != 0xF8 {
			t.Fatalf("no fixed-block-size frame header that checks at %d", pos)
		}
		end := flacFrameEnd(t, raw, pos, hdr)
		dur := int64(flacBlockSize(t, raw[pos:pos+hdr]))
		frames = append(frames, FLACFrame{Off: pos, End: end, Pos: sample, Dur: dur})
		sample, pos = sample+dur, end
	}
	return frames
}

// DamageFLACFrames returns raw with 16 bytes inverted in the middle of each
// frame named by its index in frames, so that frame's checksum fails and
// every other frame is untouched: a hole a read steps over.
func DamageFLACFrames(t testing.TB, raw []byte, frames []FLACFrame, which ...int) []byte {
	t.Helper()
	out := append([]byte(nil), raw...)
	for _, k := range which {
		f := frames[k]
		if f.End-f.Off < 64 {
			t.Fatalf("frame %d is %d bytes, too short to damage inside", k, f.End-f.Off)
		}
		mid := (f.Off + f.End) / 2
		for i := mid - 8; i < mid+8; i++ {
			out[i] ^= 0xFF
		}
	}
	return out
}

// FLACFixture is the reference encoder's FLAC fixture, testdata/sine-s16.flac,
// and its frames: a few whole blocks and a short last one, behind a padded
// metadata block. The bytes are a copy the caller may edit.
func FLACFixture(t testing.TB) ([]byte, []FLACFrame) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(VectorsDir(), "..", "sine-s16.flac"))
	if err != nil {
		t.Fatal(err)
	}
	frames := FLACFrames(t, raw)
	if len(frames) < 4 {
		t.Fatalf("the fixture holds %d frames; its callers need a few", len(frames))
	}
	return raw, frames
}
