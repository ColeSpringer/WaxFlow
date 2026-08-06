package waxflow_test

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/opus"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
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

// TestChannelRefusalNamesTheRemedy pins U2: when the request never asked for
// a channel count, a 1-2 channel encoder's refusal carries the remedy, in
// option vocabulary rather than any boundary's spelling.
func TestChannelRefusalNamesTheRemedy(t *testing.T) {
	const frames = 4096
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	wav, src := makeWAV(t, cfg, 6, frames, 11)
	defer audio.Put(src)

	e := waxflow.New()
	info, err := e.Probe(container.BytesSource(wav), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	track := info.Default()

	// alac is the row that still refuses, and after C1 it is the only one: a
	// lossless output that silently drops four channels is a lie about what
	// it holds, while the lossy rows fold (TestWideSourceChannelPolicy).
	t.Run("alac", func(t *testing.T) {
		_, err := e.PlanTranscode(track, waxflow.TranscodeOptions{Format: "alac"})
		if err == nil {
			t.Fatal("a 6-channel source planned onto a 1-2 channel encoder")
		}
		// The wrap must not cost the error its class.
		if code := waxerr.CodeOf(err); code != waxerr.CodeUnsupportedFormat {
			t.Errorf("code = %s, want %s", code, waxerr.CodeUnsupportedFormat)
		}
		msg := err.Error()
		if !strings.Contains(msg, "channel count to 2") {
			t.Errorf("message %q does not name the remedy", msg)
		}
		// The engine is the public module; no boundary owns this string.
		for _, leak := range []string{"--channels", "ch=", "Channels:"} {
			if strings.Contains(msg, leak) {
				t.Errorf("message %q leaks a boundary's vocabulary (%q)", msg, leak)
			}
		}
	})

	// An explicit request keeps the encoder's own message: naming the same
	// option back at the caller would be noise.
	_, err = e.PlanTranscode(track, waxflow.TranscodeOptions{Format: "aac", Channels: 6})
	if err == nil {
		t.Fatal("explicit Channels=6 planned onto aac")
	}
	if strings.Contains(err.Error(), "channel count to 2") {
		t.Errorf("explicit request got the hint anyway: %v", err)
	}

	// Both rows below hold 6 channels natively and validate their own options
	// before the encoder, so the failure has nothing to do with channels. The
	// (Channels == 0 && source > 2) gate alone fires on both. A bad rate is
	// the wrong probe here: it fails in dsp.NewChain, never reaching row.plan.
	for _, tc := range []struct {
		name string
		opts waxflow.TranscodeOptions
	}{
		{"flac level", waxflow.TranscodeOptions{Format: "flac", FLACLevel: 99}},
		{"opus signal", waxflow.TranscodeOptions{Format: "opus", OpusSignal: "bogus"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.PlanTranscode(track, tc.opts)
			if err == nil {
				t.Fatal("an invalid encoder option planned")
			}
			if strings.Contains(err.Error(), "channel count to 2") {
				t.Errorf("an unrelated refusal was given a channel hint: %v", err)
			}
		})
	}
}

// TestWideSourceChannelPolicy pins C1: the writer-side table's rule for a
// source wider than the output can hold, when the caller asked for no
// particular width. The split is lossy against lossless, not one encoder
// against another; before this, only opus folded, so the same 5.1 source
// served as Opus and 415'd as AAC.
func TestWideSourceChannelPolicy(t *testing.T) {
	const frames = 4096
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	wav, src := makeWAV(t, cfg, 6, frames, 11)
	defer audio.Put(src)

	e := waxflow.New()
	info, err := e.Probe(container.BytesSource(wav), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	track := info.Default()

	cases := []struct {
		format string
		want   int // 0 means the row refuses
	}{
		{"opus", 2}, {"mp3", 2}, {"aac", 2},
		{"alac", 0},
		{"vorbis", 6}, {"flac", 6}, {"wav", 6}, {"aiff", 6},
	}
	for _, tt := range cases {
		t.Run(tt.format, func(t *testing.T) {
			plan, err := e.PlanTranscode(track, waxflow.TranscodeOptions{Format: tt.format})
			if tt.want == 0 {
				if err == nil {
					t.Fatalf("%s planned a 6-channel source; lossless must refuse rather than discard", tt.format)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: %v", tt.format, err)
			}
			if plan.Format.Channels != tt.want {
				t.Errorf("%s resolved to %d channels, want %d", tt.format, plan.Format.Channels, tt.want)
			}
		})
	}

	// An explicit request is never overridden, in either direction: mono
	// stays mono on a row that would have folded to stereo.
	plan, err := e.PlanTranscode(track, waxflow.TranscodeOptions{Format: "aac", Channels: 1})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Format.Channels != 1 {
		t.Errorf("an explicit Channels 1 resolved to %d", plan.Format.Channels)
	}
}

// TestImplicitDownmixIsAnnouncedPerRun pins the other half of C1: a fold the
// caller never asked for is visible without opting in, and it is announced
// once per transcode rather than once per process.
//
// The per-process trap is the reason the notice cannot live in the plan:
// PlanTranscode memoizes cores per (format, options), so a scripted
// conversion of 500 identically shaped 5.1 files would warn on the first and
// go silent for the other 499, which is exactly the failure C1 exists to end.
func TestImplicitDownmixIsAnnouncedPerRun(t *testing.T) {
	const frames = 4096
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	wav, src := makeWAV(t, cfg, 6, frames, 11)
	defer audio.Put(src)

	var logged bytes.Buffer
	// Default level, so this also pins Warn rather than Debug.
	e := waxflow.New(waxflow.WithLogger(slog.New(slog.NewTextHandler(&logged, nil))))

	for i := range 2 {
		var out bytes.Buffer
		if _, err := e.Transcode(context.Background(), container.BytesSource(wav), "wav", &out,
			waxflow.TranscodeOptions{Format: "aac"}); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		med, err := e.OpenStream(container.BytesSource(out.Bytes()), "m4a")
		if err != nil {
			t.Fatalf("run %d: reopening the output: %v", i, err)
		}
		got := med.Info().Default().Fmt.Channels
		med.Close()
		if got != 2 {
			t.Fatalf("run %d: output has %d channels, want the stereo fold", i, got)
		}
	}
	if n := strings.Count(logged.String(), "downmixed to fit the output format"); n != 2 {
		t.Errorf("two folds announced %d times, want 2:\n%s", n, logged.String())
	}

	// The segmented path folds through the same hook, so it must announce it
	// too: the claim that /stream and HLS behave alike was true of the audio
	// and false of the log until this call existed.
	logged.Reset()
	if _, err := e.TranscodeSegments(context.Background(), container.BytesSource(wav), "wav",
		waxflow.TranscodeOptions{Format: "aac"},
		waxflow.SegmentedOptions{SegmentSamples: 1024 * 8},
		func(mp4.Segment) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logged.String(), "downmixed to fit the output format"); n != 1 {
		t.Errorf("the segmented path announced the fold %d times, want 1:\n%s", n, logged.String())
	}

	// A fold the caller asked for is not news.
	logged.Reset()
	var out bytes.Buffer
	if _, err := e.Transcode(context.Background(), container.BytesSource(wav), "wav", &out,
		waxflow.TranscodeOptions{Format: "aac", Channels: 2}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logged.String(), "downmixed") {
		t.Errorf("an explicit channel request was reported as a surprise:\n%s", logged.String())
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
