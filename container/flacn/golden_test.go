package flacn_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/internal/testutil"
)

var update = flag.Bool("update", false, "rewrite golden files (make goldens)")

// TestGoldenMuxOutputs pins encoder plus muxer byte-exact output (frame
// coding decisions, header layout, back-patches) so refactors cannot
// silently change the wire format or the compressed bytes a cache key
// stands for. The encoder is deterministic by construction, no seeds
// involved. Regenerate with `make goldens` and review the diff.
func TestGoldenMuxOutputs(t *testing.T) {
	cases := []struct {
		name     string
		fmt      audio.Format
		frames   int
		level    int
		seekable bool
	}{
		{"golden-sine-s16-l5.flac", muxFmt(44100, 2, 16), 4096 + 500, 5, true},
		{"golden-sine-s24-l8.flac", muxFmt(48000, 1, 24), 3000, 8, true},
		{"golden-noise-s16-l0.flac", muxFmt(44100, 2, 16), 2500, 0, true},
		{"golden-sine-s16-stream.flac", muxFmt(44100, 2, 16), 3000, 5, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			src := testutil.Sine(tt.fmt, tt.frames, 997, 0.8)
			if tt.name == "golden-noise-s16-l0.flac" {
				audio.Put(src)
				src = testutil.Noise(tt.fmt, tt.frames, 99)
			}
			defer audio.Put(src)
			var raw []byte
			if tt.seekable {
				ws := &memWS{}
				encodeStream(t, src, tt.level, ws, int64(src.N))
				raw = ws.b
			} else {
				var buf bytes.Buffer
				encodeStream(t, src, tt.level, &buf, int64(src.N))
				raw = buf.Bytes()
			}
			compareGolden(t, filepath.Join("testdata", tt.name), raw, *update)
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
