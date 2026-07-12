package waxflow_test

import (
	"context"
	"slices"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/opus"
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

	// The same options with no conversion carry exactly the source decoder
	// and the encoder version (both pcm here).
	base, err := e.PlanTranscode(track, waxflow.TranscodeOptions{Format: "wav"})
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Versions) != 2 || base.Samples != frames {
		t.Fatalf("baseline plan = %+v", base)
	}
	if base.Versions[0] != pcm.Version {
		t.Fatalf("baseline versions %v must lead with the source decoder", base.Versions)
	}

	// A compressed source leads with its codec's decoder version, so a
	// decoder revision invalidates cached transcodes of that codec's
	// sources (ADR-0004); the rest of the plan is source-codec-blind.
	opusTrack := track
	opusTrack.Codec = codec.Opus
	fromOpus, err := e.PlanTranscode(opusTrack, waxflow.TranscodeOptions{Format: "wav"})
	if err != nil {
		t.Fatal(err)
	}
	if fromOpus.Versions[0] != opus.Version {
		t.Fatalf("opus-source versions %v must lead with %s", fromOpus.Versions, opus.Version)
	}
	if slices.Equal(fromOpus.Versions, base.Versions) {
		t.Fatal("opus-source and pcm-source plans must not share cache versions")
	}

	// AIFF exists in the table but has no streaming form.
	aiffPlan, err := e.PlanTranscode(track, waxflow.TranscodeOptions{Format: "aiff"})
	if err != nil {
		t.Fatal(err)
	}
	if aiffPlan.Live {
		t.Fatal("aiff must not report a streaming form")
	}

	// CBR opus reports its exact rate; unconstrained VBR output is
	// signal-dependent, so its plan leaves the rate and size hints honestly
	// unknown (the documented VBR convention FLAC also follows).
	cbrPlan, err := e.PlanTranscode(track, waxflow.TranscodeOptions{Format: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	if cbrPlan.BitRate == 0 || cbrPlan.EstimatedBytes < 0 {
		t.Fatalf("CBR opus plan = bitRate %d estimated %d, want both known", cbrPlan.BitRate, cbrPlan.EstimatedBytes)
	}
	vbrPlan, err := e.PlanTranscode(track, waxflow.TranscodeOptions{Format: "opus", OpusVBR: true})
	if err != nil {
		t.Fatal(err)
	}
	if vbrPlan.BitRate != 0 || vbrPlan.EstimatedBytes != -1 {
		t.Fatalf("VBR opus plan = bitRate %d estimated %d, want 0 and -1 (unknown)", vbrPlan.BitRate, vbrPlan.EstimatedBytes)
	}

	// MP3 follows the same contract: CBR reports its clamped rate, VBR
	// leaves rate and size unknown.
	mp3CBR, err := e.PlanTranscode(track, waxflow.TranscodeOptions{Format: "mp3", Rate: 44100})
	if err != nil {
		t.Fatal(err)
	}
	if mp3CBR.BitRate != 128000 || mp3CBR.EstimatedBytes < 0 {
		t.Fatalf("CBR mp3 plan = bitRate %d estimated %d, want 128000 and known", mp3CBR.BitRate, mp3CBR.EstimatedBytes)
	}
	mp3VBR, err := e.PlanTranscode(track, waxflow.TranscodeOptions{Format: "mp3", Rate: 44100, MP3VBR: true})
	if err != nil {
		t.Fatal(err)
	}
	if mp3VBR.BitRate != 0 || mp3VBR.EstimatedBytes != -1 {
		t.Fatalf("VBR mp3 plan = bitRate %d estimated %d, want 0 and -1 (unknown)", mp3VBR.BitRate, mp3VBR.EstimatedBytes)
	}

	// Plan validation mirrors Transcode validation.
	if _, err := e.PlanTranscode(track, waxflow.TranscodeOptions{Format: "wavpack"}); waxerr.CodeOf(err) != waxerr.CodeUnsupportedFormat {
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
	if len(outs) != 8 || outs[0].Name != "wav" || !outs[0].Live ||
		outs[1].Name != "opus" || !outs[1].Live ||
		outs[2].Name != "vorbis" || !outs[2].Live ||
		outs[3].Name != "aiff" || outs[3].Live ||
		outs[4].Name != "flac" || !outs[4].Live ||
		outs[5].Name != "mp3" || !outs[5].Live ||
		outs[6].Name != "aac" || !outs[6].Live ||
		outs[7].Name != "alac" || !outs[7].Live {
		t.Fatalf("Outputs() = %+v", outs)
	}
}
