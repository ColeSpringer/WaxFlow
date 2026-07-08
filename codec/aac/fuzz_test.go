package aac

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
)

// FuzzDecode feeds arbitrary access-unit bytes to the decoder against a
// valid stereo AAC-LC config. Invariant: no panic, and whenever a frame is
// emitted it carries exactly the configured frame length.
func FuzzDecode(f *testing.F) {
	// ASC for AAC-LC, 44100 Hz, stereo.
	asc := []byte{0x12, 0x10}
	f.Add([]byte{0x00})
	f.Add([]byte{0x21, 0x00, 0x00})       // CPE tag start
	f.Add([]byte{0x00, 0x00, 0x00, 0x00}) // SCE-ish
	f.Add([]byte{0xe0})                   // END tag

	f.Fuzz(func(t *testing.T, data []byte) {
		cfg, err := ParseASC(asc)
		if err != nil {
			t.Fatal(err)
		}
		d, err := NewDecoder(cfg, cfg.Format())
		if err != nil {
			t.Fatal(err)
		}
		_ = d.Decode(data, func(b *audio.Buffer) error {
			if b.N != int(cfg.FrameLength) {
				t.Fatalf("emitted %d frames, want %d", b.N, cfg.FrameLength)
			}
			return nil
		})
	})
}
