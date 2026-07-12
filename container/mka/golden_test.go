package mka

import (
	"bytes"
	"encoding/binary"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

var update = flag.Bool("update", false, "rewrite golden files (make goldens)")

// TestGoldenMuxOutputs pins the muxer's byte-exact wire format (EBML header,
// Segment/Info/Tracks layout, cluster and block framing, the gapless
// CodecDelay/DiscardPadding) so a refactor cannot silently change it. The
// muxer is deterministic by construction (fixed TrackUID and app strings, no
// DateUTC), so no seeds are involved. Regenerate with `make goldens` and
// review the diff.
func TestGoldenMuxOutputs(t *testing.T) {
	// A matroska PCM case (int16 stereo, definite-size clusters) and a webm
	// Opus case (CodecDelay + a DiscardPadding-bearing final BlockGroup).
	pcmFmt := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	pcmTrack := container.Track{Codec: codec.PCM, Fmt: pcmFmt, Default: true}
	var pcmPkts []codec.Packet
	for i := 0; i < 5; i++ {
		data := make([]byte, 480*4)
		for j := range data {
			data[j] = byte(i*31 + j*3)
		}
		pcmPkts = append(pcmPkts, codec.Packet{Data: data, Dur: 480, PTS: int64(i * 480)})
	}

	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8], head[9] = 1, 2
	binary.LittleEndian.PutUint16(head[10:], 312)
	binary.LittleEndian.PutUint32(head[12:], 48000)
	opusFmt := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	opusTrack := container.Track{Codec: codec.Opus, CodecConfig: head, Fmt: opusFmt, Delay: 312, Default: true}
	var opusPkts []codec.Packet
	for i := 0; i < 5; i++ {
		opusPkts = append(opusPkts, codec.Packet{Data: []byte{0x00, byte(i), 0xA5, 0x5A}, Dur: 480, PTS: int64(i * 480)})
	}

	cases := []struct {
		name    string
		track   container.Track
		opts    *MuxerOptions
		pkts    []codec.Packet
		trailer codec.Trailer
	}{
		{"golden-pcm-s16-stereo.mka", pcmTrack, nil, pcmPkts, codec.Trailer{Samples: 5 * 480}},
		{"golden-opus.webm", opusTrack, &MuxerOptions{WebM: true}, opusPkts,
			codec.Trailer{Samples: 5*480 - 312 - 100, Delay: 312, Padding: 100}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			raw := muxToBytes(t, tt.track, tt.opts, tt.pkts, tt.trailer)
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
