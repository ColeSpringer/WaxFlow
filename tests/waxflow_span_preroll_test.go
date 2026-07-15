package waxflow_test

import (
	"context"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/dsp/dither"
	"github.com/colespringer/waxflow/format"
)

// segmentsOfMedia runs the segmented path over an already-opened Media,
// which is what a span is.
func segmentsOfMedia(t *testing.T, e *waxflow.Engine, med format.Media, opts waxflow.TranscodeOptions,
	segSamples int, start int64) []mp4.Segment {
	t.Helper()
	var segs []mp4.Segment
	_, err := e.TranscodeSegmentsMedia(context.Background(), med, opts,
		waxflow.SegmentedOptions{SegmentSamples: segSamples, StartSegment: start},
		func(s mp4.Segment) error {
			segs = append(segs, s)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return segs
}

// TestSpanPrerollMatchesContinuous is the reason Headroomer exists, and it
// is the assertion A11 and A2 both rest on.
//
// A virtual track of a 44.1 kHz rip delivered at 48 kHz is resampled by
// construction, so its first samples come out of a FIR window that has to
// be full of something. Fed from nothing they are a transient, and that
// transient lands at every track boundary of a gapless album, which is
// exactly the artifact these features exist to remove. Fed from the audio
// that really precedes the span, they are the samples a continuous run of
// the whole rip delivers at that offset.
//
// So: segment the whole source, segment a span of it, decode both, and
// require the span's output to equal the continuous run's from the span's
// start onward.
//
// It compares decoded samples rather than segment bytes, and that is not
// squeamishness. The two streams are legitimately different files: the
// span's first FLAC frame is frame 0 of its own stream while the
// continuous run's is frame 47 of its, and a frame number lives in the
// frame header. Identical audio therefore has different bytes, so a byte
// compare here would fail for a reason that is not the one under test.
func TestSpanPrerollMatchesContinuous(t *testing.T) {
	const rate, channels, frames = 44100, 2, 600_000
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(rate, channels, audio.DefaultLayout(channels))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 606)
	raw := wavFrom(t, cfg, whole)

	e := waxflow.New()
	// FLAC at 48 kHz off a 44.1 kHz source: lossless, so decoded samples
	// are the encoder's input exactly, and resampled, so the FIR window is
	// engaged. This is the CUE-rip-to-HLS case in miniature.
	//
	// Dither is switched off, and that is the third thing this comparison
	// has to control for rather than a shortcut. TPDF is keyed by absolute
	// position, deliberately, so a span (whose positions start at 0) and
	// the continuous run (whose are 272000 higher at the same audio) draw
	// different dither and land up to an LSB apart everywhere, priming or
	// no priming. Plain rounding is position-independent, which leaves the
	// FIR window's contents as the only thing that can make these two
	// disagree, which is the thing under test.
	opts := waxflow.TranscodeOptions{Format: "flac", Rate: 48000, Shaping: dither.None}

	const l, m = 160, 147 // 48000/44100, reduced

	// The span's start on the source timeline: far enough in that a full
	// priming window of real audio lies ahead of it, and a multiple of m.
	//
	// The multiple is not a convenience, it is the condition under which
	// the question this test asks is even well posed, and the reason is
	// worth stating because it is not obvious. A resampler anchors its
	// output grid on the position it is handed: the span's sample 0 is its
	// own source sample from, while the continuous run's grid is anchored
	// at the source's sample 0. Those two grids coincide only when
	// from*L/M is an integer, and 250000*160/147 = 272108.84 is not: the
	// span's first output sample would fall strictly between two of the
	// continuous run's, so the two could never agree no matter how well
	// primed. That is inherent to resampling a sub-range, not a defect, and
	// it is what OffsetFor's phase term exists to carry.
	//
	// So the grids are made to coincide, and what is left on the table is
	// exactly the priming: with a cold window the span's first samples are
	// wrong (a window full of zeros is not the signal), with a warm one
	// they are the continuous run's.
	const from int64 = 147 * 1700 // 249900; from*160/147 = 272000 exactly
	outFrom := (from*l + m - 1) / m

	// Assert the premises, because each degenerate case passes for the
	// wrong reason and looks exactly like a pass: a span at the source's
	// start has no headroom and primes nothing, and a span off the output
	// grid cannot match its reference at all.
	if from <= 0 {
		t.Fatal("the span must start mid-stream or this test is vacuous")
	}
	if from%m != 0 || outFrom*m != from*l {
		t.Fatalf("the span starts at %d, which is off the output grid (%d*%d/%d is not an integer); "+
			"the comparison below would be meaningless", from, from, l, m)
	}

	decodeRun := func(med format.Media, plan *waxflow.SegmentPlan, capMax int) *audio.Buffer {
		t.Helper()
		init, err := e.InitSegment(plan, opts)
		if err != nil {
			t.Fatal(err)
		}
		stream := append([]byte{}, init...)
		for _, s := range segmentsOfMedia(t, e, med, opts, plan.SegmentSamples, 0) {
			stream = append(stream, s.Data...)
		}
		out, err := e.OpenStream(container.BytesSource(stream), "mp4")
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		return decodeMedia(t, out, capMax)
	}

	// The continuous run: the whole source, from segment 0, so its
	// resampler is warm everywhere past its own start.
	medWhole, err := e.OpenStream(container.BytesSource(raw), "wav")
	if err != nil {
		t.Fatal(err)
	}
	wholePlan, err := e.PlanSegments(medWhole.Info().Default(), opts, 0)
	if err != nil {
		medWhole.Close()
		t.Fatal(err)
	}
	contPCM := decodeRun(medWhole, wholePlan, int(wholePlan.Samples)+wholePlan.SegmentSamples)
	medWhole.Close()
	defer audio.Put(contPCM)

	// The span: the same audio, addressed as its own stream from `from`.
	medSpan, err := e.OpenStream(container.BytesSource(raw), "wav")
	if err != nil {
		t.Fatal(err)
	}
	sl, err := waxflow.Slice(medSpan, from, waxflow.ToEnd)
	if err != nil {
		medSpan.Close()
		t.Fatal(err)
	}
	if h, ok := sl.(waxflow.Headroomer); !ok || h.Headroom() != from {
		sl.Close()
		t.Fatal("the span reports no usable headroom, so priming cannot engage and this test is vacuous")
	}
	spanPlan, err := e.PlanSegments(sl.Info().Default(), opts, 0)
	if err != nil {
		sl.Close()
		t.Fatal(err)
	}
	spanPCM := decodeRun(sl, spanPlan, int(spanPlan.Samples)+spanPlan.SegmentSamples)
	sl.Close()
	defer audio.Put(spanPCM)

	if spanPCM.N == 0 || contPCM.N == 0 {
		t.Fatalf("decoded nothing: span %d, continuous %d", spanPCM.N, contPCM.N)
	}
	n := min(spanPCM.N, contPCM.N-int(outFrom))
	if n < 8192 {
		t.Fatalf("only %d samples overlap; too few to say anything", n)
	}
	// The head is what the priming decides, so report the first difference
	// with its distance from the span's start: a transient shows up in the
	// first tap-count samples and nowhere else.
	for ch := range channels {
		sp, co := spanPCM.ChanI(ch), contPCM.ChanI(ch)
		for i := range n {
			if sp[i] != co[int(outFrom)+i] {
				t.Fatalf("channel %d: the span's sample %d is %d, the continuous run's sample %d is %d. "+
					"The span's resampler was primed from silence rather than from the audio ahead of its window.",
					ch, i, sp[i], int(outFrom)+i, co[int(outFrom)+i])
			}
		}
	}
}

// TestSpanPrerollSeeksBeforeItsWindow pins the mechanism directly, so a
// failure in the test above can be told apart from a failure here: a span
// must hand back real audio from before its own sample 0, which is what a
// file cannot do and what makes priming possible at all.
func TestSpanPrerollSeeksBeforeItsWindow(t *testing.T) {
	const frames = 40000
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 31)
	raw := wavFrom(t, cfg, whole)

	e := waxflow.New()
	med, err := e.OpenStream(container.BytesSource(raw), "wav")
	if err != nil {
		t.Fatal(err)
	}
	const from int64 = 10_000
	sl, err := waxflow.Slice(med, from, 30_000)
	if err != nil {
		med.Close()
		t.Fatal(err)
	}
	defer sl.Close()

	// Seek 500 samples ahead of the window's start.
	landed, err := sl.SeekSample(-500)
	if err != nil {
		t.Fatalf("a span refused a seek into its own headroom: %v", err)
	}
	if landed != -500 {
		t.Fatalf("seek to -500 landed at %d", landed)
	}
	buf := audio.Get(f, 500)
	defer audio.Put(buf)
	if err := sl.ReadChunk(buf); err != nil {
		t.Fatal(err)
	}
	if buf.Pos != -500 {
		t.Errorf("the pre-roll chunk is stamped %d, want -500: positions ahead of a span are negative", buf.Pos)
	}
	// And the samples are the source's, not silence.
	src := whole.ChanI(0)
	out := buf.ChanI(0)
	for i := range buf.N {
		if out[i] != src[int(from)-500+i] {
			t.Fatalf("pre-roll sample %d = %d, want the source's sample %d (%d)",
				i, out[i], int(from)-500+i, src[int(from)-500+i])
		}
	}

	// Reading on must cross 0 and carry into the window continuously.
	if _, err := sl.SeekSample(-10); err != nil {
		t.Fatal(err)
	}
	big := audio.Get(f, 100)
	defer audio.Put(big)
	if err := sl.ReadChunk(big); err != nil {
		t.Fatal(err)
	}
	if big.Pos != -10 {
		t.Fatalf("chunk stamped %d, want -10", big.Pos)
	}
	if big.N < 20 {
		t.Fatalf("pre-roll read returned %d frames, too few to cross the window's start", big.N)
	}
	for i := range big.N {
		if got, want := big.ChanI(0)[i], src[int(from)-10+i]; got != want {
			t.Fatalf("sample %d across the window's start = %d, want %d", i, got, want)
		}
	}
}

// TestSpanRefusesSeekBeforeTheSource is the honest bound on the headroom: a
// span may reach back into its source, not past it.
func TestSpanRefusesSeekBeforeTheSource(t *testing.T) {
	const frames = 20000
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(48000, 1, audio.DefaultLayout(1))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 8)
	raw := wavFrom(t, cfg, whole)

	e := waxflow.New()
	med, err := e.OpenStream(container.BytesSource(raw), "wav")
	if err != nil {
		t.Fatal(err)
	}
	sl, err := waxflow.Slice(med, 1000, 5000)
	if err != nil {
		med.Close()
		t.Fatal(err)
	}
	defer sl.Close()
	if _, err := sl.SeekSample(-1001); err == nil {
		t.Fatal("a span allowed a seek before its source's own start")
	}
	// The edge is reachable.
	if _, err := sl.SeekSample(-1000); err != nil {
		t.Fatalf("a span refused a seek to its source's start: %v", err)
	}
}

// TestSliceZeroFromHasNoHeadroom: a span starting at the source's start has
// nothing ahead of it, so it behaves exactly like a file and primes exactly
// as a file does.
func TestSliceZeroFromHasNoHeadroom(t *testing.T) {
	const frames = 10000
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(48000, 1, audio.DefaultLayout(1))
	whole := audio.Get(f, frames)
	defer audio.Put(whole)
	whole.N = frames
	synth(whole, 2)
	raw := wavFrom(t, cfg, whole)

	e := waxflow.New()
	med, err := e.OpenStream(container.BytesSource(raw), "wav")
	if err != nil {
		t.Fatal(err)
	}
	sl, err := waxflow.Slice(med, 0, 5000)
	if err != nil {
		med.Close()
		t.Fatal(err)
	}
	defer sl.Close()
	h, ok := sl.(waxflow.Headroomer)
	if !ok {
		t.Fatal("a slice does not implement Headroomer")
	}
	if h.Headroom() != 0 {
		t.Errorf("Headroom() = %d, want 0 for a span at the source's start", h.Headroom())
	}
	if _, err := sl.SeekSample(-1); err == nil {
		t.Error("a span at the source's start allowed a negative seek")
	}
}
