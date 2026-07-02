package aiff

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

// TestGoldenMuxOutputs pins the muxer's byte-exact output. Regenerate
// with `make goldens` and review the diff.
func TestGoldenMuxOutputs(t *testing.T) {
	cases := []struct {
		name     string
		cfg      pcm.Config
		channels int
		frames   int
	}{
		{"golden-s16.aiff", pcm.Config{Encoding: pcm.SignedInt, Bits: 16, BigEndian: true}, 2, 300},
		{"golden-s24.aiff", pcm.Config{Encoding: pcm.SignedInt, Bits: 24, BigEndian: true}, 2, 100},
		{"golden-f32.aifc", pcm.Config{Encoding: pcm.Float, Bits: 32, BigEndian: true}, 1, 200},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.cfg.PCMFormat(48000, tt.channels, audio.DefaultLayout(tt.channels))
			ws := &memWS{}
			muxAIFF(t, ws, tt.cfg, f, wireBytes(tt.cfg, tt.channels, tt.frames, 99), int64(tt.frames))
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
