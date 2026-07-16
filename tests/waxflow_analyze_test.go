package waxflow_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/dsp"
)

// analyzeFixture is a FLAC-encoded source with a real silent span in it:
// tone, one second of digital silence, tone. The silence is exact (ampWAV
// multiplies its amplitude into the sine, so 0 is 0), which is what lets
// the silence assertions below be about the detector rather than about how
// quiet the fixture happens to be.
//
// FLAC rather than WAV so the decode under test is a real one, and lossless
// so the tapped samples can be compared bit-for-bit against a second decode
// of the same bytes.
const (
	analyzeToneFrames = 3 * 48000 // 3 s, either side
	analyzeGapFrames  = 48000     // 1 s, well past DefaultSilenceMinDuration
	analyzeFrames     = 2*analyzeToneFrames + analyzeGapFrames
)

func analyzeFixture(t *testing.T) []byte {
	t.Helper()
	raw := ampWAV(t, analyzeFrames, func(i int) float64 {
		if i >= analyzeToneFrames && i < analyzeToneFrames+analyzeGapFrames {
			return 0 // exact digital silence
		}
		return 0.4
	})
	var out bytes.Buffer
	e := waxflow.New()
	if _, err := e.Transcode(context.Background(), container.BytesSource(raw), "wav", &out,
		waxflow.TranscodeOptions{Format: "flac"}); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return out.Bytes()
}

// analyzeDecodeFloat decodes src the way Analyze does (the source's own
// rate and layout, float domain, no resample or mix) and returns the
// samples channel by channel. It is the independent reference the tap is
// compared against. The package's own decodeAll is not it: that one reads
// the media directly, in the source's domain, and the tap sees the float
// chain's output.
func analyzeDecodeFloat(t *testing.T, src []byte) [][]float32 {
	t.Helper()
	e := waxflow.New()
	med, err := e.OpenStream(container.BytesSource(src), "flac")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	chain, err := dsp.NewChain(dsp.NewSource(med, med.Info().Default().Fmt), dsp.ChainSpec{Float: true})
	if err != nil {
		t.Fatal(err)
	}
	defer chain.Release()
	f := chain.Format()
	buf := audio.Get(f, audio.StandardChunk)
	defer audio.Put(buf)
	got := make([][]float32, f.Channels)
	for {
		err := chain.ReadChunk(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for c := range got {
			got[c] = append(got[c], buf.ChanF(c)...)
		}
	}
	return got
}

// TestAnalyzeTapSeesEveryFrameOnce is the seam's core claim: the tap rides
// the analysis's own decode, so it must see exactly what a straight decode
// produces, in order and without gaps or repeats. A tap that saw a chunk
// twice, or missed the short final one, would still produce a plausible
// waveform, so this compares sample values and not just a count.
func TestAnalyzeTapSeesEveryFrameOnce(t *testing.T) {
	src := analyzeFixture(t)
	want := analyzeDecodeFloat(t, src)

	var got [][]float32
	e := waxflow.New()
	res, err := e.Analyze(context.Background(), container.BytesSource(src), "flac",
		waxflow.AnalyzeOptions{Tap: func(chans [][]float32) error {
			if got == nil {
				got = make([][]float32, len(chans))
			}
			for c := range chans {
				// The slices are borrowed, so this must copy. append to a
				// nil-backed slice does.
				got[c] = append(got[c], chans[c]...)
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("tap saw %d channels, decode produced %d", len(got), len(want))
	}
	for c := range want {
		if len(got[c]) != len(want[c]) {
			t.Fatalf("channel %d: tap saw %d frames, decode produced %d", c, len(got[c]), len(want[c]))
		}
		for i := range want[c] {
			if got[c][i] != want[c][i] {
				t.Fatalf("channel %d frame %d: tap saw %v, decode produced %v", c, i, got[c][i], want[c][i])
			}
		}
	}
	if int64(len(got[0])) != res.Samples {
		t.Errorf("tap saw %d frames, AnalyzeResult.Samples reports %d", len(got[0]), res.Samples)
	}
}

// TestAnalyzeNilTapIsFree pins the additive claim: a caller that sets no
// tap gets the analysis it got before the field existed. The observable
// form of that is a nil tap and a no-op tap agreeing exactly, since a tap
// that perturbed the measurement would move one and not the other.
func TestAnalyzeNilTapIsFree(t *testing.T) {
	src := analyzeFixture(t)
	e := waxflow.New()
	analyze := func(opts waxflow.AnalyzeOptions) *waxflow.AnalyzeResult {
		t.Helper()
		res, err := e.Analyze(context.Background(), container.BytesSource(src), "flac", opts)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	bare := analyze(waxflow.AnalyzeOptions{})
	tapped := analyze(waxflow.AnalyzeOptions{Tap: func([][]float32) error { return nil }})

	if *bare != *tapped {
		t.Errorf("a no-op tap changed the measurement:\n bare:   %+v\n tapped: %+v", *bare, *tapped)
	}
}

// TestAnalyzeMediaEqualsAnalyze is B2's claim: the open step is the only
// difference between the two entry points.
func TestAnalyzeMediaEqualsAnalyze(t *testing.T) {
	src := analyzeFixture(t)
	e := waxflow.New()

	want, err := e.Analyze(context.Background(), container.BytesSource(src), "flac", waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}

	med, err := e.OpenStream(container.BytesSource(src), "flac")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	got, err := e.AnalyzeMedia(context.Background(), med, waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if *got != *want {
		t.Errorf("AnalyzeMedia disagrees with Analyze:\n got:  %+v\n want: %+v", *got, *want)
	}
}

// TestAnalyzeTapComposesWithSilence drives both analyzers plus the tap over
// one decode: all three must fire on the same chunks and none may perturb
// another. It doubles as the only end-to-end coverage of a Silence analysis
// that actually detects something: the server's silence test runs over
// sine.wav, which has no silent span, so nothing has ever asserted that a
// span was found rather than that the field was populated.
func TestAnalyzeTapComposesWithSilence(t *testing.T) {
	src := analyzeFixture(t)
	e := waxflow.New()

	// Reference measurements, each analyzer alone.
	loudOnly, err := e.Analyze(context.Background(), container.BytesSource(src), "flac", waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	silOnly, err := e.Analyze(context.Background(), container.BytesSource(src), "flac",
		waxflow.AnalyzeOptions{Silence: &waxflow.SilenceOptions{}})
	if err != nil {
		t.Fatal(err)
	}

	// The fixture must have a span to find, or every assertion below passes
	// on a detection that was never there.
	if len(silOnly.Silence.Spans) != 1 {
		t.Fatalf("the fixture's own silence is %d spans, want exactly 1; it is not exercising the detector",
			len(silOnly.Silence.Spans))
	}
	// The span is the gap, give or take the tone's own zero crossings at
	// its edges: 440 Hz at 48 kHz completes exactly 1320 cycles by the gap's
	// start, so the tone's first sample after the gap is itself a zero and
	// extends the silent run by one. Bounding the slop rather than pinning
	// the indices keeps this an assertion about the gap and not about the
	// phase the fixture happens to resume on.
	const edgeSlop = 4
	gapFrom, gapTo := int64(analyzeToneFrames), int64(analyzeToneFrames+analyzeGapFrames)
	if got := silOnly.Silence.Spans[0]; got.From < gapFrom-edgeSlop || got.From > gapFrom+edgeSlop ||
		got.To < gapTo-edgeSlop || got.To > gapTo+edgeSlop {
		t.Fatalf("detected span %+v is not the fixture's gap [%d, %d)", got, gapFrom, gapTo)
	}

	var tapFrames int64
	both, err := e.Analyze(context.Background(), container.BytesSource(src), "flac",
		waxflow.AnalyzeOptions{
			Silence: &waxflow.SilenceOptions{},
			Tap: func(chans [][]float32) error {
				tapFrames += int64(len(chans[0]))
				return nil
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	if tapFrames != both.Samples {
		t.Errorf("tap saw %d frames alongside the detector, analysis measured %d", tapFrames, both.Samples)
	}
	if both.IntegratedLUFS != loudOnly.IntegratedLUFS || both.TruePeakDB != loudOnly.TruePeakDB {
		t.Errorf("the tap or the detector perturbed the meter: %.4f LUFS / %.4f dBTP, want %.4f / %.4f",
			both.IntegratedLUFS, both.TruePeakDB, loudOnly.IntegratedLUFS, loudOnly.TruePeakDB)
	}
	if len(both.Silence.Spans) != len(silOnly.Silence.Spans) || both.Silence.Spans[0] != silOnly.Silence.Spans[0] {
		t.Errorf("the tap perturbed the detector: %+v, want %+v", both.Silence.Spans, silOnly.Silence.Spans)
	}
}

// TestAnalyzeTapSlicesAreBorrowed documents the copy requirement by
// pinning the fact that makes it real: the slices alias one pooled chunk
// buffer, which the next chunk overwrites. A tap that retained them would
// be reading the following chunk's samples, not its own.
func TestAnalyzeTapSlicesAreBorrowed(t *testing.T) {
	src := analyzeFixture(t)
	var backing []*float32
	var chunks int
	e := waxflow.New()
	_, err := e.Analyze(context.Background(), container.BytesSource(src), "flac",
		waxflow.AnalyzeOptions{Tap: func(chans [][]float32) error {
			chunks++
			if backing == nil {
				backing = make([]*float32, len(chans))
				for c := range chans {
					backing[c] = &chans[c][0]
				}
				return nil
			}
			for c := range chans {
				if &chans[c][0] != backing[c] {
					t.Errorf("chunk %d channel %d: backing array moved; the borrow doc says it does not",
						chunks, c)
				}
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	// A single-chunk fixture would pass this vacuously.
	if chunks < 2 {
		t.Fatalf("fixture produced %d chunk(s); the aliasing claim needs at least 2", chunks)
	}
}

// TestAnalyzeTapErrorFailsTheAnalysis is why the seam returns an error at
// all: a tap that fails must fail the analysis, rather than leave the
// caller to stash the error in a closure and remember to check it.
func TestAnalyzeTapErrorFailsTheAnalysis(t *testing.T) {
	src := analyzeFixture(t)
	boom := errors.New("tap failed")
	var calls int
	e := waxflow.New()
	res, err := e.Analyze(context.Background(), container.BytesSource(src), "flac",
		waxflow.AnalyzeOptions{Tap: func([][]float32) error {
			calls++
			return boom
		}})
	if !errors.Is(err, boom) {
		t.Fatalf("Analyze error = %v, want it to wrap %v", err, boom)
	}
	if res != nil {
		t.Errorf("Analyze returned a result alongside the error: %+v", res)
	}
	if calls != 1 {
		t.Errorf("tap called %d times; a failing tap must abort the decode, not run it out", calls)
	}
}
