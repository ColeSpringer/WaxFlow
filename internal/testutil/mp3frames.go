package testutil

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// SyntheticMP3FrameLen is the size of one frame SyntheticMP3Frames builds.
const SyntheticMP3FrameLen = 417

// SyntheticMP3Frames returns n whole MPEG-1 Layer III frames: 128 kbit/s,
// 44.1 kHz, stereo, no CRC, 417 bytes each, with payload bytes that are never
// 0xFF so a resync scan cannot mistake the middle of one for a sync word.
// Nothing decodes it and the tests that walk it do not need to: a frame walk
// reads headers and hops. It is the run container/internal/mpegframes's own
// tests build, for the owners that need a payload longer than any committed
// fixture (the read-ahead window is 128 KiB, and every fixture is smaller).
func SyntheticMP3Frames(n int) []byte {
	f := make([]byte, SyntheticMP3FrameLen)
	copy(f, []byte{0xFF, 0xFB, 0x90, 0x00})
	for i := 4; i < len(f); i++ {
		if f[i] = byte(i); f[i] == 0xFF {
			f[i] = 0
		}
	}
	return bytes.Repeat(f, n)
}

// ShortMP3FrameLen is the size of one frame ShortMP3Frames builds.
const ShortMP3FrameLen = 24

// ShortMP3Frames returns n whole MPEG-2 Layer III frames: 8 kbit/s, 24 kHz,
// mono, no CRC, 576 samples in 24 bytes each, the smallest frame a walk
// accepts, so a run long enough for a frame index snapshot is under 100 KB.
func ShortMP3Frames(n int) []byte {
	f := make([]byte, ShortMP3FrameLen)
	copy(f, []byte{0xFF, 0xF3, 0x14, 0xC4})
	return bytes.Repeat(f, n)
}

// UnderDeclaredMP3 returns raw, an MP3 that opens with a Xing or Info frame
// stating both counts, twice over, with that frame's byte count set to span
// both copies. The frame still counts the frames of one copy, so the file
// holds twice what it declares, inside the bytes it claims. Left at its own
// value the byte count would end where the second copy begins, which is a
// joined pair, and a joined pair declares both.
func UnderDeclaredMP3(tb testing.TB, raw []byte) []byte {
	tb.Helper()
	head := raw[:min(len(raw), 4096)]
	at := bytes.Index(head, []byte("Xing"))
	if i := bytes.Index(head, []byte("Info")); i >= 0 && (at < 0 || i < at) {
		at = i
	}
	if at < 0 || at+16 > len(raw) || binary.BigEndian.Uint32(raw[at+4:])&3 != 3 {
		tb.Fatal("the fixture opens with no metadata frame stating both counts")
	}
	doubled := append(append([]byte(nil), raw...), raw...)
	binary.BigEndian.PutUint32(doubled[at+12:], uint32(len(doubled)))
	return doubled
}
