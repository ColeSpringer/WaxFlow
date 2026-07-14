package aac

import "testing"

// TestParseASCExplicitSBR pins the rate an explicitly signalled SBR/PS config
// reports. Per ISO/IEC 14496-3 §1.6.2.1 the first samplingFrequencyIndex is
// already the core rate and extensionSamplingFrequencyIndex is the doubled
// output rate, so unwrapping must keep the first and never halve it.
//
// The SBR case is the exact bitstream from the downstream repro: AOT 5,
// sfIdx 6 (24000), chanCfg 2, extSfIdx 3 (48000), AOT 2. It returned 12000
// before aac-dec-2, an octave down at half speed.
func TestParseASCExplicitSBR(t *testing.T) {
	for _, tc := range []struct {
		name    string
		asc     []byte
		rate    int
		extRate int
		sbr, ps bool
	}{
		// 00101 0110 0010 0011 00010 0
		{"sbr", []byte{0x2B, 0x11, 0x88}, 24000, 48000, true, false},
		// 11101 0110 0010 0011 00010 0: the same, with AOT 29 (PS).
		{"ps", []byte{0xEB, 0x11, 0x88}, 24000, 48000, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseASC(tc.asc)
			if err != nil {
				t.Fatalf("ParseASC: %v", err)
			}
			if cfg.SampleRate != tc.rate {
				t.Errorf("SampleRate = %d, want %d (the core rate the base layer codes at)", cfg.SampleRate, tc.rate)
			}
			if cfg.ObjectType != aotAACLC {
				t.Errorf("ObjectType = %d, want %d: the wrapper must unwrap to its base type", cfg.ObjectType, aotAACLC)
			}
			if cfg.ExtensionRate != tc.extRate {
				t.Errorf("ExtensionRate = %d, want %d", cfg.ExtensionRate, tc.extRate)
			}
			if cfg.SBR != tc.sbr || cfg.PS != tc.ps {
				t.Errorf("SBR/PS = %v/%v, want %v/%v", cfg.SBR, cfg.PS, tc.sbr, tc.ps)
			}
			if cfg.Channels != 2 {
				t.Errorf("Channels = %d, want 2", cfg.Channels)
			}
			if cfg.FrameLength != 1024 {
				t.Errorf("FrameLength = %d, want 1024", cfg.FrameLength)
			}
		})
	}
}

// TestParseASCImplicitMatchesExplicit is the property that made deleting the
// halving the right fix rather than rejecting SBR. The same HE-AAC content
// signals explicitly in an M4A (an esds with AOT 5) and implicitly in ADTS
// (no ASC at all, so AOT 2 at the core rate). Both must report the rate the
// base layer codes at, or one codec behaves two ways depending on its
// container.
func TestParseASCImplicitMatchesExplicit(t *testing.T) {
	// 00010 0110 0010 0: AOT 2, sfIdx 6 (24000), chanCfg 2. What an ADTS
	// header at the same core rate synthesizes.
	implicit, err := ParseASC([]byte{0x13, 0x10})
	if err != nil {
		t.Fatalf("ParseASC(implicit): %v", err)
	}
	explicit, err := ParseASC([]byte{0x2B, 0x11, 0x88})
	if err != nil {
		t.Fatalf("ParseASC(explicit): %v", err)
	}
	if implicit.SampleRate != explicit.SampleRate {
		t.Errorf("implicit rate %d != explicit rate %d: signalling must not change the reported rate",
			implicit.SampleRate, explicit.SampleRate)
	}
	if implicit.SampleRate != 24000 {
		t.Errorf("implicit SampleRate = %d, want 24000", implicit.SampleRate)
	}
	// Only the explicit form can be warned about; the implicit form is
	// indistinguishable from plain AAC-LC without parsing the extension.
	if implicit.SBR {
		t.Error("implicit ASC must not claim SBR: it is not detectable from the config")
	}
}
