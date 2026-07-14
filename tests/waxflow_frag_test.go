package waxflow_test

// Fragmented-MP4 read-back: the aac and alac output rows write
// fragmented (CMAF) MP4 through mp4.NewMuxer, which the demuxer previously
// could not read back (it parsed progressive moov/stbl only). This closes that
// asymmetry: transcode to fragmented MP4 and re-read it through the production
// facade (format.Open -> mp4 demuxer's fragmented branch -> decode). ALAC is
// lossless, so it must reconstruct bit for bit; AAC must land the exact gapless
// sample count, proving the init edit list drives the fragmented trims.

import (
	"context"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
)

func TestFragmentedMP4ReadBack(t *testing.T) {
	e := waxflow.New()
	const frames = 9111
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}

	t.Run("alac lossless", func(t *testing.T) {
		wav, src := makeWAV(t, cfg, 2, frames, 51)
		defer audio.Put(src)
		out := &memWS{}
		res, err := e.Transcode(context.Background(), container.BytesSource(wav), "", out,
			waxflow.TranscodeOptions{Format: "alac"})
		if err != nil {
			t.Fatalf("transcode alac: %v", err)
		}
		if res.Samples != frames {
			t.Fatalf("Samples = %d, want %d", res.Samples, frames)
		}
		// Re-read the fragmented MP4 through the production demuxer.
		got := readAll(t, e, out.b, frames)
		defer audio.Put(got)
		equalPCM(t, src, got)
	})

	t.Run("aac gapless", func(t *testing.T) {
		wav, src := makeWAV(t, cfg, 2, frames, 53)
		defer audio.Put(src)
		out := &memWS{}
		res, err := e.Transcode(context.Background(), container.BytesSource(wav), "", out,
			waxflow.TranscodeOptions{Format: "aac"})
		if err != nil {
			t.Fatalf("transcode aac: %v", err)
		}
		if res.Samples != frames {
			t.Fatalf("Samples = %d, want %d", res.Samples, frames)
		}
		got := readAll(t, e, out.b, frames)
		defer audio.Put(got)
		if got.N != frames {
			t.Fatalf("decoded %d frames, want %d (fragmented gapless trim failed)", got.N, frames)
		}
	})
}

// TestFragmentedMP4Probe checks that a fragmented file we wrote probes as the
// right codec through the production facade (the demuxer no longer rejects its
// own output).
func TestFragmentedMP4Probe(t *testing.T) {
	e := waxflow.New()
	wav, src := makeWAV(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, 4096, 55)
	defer audio.Put(src)
	out := &memWS{}
	if _, err := e.Transcode(context.Background(), container.BytesSource(wav), "", out,
		waxflow.TranscodeOptions{Format: "aac"}); err != nil {
		t.Fatalf("transcode: %v", err)
	}
	info, err := e.Probe(container.BytesSource(out.b), "", nil)
	if err != nil {
		t.Fatalf("probe fragmented mp4: %v", err)
	}
	if info.Container != "mp4" {
		t.Errorf("Container = %q, want mp4", info.Container)
	}
	if d := info.Default(); d.Codec != "aac-lc" {
		t.Errorf("codec = %q, want aac-lc", d.Codec)
	}
}
