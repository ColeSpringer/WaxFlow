package waxflow_test

// The samples a file's header declares past the end of what it holds are
// counted (format.Info.Truncated), where a probe can see them and where only
// a read can, through every Media that wraps the read and onto the engine's
// results.

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestTruncatedMP3IsCountedOnceItsReadEnds: an MP3's frames are walked as
// they are read, so the count its metadata frame declares is only held
// against the run when a read reaches the end. Until then Truncated is 0,
// and a tolerant probe, which reads headers, reports 0 too.
func TestTruncatedMP3IsCountedOnceItsReadEnds(t *testing.T) {
	full, err := os.ReadFile(repoPath("testdata", "sine-vbr.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	cut := full[:len(full)*6/10]
	info, err := format.Probe(context.Background(), container.BytesSource(cut), "mp3", nil)
	if err != nil {
		t.Fatal(err)
	}
	declared := info.Default()
	if declared.Samples <= 0 {
		t.Fatalf("the fixture declares %d samples; this cell needs a metadata frame's count", declared.Samples)
	}
	if info.Truncated != 0 {
		t.Fatalf("a probe that read only headers counts %d samples truncated", info.Truncated)
	}
	med, err := format.Open(container.BytesSource(cut), "mp3", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	if got := med.Info().Truncated; got != 0 {
		t.Fatalf("%d samples truncated before any read", got)
	}
	delivered := drain(t, med)
	got := med.Info().Truncated
	// On the played timeline: what was read and what is counted make the
	// length the metadata frame declared.
	if want := declared.Samples - delivered; got != want || got <= 0 {
		t.Fatalf("%d samples truncated after a read of %d, want %d: the declared %d less the read",
			got, delivered, want, declared.Samples)
	}
	walked, err := format.Open(container.BytesSource(cut), "mp3", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer walked.Close()
	if err := format.WalkMedia(context.Background(), walked); err != nil {
		t.Fatal(err)
	}
	if w := walked.Info().Truncated; w != got {
		t.Fatalf("a walk counts %d samples truncated, a read %d", w, got)
	}
}

// cutShortFLAC is a native FLAC cut off after its first keep frames, and
// how many samples its STREAMINFO still declares past them.
func cutShortFLAC(t *testing.T, seconds int, keep func(frames int) int) (raw []byte, truncated int64) {
	t.Helper()
	raw = transcodeBytes(t, loopedWAV(t, seconds), waxflow.TranscodeOptions{Format: "flac"})
	frames := testutil.FLACFrames(t, raw)
	last := frames[len(frames)-1]
	k := keep(len(frames))
	return raw[:frames[k].Off], last.Pos + last.Dur - frames[k].Pos
}

// keepSixTenths keeps the first six tenths of n frames.
func keepSixTenths(n int) int { return n * 6 / 10 }

// TestSliceInheritsTruncationWhereItReachesTheEnd: what a file lost off its
// end belongs to a window that runs to the source's end, however that end was
// spelled, and is past any window that stops short of it. A cut answers the
// same way (TestCutReachingTheEndForwardsTruncation), so the count does not
// depend on which rung served the span.
func TestSliceInheritsTruncationWhereItReachesTheEnd(t *testing.T) {
	raw, truncated := cutShortFLAC(t, 10, keepSixTenths)
	held := probeTrack(t, raw, "flac").Samples
	for _, c := range []struct {
		name string
		to   int64
		want int64
	}{
		{"open-ended", waxflow.ToEnd, truncated},
		{"bounded at the source's end", held, truncated},
		{"bounded short of it", held - 1, 0},
		{"bounded well inside", 50000, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			med, err := format.Open(container.BytesSource(raw), "flac", nil)
			if err != nil {
				t.Fatal(err)
			}
			sl, err := waxflow.Slice(med, 1000, c.to)
			if err != nil {
				med.Close()
				t.Fatal(err)
			}
			defer sl.Close()
			drain(t, sl)
			if got := sl.Info().Truncated; got != c.want {
				t.Fatalf("Truncated is %d, want %d (the source's is %d)", got, c.want, med.Info().Truncated)
			}
		})
	}
}

// TestConcatSumsItsMembersTruncation: a timeline's Truncated is its
// members', each on the envelope's timeline.
func TestConcatSumsItsMembersTruncation(t *testing.T) {
	e := waxflow.New()
	a, ta := cutShortFLAC(t, 10, keepSixTenths)
	b, tb := cutShortFLAC(t, 10, func(n int) int { return n / 2 })
	am := flacMember(t, e, a)
	rate := am.Track.Fmt.Rate
	fast, _ := ratedWAV(t, 2*rate+rate/3, am.Track.Fmt.Channels, 5000, 9)
	for _, c := range []struct {
		name string
		ms   []waxflow.ConcatSource
		in   int64
	}{
		{"at the members' rate", []waxflow.ConcatSource{am, flacMember(t, e, b)}, ta + tb},
		{"resampled", append([]waxflow.ConcatSource{am}, timelineMembers(t, e, fast)...), ta},
	} {
		t.Run(c.name, func(t *testing.T) {
			med, err := waxflow.Concat(c.ms, waxflow.ConcatOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			env := med.Info().Default().Fmt.Rate
			drain(t, med)
			want := (c.in*int64(env) + int64(rate)/2) / int64(rate)
			if got := med.Info().Truncated; got != want {
				t.Fatalf("Truncated is %d, want %d: the members' %d samples at %d Hz, on the envelope's %d Hz",
					got, want, c.in, rate, env)
			}
		})
	}
}

// TestResultsReportTruncatedSamples: an analysis, a transcode and a copy each
// say how much their source declared past its end, at the rate of the result
// that carries it.
func TestResultsReportTruncatedSamples(t *testing.T) {
	raw, truncated := cutShortFLAC(t, 10, keepSixTenths)
	rate := probeTrack(t, raw, "flac").Fmt.Rate
	e := waxflow.New()
	ctx := context.Background()
	src := container.BytesSource(raw)

	res, err := e.Analyze(ctx, src, "flac", waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.TruncatedSamples != truncated {
		t.Errorf("Analyze: TruncatedSamples is %d, want %d", res.TruncatedSamples, truncated)
	}
	group, err := e.AnalyzeGroup(ctx, []waxflow.GroupMember{
		{Open: func() (format.Media, error) { return e.OpenStream(src, "flac") }},
		{Open: func() (format.Media, error) { return e.OpenStream(src, "flac") }},
	}, waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := group.Members[1].TruncatedSamples; got != truncated {
		t.Errorf("AnalyzeGroup: a member's TruncatedSamples is %d, want %d", got, truncated)
	}
	if got := group.Group.TruncatedSamples; got != 2*truncated {
		t.Errorf("AnalyzeGroup: the group's TruncatedSamples is %d, want its members' %d", got, 2*truncated)
	}
	for _, out := range []int{0, 48000} {
		var dst bytes.Buffer
		tres, err := e.Transcode(ctx, src, "flac", &dst, waxflow.TranscodeOptions{Format: "wav", Rate: out})
		if err != nil {
			t.Fatal(err)
		}
		want := truncated
		if out != 0 {
			want = (truncated*int64(out) + int64(rate)/2) / int64(rate)
		}
		if tres.TruncatedSamples != want {
			t.Errorf("Transcode to %d Hz: TruncatedSamples is %d, want %d", tres.Format.Rate, tres.TruncatedSamples, want)
		}
	}
	var dst bytes.Buffer
	cres, err := e.Remux(ctx, src, "flac", &dst, waxflow.TranscodeOptions{Format: "flac"})
	if err != nil {
		t.Fatal(err)
	}
	if cres.TruncatedSamples != truncated {
		t.Errorf("Remux: TruncatedSamples is %d, want %d", cres.TruncatedSamples, truncated)
	}
}

// TestLossNote: the one line the two counts warrant, worded once for every
// surface that reports a result, and nothing for a source that lost nothing.
// Both counts are the result's, at its rate, so neither is worded as a number
// of the source's own samples: a transcode that resamples reports a different
// count than a probe of the same file does.
func TestLossNote(t *testing.T) {
	f := audio.Format{Rate: 48000, Channels: 2, Type: audio.Int, BitDepth: 16}
	for _, tc := range []struct {
		name            string
		lost, truncated int64
		rate            int
		want            string
	}{
		{"whole", 0, 0, 48000, ""},
		{"a hole", 4800, 0, 48000, "4800 samples (0.10 s) are silence where damage took the audio"},
		{"a cut tail", 0, 96000, 48000, "the audio ends 96000 samples (2.00 s) short of the length its source declares"},
		{"both", 4800, 96000, 48000, "4800 samples (0.10 s) are silence where damage took the audio; " +
			"the audio ends 96000 samples (2.00 s) short of the length its source declares"},
		{"one sample", 1, 1, 48000, "1 sample (0.00 s) is silence where damage took the audio; " +
			"the audio ends 1 sample (0.00 s) short of the length its source declares"},
		// A group's result has no one rate to tell the time by.
		{"no rate", 4800, 96000, 0, "4800 samples are silence where damage took the audio; " +
			"the audio ends 96000 samples short of the length its source declares"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := f
			f.Rate = tc.rate
			tr := &waxflow.TranscodeResult{Format: f, LostSamples: tc.lost, TruncatedSamples: tc.truncated}
			if got := tr.LossNote(); got != tc.want {
				t.Errorf("TranscodeResult.LossNote() = %q, want %q", got, tc.want)
			}
			ar := &waxflow.AnalyzeResult{Format: f, LostSamples: tc.lost, TruncatedSamples: tc.truncated}
			if got := ar.LossNote(); got != tc.want {
				t.Errorf("AnalyzeResult.LossNote() = %q, want %q", got, tc.want)
			}
		})
	}
	if got := (*waxflow.TranscodeResult)(nil).LossNote(); got != "" {
		t.Errorf("a nil TranscodeResult notes %q", got)
	}
	if got := (*waxflow.AnalyzeResult)(nil).LossNote(); got != "" {
		t.Errorf("a nil AnalyzeResult notes %q", got)
	}
}
