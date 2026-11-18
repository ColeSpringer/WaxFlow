package testutil

import "bytes"

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
