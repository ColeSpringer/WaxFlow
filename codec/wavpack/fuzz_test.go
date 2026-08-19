package wavpack

import (
	"encoding/binary"
	"testing"

	"github.com/colespringer/waxflow/audio"
)

// FuzzDecode feeds arbitrary block bytes to the decoder against a config whose
// channel count and width are fuzzed too, so the mono, stereo, false-stereo,
// and extended-integer paths are all reachable. The selectors map onto valid
// ranges, so every iteration builds a real config.
//
// Invariants: no panic, and an emitted buffer never reports more frames than
// the block header declared, which is the bound the decoder's scratch is sized
// from.
func FuzzDecode(f *testing.F) {
	block := firstBlock(f)
	f.Add(block, uint8(1), uint8(1))
	f.Add(block[:BlockHeaderLen], uint8(1), uint8(1))
	f.Add(withFlags(block, binary.LittleEndian.Uint32(block[24:])|flagMono), uint8(0), uint8(1))
	f.Add(withFlags(block, binary.LittleEndian.Uint32(block[24:])|flagInt32Data), uint8(1), uint8(3))
	f.Add([]byte("wvpk\x18\x00\x00\x00\x10\x04"), uint8(1), uint8(1))

	f.Fuzz(func(t *testing.T, data []byte, chanSel, depthSel uint8) {
		cfg := Config{
			Rate:     44100,
			Channels: int(chanSel)%2 + 1,
			BitDepth: (int(depthSel)%4 + 1) * 8,
		}
		cfg.ValidBits = cfg.BitDepth
		d, err := NewDecoder(cfg, cfg.Format())
		if err != nil {
			t.Fatalf("valid config rejected: %v", err)
		}
		defer d.Release()
		declared := 0
		if h, err := ParseBlockHeader(data); err == nil {
			declared = h.BlockSamples
		}
		_ = d.Decode(data, func(b *audio.Buffer) error {
			if b.N > declared {
				t.Fatalf("emitted %d frames for a block declaring %d", b.N, declared)
			}
			if b.Fmt != cfg.Format() {
				t.Fatalf("emitted %v, want %v", b.Fmt, cfg.Format())
			}
			return nil
		})
	})
}
