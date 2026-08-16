package aac

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
)

// FuzzDecode feeds arbitrary access-unit bytes to the decoder against a
// valid config. Invariant: no panic, and whenever a frame is emitted it
// carries exactly the configured output length.
//
// configSel picks the decoder configuration, because the decoder's
// bitstream-driven indexing is per configuration: the element sequence is
// checked and every channel is routed through a slot table that is only a
// permutation past stereo, and the SBR fill routing only exists on the
// SBR-configured element loop. The mapping is configSel % 14:
//
//	0     AAC-LC stereo (the plain default)
//	1..6  AAC-LC channel configuration 1..6
//	7..12 explicit hierarchical SBR, channel configuration 1..6
//	13    AAC-LC stereo with the 0x2b7 sync-extension SBR signalling
//	14    explicit AOT-29 (HE-AAC v2): PS over the mono SBR chain
func FuzzDecode(f *testing.F) {
	f.Add([]byte{0x00}, uint8(2))
	f.Add([]byte{0x21, 0x00, 0x00}, uint8(2))       // CPE tag start
	f.Add([]byte{0x00, 0x00, 0x00, 0x00}, uint8(2)) // SCE-ish
	f.Add([]byte{0xe0}, uint8(2))                   // END tag
	f.Add([]byte{0x00, 0x20, 0x20, 0x60}, uint8(6)) // 5.1's SCE, CPE, CPE, LFE
	f.Add([]byte{0x20, 0x00}, uint8(6))             // 5.1 opening on the wrong element
	f.Add([]byte{0x60, 0x00}, uint8(2))             // an LFE where no LFE channel exists
	// SBR-configured decoders: the element loop, fill routing, and
	// concealment paths.
	f.Add([]byte{0x21, 0x00, 0x00}, uint8(8))                          // stereo SBR CPE, no fill: concealment
	f.Add([]byte{0x21, 0x00, 0x00, 0xC0, 0x00}, uint8(8))              // CPE then a FIL
	f.Add([]byte{0x21, 0x00, 0xC2, 0xD6, 0x00, 0x00}, uint8(8))        // FIL with ext 13
	f.Add([]byte{0x00, 0x00, 0xC1, 0x00, 0xE0}, uint8(7))              // mono SBR: SCE, FIL, END
	f.Add([]byte{0x00, 0x20, 0x20, 0x60, 0xC2, 0xD6, 0x00}, uint8(12)) // 5.1 SBR: a FIL right after the LFE
	f.Add([]byte{0x21, 0x00, 0x00, 0xC0, 0x00}, uint8(13))             // sync-extension-signalled stereo
	// The v2 shape: PS payloads ride the SBR extended-data block of a
	// mono SCE's fill.
	f.Add([]byte{0x00, 0x00, 0xC1, 0x00, 0xE0}, uint8(14))       // mono SCE, FIL, END under PS
	f.Add([]byte{0x00, 0x00, 0xC2, 0xD6, 0x00, 0x00}, uint8(14)) // FIL with ext 13 under PS

	f.Fuzz(func(t *testing.T, data []byte, configSel uint8) {
		sel := int(configSel % 15)
		var asc []byte
		switch {
		case sel == 0:
			asc = ascForFuzz(2)
		case sel <= 6:
			asc = ascForFuzz(sel)
		case sel <= 12:
			asc = ascSBRForFuzz(sel - 6)
		case sel == 13:
			asc = ascSyncExtForFuzz(2)
		default:
			asc = ascPSForFuzz()
		}
		cfg, err := ParseASC(asc)
		if err != nil {
			t.Fatalf("selector %d: %v", sel, err)
		}
		fm, err := cfg.Format()
		if err != nil {
			t.Fatalf("selector %d: %v", sel, err)
		}
		d, err := NewDecoder(cfg, fm)
		if err != nil {
			t.Fatalf("selector %d: %v", sel, err)
		}
		_ = d.Decode(data, func(b *audio.Buffer) error {
			if b.N != cfg.OutputSamplesPerAU() {
				t.Fatalf("emitted %d frames, want %d", b.N, cfg.OutputSamplesPerAU())
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

// ascSBRForFuzz builds an explicit hierarchical HE-AAC config at
// 24000/48000 for a channel configuration: AOT 5, sfIdx 6, chanCfg,
// extSfIdx 3, AOT 2, then the three zero GASpecificConfig flags; 25 bits
// padded to 4 bytes.
func ascSBRForFuzz(chanConfig int) []byte {
	var bits uint64
	bits = 5           // AOT 5
	bits = bits<<4 | 6 // core 24000
	bits = bits<<4 | uint64(chanConfig)
	bits = bits<<4 | 3 // extension 48000
	bits = bits<<5 | 2 // AOT 2
	bits <<= 3         // GASpecificConfig flags
	bits <<= 7         // pad to 32 bits
	return []byte{byte(bits >> 24), byte(bits >> 16), byte(bits >> 8), byte(bits)}
}

// ascPSForFuzz builds the explicit AOT-29 (HE-AAC v2) config over the
// mono core at 24000/48000: fdk's own hierarchical form.
func ascPSForFuzz() []byte {
	return []byte{0xEB, 0x09, 0x88, 0x00}
}

// ascSyncExtForFuzz builds the backward-compatible form: a plain AAC-LC
// config at 24000 followed by the 0x2b7 sync extension announcing SBR at
// 48000; 37 bits padded to 5 bytes.
func ascSyncExtForFuzz(chanConfig int) []byte {
	var bits uint64
	bits = 2           // AOT 2
	bits = bits<<4 | 6 // core 24000
	bits = bits<<4 | uint64(chanConfig)
	bits <<= 3              // GASpecificConfig flags
	bits = bits<<11 | 0x2b7 // extension sync word
	bits = bits<<5 | 5      // extension AOT 5
	bits = bits<<1 | 1      // sbrPresentFlag
	bits = bits<<4 | 3      // extension 48000
	bits <<= 3              // pad to 40 bits
	return []byte{byte(bits >> 32), byte(bits >> 24), byte(bits >> 16), byte(bits >> 8), byte(bits)}
}
