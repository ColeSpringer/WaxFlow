package wv_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/container/wv"
	"github.com/colespringer/waxflow/internal/testutil"
)

// unsure is raw with junk behind it, which leaves the open's length not
// exact, so a measure has to walk.
func unsure(raw []byte) []byte {
	return append(append([]byte(nil), raw...), make([]byte, 64)...)
}

// TestAMeasureOfAnExactLengthReadsNothing: a length the open called exact is
// the measure, at no read.
func TestAMeasureOfAnExactLengthReadsNothing(t *testing.T) {
	src := &testutil.CountingSource{Src: container.BytesSource(fixture(t, "seek-mono.wv"))}
	d, err := wv.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	exactAs(t, d, 40000)
	src.Reset()
	if n, err := d.MeasureSamples(context.Background(), 0); err != nil || n != 40000 {
		t.Fatalf("measured %d (%v), want 40000", n, err)
	}
	if c := costOf(src); c.reads != 0 {
		t.Errorf("the measure took %d reads of %d bytes, want none", c.reads, c.bytes)
	}
}

// TestAMeasureIsAWalkOfTheHeaders: a length the open would not promise is
// measured at a header a block, with the track and the read left alone: where
// the indexes end in a stream that bears its skips out, the old length if not.
func TestAMeasureIsAWalkOfTheHeaders(t *testing.T) {
	mono := fixture(t, "seek-mono.wv")
	for _, tc := range []struct {
		name string
		raw  []byte
		want int64
	}{
		{"a whole stream", unsure(mono), 40000},
		{"a block gone", unsure(without(t, mono, 3)), 40000},
		{"the last block's index raised", unsure(withIndex(t, mono, 19, 38000+7777)), 47777},
		{"a step back", unsure(withIndex(t, mono, 5, 2000)), 40000},
		{"a stray block behind the last", unsure(append(append([]byte(nil), mono...), otherShape(1000000)...)), 40000},
		{"junk past what a read crosses", unsure(pastBound(t)), 1 << 19},
		// Read on in order, the two streams run past any length their indexes
		// give, so the measure is the one the open gave before: the stray block's.
		{"two streams and a stray block behind them", unsure(slices.Concat(mono, mono, otherShape(1000000))), 1003584},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &testutil.CountingSource{Src: container.BytesSource(tc.raw)}
			d, err := wv.NewDemuxer(src, nil)
			if err != nil {
				t.Fatal(err)
			}
			before := d.Tracks()[0]
			if before.SamplesExact {
				t.Fatal("the open called the length exact; the case needs one it did not")
			}
			src.Reset()
			n, err := d.MeasureSamples(context.Background(), 0)
			if err != nil || n != tc.want {
				t.Fatalf("measured %d (%v), want %d", n, err, tc.want)
			}
			// A header a block, the windows a resync scans, and the tail's
			// where the open had none.
			blocks := len(blockStarts(t, tc.raw))
			within(t, "the measure", costOf(src), cost{blocks + 4 + len(tc.raw)/srcwin.Chunk, int64(blocks)*32 + int64(len(tc.raw))})
			if after := d.Tracks()[0]; !slices.Equal(after.CodecConfig, before.CodecConfig) ||
				after.Samples != before.Samples || after.SamplesExact {
				t.Errorf("the measure moved the track from %+v to %+v", before, after)
			}
			if again, err := d.MeasureSamples(context.Background(), 0); err != nil || again != n {
				t.Errorf("a second measure answered %d (%v), the first %d", again, err, n)
			}
			if c := costOf(src); c.reads != 0 {
				t.Errorf("a second measure took %d reads, want none", c.reads)
			}
			got, end := readAll(t, d)
			want, wantEnd := readAll(t, open(t, tc.raw))
			if !slices.EqualFunc(got, want, func(a, b container.Packet) bool {
				return a.PTS == b.PTS && a.Dur == b.Dur && a.Lost == b.Lost && a.Discont == b.Discont
			}) || end != wantEnd {
				t.Errorf("a read behind the measure gave %d packets ending %v, one without %d ending %v", len(got), end, len(want), wantEnd)
			}
		})
	}
}

// TestAMeasureReportsAChangeOfShape: a block of another shape that carries on
// from the stream is the measure's finding, which Strict refuses. The length
// is the one every open once gave, so a seek still reaches the blocks behind.
func TestAMeasureReportsAChangeOfShape(t *testing.T) {
	// No total and a second block off its place: an open that promises no
	// length and warns of nothing.
	mono := withIndex(t, fixture(t, "seek-mono.wv"), 1, 2500)
	starts := blockStarts(t, mono)
	if err := wavpack.SetTotalSamples(mono[:starts[1]], -1); err != nil {
		t.Fatal(err)
	}
	at := starts[10]
	raw := slices.Concat(mono[:at], otherShape(20000), mono[at:])
	const want = "a block of another shape carries on from the stream, which changes shape there"
	d := open(t, raw)
	if n, err := d.MeasureSamples(context.Background(), 0); err != nil || n != 40000 {
		t.Fatalf("measured %d (%v), want 40000", n, err)
	}
	if !slices.ContainsFunc(d.Warnings(), func(w container.Warning) bool {
		return w.Kind == container.Damage && w.Offset == int64(at) && w.Msg == want
	}) {
		t.Errorf("warnings %+v, want damage at %d naming the change", d.Warnings(), at)
	}
	// Where the open's own walk ended at the change, the measure is still
	// the length a seek reaches.
	whole := fixture(t, "seek-mono.wv")
	walked := open(t, unsure(slices.Concat(whole[:at], otherShape(20000), whole[at:])))
	if n, err := walked.MeasureSamples(context.Background(), 0); err != nil || n != 40000 {
		t.Errorf("behind an open that walked, measured %d (%v), want 40000", n, err)
	}
	// A second measure finds nothing new.
	told := d.DamageCount()
	if n, err := d.MeasureSamples(context.Background(), 0); err != nil || n != 40000 || d.DamageCount() != told {
		t.Errorf("a second measure answered %d (%v) and counted %d findings where there were %d", n, err, d.DamageCount(), told)
	}
	strict, err := wv.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strict.MeasureSamples(context.Background(), 0); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("a Strict measure answered %v, want a refusal naming the change", err)
	}
}

// TestACutTailBehindAChangeOfShapeStatesWhatASeekReaches: where the open's
// walk of a cut tail meets a change of shape, it states the measure's length,
// not exact, and counts truncated only what no seek reaches.
func TestACutTailBehindAChangeOfShapeStatesWhatASeekReaches(t *testing.T) {
	whole := fixture(t, "seek-mono.wv")
	starts := blockStarts(t, whole)
	// Cut 150 bytes into the block in front of the last: 36000 samples whole.
	raw := slices.Concat(whole[:starts[10]], otherShape(20000), whole[starts[10]:starts[19]-150])
	d := open(t, raw)
	if tr := d.Tracks()[0]; tr.Samples != 36000 || tr.SamplesExact || d.TruncatedSamples(0) != 4000 {
		t.Errorf("the track says %d samples, exact %v, %d truncated; want 36000, not exact, 4000",
			tr.Samples, tr.SamplesExact, d.TruncatedSamples(0))
	}
	oneDamage(t, d, 0, "the header declares 40000 samples but the blocks end at 36000")
	if n, err := d.MeasureSamples(context.Background(), 0); err != nil || n != 36000 {
		t.Errorf("measured %d (%v), want the 36000 stated", n, err)
	}
}

// TestAMeasureReachesTheRunASeekReaches: in a stream that does not bear its
// skips out every block is read on in order, so the measure is no less than
// where that read leaves the run of blocks at their own indexes, which a seek
// reaches too, where the last block's own end says less or nothing. A block
// behind it read off its own index is one a seek lands on elsewhere.
func TestAMeasureReachesTheRunASeekReaches(t *testing.T) {
	mono := fixture(t, "seek-mono.wv")
	starts := blockStarts(t, mono)
	swapped := slices.Concat(mono[starts[1]:starts[2]], mono[:starts[1]], mono[starts[2]:])
	for _, tc := range []struct {
		name string
		raw  []byte
		want int64
	}{
		{"the last index zeroed", withIndex(t, mono, 19, 0), 38000},
		{"the last index lowered by a block", withIndex(t, mono, 19, 36000), 38000},
		{"the first index past the last block", unsure(withIndex(t, mono, 0, 50000)), 2000},
		// The run ends at the second copy, the last block's end says as much.
		{"the stream twice", unsure(slices.Concat(mono, mono)), 40000},
		// Read on in order the swapped pair runs 2000 past the indexes, where
		// no seek lands: the last block's own end stands.
		{"the first two blocks swapped", unsure(swapped), 38000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if n, err := open(t, tc.raw).MeasureSamples(context.Background(), 0); err != nil || n != tc.want {
				t.Errorf("measured %d (%v), want %d", n, err, tc.want)
			}
		})
	}
}

// TestAMeasureStopsWhenItsContextDoes: a measure under a context that is
// done returns its error and no length, and the next one answers.
func TestAMeasureStopsWhenItsContextDoes(t *testing.T) {
	d := open(t, unsure(fixture(t, "seek-mono.wv")))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n, err := d.MeasureSamples(ctx, 0); !errors.Is(err, context.Canceled) || n >= 0 {
		t.Fatalf("a measure under a done context answered %d (%v)", n, err)
	}
	if n, err := d.MeasureSamples(context.Background(), 0); err != nil || n != 40000 {
		t.Fatalf("the next measure answered %d (%v), want 40000", n, err)
	}
}

// TestAMeasureThatCannotReadIsNoAnswer: a measure a read fails in returns the
// read's error and keeps nothing of it, so the next one answers.
func TestAMeasureThatCannotReadIsNoAnswer(t *testing.T) {
	// Long enough that the open leaves the middle unread.
	raw := unsure(without(t, noiseFile(t, muxFormat, 1<<19), 3))
	starts := blockStarts(t, raw)
	size := int64(len(raw))
	src := &testutil.FailingSource{Src: container.BytesSource(raw), From: size}
	d, err := wv.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	mid := len(starts) / 2
	src.From, src.To = int64(starts[mid]), int64(starts[mid+1])
	if n, err := d.MeasureSamples(context.Background(), 0); err == nil || n >= 0 {
		t.Fatalf("a measure over a failing source answered %d (%v), want its error and no length", n, err)
	}
	src.From, src.To = size, 0
	if n, err := d.MeasureSamples(context.Background(), 0); err != nil || n != 1<<19 {
		t.Fatalf("the next measure answered %d (%v), want %d", n, err, 1<<19)
	}
	pkts, end := readAll(t, d)
	if want, wantEnd := readAll(t, open(t, raw)); end != io.EOF || wantEnd != io.EOF || len(pkts) != len(want) {
		t.Errorf("the read gave %d packets ending %v, want the %d of a read with no measure and EOF", len(pkts), end, len(want))
	}
}

// audioStarts are the offsets of raw's audio blocks, and where each ends.
func audioStarts(t *testing.T, raw []byte) (starts, ends []int) {
	t.Helper()
	return testutil.WavPackAudioBlocks(t, raw)
}

// TestATailPastTheTotalStatesWhatItStatedBefore: where the tail runs past
// the header's total and the block in front of it does not carry on to it,
// the open states the tail's end, not exact, notes the overrun and counts
// nothing truncated.
func TestATailPastTheTotalStatesWhatItStatedBefore(t *testing.T) {
	// reindexed is base with audio block k's index set by f, and the index
	// it had.
	reindexed := func(base string, k int, f func(index, samples int64) int64) (out []byte, was, n int64) {
		out = append([]byte(nil), fixture(t, base)...)
		s, e := audioStarts(t, out)
		k = (k + len(s)) % len(s)
		h, err := wavpack.ParseBlockHeader(out[s[k]:])
		if err != nil {
			t.Fatal(err)
		}
		n = int64(h.BlockSamples)
		binary.LittleEndian.PutUint32(out[s[k]+16:], uint32(f(h.BlockIndex, n)))
		wavpack.UpdateBlockChecksum(out[s[k]:e[k]])
		return out, h.BlockIndex, n
	}
	type input struct {
		name       string
		raw        []byte
		total, end int64 // the header's total, and the tail's end stated before
	}
	var ins []input
	// id-small-total-/lower1@18: a total 10000 short, block 18 one sample low.
	raw, _, _ := reindexed("seek-mono.wv", 18, func(i, _ int64) int64 { return i - 1 })
	if err := wavpack.SetTotalSamples(raw[:blockStarts(t, raw)[1]], 30000); err != nil {
		t.Fatal(err)
	}
	ins = append(ins, input{"lower1@18", raw, 30000, 40000})
	// tagged.wv/raiseN@1: the last of two blocks raised by its own count.
	raw, was, n := reindexed("tagged.wv", 1, func(i, n int64) int64 { return i + n })
	ins = append(ins, input{"raiseN@1", raw, was + n, was + 2*n})
	// sine-s16/last=15434: the last block's index one short of the total.
	raw, was, n = reindexed("sine-s16.wv", -1, func(int64, int64) int64 { return 15434 })
	ins = append(ins, input{"last=15434", raw, was + n, 15434 + n})
	// A tail past what the bytes could hold leaves the total standing.
	if tr := open(t, withIndex(t, fixture(t, "seek-mono.wv"), 19, 1<<30)).Tracks()[0]; tr.Samples != 40000 || tr.SamplesExact {
		t.Errorf("past what the bytes hold the track says %d samples, exact %v, want 40000, not exact", tr.Samples, tr.SamplesExact)
	}
	// lower1@18 with no total states no length.
	bare := append([]byte(nil), ins[0].raw...)
	if err := wavpack.SetTotalSamples(bare[:blockStarts(t, bare)[1]], -1); err != nil {
		t.Fatal(err)
	}
	if tr := open(t, bare).Tracks()[0]; tr.Samples >= 0 {
		t.Errorf("with no total the track says %d samples, exact %v, want none", tr.Samples, tr.SamplesExact)
	}
	for _, in := range ins {
		t.Run(in.name, func(t *testing.T) {
			d := open(t, in.raw)
			if tr := d.Tracks()[0]; tr.Samples != in.end || tr.SamplesExact || d.TruncatedSamples(0) != 0 {
				t.Errorf("the track says %d samples, exact %v, %d truncated; want %d, not exact, 0",
					tr.Samples, tr.SamplesExact, d.TruncatedSamples(0), in.end)
			}
			note := fmt.Sprintf("the header declares %d samples but the blocks run to %d", in.total, in.end)
			if d.DamageCount() != 0 || !slices.ContainsFunc(d.Warnings(), func(w container.Warning) bool { return w.Msg == note }) {
				t.Errorf("warnings %+v, %d findings; want no damage and the note %q", d.Warnings(), d.DamageCount(), note)
			}
		})
	}
}

// TestALengthNotExactIsNoLessThanTheLastBlocksEnd: where the open calls no
// length exact and states one, it states at least the last audio block's
// own end where the bytes could hold it, whatever shape that block is: the
// overrun is noted and nothing counts truncated.
func TestALengthNotExactIsNoLessThanTheLastBlocksEnd(t *testing.T) {
	whole := fixture(t, "seek-mono.wv")
	starts := blockStarts(t, whole)
	end := starts[len(starts)-1]
	blocks, tags := whole[:end], whole[end:]
	bare := slices.Concat(blocks, blocks, otherShape(40000), tags)
	if err := wavpack.SetTotalSamples(bare[:starts[1]], -1); err != nil {
		t.Fatal(err)
	}
	// rnd158 reduced: the last four blocks lowered by a block, so the tail
	// ends short of the total, and a block of another shape behind them.
	low := append([]byte(nil), blocks...)
	for k := 16; k < 20; k++ {
		low = withIndex(t, low, k, uint32(2000*k-2000))
	}
	for _, in := range []struct {
		name string
		raw  []byte
		end  int64
		note string
	}{
		{"twice+foreign40000", slices.Concat(blocks, blocks, otherShape(40000), tags), 43584,
			"the header declares 40000 samples but the blocks run to 43584"},
		{"no total, twice+foreign40000", bare, 43584,
			"the tail ends at 40000 samples but the blocks run to 43584"},
		{"lowrun2000x4@16+foreign43000", slices.Concat(low, otherShape(43000), tags), 46584,
			"the header declares 40000 samples but the blocks run to 46584"},
	} {
		t.Run(in.name, func(t *testing.T) {
			d := open(t, in.raw)
			if tr := d.Tracks()[0]; tr.Samples != in.end || tr.SamplesExact || d.TruncatedSamples(0) != 0 {
				t.Errorf("the track says %d samples, exact %v, %d truncated; want %d, not exact, 0",
					tr.Samples, tr.SamplesExact, d.TruncatedSamples(0), in.end)
			}
			if w := d.Warnings(); len(w) != 1 || w[0].Kind != container.Note || w[0].Msg != in.note {
				t.Errorf("the open warned %+v, want the note %q alone", w, in.note)
			}
		})
	}
}
