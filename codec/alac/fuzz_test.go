package alac

import (
	"encoding/binary"
	"testing"

	"github.com/colespringer/waxflow/audio"
)

// cookie builds a valid ALACSpecificConfig for the fuzz target.
func cookie(frameLen uint32, bitDepth, channels, rate int) []byte {
	b := make([]byte, CookieLen)
	binary.BigEndian.PutUint32(b[0:], frameLen)
	b[5] = byte(bitDepth)
	b[6], b[7], b[8] = 40, 10, 14 // pb, mb, kb defaults
	b[9] = byte(channels)
	binary.BigEndian.PutUint16(b[10:], 255) // maxRun
	binary.BigEndian.PutUint32(b[12:], 1024)
	binary.BigEndian.PutUint32(b[16:], 0)
	binary.BigEndian.PutUint32(b[20:], uint32(rate))
	return b
}

// FuzzDecode feeds arbitrary packet bytes to the decoder against a config
// whose frame length, bit depth, and channel count are fuzzed too, so small
// frame lengths (which exercise the predictor warm-up clamp) and the 20/24/
// 32-bit paths are reachable, not just 16-bit mono/stereo at 4096. The
// selectors map onto valid ranges so every iteration builds a real config.
// Invariant: no panic, and an emitted buffer never reports more frames than
// the configured frame length.
func FuzzDecode(f *testing.F) {
	f.Add([]byte{0x20, 0x00}, uint16(4096), uint8(0), uint8(0))                 // 16-bit mono
	f.Add([]byte{0x21, 0x00, 0x00, 0x00}, uint16(4096), uint8(0), uint8(1))     // 16-bit stereo
	f.Add([]byte{0xe0}, uint16(7), uint8(2), uint8(0))                          // tiny frame, 24-bit
	f.Add([]byte{0x00, 0x00, 0x08, 0x00, 0x00}, uint16(16), uint8(3), uint8(1)) // 32-bit stereo

	depths := []int{16, 20, 24, 32}
	f.Fuzz(func(t *testing.T, data []byte, frameSel uint16, depthSel, chanSel uint8) {
		bitDepth := depths[int(depthSel)%len(depths)]
		channels := int(chanSel)%2 + 1
		frameLen := uint32(frameSel)%maxFrameLength + 1 // 1..maxFrameLength
		cfg, err := ParseMagicCookie(cookie(frameLen, bitDepth, channels, 44100))
		if err != nil {
			t.Fatalf("valid cookie rejected: %v", err)
		}
		d, err := NewDecoder(cfg, cfg.Format())
		if err != nil {
			t.Fatal(err)
		}
		_ = d.Decode(data, func(b *audio.Buffer) error {
			if b.N > int(cfg.FrameLength) {
				t.Fatalf("emitted %d frames, cap is %d", b.N, cfg.FrameLength)
			}
			return nil
		})
	})
}
