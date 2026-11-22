package waxflow_test

// A damaged WavPack source through the engine: a copy carries its holes as
// its blocks, MeasureLength asks the demuxer before it seeks, and a span to
// the length an open states is served.

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/wv"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

// wvFixture is a WavPack fixture from container/wv's testdata or the tree's.
func wvFixture(t *testing.T, name string) []byte {
	t.Helper()
	for _, dir := range []string{filepath.Join("..", "container", "wv", "testdata"), filepath.Join("..", "testdata")} {
		if raw, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			return raw
		}
	}
	t.Fatalf("fixture %s not found", name)
	return nil
}

// wvUnsure is raw with junk behind it, which leaves the open's length not
// exact, so a measure has to walk.
func wvUnsure(raw []byte) []byte {
	return append(append([]byte(nil), raw...), make([]byte, 64)...)
}

// wvBare is raw with its header's total cleared.
func wvBare(t *testing.T, raw []byte) []byte {
	t.Helper()
	out := append([]byte(nil), raw...)
	if err := wavpack.SetTotalSamples(out[:testutil.WavPackBlockStarts(t, out)[1]], -1); err != nil {
		t.Fatal(err)
	}
	return out
}

// wvBlocksOf is every packet of raw with its data kept.
func wvBlocksOf(t *testing.T, raw []byte) []container.Packet {
	t.Helper()
	d, err := wv.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkts []container.Packet
	for {
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			return pkts
		}
		pkt.Data = bytes.Clone(pkt.Data)
		pkts = append(pkts, pkt)
	}
}

// wvFramesOf is the first channel of a span of raw on the length its open
// states, nil where the span is refused.
func wvFramesOf(t *testing.T, raw []byte, from, to int64) (got []int32) {
	t.Helper()
	med, err := format.Open(container.BytesSource(raw), "wavpack", nil)
	if err != nil {
		t.Fatal(err)
	}
	sl, err := waxflow.Slice(med, from, to)
	if err != nil {
		med.Close()
		t.Errorf("Slice(%d, %d): %v", from, to, err)
		return nil
	}
	defer sl.Close()
	for {
		buf := audio.Get(sl.Info().Default().Fmt, audio.StandardChunk)
		err := sl.ReadChunk(buf)
		got = append(got, buf.ChanI(0)...)
		audio.Put(buf)
		if err == io.EOF {
			return got
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestWavPackCopyCarriesAHoleAsItsBlocks: a WavPack block states its own
// index, so a copy of a stream with a hole writes the blocks as they are,
// counts the hole in its length as a transcode does, and reports it lost.
func TestWavPackCopyCarriesAHoleAsItsBlocks(t *testing.T) {
	raw := testutil.WavPackWithout(t, wvFixture(t, "seek-mono.wv"), 10)
	d, err := wv.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	track := d.Tracks()[0]
	if waxflow.Cuttable(track) {
		t.Error("a WavPack track is cuttable; a cut copy would have to place its holes")
	}
	dst := &testutil.MemWriteSeeker{}
	res, err := waxflow.New().RemuxDemuxer(context.Background(), d, track, dst, waxflow.TranscodeOptions{Format: "wavpack"})
	if err != nil {
		t.Fatalf("the copy failed: %v", err)
	}
	if res.Samples != 40000 || res.LostSamples != 2000 {
		t.Errorf("the copy reports %d samples and %d lost, want 40000 and 2000", res.Samples, res.LostSamples)
	}
	if len(res.InputWarnings) != 1 || !strings.Contains(res.InputWarnings[0], "2000 samples lost") {
		t.Errorf("the copy's warnings are %+v, want the hole's", res.InputWarnings)
	}
	in, out := wvBlocksOf(t, raw), wvBlocksOf(t, dst.Buf)
	if len(in) != len(out) {
		t.Fatalf("the copy holds %d blocks, its source %d", len(out), len(in))
	}
	// The hole is on the copy's timeline, so its header keeps the length.
	if h, err := wavpack.ParseBlockHeader(dst.Buf); err != nil || h.TotalSamples != 40000 {
		t.Errorf("the copy's header declares %d samples (%v), want the 40000 it reads back", h.TotalSamples, err)
	}
	for i := range in {
		if a, b := in[i], out[i]; a.PTS != b.PTS || a.Dur != b.Dur || a.Lost != b.Lost || !bytes.Equal(a.Data, b.Data) {
			t.Fatalf("block %d of the copy is at %d (%d samples, %d lost, %d bytes), its source's at %d (%d, %d, %d)",
				i, b.PTS, b.Dur, b.Lost, len(b.Data), a.PTS, a.Dur, a.Lost, len(a.Data))
		}
	}
}

// TestWavPackCopyOfAHoleIntoAStreamCompletes: a destination that cannot
// seek keeps the length the header promised, since the copy counts the hole,
// and progress reaches the total.
func TestWavPackCopyOfAHoleIntoAStreamCompletes(t *testing.T) {
	raw := testutil.WavPackWithout(t, wvFixture(t, "seek-mono.wv"), 10)
	d, err := wv.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	var dst bytes.Buffer
	var done, total int64
	res, err := waxflow.New().RemuxDemuxer(context.Background(), d, d.Tracks()[0], &dst,
		waxflow.TranscodeOptions{Format: "wavpack"}, waxflow.WithProgress(func(d, t int64) { done, total = d, t }))
	if err != nil {
		t.Fatalf("the copy failed: %v", err)
	}
	if done != 40000 || total != 40000 {
		t.Errorf("progress ended at %d of %d, want 40000 of 40000", done, total)
	}
	if res.Samples != 40000 || res.LostSamples != 2000 {
		t.Errorf("the copy reports %d samples and %d lost, want 40000 and 2000", res.Samples, res.LostSamples)
	}
	if h, err := wavpack.ParseBlockHeader(dst.Bytes()); err != nil || h.TotalSamples != 40000 {
		t.Errorf("the copy's header declares %d samples (%v), want 40000", h.TotalSamples, err)
	}
}

// TestWavPackMeasureDecodesNothing: the engine measures a length the open
// would not promise from the headers, so a last block that does not decode
// fails no measure, and the span held to the measure delivers every block in
// front.
func TestWavPackMeasureDecodesNothing(t *testing.T) {
	raw := wvUnsure(wvFixture(t, "seek-mono.wv"))
	starts := testutil.WavPackBlockStarts(t, raw)
	raw[starts[19]+60] ^= 0xff // the last block now fails its checksum
	ctx := context.Background()
	med, err := format.Open(container.BytesSource(raw), "wavpack", nil)
	if err != nil {
		t.Fatal(err)
	}
	measured, err := waxflow.MeasureLength(ctx, med)
	if err != nil || measured.Samples != 40000 || !measured.SamplesExact {
		t.Fatalf("measured %d, exact %v (%v), want 40000 exact and no error", measured.Samples, measured.SamplesExact, err)
	}
	sl, err := waxflow.SliceMeasured(ctx, med, measured, 0, measured.Samples)
	if err != nil {
		t.Fatal(err)
	}
	defer sl.Close()
	// The decode fails at the damaged block, with the chunks in front delivered.
	var n int64
	for err == nil {
		buf := audio.Get(measured.Fmt, audio.StandardChunk)
		if err = sl.ReadChunk(buf); err == nil {
			n += int64(buf.N)
		}
		audio.Put(buf)
	}
	if n < 38000-audio.StandardChunk || !strings.Contains(err.Error(), "fails its CRC") {
		t.Errorf("the span delivered %d samples and ended %v, want the chunks in front of sample 38000 and the block's CRC error", n, err)
	}
}

// TestWavPackMeasureOfAStreamReadOnInOrder: a stream read on in order whose
// last block ends in front of its first index is measured from its headers:
// its first block, the one block at its own index.
func TestWavPackMeasureOfAStreamReadOnInOrder(t *testing.T) {
	raw := wvUnsure(testutil.WavPackWithIndex(t, wvFixture(t, "seek-mono.wv"), 0, 50000))
	med, err := format.Open(container.BytesSource(raw), "wavpack", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	if measured, err := waxflow.MeasureLength(context.Background(), med); err != nil || measured.Samples != 2000 {
		t.Errorf("the engine measured %d (%v), want the 2000 of its first block", measured.Samples, err)
	}
}

// wvRealIn is the samples a span of raw from 0 to its measured length
// delivers that are not silence, and how the decode ended.
func wvRealIn(t *testing.T, raw []byte) (real int64, end error) {
	t.Helper()
	ctx := context.Background()
	med, err := format.Open(container.BytesSource(raw), "wavpack", nil)
	if err != nil {
		t.Fatal(err)
	}
	measured, err := waxflow.MeasureLength(ctx, med)
	if err != nil {
		t.Fatalf("the measure failed: %v", err)
	}
	sl, err := waxflow.SliceMeasured(ctx, med, measured, 0, measured.Samples)
	if err != nil {
		t.Fatal(err)
	}
	defer sl.Close()
	for err == nil {
		buf := audio.Get(measured.Fmt, audio.StandardChunk)
		if err = sl.ReadChunk(buf); err == nil || err == io.EOF {
			real += int64(buf.N)
		}
		audio.Put(buf)
	}
	return real - sl.Info().Lost, err
}

// TestWavPackSpanOverTheMeasuredLengthLosesNothing: skips that the stream
// does not bear out are read on in order, so a span from 0 to the measured
// length delivers every sample the blocks hold, whatever the open promised.
func TestWavPackSpanOverTheMeasuredLengthLosesNothing(t *testing.T) {
	mono := wvFixture(t, "seek-mono.wv")
	bare := wvBare(t, mono)
	without, withIndex := testutil.WavPackWithout, testutil.WavPackWithIndex
	again := func(raw []byte, k int) []byte { // block k twice
		s := testutil.WavPackBlockStarts(t, raw)
		return slices.Concat(raw[:s[k+1]], raw[s[k]:s[k+1]], raw[s[k+1]:])
	}
	for _, tc := range []struct {
		name  string
		raw   []byte
		real  int64
		short bool // the exact length states more than the blocks hold
	}{
		{"no total, two blocks raised alike", withIndex(t, withIndex(t, bare, 1, 2000+7777), 2, 4000+7777), 40000, false},
		{"two blocks raised a block and one gone behind them", without(t, withIndex(t, withIndex(t, mono, 2, 6000), 3, 8000), 6), 38000, true},
		{"a cut tail, a block gone and one twice", again(without(t, mono, 1, 18, 19), 5), 36000, false},
		// One bad last index: the length held is where a seek still lands
		// on the blocks' own indexes, not the last block's own end.
		{"the last index zeroed", withIndex(t, mono, 19, 0), 38000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			real, end := wvRealIn(t, tc.raw)
			if short := end != io.EOF && strings.Contains(end.Error(), "the source ended"); real != tc.real || short != tc.short || !short && end != io.EOF {
				t.Errorf("the span delivered %d real samples ending %v, want %d (short of its length: %v)", real, end, tc.real, tc.short)
			}
		})
	}
}

// TestWavPackCutTailBehindAChangeOfShapeServesItsSpans: where the open's
// walk of a cut tail meets a change of shape, the length it states serves
// the spans the engine served before the open read the indexes.
func TestWavPackCutTailBehindAChangeOfShapeServesItsSpans(t *testing.T) {
	whole := wvFixture(t, "seek-mono.wv")
	starts := testutil.WavPackBlockStarts(t, whole)
	// Cut 150 bytes into the block in front of the last: 36000 samples whole.
	raw := slices.Concat(whole[:starts[10]], testutil.WavPackOtherShape(20000), whole[starts[10]:starts[19]-150])
	want := wvFramesOf(t, whole, 0, waxflow.ToEnd)
	for _, w := range []struct{ from, to, n int64 }{{25000, waxflow.ToEnd, 11000}, {30000, 36000, 6000}} {
		if got := wvFramesOf(t, raw, w.from, w.to); int64(len(got)) != w.n || !slices.Equal(got, want[w.from:w.from+w.n]) {
			t.Errorf("Slice(%d, %d) gave %d frames, want the source's %d from %d", w.from, w.to, len(got), w.n, w.from)
		}
	}
}

// TestWavPackTailPastTheTotalServesTheSpansItServed: where the tail runs
// past the header's total and the block in front of it does not carry on to
// it, a span past the total is served as before, with the total and without.
func TestWavPackTailPastTheTotalServesTheSpansItServed(t *testing.T) {
	// reindexed is base with audio block k's index set by f, and the index
	// it had.
	reindexed := func(base string, k int, f func(index, samples int64) int64) (out []byte, was, n int64) {
		out = append([]byte(nil), wvFixture(t, base)...)
		s, e := testutil.WavPackAudioBlocks(t, out)
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
	type span struct{ from, to int64 }
	type input struct {
		name, base string
		raw        []byte
		end        int64 // the tail's end, which the open states
		shift      int64 // how far past its source position a frame sits
		spans      []span
	}
	var ins []input
	// id-small-total-/lower1@18: a total 10000 short, block 18 one sample low.
	raw, _, _ := reindexed("seek-mono.wv", 18, func(i, _ int64) int64 { return i - 1 })
	if err := wavpack.SetTotalSamples(raw[:testutil.WavPackBlockStarts(t, raw)[1]], 30000); err != nil {
		t.Fatal(err)
	}
	ins = append(ins, input{"lower1@18", "seek-mono.wv", raw, 40000, 0, []span{{35000, waxflow.ToEnd}, {30000, 40000}}})
	// tagged.wv/raiseN@1: the last of two blocks raised by its own count.
	raw, was, n := reindexed("tagged.wv", 1, func(i, n int64) int64 { return i + n })
	ins = append(ins, input{"raiseN@1", "tagged.wv", raw, was + 2*n, n, []span{{20000, waxflow.ToEnd}}})
	// sine-s16/last=15434: the last block's index one short of the total.
	raw, was, n = reindexed("sine-s16.wv", -1, func(int64, int64) int64 { return 15434 })
	ins = append(ins, input{"last=15434", "sine-s16.wv", raw, 15434 + n, 15434 - was, []span{{20000, waxflow.ToEnd}}})
	// lower1@18 with no total states no length, and serves the same spans.
	bare := wvBare(t, ins[0].raw)
	want := wvFramesOf(t, wvFixture(t, "seek-mono.wv"), 0, waxflow.ToEnd)
	for _, w := range ins[0].spans {
		to := w.to
		if to == waxflow.ToEnd {
			to = 40000
		}
		if got := wvFramesOf(t, bare, w.from, w.to); !slices.Equal(got, want[w.from:to]) {
			t.Errorf("with no total Slice(%d, %d) gave %d frames, want the source's %d from %d", w.from, w.to, len(got), to-w.from, w.from)
		}
	}
	for _, in := range ins {
		t.Run(in.name, func(t *testing.T) {
			want := wvFramesOf(t, wvFixture(t, in.base), 0, waxflow.ToEnd)
			for _, w := range in.spans {
				to := in.end
				if w.to != waxflow.ToEnd {
					to = w.to
				}
				at := w.from - in.shift
				got := wvFramesOf(t, in.raw, w.from, w.to)
				if int64(len(got)) != to-w.from || !slices.Equal(got, want[at:at+to-w.from]) {
					t.Errorf("Slice(%d, %d) gave %d frames, want the source's %d from %d", w.from, w.to, len(got), to-w.from, at)
				}
			}
		})
	}
}

// TestWavPackLengthNotExactServesSpansToTheLastBlocksEnd: a length the open
// states and does not call exact runs to the last audio block's own end,
// whatever shape that block is, and a span to it is served.
func TestWavPackLengthNotExactServesSpansToTheLastBlocksEnd(t *testing.T) {
	whole := wvFixture(t, "seek-mono.wv")
	starts := testutil.WavPackBlockStarts(t, whole)
	end := starts[len(starts)-1]
	blocks, tags := whole[:end], whole[end:]
	ref := wvFramesOf(t, whole, 0, waxflow.ToEnd)
	foreign := testutil.WavPackOtherShape
	// The last four blocks lowered by a block, so the tail ends short of the
	// total, and a block of another shape behind them.
	low := append([]byte(nil), blocks...)
	for k := 16; k < 20; k++ {
		low = testutil.WavPackWithIndex(t, low, k, uint32(2000*k-2000))
	}
	for _, in := range []struct {
		name  string
		raw   []byte
		spans [][2]int64
		want  []int32
	}{
		{"twice+foreign40000", slices.Concat(blocks, blocks, foreign(40000), tags),
			[][2]int64{{0, 43584}, {0, 41000}}, slices.Concat(ref, ref[:3584])},
		{"no total, twice+foreign40000", wvBare(t, slices.Concat(blocks, blocks, foreign(40000), tags)),
			[][2]int64{{0, 43584}, {0, 41000}}, slices.Concat(ref, ref[:3584])},
		{"lowrun2000x4@16+foreign43000", slices.Concat(low, foreign(43000), tags),
			[][2]int64{{0, 40000}, {30000, 40000}}, ref},
	} {
		t.Run(in.name, func(t *testing.T) {
			for _, s := range in.spans {
				if got := wvFramesOf(t, in.raw, s[0], s[1]); !slices.Equal(got, in.want[s[0]:s[1]]) {
					t.Errorf("Slice(%d, %d) gave %d frames, want the %d the source holds there", s[0], s[1], len(got), s[1]-s[0])
				}
			}
		})
	}
}

// TestWavPackStrayBlockTranscodes: a transcode steps over a block of another
// shape that is not the stream's, and delivers the stream.
func TestWavPackStrayBlockTranscodes(t *testing.T) {
	whole := wvFixture(t, "seek-mono.wv")
	starts := testutil.WavPackBlockStarts(t, whole)
	end := starts[len(starts)-1]
	raw := slices.Concat(whole[:end], testutil.WavPackOtherShape(0), whole[end:])
	var out testutil.MemWriteSeeker
	res, err := waxflow.New().Transcode(context.Background(), container.BytesSource(raw), "wavpack", &out, waxflow.TranscodeOptions{Format: "wav"})
	if err != nil || res.Samples != 40000 {
		t.Fatalf("a transcode delivered %d samples (%v), want 40000", res.Samples, err)
	}
}

// TestWavPackJunkPastTheResyncBoundIsMeasuredWhereSeeksReach: junk longer
// than a resync crosses ends a read from the first block there, and seeks
// still find the blocks behind it. The measure is where the seeks reach, so
// every window from the junk on is served, and a window across the junk
// ends where the read does, as before the demuxer measured.
func TestWavPackJunkPastTheResyncBoundIsMeasuredWhereSeeksReach(t *testing.T) {
	mono := wvFixture(t, "seek-mono.wv")
	s := testutil.WavPackBlockStarts(t, mono)
	end := s[len(s)-1]
	// Past the 1 MiB a resync crosses, with the last 100 bytes cut.
	raw := testutil.JunkOf("junk-probe", mono[:end], s[10], 1<<20+testutil.JunkLen, 'w').Cut(100).Raw
	want := wvFramesOf(t, mono, 0, waxflow.ToEnd)
	ctx := context.Background()
	span := func(from, to int64) (got []int32, end error) {
		med, err := format.Open(container.BytesSource(raw), "wavpack", nil)
		if err != nil {
			t.Fatal(err)
		}
		measured, err := waxflow.MeasureLength(ctx, med)
		if err != nil || measured.Samples != 38000 {
			t.Fatalf("measured %d (%v), want the 38000 seeks reach", measured.Samples, err)
		}
		sl, err := waxflow.SliceMeasured(ctx, med, measured, from, to)
		if err != nil {
			t.Fatal(err)
		}
		defer sl.Close()
		for {
			buf := audio.Get(measured.Fmt, audio.StandardChunk)
			err := sl.ReadChunk(buf)
			got = append(got, buf.ChanI(0)...)
			audio.Put(buf)
			if err != nil {
				return got, err
			}
		}
	}
	for _, w := range [][2]int64{{0, 20000}, {20000, 38000}, {30000, 38000}} {
		if got, end := span(w[0], w[1]); end != io.EOF || !slices.Equal(got, want[w[0]:w[1]]) {
			t.Errorf("[%d, %d) gave %d frames ending %v, want the source's %d", w[0], w[1], len(got), end, w[1]-w[0])
		}
	}
	// Across the junk the read ends at it: the resync's bound.
	if got, end := span(0, 38000); end == io.EOF || !strings.Contains(end.Error(), "the source ended") || !slices.Equal(got, want[:20000]) {
		t.Errorf("[0, 38000) gave %d frames ending %v, want the 20000 in front of the junk and a short source", len(got), end)
	}
}
