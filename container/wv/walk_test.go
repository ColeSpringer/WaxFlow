package wv_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/container/wv"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// cost is what a source was asked for.
type cost struct {
	reads int
	bytes int64
}

func costOf(src *testutil.CountingSource) cost {
	c := cost{src.Reads, src.Bytes}
	src.Reset()
	return c
}

// within fails unless c is at most most.
func within(t *testing.T, what string, c, most cost) {
	t.Helper()
	if c.reads > most.reads || c.bytes > most.bytes {
		t.Errorf("%s took %d reads of %d bytes, want at most %d of %d", what, c.reads, c.bytes, most.reads, most.bytes)
	}
}

// headers is seek-mono's first block and then n-1 blocks that are a header
// alone, each of 2000 samples and numbered on, with the total stated. skip
// raises the index of block k and of every block behind it.
func headers(t testing.TB, n int, skip map[int]uint32) []byte {
	t.Helper()
	mono := fixture(t, "seek-mono.wv")
	raw := append([]byte(nil), mono[:blockStarts(t, mono)[1]]...)
	total := int64(n) * 2000
	for _, by := range skip {
		total += int64(by)
	}
	if err := wavpack.SetTotalSamples(raw, total); err != nil {
		t.Fatal(err)
	}
	hdr := append([]byte(nil), mono[:wavpack.BlockHeaderLen]...)
	binary.LittleEndian.PutUint32(hdr[4:], wavpack.BlockHeaderLen-8)
	index := uint32(2000)
	for k := 1; k < n; k++ {
		index += skip[k]
		binary.LittleEndian.PutUint32(hdr[16:], index)
		raw = append(raw, hdr...)
		index += 2000
	}
	return raw
}

// TestAHoleIsToldByOneWalkOfTheHeaders: a block gone from a long stream is a
// hole where it lies, said by a walk that reads one header a block, once.
func TestAHoleIsToldByOneWalkOfTheHeaders(t *testing.T) {
	whole := noiseFile(t, muxFormat, 1<<21)
	holed := without(t, whole, 3)
	starts := blockStarts(t, holed)
	blocks := len(starts) - 1
	const block, total = wavpack.BlockSamples, 1 << 21
	// read is a whole read; across is ten seeks to block 1, each with three
	// packets behind it, which cross the skip.
	measure := func(raw []byte) (d *wv.Demuxer, pkts []container.Packet, read, across cost) {
		src := &testutil.CountingSource{Src: container.BytesSource(raw)}
		d, err := wv.NewDemuxer(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		src.Reset()
		held := 0
		for {
			var pkt container.Packet
			err := d.ReadPacket(&pkt)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			held = max(held, d.ResidentForTest())
			pkt.Data = nil
			pkts = append(pkts, pkt)
		}
		if held > 3*srcwin.Chunk {
			t.Errorf("the read's window held %d bytes, want at most three windows", held)
		}
		read = costOf(src)
		var pkt container.Packet
		for range 10 {
			if _, err := d.SeekSample(context.Background(), 0, block+10); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if err := d.ReadPacket(&pkt); err != nil {
					t.Fatal(err)
				}
			}
		}
		return d, pkts, read, costOf(src)
	}
	_, _, read, across := measure(whole)
	d, pkts, holedRead, holedAcross := measure(holed)
	t.Logf("%d bytes whole: a read %+v, ten seeks and reads %+v", len(whole), read, across)
	t.Logf("with block 3 gone: a read %+v, ten seeks and reads %+v", holedRead, holedAcross)
	samples, lost := inOrder(t, pkts)
	if samples != total-block || lost != block || pkts[3].Lost != block {
		t.Fatalf("the read delivered %d samples owing %d, packet 3 %d; want the hole in front of packet 3", samples, lost, pkts[3].Lost)
	}
	oneDamage(t, d, starts[3], "the blocks skip from sample 24576 to 32768: 8192 samples lost")
	exactAs(t, d, total)
	if hops, resynced, gaps, done := d.PathForTest(); hops != blocks-1 || resynced != 0 || gaps != 1 || !done {
		t.Errorf("the walk took %d hops and scanned %d bytes for %d gaps, done %v; want %d hops, none, one gap, done", hops, resynced, gaps, done, blocks-1)
	}
	// One read of a header a block behind the first, and nothing twice.
	hdrs := cost{blocks - 1, int64(blocks-1) * wavpack.BlockHeaderLen}
	within(t, "a read across the hole", holedRead, cost{read.reads + hdrs.reads, read.bytes + hdrs.bytes})
	if holedRead.reads < read.reads+hdrs.reads/2 {
		t.Errorf("a read across the hole took %d reads and one of the whole stream %d: no walk ran, so this pins nothing", holedRead.reads, read.reads)
	}
	within(t, "ten seeks and reads across the hole, after the read", holedAcross, cost{across.reads, int64(across.reads) * srcwin.Chunk})
	if real, silence := realThroughFormat(t, holed); real != total-block || silence != block {
		t.Errorf("a decode delivered %d real samples and %d of silence, want %d and the hole's %d", real, silence, total-block, block)
	}

	// A seek that meets the skip first pays for the walk, and a second does not.
	src := &testutil.CountingSource{Src: container.BytesSource(holed)}
	fresh, err := wv.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.Reset()
	seek := func() cost {
		if _, err := fresh.SeekSample(context.Background(), 0, 4*block+10); err != nil {
			t.Fatal(err)
		}
		return costOf(src)
	}
	first, second := seek(), seek()
	t.Logf("on a fresh open: the first seek across the skip %+v, the second %+v", first, second)
	within(t, "the first seek across the skip", first, cost{second.reads + hdrs.reads, second.bytes + hdrs.bytes})
	if first.reads < second.reads+hdrs.reads/2 {
		t.Errorf("the first seek took %d reads and the second %d: no walk ran, so this pins nothing", first.reads, second.reads)
	}
}

// TestAWalkLeavesTheReadsWindowAlone: the walk reads through a window of its
// own, so the packet whose step met the skip is whole and the read's window
// is what a read with no skip holds.
func TestAWalkLeavesTheReadsWindowAlone(t *testing.T) {
	holed := without(t, noiseFile(t, muxFormat, 1<<21), 3)
	starts := blockStarts(t, holed)
	src := &testutil.CountingSource{Src: container.BytesSource(holed), Log: true}
	d, err := wv.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.Reset()
	var pkt container.Packet
	for k := range 3 {
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(pkt.Data, holed[starts[k]:starts[k+1]]) {
			t.Fatalf("packet %d does not hold its block's bytes", k)
		}
	}
	// The third packet's step met the skip and walked the headers behind it.
	if !slices.ContainsFunc(src.Spans, func(r [2]int64) bool { return r[0] > int64(len(holed))/2 && r[1]-r[0] == wavpack.BlockHeaderLen }) {
		t.Fatalf("the reads %v hold no header of the stream's second half: no walk ran, so this pins nothing", src.Spans)
	}
	if n := d.ResidentForTest(); n > 3*srcwin.Chunk {
		t.Errorf("the read's window holds %d bytes behind the walk, want at most three windows", n)
	}
	if at := d.WindowStartForTest(int64(starts[3])); at > int64(starts[2]) {
		t.Errorf("the read's window starts at %d, past the packet it just delivered at %d", at, starts[2])
	}
}

// TestASkipIsAHoleOnlyInAStreamThatBearsEverySkipOut: a skip is a hole where
// no index steps back from the first block to the last. One step back, in
// front of the skip or behind it, and every skip is read on in order.
func TestASkipIsAHoleOnlyInAStreamThatBearsEverySkipOut(t *testing.T) {
	mono := fixture(t, "seek-mono.wv")
	starts := blockStarts(t, mono)
	again := func(raw []byte, k int) []byte { // block k twice
		s := blockStarts(t, raw)
		return spliced(raw, s[k+1], raw[s[k]:s[k+1]])
	}
	for _, tc := range []struct {
		name          string
		raw           []byte
		samples, lost int64
	}{
		{"blocks 5 and 12 gone", without(t, mono, 5, 12), 36000, 4000},
		{"block 12 gone, block 3 twice in front of it", again(without(t, mono, 12), 3), 40000, 0},
		{"block 5 gone, block 15 twice behind it", again(without(t, mono, 5), 14), 40000, 0},
		{"block 12 gone, block 3's index raised in front of it", withIndex(t, without(t, mono, 12), 3, 9000), 38000, 0},
		{"blocks 2 and 3 raised alike and block 6 gone", withIndex(t, withIndex(t, without(t, mono, 6), 2, 6000), 3, 8000), 38000, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pkts, end := readAll(t, open(t, tc.raw))
			if samples, lost := inOrder(t, pkts); end != io.EOF || samples != tc.samples || lost != tc.lost {
				t.Errorf("the read delivered %d samples owing %d, ending %v; want %d owing %d", samples, lost, end, tc.samples, tc.lost)
			}
			if real, lost := realThroughFormat(t, tc.raw); real != tc.samples || lost != tc.lost {
				t.Errorf("a decode delivered %d real samples and %d of silence, want %d and %d", real, lost, tc.samples, tc.lost)
			}
		})
	}
	_ = starts
}

// TestManyHolesCostOneWalk: every hole of a stream is told by the one walk.
func TestManyHolesCostOneWalk(t *testing.T) {
	whole := noiseFile(t, muxFormat, 1<<21)
	blocks := len(blockStarts(t, whole)) - 1
	var gone []int
	for k := 3; k < blocks-1; k += 8 {
		gone = append(gone, k)
	}
	raw := without(t, whole, gone...)
	src := &testutil.CountingSource{Src: container.BytesSource(raw)}
	d, err := wv.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.Reset()
	pkts, end := readAll(t, d)
	samples, lost := inOrder(t, pkts)
	if want := int64(blocks-len(gone)) * wavpack.BlockSamples; end != io.EOF || samples != want || lost != int64(len(gone))*wavpack.BlockSamples {
		t.Fatalf("read %d samples owing %d, ending %v; want %d owing every hole's", samples, lost, end, want)
	}
	for _, k := range []int{3, 10, 17} {
		if pkts[k].Lost != wavpack.BlockSamples {
			t.Errorf("packet %d owes %d, want the hole in front of it", k, pkts[k].Lost)
		}
	}
	left := blocks - len(gone)
	if most := int64(len(raw)) + int64(left)*wavpack.BlockHeaderLen; src.Bytes > most {
		t.Errorf("a read over %d holes took %d bytes of a %d-byte stream, want at most %d: the stream and its headers once", len(gone), src.Bytes, len(raw), most)
	}
}

// TestTheWalkIsBoundedInHopsAndInGaps: a stream of more blocks than the walk
// hops, or of more gaps than it keeps, has no answer for a skip, which is
// read on in order; one inside both bounds has.
func TestTheWalkIsBoundedInHopsAndInGaps(t *testing.T) {
	gaps := func(n int) map[int]uint32 {
		m := map[int]uint32{}
		for k := 1; k <= n; k++ {
			m[2*k] = 2000
		}
		return m
	}
	for _, tc := range []struct {
		name   string
		blocks int
		skip   map[int]uint32
		holes  bool
		why    string
	}{
		{"as many blocks as the walk hops", wv.MaxHopsForTest + 1, map[int]uint32{5: 2000}, true, ""},
		{"a block more than the walk hops", wv.MaxHopsForTest + 2, map[int]uint32{5: 2000}, false, "which could not be checked"},
		{"as many gaps as are kept", 3 * wv.MaxGapsForTest, gaps(wv.MaxGapsForTest), true, ""},
		{"a gap more than are kept", 3 * wv.MaxGapsForTest, gaps(wv.MaxGapsForTest + 1), false, "which nothing bears out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := headers(t, tc.blocks, tc.skip)
			src := &testutil.CountingSource{Src: container.BytesSource(raw)}
			d, err := wv.NewDemuxer(src, nil)
			if err != nil {
				t.Fatal(err)
			}
			src.Reset()
			pkts, end := readAll(t, d)
			samples, lost := inOrder(t, pkts)
			if end != io.EOF || samples != int64(tc.blocks)*2000 || (lost == int64(len(tc.skip))*2000) != tc.holes || !tc.holes && lost != 0 {
				t.Fatalf("read %d samples owing %d, ending %v; want every block and holes %v", samples, lost, end, tc.holes)
			}
			hops, _, kept, done := d.PathForTest()
			if hops > wv.MaxHopsForTest || kept > wv.MaxGapsForTest || done != (tc.blocks <= wv.MaxHopsForTest+1) {
				t.Errorf("the walk took %d hops and kept %d gaps, done %v", hops, kept, done)
			}
			// The walk's reads are its hops, a header each.
			if most := int64(len(raw)) + int64(hops)*wavpack.BlockHeaderLen; src.Bytes > most {
				t.Errorf("the read took %d bytes, want at most the stream's %d and %d headers", src.Bytes, len(raw), hops)
			}
			if !tc.holes && !strings.Contains(d.Warnings()[0].Msg, tc.why) {
				t.Errorf("the first skip is warned as %q, want it %s", d.Warnings()[0].Msg, tc.why)
			}
		})
	}
}

// TestAFaultInTheWalkIsNoAnswer: a walk that meets a read it cannot make is
// no answer and no error, and the read fails at the fault with every block
// in front delivered. Once the source reads, the walk goes on and answers.
func TestAFaultInTheWalkIsNoAnswer(t *testing.T) {
	raw := without(t, noiseFile(t, muxFormat, 1<<21), 3, 30)
	starts := blockStarts(t, raw)
	for _, from := range []int{40, 120, 250} {
		fault := func() *testutil.FailingSource {
			return &testutil.FailingSource{Src: container.BytesSource(raw), From: int64(starts[from]), To: int64(starts[from] + 40)}
		}
		// The blocks a window can hold in front of the fault are not read.
		least, most := int64(from-6)*wavpack.BlockSamples, int64(from)*wavpack.BlockSamples
		src := fault()
		d, err := wv.NewDemuxer(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		var pkts []container.Packet
		var pkt container.Packet
		var end error
		for end == nil {
			if end = d.ReadPacket(&pkt); end == nil {
				pkts = append(pkts, pkt)
			}
		}
		samples, lost := inOrder(t, pkts)
		if waxerr.CodeOf(end) != waxerr.CodeSourceUnreadable || samples < least || samples > most || lost != 0 {
			t.Errorf("a fault at block %d: the read delivered %d samples owing %d and ended %v; want %d to %d, nothing owed, and the read's error",
				from, samples, lost, end, least, most)
		}
		if ws := d.Warnings(); len(ws) < 2 || !strings.Contains(ws[0].Msg, "which could not be checked; read on in order") {
			t.Errorf("a fault at block %d: warnings %v, want the skips left unchecked", from, ws)
		}
		if src.Failed < 3 {
			t.Errorf("a fault at block %d: %d reads failed, want the walk's at each skip and the read's: this pins nothing", from, src.Failed)
		}
		med, err := format.Open(fault(), "wavpack", nil)
		if err != nil {
			t.Fatal(err)
		}
		n := int64(0)
		for err == nil {
			buf := audio.Get(muxFormat, audio.StandardChunk)
			if err = med.ReadChunk(buf); err == nil {
				n += int64(buf.N)
			}
			audio.Put(buf)
		}
		med.Close()
		if waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable || n < least-4096 || n > most {
			t.Errorf("a fault at block %d: a decode delivered %d samples and ended %v; want about %d to %d and the read's error", from, n, err, least, most)
		}
	}
	// The source reads again behind the first skip: the second is a hole.
	src := &testutil.FailingSource{Src: container.BytesSource(raw), From: int64(starts[250]), To: int64(starts[250] + 40)}
	d, err := wv.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	for k := range 60 {
		if k == 10 {
			src.From, src.To = int64(len(raw)), int64(len(raw))+1
		}
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatal(err)
		}
		// The hole owes what the unchecked skip left the read short of too.
		if k == 29 {
			if pkt.Lost != 2*wavpack.BlockSamples {
				t.Errorf("packet 29 owes %d, want both gaps' %d once the source reads", pkt.Lost, 2*wavpack.BlockSamples)
			}
		} else if pkt.Lost != 0 {
			t.Errorf("packet %d owes %d", k, pkt.Lost)
		}
	}
}

// TestAnOpenWithNoTotalReadsOneHeaderMore pins what an exact length costs a
// stream whose header states no total and whose last two blocks the tail
// window holds: the header behind the first block, in one read of 32 bytes.
func TestAnOpenWithNoTotalReadsOneHeaderMore(t *testing.T) {
	for _, n := range []int{100000, 200000} {
		raw := noiseFile(t, muxFormat, n)
		bare := append([]byte(nil), raw...)
		if err := wavpack.SetTotalSamples(bare[:blockStarts(t, raw)[1]], -1); err != nil {
			t.Fatal(err)
		}
		var costs [2]cost
		for i, file := range [][]byte{raw, bare} {
			src := &testutil.CountingSource{Src: container.BytesSource(file)}
			d, err := wv.NewDemuxer(src, nil)
			if err != nil {
				t.Fatal(err)
			}
			exactAs(t, d, int64(n))
			costs[i] = costOf(src)
		}
		if want := (cost{costs[0].reads + 1, costs[0].bytes + wavpack.BlockHeaderLen}); costs[1] != want {
			t.Errorf("%d samples: an open with no total took %+v and one with a total %+v, want one read of a header more", n, costs[1], costs[0])
		}
	}
}

// TestAnOpenWithNoTotalAndLargeBlocksLooksBackOneBlock pins the same for
// blocks the tail window does not hold two of, with no total or carved: the
// open reads back for the block in front of the tail, at that block's bytes.
func TestAnOpenWithNoTotalAndLargeBlocksLooksBackOneBlock(t *testing.T) {
	for _, size := range []int{100 << 10, 200 << 10, 600 << 10} {
		file := bigBlocks(t, size, 12)
		bare := append([]byte(nil), file...)
		binary.LittleEndian.PutUint32(bare[12:], 0xffffffff)
		// Every index raised alike, and each total with it.
		carved := append([]byte(nil), file...)
		for _, at := range blockStarts(t, file)[:13] {
			binary.LittleEndian.PutUint32(carved[at+16:], binary.LittleEndian.Uint32(carved[at+16:])+100000)
		}
		var costs [3]cost
		for i, raw := range [][]byte{file, bare, carved} {
			src := &testutil.CountingSource{Src: container.BytesSource(raw)}
			d, err := wv.NewDemuxer(src, nil)
			if err != nil {
				t.Fatal(err)
			}
			exactAs(t, d, 13*wavpack.BlockSamples)
			costs[i] = costOf(src)
		}
		t.Logf("%d KiB blocks: with a total %+v, with none %+v, carved %+v", size>>10, costs[0], costs[1], costs[2])
		// The block in front of the tail, in windows, and the header behind
		// the first block.
		most := cost{costs[0].reads + size/srcwin.Chunk + 2, costs[0].bytes + int64(size) + srcwin.Chunk + wavpack.BlockHeaderLen}
		within(t, "an open with no total", costs[1], most)
		within(t, "an open of a carved stream", costs[2], most)
	}
}

// TestAReadThatFailsInALengthCheckIsNotTheOpensError: a read the open makes
// only to check an index, or behind a block of another shape where its tail
// scan once ended, is no answer when it fails, and the open stands.
func TestAReadThatFailsInALengthCheckIsNotTheOpensError(t *testing.T) {
	raw := noiseFile(t, muxFormat, 100000)
	starts := blockStarts(t, raw)
	bare := append([]byte(nil), raw...)
	if err := wavpack.SetTotalSamples(bare[:starts[1]], -1); err != nil {
		t.Fatal(err)
	}
	// Junk and a block of another shape behind the stream: the scan passes
	// the block over and finds no block of the stream's that reaches the end.
	behind := append(append(append([]byte(nil), raw...), bytes.Repeat([]byte{0x77, 0x11}, 50)...), otherShape(0)...)
	for _, tc := range []struct {
		name    string
		raw     []byte
		at      int64
		samples int64
	}{
		{"the header behind the first block", bare, int64(starts[1]), -1},
		{"the tail scan behind a block of another shape", behind, int64(len(raw)) / 3, 100000},
		{"the walk behind a block of another shape", behind, int64(len(raw))/2 + 100, 100000},
	} {
		src := &testutil.FailingSource{Src: container.BytesSource(tc.raw), From: tc.at, To: tc.at + 1}
		d, err := wv.NewDemuxer(src, nil)
		if err != nil || src.Failed == 0 {
			t.Errorf("%s: the open returned %v after %d failed reads, want an open that stands on one", tc.name, err, src.Failed)
			continue
		}
		if tr := d.Tracks()[0]; tr.Samples != tc.samples || tr.SamplesExact || len(d.Warnings()) != 0 {
			t.Errorf("%s: the track says %d samples, exact %v, with warnings %v; want %d, not exact, and none",
				tc.name, tr.Samples, tr.SamplesExact, d.Warnings(), tc.samples)
		}
	}
}

// TestASkipBehindRunsOfJunkLeavesTheWindowAlone: pairs of sample-free blocks,
// each behind junk a resync just crosses, lie behind the block a skip lands
// on. The walk runs out of the bytes its resyncs may scan, in its own window.
func TestASkipBehindRunsOfJunkLeavesTheWindowAlone(t *testing.T) {
	mono := withIndex(t, fixture(t, "seek-mono.wv"), 1, 7000)
	starts := blockStarts(t, mono)
	free := append([]byte(nil), mono[:wavpack.BlockHeaderLen]...)
	binary.LittleEndian.PutUint32(free[4:], wavpack.BlockHeaderLen-8)
	binary.LittleEndian.PutUint32(free[20:], 0)
	var run []byte
	for range 16 {
		run = append(append(append(run, bytes.Repeat([]byte{'x'}, wv.MaxResyncForTest-4096)...), free...), free...)
	}
	src := &testutil.CountingSource{Src: container.BytesSource(spliced(mono, starts[2], run))}
	d := openAndReadOne(t, src)
	if n := d.ResidentForTest(); n > 3*srcwin.Chunk {
		t.Errorf("the window holds %d bytes after the first packet, want at most %d", n, 3*srcwin.Chunk)
	}
	if ws := d.Warnings(); len(ws) != 1 || !strings.Contains(ws[0].Msg, "which could not be checked") {
		t.Errorf("warnings %v, want the skip left unchecked and read on in order", ws)
	}
	if _, resynced, _, done := d.PathForTest(); done || resynced > lookaheadBound+resyncBound {
		t.Errorf("the walk scanned %d bytes across resyncs, done %v; want it out of its %d and not done", resynced, done, lookaheadBound)
	}
}

// TestTheWalksWindowHoldsOneResync: the walk lets go of what lies behind each
// step, so its window holds one run of junk and not every run it crossed.
func TestTheWalksWindowHoldsOneResync(t *testing.T) {
	mono := fixture(t, "seek-mono.wv")
	starts := blockStarts(t, mono)
	const junk = 600 << 10
	raw := mono
	for _, k := range []int{15, 10, 5} {
		raw = spliced(raw, starts[k], bytes.Repeat([]byte{'x'}, junk))
	}
	d := open(t, raw)
	if most := d.WalkResidentForTest(); most > junk+2*srcwin.Chunk {
		t.Errorf("the walk's window held %d bytes, want at most one run of junk and two windows, %d", most, junk+2*srcwin.Chunk)
	}
	if _, resynced, _, done := d.PathForTest(); !done || resynced != 3*junk {
		t.Errorf("the walk scanned %d bytes across resyncs, done %v; want %d and done", resynced, done, 3*junk)
	}
}

// TestAHoleEndsInsideTheLengthADecodeIsCutAt: where the walk ends short of
// the tail, a skip whose blocks run past the exact length is read on in
// order, since silence there would push real audio past the cut.
func TestAHoleEndsInsideTheLengthADecodeIsCutAt(t *testing.T) {
	const block, total, skip = wavpack.BlockSamples, 1 << 19, 200000
	raw := pastBound(t) // a read ends at the junk in front of block 40
	for k := 10; k < 40; k++ {
		raw = withIndex(t, raw, k, uint32(k*block+skip))
	}
	d := open(t, raw)
	exactAs(t, d, total)
	pkts, _ := readAll(t, d)
	if samples, lost := inOrder(t, pkts); samples != 40*block || lost != 0 {
		t.Errorf("the read delivered %d samples owing %d, want %d owing none", samples, lost, 40*block)
	}
	if real, lost := realThroughFormat(t, raw); real != 40*block || lost != 0 {
		t.Errorf("a decode delivered %d real samples and %d of silence, want %d and none", real, lost, 40*block)
	}
}

// lookaheadBound is the bytes a walk's resyncs may scan: the pins hold it as
// a number, so lifting it fails them.
const lookaheadBound = 4 << 20

// TestAHoleIsBoundByWhatTheBytesCouldHold: a skip is a hole up to the samples
// the bytes in front of the block's end could hold as blocks, and not one
// sample past. So no file states more silence than one of its size could hold.
func TestAHoleIsBoundByWhatTheBytesCouldHold(t *testing.T) {
	const n, k = 8, 3
	plain := headers(t, n, nil)
	endOff := blockStarts(t, plain)[k] + wavpack.BlockHeaderLen
	// The skip that puts block k's last sample on the bound.
	most := uint32(wavpack.MaxSamplesIn(int64(endOff)) - 2000*(k+1))
	for _, tc := range []struct {
		name string
		skip uint32
		lost int64
	}{
		{"on the bound", most, int64(most)},
		{"one sample past it", most + 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := headers(t, n, map[int]uint32{k: tc.skip})
			pkts, end := readAll(t, open(t, raw))
			samples, lost := inOrder(t, pkts)
			if end != io.EOF || samples != n*2000 || lost != tc.lost {
				t.Fatalf("the read delivered %d samples owing %d, ending %v; want %d owing %d", samples, lost, end, n*2000, tc.lost)
			}
			if held := wavpack.MaxSamplesIn(int64(len(raw))); samples+lost > held {
				t.Errorf("the read states %d samples, more than the %d a file of %d bytes could hold", samples+lost, held, len(raw))
			}
		})
	}
}
