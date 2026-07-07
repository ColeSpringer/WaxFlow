package waxflow_test

import (
	"context"
	"slices"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

func TestTranscodeFromSample(t *testing.T) {
	const frames, from = 4096, 1000
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	wav, src := makeWAV(t, cfg, 2, frames, 42)
	defer audio.Put(src)

	e := waxflow.New()
	out := &memWS{}
	res, err := e.Transcode(context.Background(), container.BytesSource(wav), "", out,
		waxflow.TranscodeOptions{Format: "wav", FromSample: from})
	if err != nil {
		t.Fatal(err)
	}
	if res.Samples != frames-from {
		t.Fatalf("output samples = %d, want %d", res.Samples, frames-from)
	}
	got := readAll(t, e, out.b, frames-from)
	defer audio.Put(got)
	for c := 0; c < 2; c++ {
		want := src.ChanI(c)[from:]
		have := got.ChanI(c)
		for i := range want {
			if want[i] != have[i] {
				t.Fatalf("channel %d sample %d: got %d, want %d (seek not sample-exact)", c, i, have[i], want[i])
			}
		}
	}

	// Past-end and negative starts fail closed or come back empty, never
	// panic.
	if _, err := e.Transcode(context.Background(), container.BytesSource(wav), "", &memWS{},
		waxflow.TranscodeOptions{Format: "wav", FromSample: -1}); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Fatalf("negative FromSample: %v", err)
	}
}

func TestPlanTranscode(t *testing.T) {
	const frames = 4096
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	wav, src := makeWAV(t, cfg, 2, frames, 7)
	defer audio.Put(src)

	e := waxflow.New()
	info, err := e.Probe(container.BytesSource(wav), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	track := info.Default()

	plan, err := e.PlanTranscode(track, waxflow.TranscodeOptions{Format: "wav", Rate: 24000, FromSample: 96})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Container != "wav" || !plan.Live {
		t.Fatalf("plan = %+v, want live wav", plan)
	}
	if plan.Format.Rate != 24000 {
		t.Fatalf("plan rate = %d", plan.Format.Rate)
	}
	// 48k -> 24k halves the remaining 4000 source frames.
	if plan.Samples != (frames-96)/2 {
		t.Fatalf("plan samples = %d, want %d", plan.Samples, (frames-96)/2)
	}
	if plan.BytesPerFrame != 2*2 {
		t.Fatalf("plan bytes/frame = %d", plan.BytesPerFrame)
	}
	if !slices.Contains(plan.Versions, pcm.Version) {
		t.Fatalf("plan versions %v missing encoder version", plan.Versions)
	}
	if len(plan.Versions) < 2 {
		t.Fatalf("resampling plan must carry DSP node versions, got %v", plan.Versions)
	}

	// The same options with no conversion carry only the encoder version.
	base, err := e.PlanTranscode(track, waxflow.TranscodeOptions{Format: "wav"})
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Versions) != 1 || base.Samples != frames {
		t.Fatalf("baseline plan = %+v", base)
	}

	// AIFF exists in the table but has no streaming form.
	aiffPlan, err := e.PlanTranscode(track, waxflow.TranscodeOptions{Format: "aiff"})
	if err != nil {
		t.Fatal(err)
	}
	if aiffPlan.Live {
		t.Fatal("aiff must not report a streaming form")
	}

	// Plan validation mirrors Transcode validation.
	if _, err := e.PlanTranscode(track, waxflow.TranscodeOptions{Format: "opus"}); waxerr.CodeOf(err) != waxerr.CodeUnsupportedFormat {
		t.Fatalf("unknown format: %v", err)
	}
	if _, err := e.PlanTranscode(track, waxflow.TranscodeOptions{Format: "wav", FromSample: -5}); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Fatalf("negative FromSample: %v", err)
	}
	if _, err := e.PlanTranscode(track, waxflow.TranscodeOptions{Format: "wav", GainDB: 999}); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Fatalf("wild gain: %v", err)
	}
}

func TestOutputsTable(t *testing.T) {
	outs := waxflow.Outputs()
	if len(outs) != 4 || outs[0].Name != "wav" || !outs[0].Live ||
		outs[1].Name != "aiff" || outs[1].Live ||
		outs[2].Name != "flac" || !outs[2].Live ||
		outs[3].Name != "mp3" || !outs[3].Live {
		t.Fatalf("Outputs() = %+v", outs)
	}
}
