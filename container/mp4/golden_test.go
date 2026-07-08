package mp4

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/alac"
)

var update = flag.Bool("update", false, "rewrite golden files (make goldens)")

// TestGoldenMuxOutputs pins the encoder-plus-muxer byte-exact output (box
// layout, the ALAC frame coding, fragment boundaries) so a refactor cannot
// silently change the wire format or the compressed bytes a cache key stands
// for. Both the encoder and the muxer are deterministic by construction, no
// seeds involved. Regenerate with `make goldens` and review the diff.
func TestGoldenMuxOutputs(t *testing.T) {
	cases := []struct {
		name    string
		fmt     audio.Format
		frames  int
		fragTgt int
	}{
		// 16-bit stereo across two-plus fragments (compressed CPE, mixRes).
		{"golden-s16-stereo.m4a", fmtFor(44100, 2, 16), alac.FrameSize*2 + 500, alac.FrameSize},
		// 24-bit mono, single fragment (compressed SCE).
		{"golden-s24-mono.m4a", fmtFor(48000, 1, 24), 3000, 0},
		// 32-bit stereo (verbatim escape frames).
		{"golden-s32-stereo.m4a", fmtFor(48000, 2, 32), 2500, 0},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			src := audio.Get(tt.fmt, tt.frames)
			defer audio.Put(src)
			src.N = tt.frames
			fillTone(src)
			raw := muxALAC(t, src, tt.fragTgt)
			path := filepath.Join("testdata", "golden", tt.name)
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("missing golden %s (run `make goldens`): %v", path, err)
			}
			if !bytes.Equal(raw, want) {
				t.Errorf("output differs from %s (%d vs %d bytes); if intentional, `make goldens` and review", path, len(raw), len(want))
			}
		})
	}
}

func fmtFor(rate, ch, depth int) audio.Format {
	return audio.Format{Rate: rate, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Int, BitDepth: depth}
}
