package waxflow_test

// The clipping-report cell, both halves. A float source can hold level an
// integer output cannot: samples past full scale, which the quantizer clamps
// at the rails and counts (ClippedSamples), and crests between samples, which
// quantization preserves and playback clips, which the output true-peak
// meter reads (TruePeak). Together they let a caller say what happened to
// the level instead of silently shipping a flattened peak.

import (
	"bytes"
	"context"
	"math"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

const (
	clipRate   = 44100
	clipFrames = 8192
	// Overs per channel. testutil.HotFloatChans spaces them, so the count
	// stays readable against the fixture.
	clipOvers = 13
)

// clipFixture renders the shared clipping fixture in memory: a quiet tone
// touching both exact rails, with overs planted past full scale when asked.
func clipFixture(t *testing.T, overs bool) []byte {
	t.Helper()
	n := 0
	if overs {
		n = clipOvers
	}
	return testutil.FloatWAVBytes(t, clipRate, testutil.HotFloatChans(t, clipRate, clipFrames, n))
}

// TestTranscodeReportsClipping: the planted overs reach the caller as an
// exact count, on every integer output, and a source that only touches the
// rails reports nothing. The count is per channel sample, so a stereo
// fixture with K overs in each channel reports 2K.
func TestTranscodeReportsClipping(t *testing.T) {
	e := waxflow.New()
	cases := []struct {
		name string
		opts waxflow.TranscodeOptions
	}{
		{"flac", waxflow.TranscodeOptions{Format: "flac"}}, // float source quantizes to 24 bits
		{"wav16", waxflow.TranscodeOptions{Format: "wav", BitDepth: 16}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out memWS
			res, err := e.Transcode(context.Background(), container.BytesSource(clipFixture(t, true)), "wav", &out, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if want := int64(2 * clipOvers); res.ClippedSamples != want {
				t.Errorf("ClippedSamples = %d, want %d", res.ClippedSamples, want)
			}
			if res.TruePeak <= 0 {
				t.Errorf("TruePeak = %v; the transcode rung must measure its output", res.TruePeak)
			}

			var clean memWS
			res, err = e.Transcode(context.Background(), container.BytesSource(clipFixture(t, false)), "wav", &clean, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if res.ClippedSamples != 0 {
				t.Errorf("a source touching only the rails reported %d clipped samples, want 0",
					res.ClippedSamples)
			}
		})
	}
}

// TestTranscodeFloatOutputReportsNoClipping: a float output carries an over
// as it stands, so there is nothing to clamp and nothing to count; the true
// peak still reports the level, since a hot float file clips playback the
// same way. This is the other half of the CLI's advice ("choose a float
// output"), and it would be an empty suggestion if the count were about the
// source alone. A WAV with no requested depth keeps the float source float,
// so this is that path.
func TestTranscodeFloatOutputReportsNoClipping(t *testing.T) {
	var out memWS
	res, err := waxflow.New().Transcode(context.Background(),
		container.BytesSource(clipFixture(t, true)), "wav", &out,
		waxflow.TranscodeOptions{Format: "wav"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Format.Type != audio.Float {
		t.Fatalf("output is %v, want float; this test is not exercising the float path", res.Format)
	}
	if res.ClippedSamples != 0 {
		t.Errorf("float output reported %d clipped samples, want 0", res.ClippedSamples)
	}
	if res.TruePeak < 1.4 {
		t.Errorf("TruePeak = %.3f, want at least the surviving 1.5 overs; the meter must run on float output too",
			res.TruePeak)
	}
	// Zero because the overs SURVIVED, not because nothing looked. Reading
	// them back out of the written file is what makes "choose a float
	// output" real advice rather than a way to silence the count.
	med, err := format.Open(container.BytesSource(out.b), "wav", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	f := med.Info().Default().Fmt
	chunk := audio.Get(f, audio.StandardChunk)
	defer audio.Put(chunk)
	over := 0
	for med.ReadChunk(chunk) == nil {
		for c := 0; c < f.Channels; c++ {
			for _, v := range chunk.ChanF(c)[:chunk.N] {
				if math.Abs(float64(v)) > 1 {
					over++
				}
			}
		}
	}
	if want := 2 * clipOvers; over != want {
		t.Errorf("the float output holds %d samples past full scale, want %d; "+
			"the overs must pass through, not be silently clamped", over, want)
	}
}

// TestSegmentsReportClipping: the segmented rung runs the same chain, so it
// reports the same way, count and true peak both. ALAC is the integer
// output with an HLS form.
func TestSegmentsReportClipping(t *testing.T) {
	res, err := waxflow.New().TranscodeSegments(context.Background(),
		container.BytesSource(clipFixture(t, true)), "wav",
		waxflow.TranscodeOptions{Format: "alac"},
		waxflow.SegmentedOptions{SegmentSamples: 4096},
		func(mp4.Segment) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(2 * clipOvers); res.ClippedSamples != want {
		t.Errorf("ClippedSamples = %d, want %d", res.ClippedSamples, want)
	}
	if res.TruePeak <= 0 {
		t.Errorf("TruePeak = %v; the segmented rung must measure its output", res.TruePeak)
	}
}

// TestRemuxReportsNoClipping pins "zero by construction" on the copy rung,
// using the very file the clipping transcode produced. A copy of an already
// clamped file has clamped nothing itself, and saying otherwise would warn
// on every later pass over the same audio. The true peak is zero for the
// stronger reason: a remux never decodes, so there is nothing to measure.
func TestRemuxReportsNoClipping(t *testing.T) {
	e := waxflow.New()
	var flac memWS
	res, err := e.Transcode(context.Background(), container.BytesSource(clipFixture(t, true)), "wav", &flac,
		waxflow.TranscodeOptions{Format: "flac"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ClippedSamples == 0 {
		t.Fatal("the fixture clipped nothing; the rest of this test proves nothing")
	}

	var out bytes.Buffer
	res, err = e.Remux(context.Background(), container.BytesSource(flac.b), "flac", &out,
		waxflow.TranscodeOptions{Format: "flac", Container: "mka"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ClippedSamples != 0 {
		t.Errorf("remux reported %d clipped samples, want 0", res.ClippedSamples)
	}
	if res.TruePeak != 0 {
		t.Errorf("remux reported a true peak of %v, want 0; a copy rung never decodes", res.TruePeak)
	}
}

// clipFixtureAAC encodes the too-loud fixture to AAC, which the cut and
// segmented rungs both need. It doubles as an assertion: a lossy encoder
// never crosses into the integer domain, so it clamps nothing.
func clipFixtureAAC(t *testing.T, e *waxflow.Engine) []byte {
	t.Helper()
	var out memWS
	res, err := e.Transcode(context.Background(), container.BytesSource(clipFixture(t, true)), "wav", &out,
		waxflow.TranscodeOptions{Format: "aac"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ClippedSamples != 0 {
		t.Errorf("a float-fed lossy encode reported %d clipped samples, want 0", res.ClippedSamples)
	}
	return out.b
}

// TestCutReportsNoClipping pins the same on the cut rung, which needs its own
// fixture: only Opus and AAC packets survive being moved (cutCodecs), so a
// cuttable source is never one that quantized.
func TestCutReportsNoClipping(t *testing.T) {
	e := waxflow.New()
	m4a := clipFixtureAAC(t, e)
	grid, err := e.PacketGrid(container.BytesSource(m4a), "m4a")
	if err != nil {
		t.Fatal(err)
	}
	var cut bytes.Buffer
	res, err := e.CutStream(context.Background(), container.BytesSource(m4a), "m4a", &cut,
		waxflow.TranscodeOptions{Format: "aac"},
		[]waxflow.Span{{From: 0, To: clipFrames / 2}}, grid, -1)
	if err != nil {
		t.Fatal(err)
	}
	if res.ClippedSamples != 0 {
		t.Errorf("cut reported %d clipped samples, want 0", res.ClippedSamples)
	}
	if res.TruePeak != 0 {
		t.Errorf("cut reported a true peak of %v, want 0; a copy rung never decodes", res.TruePeak)
	}
}

// TestRemuxSegmentsReportNoClipping covers the fourth result site: the
// segmented remux builds its own SegmentedResult, so it must stay zero too.
func TestRemuxSegmentsReportNoClipping(t *testing.T) {
	e := waxflow.New()
	res, err := e.RemuxSegments(context.Background(), container.BytesSource(clipFixtureAAC(t, e)), "m4a",
		waxflow.TranscodeOptions{Format: "aac"},
		waxflow.SegmentedOptions{SegmentSamples: 1024},
		func(mp4.Segment) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if res.ClippedSamples != 0 {
		t.Errorf("segmented remux reported %d clipped samples, want 0", res.ClippedSamples)
	}
	if res.TruePeak != 0 {
		t.Errorf("segmented remux reported a true peak of %v, want 0", res.TruePeak)
	}
}

// TestIntersampleOversReportTruePeakNotClipping: a source over only
// between samples clamps nothing, so the count stays zero and TruePeak is
// the field that reports it. A repro specified in dBTP alone cannot say
// which field answers it; the stored samples decide.
func TestIntersampleOversReportTruePeakNotClipping(t *testing.T) {
	const frames = 44100
	src := testutil.FloatWAVBytes(t, clipRate, testutil.IntersampleHotChans(clipRate, frames))

	e := waxflow.New()
	an, err := e.Analyze(context.Background(), container.BytesSource(src), "wav", waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if an.TruePeakDB < 1 {
		t.Fatalf("fixture measures %+.2f dBTP, want well over 0; it is not over between samples",
			an.TruePeakDB)
	}
	if an.SamplePeakDB >= 0 {
		t.Fatalf("fixture's stored peak is %+.2f dBFS, want under 0; a stored over would fire the count instead",
			an.SamplePeakDB)
	}
	var out memWS
	res, err := e.Transcode(context.Background(), container.BytesSource(src), "wav", &out,
		waxflow.TranscodeOptions{Format: "flac"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ClippedSamples != 0 {
		t.Errorf("a %+.2f dBTP source with no sample over reported %d clipped samples, want 0",
			an.TruePeakDB, res.ClippedSamples)
	}
	if res.TruePeak <= 1 {
		t.Errorf("TruePeak = %.4f, want over 1; the between-sample overs survived quantization and must be reported",
			res.TruePeak)
	}
	if !res.Quantized {
		t.Error("Quantized = false on a float-to-FLAC transcode; the true-peak note would never fire")
	}
	if note := res.LevelNote(); !strings.HasPrefix(note, "true peak: ") {
		t.Errorf("LevelNote() = %q, want the true-peak note", note)
	}
}

// TestIntegerCopyStaysSilent: the same hot waveform entering as int16 is a
// copy, not a new generation. No quantizer runs, nothing is metered, and
// no note fires, so a byte-faithful pass over a loud master cannot nag.
func TestIntegerCopyStaysSilent(t *testing.T) {
	src := testutil.PCM16WAVBytes(t, clipRate, testutil.IntersampleHotChans(clipRate, 44100))

	var out memWS
	res, err := waxflow.New().Transcode(context.Background(), container.BytesSource(src), "wav", &out,
		waxflow.TranscodeOptions{Format: "flac"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ClippedSamples != 0 || res.Quantized {
		t.Errorf("clipped %d, quantized %v; an int16 source to 16-bit FLAC must not requantize",
			res.ClippedSamples, res.Quantized)
	}
	if res.TruePeak != 0 {
		t.Errorf("TruePeak = %.4f, want 0; a pure integer pass is not metered", res.TruePeak)
	}
	if note := res.LevelNote(); note != "" {
		t.Errorf("LevelNote() = %q, want none on a copy path", note)
	}
}

// TestLevelNote pins the shared decision every warning surface prints.
func TestLevelNote(t *testing.T) {
	base := waxflow.TranscodeResult{
		Samples: 1000,
		Format:  audio.Format{Rate: 44100, Channels: 2, Type: audio.Int, BitDepth: 16},
	}
	for _, tc := range []struct {
		name string
		mod  func(*waxflow.TranscodeResult)
		want string
	}{
		{"quiet", func(r *waxflow.TranscodeResult) { r.Quantized = true }, ""},
		{"clipped", func(r *waxflow.TranscodeResult) { r.ClippedSamples = 7; r.Quantized = true },
			"clipping: 7 of 2000 output samples exceeded full scale"},
		{"clipped wins over true peak", func(r *waxflow.TranscodeResult) {
			r.ClippedSamples = 7
			r.Quantized = true
			r.TruePeak = 1.5
		}, "clipping: 7 of 2000 output samples exceeded full scale"},
		{"true peak with quantizer", func(r *waxflow.TranscodeResult) { r.Quantized = true; r.TruePeak = 1.2 },
			"true peak: +1.58 dBTP; no stored sample is over, " +
				"but the waveform between samples crosses full scale and playback can clip it"},
		{"true peak without quantizer stays silent", func(r *waxflow.TranscodeResult) { r.TruePeak = 1.5 }, ""},
		{"exactly full scale stays silent", func(r *waxflow.TranscodeResult) { r.Quantized = true; r.TruePeak = 1 }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.mod(&r)
			if got := r.LevelNote(); got != tc.want {
				t.Errorf("LevelNote() = %q, want %q", got, tc.want)
			}
		})
	}
	if got := (*waxflow.TranscodeResult)(nil).LevelNote(); got != "" {
		t.Errorf("nil result: LevelNote() = %q, want empty", got)
	}
}

// TestClippingDropsTruePeakNotLoudness reproduces the finding that
// motivated both fields (the 2026-08-15 WaxTap end-to-end report): +0.09
// dBTP float source to 24-bit FLAC came out at +0.01 dBTP, silently. The
// drop is the signature of real samples hitting the rail (an
// intersample-only source keeps its peak, the test above), so this pins
// the count firing, the peak dropping, and the loudness holding.
func TestClippingDropsTruePeakNotLoudness(t *testing.T) {
	const frames = clipRate * 2
	// +0.09 dBFS, with samples landing near the crests so the stored peak is
	// over the rail rather than only the reconstructed waveform.
	chans := make([][]float32, 2)
	for c := range chans {
		chans[c] = make([]float32, frames)
		for i := range chans[c] {
			chans[c][i] = float32(1.0104 * math.Sin(2*math.Pi*997*float64(i)/clipRate))
		}
	}
	src := testutil.FloatWAVBytes(t, clipRate, chans)

	e := waxflow.New()
	in, err := e.Analyze(context.Background(), container.BytesSource(src), "wav", waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if in.TruePeakDB < 0.05 || in.TruePeakDB > 0.15 {
		t.Fatalf("fixture measures %+.3f dBTP, want about +0.09; it is not the report's case", in.TruePeakDB)
	}

	var out memWS
	res, err := e.Transcode(context.Background(), container.BytesSource(src), "wav", &out,
		waxflow.TranscodeOptions{Format: "flac"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ClippedSamples <= 0 {
		t.Fatalf("the report's own case reported %d clipped samples; it must be caught", res.ClippedSamples)
	}

	// The report's other numbers: the true peak drops to about +0.01 and the
	// loudness is unmoved, the clamp being confined to a few samples. The
	// transcode's own TruePeak must agree with an analysis of the file it
	// wrote exactly: same samples, same interpolator.
	back, err := e.Analyze(context.Background(), container.BytesSource(out.b), "flac", waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := 20 * math.Log10(res.TruePeak); math.Abs(got-back.TruePeakDB) > 1e-6 {
		t.Errorf("the transcode metered %+.6f dBTP, analyzing its output reads %+.6f; they measure the same samples",
			got, back.TruePeakDB)
	}
	if back.TruePeakDB >= in.TruePeakDB {
		t.Errorf("output measures %+.3f dBTP against the source's %+.3f; the clamp must lower it",
			back.TruePeakDB, in.TruePeakDB)
	}
	if d := math.Abs(back.IntegratedLUFS - in.IntegratedLUFS); d > 0.1 {
		t.Errorf("loudness moved %.2f LU across the clamp, want it preserved", d)
	}
}
