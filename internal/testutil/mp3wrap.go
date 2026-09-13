package testutil

import (
	"encoding/binary"
	"math"
	"testing"
)

// Wrapping one MPEG audio bitstream in another container, for the fixtures
// no tool will make: ffmpeg's aiff muxer refuses MP3 outright and its
// demuxer refuses the result, so an AIFF-C '.mp3' has to be built here, and
// more than one test binary wants the same bytes to compare against each
// other.
//
// The frames come out of the committed WAV rather than off an encoder, so
// the AIFF-C holds exactly the bitstream every other MP3 fixture in the tree
// holds: one encode, wrapped again.

// AIFCFrames wraps a payload in a minimal AIFF-C file (FORM, FVER, COMM,
// SSND) under the given compression type, declaring frames in COMM.
func AIFCFrames(comp string, rate, channels, bits int, frames uint32, payload []byte) []byte {
	var b []byte
	put := func(parts ...[]byte) {
		for _, p := range parts {
			b = append(b, p...)
		}
	}
	u16 := func(v uint16) []byte { return []byte{byte(v >> 8), byte(v)} }
	u32 := func(v uint32) []byte { return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)} }

	put([]byte("FORM"), u32(0), []byte("AIFC"))
	put([]byte("FVER"), u32(4), u32(0xA2805140))
	ext := ext80(float64(rate))
	put([]byte("COMM"), u32(24), u16(uint16(channels)), u32(frames), u16(uint16(bits)), ext[:],
		[]byte(comp), []byte{0, 0})
	put([]byte("SSND"), u32(uint32(8+len(payload))), u32(0), u32(0), payload)
	if len(payload)%2 == 1 {
		b = append(b, 0)
	}
	binary.BigEndian.PutUint32(b[4:], uint32(len(b)-8))
	return b
}

// ext80 encodes a positive rate as the IEEE 754 80-bit extended float COMM
// states its sample rate in. It mirrors container/aiff's own encoder, which is
// unexported, and carries its guards for the same reason: a rate outside the
// range encodes as zero rather than as a wrong number.
func ext80(v float64) [10]byte {
	var b [10]byte
	if v <= 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		return b
	}
	frac, e := math.Frexp(v) // v = frac * 2^e, frac in [0.5, 1)
	exp := e + 16382
	if exp <= 0 || exp >= 0x7FFF {
		return b
	}
	b[0] = byte(exp >> 8)
	b[1] = byte(exp)
	binary.BigEndian.PutUint64(b[2:], uint64(frac*(1<<63)*2))
	return b
}

// WAVDataChunk returns a WAV's data chunk payload: its audio as it stands,
// with no decoding and no knowledge of what coded it. The chunks are walked
// rather than searched for, since a payload can hold the word "data" too.
func WAVDataChunk(t testing.TB, raw []byte) []byte {
	t.Helper()
	le := binary.LittleEndian
	for off := 12; off+8 <= len(raw); {
		size := int64(le.Uint32(raw[off+4:]))
		body := int64(off) + 8 + size
		if body > int64(len(raw)) {
			break
		}
		if string(raw[off:off+4]) == "data" {
			return raw[off+8 : body]
		}
		off = int(body + size&1)
	}
	t.Fatal("no data chunk")
	return nil
}
