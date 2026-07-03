package mp3_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/mp3"
)

func fixture(f *testing.F, name string) []byte {
	f.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		f.Fatal(err)
	}
	return raw
}

// FuzzDecode asserts the packet decoder's invariants on arbitrary
// payloads behind a valid header: no panics, and every accepted packet
// emits exactly its header's frame count (silence when damaged), which
// is what position bookkeeping stands on.
func FuzzDecode(f *testing.F) {
	for _, name := range []string{"sine-untagged.mp3", "sine-8000-cbr16.mp3", "sine-22050-vbr.mp3"} {
		raw := fixture(f, name)
		if h, err := mp3.ParseHeader(raw); err == nil && h.Size() > 0 && h.Size() <= len(raw) {
			f.Add(raw[:h.Size()])
		}
		f.Add(raw[:min(600, len(raw))])
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := mp3.ParseHeader(data)
		if err != nil {
			return
		}
		d, err := mp3.NewDecoder(h.PCMFormat())
		if err != nil {
			t.Fatalf("header parsed but format rejected: %v", err)
		}
		emitted := 0
		err = d.Decode(data, func(b *audio.Buffer) error {
			emitted += b.N
			return nil
		})
		if err != nil {
			return // wiring-bug class errors are allowed to reject
		}
		if emitted != h.SamplesPerFrame() {
			t.Fatalf("emitted %d samples, header says %d", emitted, h.SamplesPerFrame())
		}
		// A second packet through the same decoder exercises the
		// reservoir roll.
		_ = d.Decode(data, func(*audio.Buffer) error { return nil })
	})
}
