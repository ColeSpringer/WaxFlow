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

// TestTrackIDAndOutputShape pins the codec identity and per-AU output
// length each signaling form resolves to: explicit v1 is codec.HEAAC at
// the extension rate with 2048-sample AUs; explicit PS over the mono core
// it is defined on (v2) likewise, widened to stereo; explicit PS over a
// stereo core is a shape PS does not define, so it keeps the warned LC
// base-layer path; an implicit (bare LC) config is LC.
func TestTrackIDAndOutputShape(t *testing.T) {
	for _, tc := range []struct {
		name    string
		asc     []byte
		id      string
		rate    int
		chans   int
		perAU   int
		warning bool
	}{
		{"explicit v1", []byte{0x2B, 0x11, 0x88}, "he-aac", 48000, 2, 2048, false},
		{"fdk 44100 v1", []byte{0x2B, 0x92, 0x08, 0x00}, "he-aac", 44100, 2, 2048, false},
		// 11101 0110 0001 0011 00010 0: AOT 29, core 24000 mono, ext 48000.
		{"explicit ps v2", []byte{0xEB, 0x09, 0x88}, "he-aac", 48000, 2, 2048, false},
		{"explicit ps stereo core", []byte{0xEB, 0x11, 0x88}, "aac-lc", 24000, 2, 1024, true},
		{"plain lc", []byte{0x13, 0x10}, "aac-lc", 24000, 2, 1024, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseASC(tc.asc)
			if err != nil {
				t.Fatalf("ParseASC: %v", err)
			}
			if got := string(TrackID(cfg)); got != tc.id {
				t.Errorf("TrackID = %q, want %q", got, tc.id)
			}
			f, err := cfg.Format()
			if err != nil {
				t.Fatalf("Format: %v", err)
			}
			if f.Rate != tc.rate {
				t.Errorf("Format rate = %d, want %d", f.Rate, tc.rate)
			}
			if f.Channels != tc.chans {
				t.Errorf("Format channels = %d, want %d", f.Channels, tc.chans)
			}
			if got := cfg.OutputSamplesPerAU(); got != tc.perAU {
				t.Errorf("OutputSamplesPerAU = %d, want %d", got, tc.perAU)
			}
			if got := cfg.SBRWarning() != ""; got != tc.warning {
				t.Errorf("SBRWarning present = %v, want %v", got, tc.warning)
			}
		})
	}
}

// TestParseASCSyncExtension pins the backward-compatible signaling form the
// conformance streams use: a plain LC config followed by the 0x2b7 sync
// extension carrying AOT 5 and the extension rate. It must resolve to the
// same identity as the hierarchical form.
func TestParseASCSyncExtension(t *testing.T) {
	var w sbrTestBits
	w.put(2, 5)      // AOT 2 (LC)
	w.put(6, 4)      // sfIdx 6 = 24000
	w.put(2, 4)      // chanCfg 2
	w.put(0, 3)      // GASpecificConfig: frameLen 0, dependsOnCore 0, extFlag 0
	w.put(0x2b7, 11) // sync extension
	w.put(5, 5)      // AOT 5 (SBR)
	w.put(1, 1)      // sbrPresent
	w.put(3, 4)      // extSfIdx 3 = 48000
	w.w.align()
	asc := append([]byte(nil), w.w.buf...)

	cfg, err := ParseASC(asc)
	if err != nil {
		t.Fatalf("ParseASC(sync-ext): %v", err)
	}
	if !cfg.SBR || cfg.PS {
		t.Errorf("SBR/PS = %v/%v, want true/false", cfg.SBR, cfg.PS)
	}
	if cfg.SampleRate != 24000 || cfg.ExtensionRate != 48000 {
		t.Errorf("rates = %d/%d, want 24000/48000", cfg.SampleRate, cfg.ExtensionRate)
	}
	if got := string(TrackID(cfg)); got != "he-aac" {
		t.Errorf("TrackID = %q, want he-aac", got)
	}
}
