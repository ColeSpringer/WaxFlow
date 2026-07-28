package aac

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
)

// FuzzDecode feeds arbitrary access-unit bytes to the decoder against a
// valid AAC-LC config. Invariant: no panic, and whenever a frame is
// emitted it carries exactly the configured frame length.
//
// configSel picks the channel configuration, because the decoder's
// bitstream-driven indexing is per configuration: the element sequence is
// checked and every channel is routed through a slot table that is only a
// permutation past stereo, so a stereo-only target would never reach the
// arithmetic most able to index out of bounds.
func FuzzDecode(f *testing.F) {
	f.Add([]byte{0x00}, uint8(2))
	f.Add([]byte{0x21, 0x00, 0x00}, uint8(2))       // CPE tag start
	f.Add([]byte{0x00, 0x00, 0x00, 0x00}, uint8(2)) // SCE-ish
	f.Add([]byte{0xe0}, uint8(2))                   // END tag
	f.Add([]byte{0x00, 0x20, 0x20, 0x60}, uint8(6)) // 5.1's SCE, CPE, CPE, LFE
	f.Add([]byte{0x20, 0x00}, uint8(6))             // 5.1 opening on the wrong element
	f.Add([]byte{0x60, 0x00}, uint8(2))             // an LFE where no LFE channel exists

	f.Fuzz(func(t *testing.T, data []byte, configSel uint8) {
		// 1 through 6: the configurations that decode. 0, 7 and the
		// reserved values are refused at parse and have no decoder to
		// drive; ParseASC's own refusals are covered by unit tests.
		config := int(configSel%6) + 1
		cfg, err := ParseASC(ascForFuzz(config))
		if err != nil {
			t.Fatalf("configuration %d: %v", config, err)
		}
		fm, err := cfg.Format()
		if err != nil {
			t.Fatalf("configuration %d: %v", config, err)
		}
		d, err := NewDecoder(cfg, fm)
		if err != nil {
			t.Fatalf("configuration %d: %v", config, err)
		}
		_ = d.Decode(data, func(b *audio.Buffer) error {
			if b.N != int(cfg.FrameLength) {
				t.Fatalf("emitted %d frames, want %d", b.N, cfg.FrameLength)
			}
			return nil
		})
	})
}

// ascForFuzz builds an AAC-LC AudioSpecificConfig at 44100 Hz for a
// channel configuration: audioObjectType(5)=2, samplingFrequencyIndex(4)=4,
// channelConfiguration(4), then a zero GASpecificConfig.
func ascForFuzz(chanConfig int) []byte {
	bits := uint32(2)<<11 | uint32(4)<<7 | uint32(chanConfig)<<3
	return []byte{byte(bits >> 8), byte(bits)}
}
