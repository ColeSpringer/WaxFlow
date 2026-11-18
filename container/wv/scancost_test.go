package wv_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/container/wv"
	"github.com/colespringer/waxflow/internal/testutil"
)

const (
	// maxBlock is the longest block a header can state: SyncOK takes an even
	// ckSize under 1 MiB, and a block is the eight bytes ckSize leaves out too.
	maxBlock = 1<<20 - 2 + 8
	// maxConfirm is the longest read a scan's confirm makes: the largest block
	// and the header behind it.
	maxConfirm = maxBlock + wavpack.BlockHeaderLen
	// The bounds the format sets on one scan: a resync, a bisection probe and
	// the tail scan. The pins hold them as numbers, so lifting one fails them.
	resyncBound = 1 << 20
	probeBound  = 2 << 20
	tailBound   = 4 << 20
)

// blockStarts are the offsets of raw's blocks, by each header's own length,
// and where the last one ends.
func blockStarts(t testing.TB, raw []byte) []int {
	t.Helper()
	return testutil.WavPackBlockStarts(t, raw)
}

// withSize is the block header hdr stating a block of size bytes.
func withSize(hdr []byte, size int) []byte {
	h := append([]byte(nil), hdr[:wavpack.BlockHeaderLen]...)
	binary.LittleEndian.PutUint32(h[4:], uint32(size-8))
	return h
}

// plantedHeaders is n bytes of copies of hdr stating blocks of size bytes,
// end to end behind one junk byte, junk where the last would not fit. Each
// parses and agrees with hdr's stream; at the largest size each lands six
// bytes into another and at an odd offset, so on none behind an even one.
func plantedHeaders(hdr []byte, n, size int) []byte {
	b := bytes.Repeat([]byte{0x11}, n)
	h := withSize(hdr, size)
	for at := 1; at+len(h) <= n; at += len(h) {
		copy(b[at:], h)
	}
	return b
}

// spliced is raw with junk put in at off.
func spliced(raw []byte, off int, junk []byte) []byte {
	return append(append(append([]byte(nil), raw[:off]...), junk...), raw[off:]...)
}

// resyncWarning is what a read that resyncs past n bytes at off reports.
func resyncWarning(off, n int) container.Warning {
	return container.Warning{Offset: int64(off), Kind: container.Damage, Msg: fmt.Sprintf("%d unparsable bytes between blocks", n)}
}

// openAndReadOne opens src and reads the first packet, which resyncs past
// any junk behind the first block.
func openAndReadOne(t testing.TB, src container.Source) *wv.Demuxer {
	t.Helper()
	d, err := wv.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil {
		t.Fatal(err)
	}
	return d
}

// resyncScan is the read's resync over junk behind the first block of a long
// stream.
func resyncScan(t testing.TB) (testutil.JunkScan, int) {
	t.Helper()
	raw := noiseFile(t, muxFormat, 1<<19)
	at := blockStarts(t, raw)[1]
	return testutil.JunkScan{
		Pat:  []byte("wvpk"),
		File: func(junk []byte) []byte { return spliced(raw, at, junk) },
		Scan: func(t testing.TB, src container.Source, n int) {
			t.Helper()
			d := openAndReadOne(t, src)
			if want := resyncWarning(at, n); !slices.Contains(d.Warnings(), want) {
				t.Fatalf("warnings = %v, want %v", d.Warnings(), want)
			}
		},
	}, at
}

// TestJunkScanIsBoundedBySpans pins the scan a read pays for junk between
// blocks: the junk read in spans, whatever it holds.
func TestJunkScanIsBoundedBySpans(t *testing.T) {
	s, _ := resyncScan(t)
	// The open's tail window, the window the first block is read in, and the
	// block behind the junk.
	s.BoundedBySpans(t, 3*srcwin.Chunk)
}

// TestResyncOverPlantedHeadersDoesNotThrashTheWindow: junk of headers that
// parse, each stating the largest block, costs a read of about a block at the
// first and then a read a window: the window extends and never moves.
func TestResyncOverPlantedHeadersDoesNotThrashTheWindow(t *testing.T) {
	const junk = 512 << 10
	s, at := resyncScan(t)
	file := s.File(nil)
	src := &testutil.CountingSource{Src: container.BytesSource(spliced(file, at, plantedHeaders(file[:at], junk, maxBlock)))}
	d, err := wv.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.Reset()
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil {
		t.Fatal(err)
	}
	if want := resyncWarning(at, junk); !slices.Contains(d.Warnings(), want) {
		t.Fatalf("warnings = %v, want %v", d.Warnings(), want)
	}
	if src.Reads > 24 || src.Bytes > 2*junk+1<<20 {
		t.Errorf("resyncing past %d bytes of headers stating %d-byte blocks took %d reads of %d bytes, want at most 24 and %d",
			junk, maxBlock, src.Reads, src.Bytes, 2*junk+1<<20)
	}
}

// TestResyncLeavesTheWindowAtItsRange pins what a resync leaves resident. A
// scan trims nothing, so the window holds what it held in front of the scan,
// the scan's bound and a confirm's read past it, and at most a Chunk more.
func TestResyncLeavesTheWindowAtItsRange(t *testing.T) {
	s, at := resyncScan(t)
	file := s.File(nil)
	for _, tc := range []struct {
		name  string
		junk  int
		found bool
	}{
		{"a block at the last offset the scan takes", wv.MaxResyncForTest - 1, true},
		{"a block two windows past the bound", wv.MaxResyncForTest + 2*srcwin.Chunk, false},
	} {
		d, err := wv.NewDemuxer(container.BytesSource(spliced(file, at, plantedHeaders(file[:at], tc.junk, maxBlock))), nil)
		if err != nil {
			t.Fatal(err)
		}
		// The window the first block is read in, which the scan starts from.
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatal(err)
		}
		if got := slices.Contains(d.Warnings(), resyncWarning(at, tc.junk)); got != tc.found {
			t.Fatalf("%s: warnings %v, want the block behind found %v", tc.name, d.Warnings(), tc.found)
		}
		inFront := int64(at) - d.WindowStartForTest(int64(at))
		held := int64(d.ResidentForTest()) - inFront
		if held < resyncBound-wavpack.BlockHeaderLen {
			t.Fatalf("%s: the window holds %d bytes from the scan's start, fewer than it scanned: the scan moved the window", tc.name, held)
		}
		if most := int64(resyncBound + maxConfirm + srcwin.Chunk); held > most {
			t.Errorf("%s: the window holds %d bytes from the scan's start and %d in front, want at most %d from it: the bound, a confirm and a Chunk",
				tc.name, held, inFront, most)
		}
	}
}

// wideFormat is 32-bit stereo, whose noise blocks are just over half a Chunk.
var wideFormat = audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 32}

// noiseFile is n frames of noise in f, as this package's muxer writes them.
func noiseFile(t testing.TB, f audio.Format, n int) []byte {
	t.Helper()
	track, pkts := noiseBlocksIn(t, f, n)
	var dst seekBuf
	writeAll(t, &dst, track, pkts, nil)
	return dst.Buf
}

// TestALinearReadCostsOneReadPerWindow pins the packet path: a read of many
// windows costs about a read a window and reads no byte twice, whatever the
// block's size, and the window stays bounded.
func TestALinearReadCostsOneReadPerWindow(t *testing.T) {
	for _, f := range []audio.Format{muxFormat, wideFormat} {
		raw := noiseFile(t, f, 1<<20)
		src := &testutil.CountingSource{Src: container.BytesSource(raw)}
		d, err := wv.NewDemuxer(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		src.Reset()
		var pkt container.Packet
		packets, held := 0, 0
		for {
			err := d.ReadPacket(&pkt)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			packets++
			held = max(held, d.ResidentForTest())
		}
		windows := (len(raw) + srcwin.Chunk - 1) / srcwin.Chunk
		if windows < 16 || packets < windows {
			t.Fatalf("%d-bit: %d packets over %d windows, want a long stream of a packet a window or more: this pins nothing", f.BitDepth, packets, windows)
		}
		if src.Reads > windows+2 || src.Bytes > int64(len(raw)) {
			t.Errorf("%d-bit: %d packets over %d windows took %d reads of %d bytes, want about a read a window and the stream's %d bytes once",
				f.BitDepth, packets, windows, src.Reads, src.Bytes, len(raw))
		}
		if held > 3*srcwin.Chunk {
			t.Errorf("%d-bit: the window held %d bytes over %d windows of stream, want at most three windows", f.BitDepth, held, windows)
		}
	}
}

// decoyTail is a stream whose length the header never learned: its first two
// blocks, then n bytes in which an audio header every 4 KiB states the largest
// block, each landing in junk, and junk where such a block would not fit.
func decoyTail(t testing.TB, n int) []byte {
	t.Helper()
	raw := noiseFile(t, muxFormat, 3*wavpack.BlockSamples)
	file := append([]byte(nil), raw[:blockStarts(t, raw)[2]]...)
	binary.LittleEndian.PutUint32(file[12:], 0xffffffff)
	tail := bytes.Repeat([]byte{0x11}, n)
	for at := 1; at+maxBlock < n; at += 4096 {
		copy(tail[at:], withSize(raw, maxBlock))
	}
	return append(file, tail...)
}

// TestTailScanOverPlantedHeadersIsBounded: a tail full of audio headers whose
// blocks fit and tile to nothing is read once, a Chunk at a time, and every
// hop from a header lands in bytes the window already holds.
func TestTailScanOverPlantedHeadersIsBounded(t *testing.T) {
	src := &testutil.CountingSource{Src: container.BytesSource(decoyTail(t, 6<<20))}
	d := openAndReadOne(t, src) // the read moves the window to the head
	if got := d.Tracks()[0].Samples; got != -1 {
		t.Fatalf("the open measured %d samples, want none from a tail of decoys: this pins nothing", got)
	}
	src.Reset()
	if end, ok := d.ScanTailForTest(); ok {
		t.Fatalf("the tail scan took a decoy, ending at %d", end)
	}
	if src.Reads > tailBound/srcwin.Chunk || src.Bytes > tailBound {
		t.Errorf("the tail scan over %d bytes of decoys took %d reads of %d bytes, want the range once in at most %d",
			tailBound, src.Reads, src.Bytes, tailBound/srcwin.Chunk)
	}
}

// TestASkipInFrontOfSampleFreeBlocksLeavesTheWindowAlone: whether a skip is a
// hole is asked of the blocks behind it through a window that is not the
// read's, so a long run of sample-free blocks there leaves the read's alone.
func TestASkipInFrontOfSampleFreeBlocksLeavesTheWindowAlone(t *testing.T) {
	raw := withIndex(t, fixture(t, "seek-mono.wv"), 1, 7000)
	starts := blockStarts(t, raw)
	free := append([]byte(nil), raw[:wavpack.BlockHeaderLen]...)
	binary.LittleEndian.PutUint32(free[4:], wavpack.BlockHeaderLen-8)
	binary.LittleEndian.PutUint32(free[20:], 0)
	raw = spliced(raw, starts[2], bytes.Repeat(free, 200000))
	d := openAndReadOne(t, container.BytesSource(raw)) // its step places block 1
	if n := d.ResidentForTest(); n > 3*srcwin.Chunk {
		t.Errorf("the window holds %d bytes after the first packet, want at most %d", n, 3*srcwin.Chunk)
	}
}

// TestTailScanLeavesTheWindowAtItsRange pins what the open's tail scan leaves
// resident: its range, read in front of the window, and nothing past it.
func TestTailScanLeavesTheWindowAtItsRange(t *testing.T) {
	d, err := wv.NewDemuxer(container.BytesSource(decoyTail(t, 6<<20)), nil)
	if err != nil {
		t.Fatal(err)
	}
	n := d.ResidentForTest()
	if n < wv.MaxTailScanForTest {
		t.Fatalf("the window holds %d bytes after the tail scan, fewer than its range: the scan moved the window", n)
	}
	if most := tailBound + maxBlock + srcwin.Chunk; n > most {
		t.Errorf("the window holds %d bytes after the tail scan, want at most %d: the %d scanned, a block and a Chunk", n, most, tailBound)
	}
}

// bigBlocks is a stream of this tree's first block and then k blocks of size
// bytes, junk behind headers of the same stream, with the total declared: a
// shape the encoder does not write (a block is 8,192 frames at most), for
// the tail scan, which reads headers alone.
func bigBlocks(t testing.TB, size, k int) []byte {
	t.Helper()
	raw := noiseFile(t, muxFormat, wavpack.BlockSamples)
	file := append([]byte(nil), raw[:blockStarts(t, raw)[1]]...)
	index := uint32(wavpack.BlockSamples)
	for range k {
		h := withSize(raw, size)
		binary.LittleEndian.PutUint32(h[16:], index)
		file = append(append(file, h...), bytes.Repeat([]byte{0x11}, size-len(h))...)
		index += wavpack.BlockSamples
	}
	binary.LittleEndian.PutUint32(file[12:], index)
	return file
}

// TestALastBlockOverAChunkCostsTheOpenTwoTailWindows: a stream whose last
// block starts more than a Chunk from its end opens on two windows at its
// tail, and measures its whole length.
func TestALastBlockOverAChunkCostsTheOpenTwoTailWindows(t *testing.T) {
	const size = 200 << 10
	file := bigBlocks(t, size, 30)
	src := &testutil.CountingSource{Src: container.BytesSource(file), Log: true}
	d, err := wv.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.Samples != 31*wavpack.BlockSamples || !tr.SamplesExact || len(d.Warnings()) != 0 {
		t.Fatalf("the open measured %d samples (exact %v) with warnings %v, want %d exact and none",
			tr.Samples, tr.SamplesExact, d.Warnings(), 31*wavpack.BlockSamples)
	}
	// The tail scan's reads: the trailer probes in front of it read a few bytes.
	reads, read := 0, int64(0)
	for _, s := range src.Spans {
		if s[1] > int64(len(file))-tailBound && s[1]-s[0] >= 1<<10 {
			reads++
			read += s[1] - s[0]
		}
	}
	if reads != 2 || read > 2*srcwin.Chunk {
		t.Errorf("the open's tail scan read %d bytes in %d reads (%v), want two windows", read, reads, src.Spans)
	}
}

// TestAProbeLeavesTheWindowAtItsRange pins what one bisection probe leaves
// resident: a probe trims nothing while it scans, so that is its bound and a
// confirm's read past it, and at most a Chunk more.
func TestAProbeLeavesTheWindowAtItsRange(t *testing.T) {
	raw := noiseFile(t, muxFormat, 1<<19)
	at := blockStarts(t, raw)[2]
	file := spliced(raw, at, plantedHeaders(raw, 3<<20, maxBlock))
	d, err := wv.NewDemuxer(container.BytesSource(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	if off, ok := d.ProbeForTest(int64(at), int64(len(file))); ok {
		t.Fatalf("the probe found a block at %d inside the hole", off)
	}
	inFront := int64(at) - d.WindowStartForTest(int64(at))
	held := int64(d.ResidentForTest()) - inFront
	if held < probeBound-wavpack.BlockHeaderLen {
		t.Fatalf("the window holds %d bytes from the probe's start, fewer than it scanned: the probe moved the window", held)
	}
	if most := int64(probeBound + maxConfirm + srcwin.Chunk); held > most {
		t.Errorf("the window holds %d bytes from the probe's start and %d in front, want at most %d from it: the bound, a confirm and a Chunk",
			held, inFront, most)
	}
}

// TestAProbeReadsOneWindowAtItsStart: a probe that starts a little in front
// of a block reads one window from its start, which holds the block and the
// header behind it, in one read.
func TestAProbeReadsOneWindowAtItsStart(t *testing.T) {
	raw := noiseFile(t, muxFormat, 1<<19)
	at := int64(blockStarts(t, raw)[10])
	for _, back := range []int64{1, 100, 1000, 5000} {
		src := &testutil.CountingSource{Src: container.BytesSource(raw), Log: true}
		d, err := wv.NewDemuxer(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		src.Reset()
		if off, ok := d.ProbeForTest(at-back, int64(len(raw))); !ok || off != at {
			t.Fatalf("a probe %d bytes in front of the block at %d found %d (%v)", back, at, off, ok)
		}
		if want := [][2]int64{{at - back, at - back + srcwin.Chunk}}; !slices.Equal(src.Spans, want) {
			t.Errorf("a probe %d bytes in front of a block read %v, want one window from its start, %v", back, src.Spans, want)
		}
	}
}

// TestAResyncConfirmIsOneReadFromTheSpansEnd: a resync whose candidate's
// block runs past the span that offers it confirms it with one read at the
// window's end, which reaches the header behind the block; no read starts at
// the block's end.
func TestAResyncConfirmIsOneReadFromTheSpansEnd(t *testing.T) {
	const junk = 250 << 10
	s, at := resyncScan(t)
	file := s.File(s.Junk(junk, 100))
	c := int64(at + junk) // the block behind the junk
	reach := c + int64(blockStarts(t, file[c:])[1]) + wavpack.BlockHeaderLen
	src := &testutil.CountingSource{Src: container.BytesSource(file), Log: true}
	d, err := wv.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.Reset()
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(d.Warnings(), resyncWarning(at, junk)) {
		t.Fatalf("warnings = %v, want %v", d.Warnings(), resyncWarning(at, junk))
	}
	// The span that offers the block is the first read to hold its view.
	k := slices.IndexFunc(src.Spans, func(r [2]int64) bool { return r[1] >= c+wavpack.BlockHeaderLen })
	if k < 0 || src.Spans[k][1] >= reach {
		t.Fatalf("the reads %v hold the header behind the block at %d in the span that offers it: this pins nothing", src.Spans, c)
	}
	if len(src.Spans) != k+2 || src.Spans[k+1][0] != src.Spans[k][1] || src.Spans[k+1][1] < reach {
		t.Errorf("the resync read %v, want the span holding the block at %d and one read from its end to %d", src.Spans, c, reach)
	}
	if slices.ContainsFunc(src.Spans, func(r [2]int64) bool { return r[0] == reach-wavpack.BlockHeaderLen }) {
		t.Errorf("the resync read %v, with a read at the block's end, %d", src.Spans, reach-wavpack.BlockHeaderLen)
	}
}

// TestAHealthySeekCostsAWindowAProbe pins what a seek on a healthy stream
// reads: the first block's window, a window at each probe, and what the walk
// to the target reads. The figures are those reads on these streams.
func TestAHealthySeekCostsAWindowAProbe(t *testing.T) {
	for _, tc := range []struct {
		f     audio.Format
		n     int
		seeks [3][2]int64 // to 100, the middle and the last sample: reads, bytes
	}{
		{muxFormat, 1 << 19, [3][2]int64{{6, 786432}, {6, 786432}, {5, 641454}}},
		{wideFormat, 1 << 20, [3][2]int64{{8, 1048576}, {13, 1703936}, {8, 1016881}}},
	} {
		raw := noiseFile(t, tc.f, tc.n)
		for i, target := range []int64{100, int64(tc.n) / 2, int64(tc.n) - 1} {
			src := &testutil.CountingSource{Src: container.BytesSource(raw), Log: true}
			d, err := wv.NewDemuxer(src, nil)
			if err != nil {
				t.Fatal(err)
			}
			src.Reset()
			landed, err := d.SeekSample(context.Background(), 0, target)
			if err != nil || landed > target || target-landed >= wavpack.BlockSamples {
				t.Fatalf("%d-bit: a seek to %d landed at %d (%v), want the block holding it", tc.f.BitDepth, target, landed, err)
			}
			if most := tc.seeks[i]; int64(src.Reads) > most[0] || src.Bytes > most[1] {
				t.Errorf("%d-bit: a seek to %d took %d reads of %d bytes, want at most %d of %d; it read %v",
					tc.f.BitDepth, target, src.Reads, src.Bytes, most[0], most[1], src.Spans)
			}
		}
	}
}
