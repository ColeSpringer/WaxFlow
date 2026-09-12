package mpa

import (
	"bytes"
	"flag"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/internal/testutil"
)

var update = flag.Bool("update", false, "rewrite golden files (make goldens)")

// TestGoldenMuxOutputs pins the encoder plus muxer byte-exact output (frame
// coding, side info, the reservoir layout, and the gapless tag) so a refactor
// cannot silently change the wire bytes a cache key stands for. The encoder is
// deterministic by construction. Regenerate with `make goldens` and review the
// diff.
func TestGoldenMuxOutputs(t *testing.T) {
	cases := []struct {
		name                 string
		rate, channels, kbps int
		frames               int
		seekable             bool
	}{
		{"golden-44k-stereo-128.mp3", 44100, 2, 128000, 6000, false},
		{"golden-44k-mono-128-seek.mp3", 44100, 1, 128000, 5003, true},
		{"golden-48k-stereo-192.mp3", 48000, 2, 192000, 5000, false},
		{"golden-24k-stereo-96-mpeg2.mp3", 24000, 2, 96000, 4000, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			pkts, tr, samples := encodeTone(t, tt.rate, tt.channels, tt.kbps, tt.frames)
			var raw []byte
			if tt.seekable {
				ws := &memWS{}
				muxPackets(t, ws, pkts, tr, samples, tt.rate, tt.channels)
				raw = ws.Buf
			} else {
				var buf bytes.Buffer
				muxPackets(t, &buf, pkts, tr, samples, tt.rate, tt.channels)
				raw = buf.Bytes()
			}
			testutil.Golden(t, filepath.Join("testdata", tt.name), raw, *update)
		})
	}
}
