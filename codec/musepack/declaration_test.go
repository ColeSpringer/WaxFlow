package musepack_test

import (
	"testing"

	"github.com/colespringer/waxflow/codec/musepack"
)

// TestNoiseDeclarationMatchesUse pins what the SV8 seek shortcut rests on:
// container/mpc hands a seek landing the initial noise-generator state without
// scanning when the encoder info packet declares noise substitution off. That
// is trusting the encoder's word, so the word is checked here on every
// committed fixture of both encoders: a stream declaring it off codes no noise
// band, and a stream coding one declares it on. An undeclared stream (no EI
// packet) is scanned regardless and is exempt.
func TestNoiseDeclarationMatchesUse(t *testing.T) {
	for _, f := range mpcFixtures {
		t.Run(f.name(), func(t *testing.T) {
			cfg, _, pkts := demuxAll(t, readFile(t, f.path))
			counts, err := musepack.ResCounts(cfg, pkts)
			if err != nil {
				t.Fatal(err)
			}
			noise := counts[-1] > 0
			switch cfg.PNS {
			case 0:
				if noise {
					t.Errorf("declares noise substitution off and codes %d noise bands", counts[-1])
				}
			case 1:
				if !noise {
					t.Errorf("declares noise substitution on and codes no noise band")
				}
			case musepack.PNSUnknown:
				t.Logf("undeclared; %d noise bands", counts[-1])
			}
		})
	}
}
