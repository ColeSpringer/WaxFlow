package mp4

import (
	"flag"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/alac"
	"github.com/colespringer/waxflow/internal/testutil"
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
		// 24-bit mono, single fragment (compressed SCE, one byte shifted off).
		{"golden-s24-mono.m4a", fmtFor(48000, 1, 24), 3000, 0},
		// 32-bit stereo (compressed CPE, two bytes shifted off).
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
			testutil.Golden(t, path, raw, *update)
		})
	}
}

// TestGoldenProgressiveMuxOutputs pins the OTHER writer mp4.MuxerVersion
// stands for. NewProgressiveMuxer lays a flat moov-first movie with a real
// sample table, which shares no box-writing code path with the fragmented
// muxer above beyond the sample entries, so one golden cannot cover both and
// one version term covers them both anyway. Regenerate with `make goldens`
// and review the diff. The packets are canned, so the bytes are the muxer's
// alone and the same on every architecture.
func TestGoldenProgressiveMuxOutputs(t *testing.T) {
	const preSkip, padding, npkt = 312, 100, 8
	samples := int64(npkt*960 - preSkip - padding)
	track, pkts := opusTrackFor(preSkip, samples, npkt)
	raw := muxProgressive(t, track, pkts,
		codec.Trailer{Samples: samples, Delay: preSkip, Padding: padding})
	testutil.Golden(t, filepath.Join("testdata", "golden", "golden-progressive-opus.mp4"), raw, *update)
}

func fmtFor(rate, ch, depth int) audio.Format {
	return audio.Format{Rate: rate, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Int, BitDepth: depth}
}
