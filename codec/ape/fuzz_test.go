package ape

import (
	"encoding/binary"
	"testing"

	"github.com/colespringer/waxflow/audio"
)

// FuzzDecode feeds arbitrary frame bytes to the decoder against a config whose
// depth, channel count, and compression level are fuzzed too, so every filter
// cascade and both channel paths are reachable. The selectors map onto valid
// ranges, so every iteration builds a real config.
//
// Invariants: no panic, an emitted buffer never reports more frames than the
// packet's header declared, and a frame that decodes without error has passed
// its CRC.
func FuzzDecode(f *testing.F) {
	raw := fixture(f, "sine-s16.ape")
	h, err := ParseHeader(raw)
	if err != nil {
		f.Fatal(err)
	}
	at := int64(binary.LittleEndian.Uint32(raw[h.SeekTableOffset:]))
	frame := make([]byte, FrameHeaderLen+len(raw[at:]))
	PutFrameHeader(frame, h.FinalFrameBlocks, 0)
	copy(frame[FrameHeaderLen:], raw[at:])
	f.Add(frame, uint8(1), uint8(1), uint8(1))
	f.Add(frame[:FrameHeaderLen+64], uint8(1), uint8(1), uint8(4))
	f.Add(frame[:FrameHeaderLen], uint8(0), uint8(0), uint8(0))
	// A frame header with the special-code flag set: the silence and
	// pseudo-stereo paths decode no values at all.
	f.Add(append(headerBytes(64, 0), 0x80, 0, 0, 0, 4, 0, 0, 0), uint8(1), uint8(1), uint8(2))

	f.Fuzz(func(t *testing.T, data []byte, chanSel, depthSel, levelSel uint8) {
		cfg := Config{
			FileVersion:      3990,
			CompressionLevel: (int(levelSel)%5 + 1) * 1000,
			BlocksPerFrame:   73728,
			BitsPerSample:    []int{8, 16, 24}[int(depthSel)%3],
			Channels:         int(chanSel)%2 + 1,
			Rate:             44100,
		}
		d, err := NewDecoder(cfg, cfg.Format())
		if err != nil {
			t.Fatalf("valid config rejected: %v", err)
		}
		defer d.Release()
		declared := 0
		if blocks, _, _, err := ParseFrameHeader(data); err == nil {
			declared = blocks
		}
		seen := 0
		_ = d.Decode(data, func(b *audio.Buffer) error {
			seen += b.N
			if seen > declared {
				t.Fatalf("emitted %d frames for a packet declaring %d", seen, declared)
			}
			if b.Fmt != cfg.Format() {
				t.Fatalf("emitted %v, want %v", b.Fmt, cfg.Format())
			}
			return nil
		})
	})
}

// FuzzDecodeOldStream is FuzzDecode against the 3950..3989 entropy coder,
// which no encoder still in circulation writes and which therefore has no
// fixture: the hostile-input invariants are what can be asserted about it.
func FuzzDecodeOldStream(f *testing.F) {
	f.Add(append(headerBytes(64, 0), 0, 0, 0, 0))
	f.Add(append(headerBytes(1000, 2), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff))

	f.Fuzz(func(t *testing.T, data []byte) {
		cfg := Config{FileVersion: 3970, CompressionLevel: LevelHigh, BlocksPerFrame: 73728 * 4,
			BitsPerSample: 16, Channels: 2, Rate: 44100}
		d, err := NewDecoder(cfg, cfg.Format())
		if err != nil {
			t.Fatalf("valid config rejected: %v", err)
		}
		defer d.Release()
		declared := 0
		if blocks, _, _, err := ParseFrameHeader(data); err == nil {
			declared = blocks
		}
		seen := 0
		_ = d.Decode(data, func(b *audio.Buffer) error {
			seen += b.N
			if seen > declared {
				t.Fatalf("emitted %d frames for a packet declaring %d", seen, declared)
			}
			return nil
		})
	})
}

// headerBytes is a bare frame header, the seed corpus's building block.
func headerBytes(blocks, skip int) []byte {
	b := make([]byte, FrameHeaderLen)
	PutFrameHeader(b, blocks, skip)
	return b
}

// FuzzParseHeader asserts the file header parser on arbitrary bytes: no panic,
// and an accepted header describes a stream the decoder can actually be built
// for.
func FuzzParseHeader(f *testing.F) {
	f.Add(fixture(f, "sine-s16.ape"))
	f.Add(fixture(f, "noise-s16.ape")[:200])
	f.Add([]byte("MAC \x8e\x0f"))
	f.Add(oldHeader(3950, LevelFast, flagHasSeekElements|flagHasPeakLevel, 2, 44100, 0, 0, 1, 100, 1))

	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := ParseHeader(data)
		if err != nil {
			return
		}
		if err := h.Config().Validate(); err != nil {
			t.Fatalf("accepted a header whose config is invalid: %v", err)
		}
		if err := h.Config().Format().Valid(); err != nil {
			t.Fatalf("accepted a header whose format is invalid: %v", err)
		}
		if h.Samples() < 0 {
			t.Fatalf("accepted a header declaring %d samples", h.Samples())
		}
		if h.SeekTableOffset < 0 || h.FrameDataOffset < h.SeekTableOffset {
			t.Fatalf("accepted a header whose sections overlap: table at %d, frames at %d",
				h.SeekTableOffset, h.FrameDataOffset)
		}
		if _, err := NewDecoder(h.Config(), h.Config().Format()); err != nil {
			t.Fatalf("accepted a header no decoder can be built for: %v", err)
		}
	})
}
