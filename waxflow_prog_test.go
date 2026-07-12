package waxflow_test

// Progressive-MP4 output (phase 3b engine wiring): the aac and alac rows expose
// the flat (non-fragmented) MP4 form through the "progressive" container
// override. It round-trips through the demuxer's progressive path, reports as
// not-live in the plan (it back-patches, so it needs a seekable destination),
// and embeds metadata in the moov like the fragmented form.

import (
	"context"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
)

func TestProgressiveOutputRoundTrip(t *testing.T) {
	e := waxflow.New()
	const frames = 9111
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}

	t.Run("alac lossless", func(t *testing.T) {
		wav, src := makeWAV(t, cfg, 2, frames, 63)
		defer audio.Put(src)
		out := &memWS{}
		res, err := e.Transcode(context.Background(), container.BytesSource(wav), "", out,
			waxflow.TranscodeOptions{Format: "alac", Container: "progressive"})
		if err != nil {
			t.Fatalf("transcode alac/progressive: %v", err)
		}
		if res.Container != "progressive" || res.Samples != frames {
			t.Fatalf("result = %+v", res)
		}
		got := readAll(t, e, out.b, frames)
		defer audio.Put(got)
		equalPCM(t, src, got)
	})

	t.Run("aac gapless", func(t *testing.T) {
		wav, src := makeWAV(t, cfg, 2, frames, 65)
		defer audio.Put(src)
		out := &memWS{}
		if _, err := e.Transcode(context.Background(), container.BytesSource(wav), "", out,
			waxflow.TranscodeOptions{Format: "aac", Container: "progressive"}); err != nil {
			t.Fatalf("transcode aac/progressive: %v", err)
		}
		got := readAll(t, e, out.b, frames)
		defer audio.Put(got)
		if got.N != frames {
			t.Fatalf("decoded %d frames, want %d (progressive gapless trim failed)", got.N, frames)
		}
	})
}

// TestProgressivePlanLiveness pins the container-aware liveness: the default
// fragmented AAC is live (streamable), the progressive form is not.
func TestProgressivePlanLiveness(t *testing.T) {
	e := waxflow.New()
	f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	track := container.Track{Codec: "pcm", Fmt: f, Samples: 48000}

	frag, err := e.PlanTranscode(track, waxflow.TranscodeOptions{Format: "aac"})
	if err != nil {
		t.Fatalf("plan fragmented: %v", err)
	}
	if !frag.Live {
		t.Error("fragmented AAC plan Live = false, want true")
	}
	prog, err := e.PlanTranscode(track, waxflow.TranscodeOptions{Format: "aac", Container: "progressive"})
	if err != nil {
		t.Fatalf("plan progressive: %v", err)
	}
	if prog.Live {
		t.Error("progressive AAC plan Live = true, want false (back-patches, not streamable)")
	}
	// The two forms must not share a cache identity: their container differs.
	if frag.Container == prog.Container {
		t.Errorf("fragmented and progressive share Container %q", frag.Container)
	}
}

// TestProgressiveNeedsSeekableDest checks that a progressive transcode to a
// plain (non-seekable) writer is refused, since the muxer back-patches.
func TestProgressiveNeedsSeekableDest(t *testing.T) {
	e := waxflow.New()
	wav, src := makeWAV(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, 2048, 67)
	defer audio.Put(src)
	_, err := e.Transcode(context.Background(), container.BytesSource(wav), "", onlyWriter{},
		waxflow.TranscodeOptions{Format: "aac", Container: "progressive"})
	if err == nil {
		t.Error("progressive transcode to a non-seekable writer accepted; want rejection")
	}
}
