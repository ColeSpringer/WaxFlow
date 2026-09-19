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
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
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
		got := readAll(t, e, out.Buf, frames)
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
		got := readAll(t, e, out.Buf, frames)
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
	info, err := e.Probe(container.BytesSource(out.Buf), "", nil)
	if err != nil {
		t.Fatalf("probe fragmented mp4: %v", err)
	}
	if info.Container != "mp4" {
		t.Errorf("Container = %q, want mp4", info.Container)
	}
	d := info.Default()
	if d.Codec != "aac-lc" {
		t.Errorf("codec = %q, want aac-lc", d.Codec)
	}
	// Our own fragmented output states its length in an edit list, which is the
	// authoritative tier: a measurement of the content rather than a writer's
	// claim about it. The sidx tier below it must not have displaced this.
	if d.Samples != 4096 || !d.SamplesExact {
		t.Errorf("Samples = %d exact=%v, want 4096 and exact (the init's edit list)", d.Samples, d.SamplesExact)
	}
}

// TestFragmentedSeekLeavesTheLengthToBeConfirmed is the Media-level half of a
// rule the demuxer enforces: a seek builds the fragment index and settles
// nothing.
//
// format.Media caches its own copy of the track and refreshes it from Walk, not
// from SeekSample. A seek that settled the demuxer's track would therefore flip
// Walked() true while every length the Media reports stayed the open-time one,
// and a run about to write a count into headers it cannot patch reads exactly
// that flag (confirmableLength): it would commit the unverified number and
// then miss it.
func TestFragmentedSeekLeavesTheLengthToBeConfirmed(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "container", "mp4", "testdata", "ima4-frag.mov"))
	if err != nil {
		t.Fatal(err)
	}
	med, err := format.Open(container.BytesSource(raw), "mov", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	before := med.Info().Default()
	if format.MediaWalked(med) {
		t.Fatal("this cell needs a source with a deferred walk")
	}
	if _, err := med.SeekSample(1024); err != nil {
		t.Fatal(err)
	}
	if format.MediaWalked(med) {
		t.Fatal("a seek reported the payload as measured; the Media's own length never moved")
	}
	after := med.Info().Default()
	if after.Samples != before.Samples || after.SamplesExact != before.SamplesExact {
		t.Fatalf("a seek moved the Media's length (%d/%v -> %d/%v)",
			before.Samples, before.SamplesExact, after.Samples, after.SamplesExact)
	}
	// And the walk that follows still measures, on the same Media.
	if err := format.WalkMedia(med); err != nil {
		t.Fatal(err)
	}
	if !format.MediaWalked(med) || !med.Info().Default().SamplesExact {
		t.Error("the walk after a seek did not settle the Media's length")
	}
}
