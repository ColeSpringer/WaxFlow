package aiff

import (
	"flag"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/internal/testutil"
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
			testutil.Golden(t, filepath.Join("testdata", tt.name), ws.Buf, *update)
		})
	}
}
