package waxflow_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/dsp"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
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

// AnalyzeOptions.Channels folds the measurement to the encode's target
// channel count so a two-pass gain is computed on the audio the encode
// actually meters (a 5.1 measurement applied to a stereo downmix misses the
// target by the surround-weighting error). The tests below pin the fold
// against the real encode chain, the regression direction, the closed-form
// mono->stereo delta, and the no-op / rejection edges.

const (
	analyzeChRate   = 48000
	analyzeChFrames = analyzeChRate * 4 // 4 s: enough gated blocks for a stable integrated
)

// multiSine builds planar float channels, one steady sine per channel at the
// given amplitude and frequency. Distinct per-channel frequencies keep the
// channels decorrelated, so a downmix's summed power (and thus its loudness)
// genuinely differs from the source meter's weighted sum, which is what these
// tests turn on. Amplitudes stay low so the encode's downmix limiter never
// fires and the transcoded file tracks the raw fold.
func multiSine(rate, frames int, amps, freqs []float64) [][]float32 {
	chans := make([][]float32, len(amps))
	for c := range chans {
		chans[c] = make([]float32, frames)
		for i := range chans[c] {
			chans[c][i] = float32(amps[c] * math.Sin(2*math.Pi*freqs[c]*float64(i)/float64(rate)))
		}
	}
	return chans
}

// floatWAVSource writes planar channels as a float WAV and returns the bytes.
// A real WAV decode (not a raw buffer) exercises the demuxer's layout
// resolution, which the fold depends on: a plain multichannel WAV carries no
// explicit mask, so the demuxer's DefaultLayout fallback is what gives the
// meter its surround weights.
func floatWAVSource(t *testing.T, rate int, chans [][]float32) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "src.wav")
	testutil.WriteFloatWAV(t, path, rate, chans)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// fivePointOneFixture is a 5.1 source whose distinguishing energy sits in FC
// (unity in every basis) and the surrounds BL/BR (weighted x1.41 by the
// native meter but only db3-folded into a stereo/mono downmix), so the native
// measurement and the fold diverge by well over the regression margin. LFE
// carries a low tone for realism only: the fold drops it and the native meter
// weighs it zero, so it moves neither number. Channel order is DefaultLayout(6):
// FL, FR, FC, LFE, BL, BR.
func fivePointOneFixture(t *testing.T) []byte {
	return floatWAVSource(t, analyzeChRate, multiSine(analyzeChRate, analyzeChFrames,
		[]float64{0.05, 0.05, 0.15, 0.10, 0.15, 0.15},
		[]float64{311, 349, 440, 55, 587, 622}))
}

// TestAnalyzeChannelsMatchesEncode is the primary oracle: for every reachable
// fold, the mix-only measurement at the target count must match a measurement
// of the file the real encode chain (convert->mix->limiter->dither) produces
// at that count, and the source-basis measurement must be genuinely wrong.
// Both sides share mix.For but run through completely different plumbing, so
// agreement proves the wiring, not the matrix.
func TestAnalyzeChannelsMatchesEncode(t *testing.T) {
	e := waxflow.New()
	analyze := func(src []byte, opts waxflow.AnalyzeOptions) *waxflow.AnalyzeResult {
		t.Helper()
		res, err := e.Analyze(context.Background(), container.BytesSource(src), "wav", opts)
		if err != nil {
			t.Fatalf("analyze: %v", err)
		}
		return res
	}

	five := fivePointOneFixture(t)
	// Decorrelated stereo (distinct L/R frequencies): stereo->mono then reads
	// ~3 LU quieter than the source, the fully-uncorrelated end of the range.
	stereo := floatWAVSource(t, analyzeChRate, multiSine(analyzeChRate, analyzeChFrames,
		[]float64{0.2, 0.2}, []float64{311, 440}))
	mono := floatWAVSource(t, analyzeChRate, multiSine(analyzeChRate, analyzeChFrames,
		[]float64{0.25}, []float64{440}))

	cases := []struct {
		name       string
		src        []byte
		srcCh, dst int
	}{
		{"5.1->2", five, 6, 2},
		{"5.1->1", five, 6, 1},
		{"2->1", stereo, 2, 1},
		{"1->2", mono, 1, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The source must resolve to the layout we think it does, or the
			// fold and the transcode would both agree on the wrong thing and
			// the oracle would pass vacuously.
			native := analyze(tc.src, waxflow.AnalyzeOptions{})
			if native.Format.Channels != tc.srcCh {
				t.Fatalf("source is %d channels, want %d", native.Format.Channels, tc.srcCh)
			}
			if want := audio.DefaultLayout(tc.srcCh); native.Format.Layout != want {
				t.Fatalf("source layout %v, want %v (the demuxer must resolve the mask or the test is vacuous)",
					native.Format.Layout, want)
			}

			// Mix-only measurement at the target count.
			downmix := analyze(tc.src, waxflow.AnalyzeOptions{Channels: tc.dst})
			if downmix.Format.Channels != tc.dst {
				t.Fatalf("downmix measured %d channels, want %d", downmix.Format.Channels, tc.dst)
			}

			// The production oracle: transcode through the real encode chain
			// to a lossless output of the same layout, then measure it. 24-bit
			// keeps quantization off the oracle; GainDB 0 and the low fixture
			// levels keep the (inserted) downmix limiter from firing.
			var out bytes.Buffer
			if _, err := e.Transcode(context.Background(), container.BytesSource(tc.src), "wav", &out,
				waxflow.TranscodeOptions{Format: "wav", Channels: tc.dst, BitDepth: 24, GainDB: 0}); err != nil {
				t.Fatalf("transcode: %v", err)
			}
			encoded := analyze(out.Bytes(), waxflow.AnalyzeOptions{})
			if encoded.Format.Channels != tc.dst {
				t.Fatalf("encoded output is %d channels, want %d", encoded.Format.Channels, tc.dst)
			}

			if d := math.Abs(downmix.IntegratedLUFS - encoded.IntegratedLUFS); d > 0.1 {
				t.Errorf("mix-only %.4f LUFS vs encoded %.4f LUFS differ by %.4f (> 0.1): the fold does not match the encode",
					downmix.IntegratedLUFS, encoded.IntegratedLUFS, d)
			}
			// Regression direction with a concrete margin: the source-basis
			// measurement is genuinely far from what the encode produces.
			if d := math.Abs(native.IntegratedLUFS - encoded.IntegratedLUFS); d <= 0.5 {
				t.Errorf("native %.4f LUFS is only %.4f from encoded %.4f (<= 0.5): the fixture does not exercise the regression",
					native.IntegratedLUFS, d, encoded.IntegratedLUFS)
			}
		})
	}
}

// TestAnalyzeChannelsMonoToStereoAnalytic is the closed-form check: unity
// mono->stereo duplication doubles every block's channel-sum power, a pure
// +10*log10(2) shift of the integrated loudness with the peaks and the spread
// left untouched. Pinning all four outputs fixes the delta as a channel-count
// effect with nothing else drifting.
func TestAnalyzeChannelsMonoToStereoAnalytic(t *testing.T) {
	e := waxflow.New()
	src := floatWAVSource(t, analyzeChRate, multiSine(analyzeChRate, analyzeChFrames,
		[]float64{0.25}, []float64{440}))
	analyze := func(opts waxflow.AnalyzeOptions) *waxflow.AnalyzeResult {
		t.Helper()
		res, err := e.Analyze(context.Background(), container.BytesSource(src), "wav", opts)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	bare := analyze(waxflow.AnalyzeOptions{})
	dup := analyze(waxflow.AnalyzeOptions{Channels: 2})

	const wantShift = 3.0102999566398120 // 10*log10(2)
	if d := dup.IntegratedLUFS - bare.IntegratedLUFS; math.Abs(d-wantShift) > 1e-3 {
		t.Errorf("mono->stereo shifted integrated by %.6f LU, want %.6f", d, wantShift)
	}
	// Both output channels equal the mono source, so the per-channel peaks are
	// unchanged and the uniform level shift leaves the loudness range untouched.
	if dup.TruePeakDB != bare.TruePeakDB {
		t.Errorf("true peak moved under duplication: %.6f vs %.6f", dup.TruePeakDB, bare.TruePeakDB)
	}
	if dup.SamplePeakDB != bare.SamplePeakDB {
		t.Errorf("sample peak moved under duplication: %.6f vs %.6f", dup.SamplePeakDB, bare.SamplePeakDB)
	}
	if dup.LoudnessRange != bare.LoudnessRange {
		t.Errorf("loudness range moved under duplication: %.6f vs %.6f", dup.LoudnessRange, bare.LoudnessRange)
	}
}

// TestAnalyzeChannelsNoopAndValidation pins the no-op paths (Channels 0 and
// Channels equal to the source count return the source measurement verbatim)
// and the clean rejection of an unreachable target (no panic): a 6-channel
// fold from a stereo source has no mono/stereo matrix, and a negative count
// has no layout convention.
func TestAnalyzeChannelsNoopAndValidation(t *testing.T) {
	e := waxflow.New()
	analyze := func(src []byte, opts waxflow.AnalyzeOptions) (*waxflow.AnalyzeResult, error) {
		return e.Analyze(context.Background(), container.BytesSource(src), "wav", opts)
	}
	// A stereo fixture, so Channels 6 enters the fold (6 != 2) and rejects,
	// rather than hitting the no-op path a 6-channel source would.
	stereo := floatWAVSource(t, analyzeChRate, multiSine(analyzeChRate, analyzeChFrames,
		[]float64{0.2, 0.2}, []float64{311, 440}))

	bare, err := analyze(stereo, waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	zero, err := analyze(stereo, waxflow.AnalyzeOptions{Channels: 0})
	if err != nil {
		t.Fatal(err)
	}
	if *zero != *bare {
		t.Errorf("Channels 0 changed the measurement:\n zero: %+v\n bare: %+v", *zero, *bare)
	}
	same, err := analyze(stereo, waxflow.AnalyzeOptions{Channels: 2})
	if err != nil {
		t.Fatal(err)
	}
	if *same != *bare {
		t.Errorf("Channels == source count changed the measurement:\n same: %+v\n bare: %+v", *same, *bare)
	}

	// The rejection code distinguishes a malformed count from a valid-but-
	// unreachable target, and matches what the encode's chain returns for the
	// same TranscodeOptions.Channels, so a two-pass job that hands the same
	// bad value to both passes reports it the same way from either.
	for _, tc := range []struct {
		ch   int
		code waxerr.Code
	}{
		{6, waxerr.CodeUnsupportedFormat}, // a valid 5.1 mask, but no downmix targets it
		{-1, waxerr.CodeInvalidRequest},   // a negative count is malformed, not unsupported
	} {
		res, err := analyze(stereo, waxflow.AnalyzeOptions{Channels: tc.ch})
		if err == nil {
			t.Errorf("Channels %d returned no error for an unreachable fold", tc.ch)
			continue
		}
		if got := waxerr.CodeOf(err); got != tc.code {
			t.Errorf("Channels %d error code = %v, want %v (%v)", tc.ch, got, tc.code, err)
		}
		if res != nil {
			t.Errorf("Channels %d returned a result alongside the error: %+v", tc.ch, res)
		}
	}
}
