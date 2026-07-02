package riff

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
)

var update = flag.Bool("update", false, "rewrite golden files (make goldens)")

// TestGoldenMuxOutputs pins the muxer's byte-exact output (headers,
// chunk order, placeholders, patches) so refactors cannot silently change
// the wire format. Regenerate with `make goldens` and review the diff.
func TestGoldenMuxOutputs(t *testing.T) {
	cases := []struct {
		name     string
		cfg      pcm.Config
		channels int
		frames   int
		opts     *MuxerOptions
	}{
		{"golden-s16.wav", pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, 300, nil},
		{"golden-f32.wav", pcm.Config{Encoding: pcm.Float, Bits: 32}, 1, 200, nil},
		{"golden-ext-24in32.wav", pcm.Config{Encoding: pcm.SignedInt, Bits: 32, ValidBits: 24}, 6, 50, nil},
		{"golden-rf64.wav", pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, 300, &MuxerOptions{SizeLimit: 256}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.cfg.PCMFormat(48000, tt.channels, audio.DefaultLayout(tt.channels))
			ws := &memWS{}
			muxWAV(t, ws, tt.cfg, f, wireBytes(tt.cfg, tt.channels, tt.frames, 99), int64(tt.frames), tt.opts)
			compareGolden(t, filepath.Join("testdata", tt.name), ws.b, *update)
		})
	}
}

func compareGolden(t *testing.T, path string, got []byte, update bool) {
	t.Helper()
	if update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (run `make goldens`): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s (%d vs %d bytes); if intentional, `make goldens` and review", path, len(got), len(want))
	}
}
