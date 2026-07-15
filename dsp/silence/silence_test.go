package silence

import (
	"math"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

const testRate = 48000

// toneAt is a -6 dBFS 440 Hz sine, the fixtures' "loud" material.
func toneAt(rate, i int) float32 {
	return float32(0.5 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
}

// region is a run of frames that is either the tone or true digital zero.
type region struct {
	silent bool
	frames int
}

// buildAt renders a region list into one channel at rate. The cuts between
// regions are hard, never faded: across a fade the boundary is a function
// of the threshold, so two implementations would disagree there for no
// useful reason, and the ffmpeg differential could not assert boundaries at
// all.
func buildAt(rate int, regions []region) []float32 {
	n := 0
	for _, r := range regions {
		n += r.frames
	}
	out := make([]float32, n)
	at := 0
	for _, r := range regions {
		for i := 0; i < r.frames; i++ {
			if !r.silent {
				out[at+i] = toneAt(rate, at+i)
			}
		}
		at += r.frames
	}
	return out
}

func build(regions []region) []float32 { return buildAt(testRate, regions) }

// detect runs a whole signal through a detector in one chunk.
func detect(t *testing.T, chans [][]float32, thresholdDB float64, minDur time.Duration) *Detector {
	t.Helper()
	d, err := New(testRate, len(chans), thresholdDB, minDur)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Process(chans); err != nil {
		t.Fatalf("Process: %v", err)
	}
	d.Flush()
	return d
}

// TestSpansAtHardCuts pins the span boundaries against a signal whose
// silences are exact by construction. The tone dips under the threshold for
// a fraction of a sample at each zero crossing, so a span may open a sample
// or two before the cut; the tolerance is that, and nothing else.
func TestSpansAtHardCuts(t *testing.T) {
	sig := build([]region{
		{false, testRate},     // 1 s tone
		{true, testRate},      // 1 s silence  [48000, 96000)
		{false, testRate},     // 1 s tone
		{true, 2 * testRate},  // 2 s silence  [144000, 192000)
		{false, testRate / 2}, // 0.5 s tone
	})
	d := detect(t, [][]float32{sig}, -50, 500*time.Millisecond)

	spans := d.Spans()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2: %v", len(spans), spans)
	}
	want := []Span{{From: testRate, To: 2 * testRate}, {From: 3 * testRate, To: 5 * testRate}}
	const tol = testRate / 1000 // 1 ms
	for i, w := range want {
		got := spans[i]
		if abs64(got.From-w.From) > tol || abs64(got.To-w.To) > tol {
			t.Errorf("span %d = [%d,%d), want ~[%d,%d) within %d frames", i, got.From, got.To, w.From, w.To, tol)
		}
	}
	if total := d.TotalSamples(); abs64(total-3*testRate) > 2*tol {
		t.Errorf("TotalSamples = %d, want ~%d", total, 3*testRate)
	}
	if d.Samples() != int64(len(sig)) {
		t.Errorf("Samples = %d, want %d", d.Samples(), len(sig))
	}
}

// TestMinDurationDrops pins the rule that decides what is reportable: a
// silence at the minimum is kept, one a sample short is dropped.
func TestMinDurationDrops(t *testing.T) {
	const minDur = 500 * time.Millisecond
	const minLen = testRate / 2

	for _, c := range []struct {
		name  string
		gap   int
		spans int
	}{
		{"one sample short", minLen - 1, 0},
		{"exactly the minimum", minLen, 1},
		{"one sample over", minLen + 1, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The tone regions are long enough that no zero-crossing dip can
			// merge into the gap and change its length.
			sig := build([]region{{false, testRate}, {true, c.gap}, {false, testRate}})
			d := detect(t, [][]float32{sig}, -50, minDur)
			if got := len(d.Spans()); got != c.spans {
				t.Errorf("got %d spans, want %d: %v", got, c.spans, d.Spans())
			}
		})
	}
}

// TestLeadingAndTrailingSilence covers the two spans with only one real
// edge: silence at sample 0, and silence closed by Flush at end of stream.
// silencedetect reports both, and so must this.
func TestLeadingAndTrailingSilence(t *testing.T) {
	sig := build([]region{{true, testRate}, {false, testRate}, {true, testRate}})
	d := detect(t, [][]float32{sig}, -50, 500*time.Millisecond)

	spans := d.Spans()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2: %v", len(spans), spans)
	}
	if spans[0].From != 0 {
		t.Errorf("leading span starts at %d, want 0", spans[0].From)
	}
	if spans[1].To != int64(len(sig)) {
		t.Errorf("trailing span ends at %d, want %d (the stream end)", spans[1].To, len(sig))
	}
}

// TestFlushClosesTrailingSilence pins that the results are incomplete until
// Flush: without it the open trailing run is not reported at all.
func TestFlushClosesTrailingSilence(t *testing.T) {
	sig := build([]region{{false, testRate}, {true, testRate}})
	d, err := New(testRate, 1, -50, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Process([][]float32{sig}); err != nil {
		t.Fatal(err)
	}
	if got := len(d.Spans()); got != 0 {
		t.Fatalf("before Flush: got %d spans, want 0 (the run is still open)", got)
	}
	d.Flush()
	if got := len(d.Spans()); got != 1 {
		t.Fatalf("after Flush: got %d spans, want 1", got)
	}
	d.Flush()
	if got := len(d.Spans()); got != 1 {
		t.Errorf("second Flush changed the result: got %d spans, want 1", got)
	}
	if err := d.Process([][]float32{sig}); err == nil {
		t.Error("Process after Flush: want an error")
	}
}

// TestChunkingInvariance is the house invariant: the spans cannot depend on
// how the caller sliced its input, since a chunk boundary is an artifact of
// the decoder's buffering and nothing else.
func TestChunkingInvariance(t *testing.T) {
	sig := build([]region{
		{false, testRate}, {true, testRate}, {false, testRate / 3}, {true, 2 * testRate}, {false, testRate},
	})
	want := detect(t, [][]float32{sig}, -50, 500*time.Millisecond).Spans()

	for _, chunk := range []int{1, 7, 100, 4096} {
		d, err := New(testRate, 1, -50, 500*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		for off := 0; off < len(sig); off += chunk {
			end := min(off+chunk, len(sig))
			if err := d.Process([][]float32{sig[off:end]}); err != nil {
				t.Fatalf("chunk %d: Process: %v", chunk, err)
			}
		}
		d.Flush()
		if !reflect.DeepEqual(d.Spans(), want) {
			t.Errorf("chunk %d: spans %v, want %v", chunk, d.Spans(), want)
		}
	}
}

// TestSilenceNeedsEveryChannel pins the multichannel rule: the frame's peak
// is the max over channels, so one loud channel keeps the frame loud. This
// is silencedetect's own rule, verified against it in the differential.
func TestSilenceNeedsEveryChannel(t *testing.T) {
	loud := build([]region{{false, 3 * testRate}})
	half := build([]region{{false, testRate}, {true, testRate}, {false, testRate}})

	d := detect(t, [][]float32{loud, half}, -50, 500*time.Millisecond)
	if got := len(d.Spans()); got != 0 {
		t.Errorf("one channel quiet: got %d spans, want 0 (the other channel is loud)", got)
	}

	d = detect(t, [][]float32{half, half}, -50, 500*time.Millisecond)
	if got := len(d.Spans()); got != 1 {
		t.Errorf("both channels quiet: got %d spans, want 1", got)
	}
}

// TestDroppedSamplesIsTheDiagnostic pins the documented failure mode and,
// more to the point, the number that detects it. The bare Dropped count is
// large for healthy audio too (every zero crossing opens a one-sample run),
// so it separates nothing on its own; DroppedSamples against the stream
// length is what says the threshold is wrong for this source.
func TestDroppedSamplesIsTheDiagnostic(t *testing.T) {
	regions := []region{{false, testRate}, {true, testRate}, {false, testRate}, {true, testRate}}
	healthy := build(regions)

	// The same signal with a -48 dBFS noise floor in the pauses, straddling
	// the -50 dBFS threshold: one long silence fragments into many short
	// ones, each of which is then dropped.
	rng := rand.New(rand.NewSource(1))
	frag := build(regions)
	for i := range frag {
		if sec := i / testRate; sec%2 == 1 {
			frag[i] = float32((rng.Float64()*2 - 1) * 0.004)
		}
	}

	hd := detect(t, [][]float32{healthy}, -50, 500*time.Millisecond)
	fd := detect(t, [][]float32{frag}, -50, 500*time.Millisecond)

	if len(hd.Spans()) != 2 {
		t.Fatalf("healthy: got %d spans, want 2", len(hd.Spans()))
	}
	if len(fd.Spans()) != 0 {
		t.Fatalf("fragmented: got %d spans, want 0 (this is the failure being pinned)", len(fd.Spans()))
	}

	// The count alone does not separate them: healthy audio drops hundreds
	// of one-sample runs. If this ever stops being true, the Dropped doc is
	// wrong and should be revisited rather than this test relaxed.
	if hd.Dropped() < 100 {
		t.Errorf("healthy Dropped = %d, expected the zero-crossing dips to make it large; "+
			"the Dropped doc rests on this", hd.Dropped())
	}

	healthyShare := float64(hd.DroppedSamples()) / float64(len(healthy))
	fragShare := float64(fd.DroppedSamples()) / float64(len(frag))
	t.Logf("dropped share of stream: healthy %.3f%%, fragmented %.1f%%", 100*healthyShare, 100*fragShare)
	if healthyShare > 0.01 {
		t.Errorf("healthy dropped share %.3f%% exceeds 1%%; DroppedSamples should be near zero for clean audio", 100*healthyShare)
	}
	if fragShare < 0.10 {
		t.Errorf("fragmented dropped share %.1f%% under 10%%; DroppedSamples should be a sizeable share of the stream", 100*fragShare)
	}
}

// TestAllSilentAndAllLoud covers the degenerate whole-stream cases.
func TestAllSilentAndAllLoud(t *testing.T) {
	silent := make([]float32, 2*testRate)
	d := detect(t, [][]float32{silent}, -50, 500*time.Millisecond)
	if got := d.Spans(); len(got) != 1 || got[0] != (Span{From: 0, To: 2 * testRate}) {
		t.Errorf("all silent: spans %v, want one covering the stream", got)
	}
	if d.TotalSamples() != 2*testRate {
		t.Errorf("all silent: TotalSamples = %d, want %d", d.TotalSamples(), 2*testRate)
	}

	d = detect(t, [][]float32{build([]region{{false, 2 * testRate}})}, -50, 500*time.Millisecond)
	if got := len(d.Spans()); got != 0 {
		t.Errorf("all loud: got %d spans, want 0", got)
	}
	if d.TotalSamples() != 0 {
		t.Errorf("all loud: TotalSamples = %d, want 0", d.TotalSamples())
	}
}

// TestSpanLen pins the exclusive-To convention.
func TestSpanLen(t *testing.T) {
	if got := (Span{From: 100, To: 250}).Len(); got != 150 {
		t.Errorf("Len = %d, want 150", got)
	}
}

// TestNewRejects covers the constructor's bounds, including the NaN case
// the comparison is written to fail.
func TestNewRejects(t *testing.T) {
	for _, c := range []struct {
		name        string
		rate, chans int
		thresholdDB float64
		minDur      time.Duration
	}{
		{"zero rate", 0, 1, -50, time.Second},
		{"negative rate", -1, 1, -50, time.Second},
		{"zero channels", testRate, 0, -50, time.Second},
		{"negative channels", testRate, -2, -50, time.Second},
		{"threshold at full scale", testRate, 1, 0, time.Second},
		{"positive threshold", testRate, 1, 6, time.Second},
		{"NaN threshold", testRate, 1, math.NaN(), time.Second},
		{"-Inf threshold", testRate, 1, math.Inf(-1), time.Second},
		{"zero duration", testRate, 1, -50, 0},
		{"negative duration", testRate, 1, -50, -time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := New(c.rate, c.chans, c.thresholdDB, c.minDur); err == nil {
				t.Error("want an error")
			}
		})
	}
}

// TestProcessRejectsMismatchedChunks covers the Process contract.
func TestProcessRejectsMismatchedChunks(t *testing.T) {
	d, err := New(testRate, 2, -50, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Process([][]float32{make([]float32, 10)}); err == nil {
		t.Error("wrong channel count: want an error")
	}
	if err := d.Process([][]float32{make([]float32, 10), make([]float32, 9)}); err == nil {
		t.Error("ragged channel slices: want an error")
	}
}

// TestMinFrames pins the frame bound's arithmetic, which has to be exact in
// integers rather than convenient in floats.
func TestMinFrames(t *testing.T) {
	for _, c := range []struct {
		name string
		d    time.Duration
		rate int
		want int64
	}{
		{"whole frames", 500 * time.Millisecond, 48000, 24000},
		{"whole seconds", 2 * time.Second, 44100, 88200},
		// The float path computes 30869 here: 0.7 is a hair under 0.7 in
		// float64, so the product truncates one frame short and the bound
		// comes out looser than asked for.
		{"exact but unrepresentable in float64", 700 * time.Millisecond, 44100, 30870},
		// 14685.3 frames: 14685 is 332.99 ms and must not qualify.
		{"fractional rounds up", 333 * time.Millisecond, 44100, 14686},
		// Rounding up already floors at one frame, so a sub-frame minimum
		// needs no special case: zero would report every zero-crossing dip.
		{"sub-frame duration", time.Nanosecond, 48000, 1},
		{"one frame exactly", time.Second / 48000, 48000, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := minFrames(c.d, c.rate); got != c.want {
				t.Errorf("minFrames(%v, %d) = %d, want %d", c.d, c.rate, got, c.want)
			}
		})
	}

	// A duration near the type's ceiling must not overflow the multiply into
	// a negative bound, which would report the whole stream as one span.
	if got := minFrames(time.Duration(math.MaxInt64), 768000); got <= 0 {
		t.Errorf("minFrames at the maximum Duration = %d; the multiply overflowed", got)
	}

	// The constructor must carry it through.
	d, err := New(48000, 1, -50, 700*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if d.minLen != minFrames(700*time.Millisecond, 48000) {
		t.Errorf("New stored minLen %d, want %d", d.minLen, minFrames(700*time.Millisecond, 48000))
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
