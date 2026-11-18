package ogg

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// resyncHorizon is the last candidate a resync takes, past where it starts:
// maxResync is checked once a 64 KiB step, and steps overlap by three bytes.
const resyncHorizon = 1_114_060

// TestResyncHorizon pins where a resync gives up: a page at the horizon is
// found and one a byte past it is not, and running out is damage past
// maxResync bytes from the start and the end of the file short of it.
func TestResyncHorizon(t *testing.T) {
	const giveUp = "ogg: no page capture pattern within 1048576 bytes"
	pg := buildPage(0, 960, 7, 3, []byte("a page behind the junk"))
	for _, base := range []int64{0, 1, 777} {
		for _, tc := range []struct {
			junk  int64
			found bool
		}{{resyncHorizon - 1, true}, {resyncHorizon, true}, {resyncHorizon + 1, false}, {resyncHorizon + 2, false}} {
			d := scanDemuxer(container.BytesSource(append(make([]byte, base+tc.junk), pg...)))
			var p page
			at, ok, err := d.nextPageAt(base, &p)
			switch {
			case tc.found && (err != nil || !ok || at != base+tc.junk || p.size != int64(len(pg))):
				t.Errorf("from %d, a page %d bytes on: at %d, %v, %v; want it found", base, tc.junk, at, ok, err)
			case !tc.found && (ok || waxerr.CodeOf(err) != waxerr.CodeMalformedInput || err.Error() != giveUp):
				t.Errorf("from %d, a page %d bytes on: at %d, %v, %v; want %q", base, tc.junk, at, ok, err, giveUp)
			}
		}
		// No page at all: the scan runs into the end of the data.
		for _, tc := range []struct {
			left   int64
			damage bool
		}{{0, false}, {3, false}, {4, false}, {1_048_579, false}, {1_048_580, true}, {resyncHorizon + 4, true}, {2 << 20, true}} {
			d := scanDemuxer(container.BytesSource(make([]byte, base+tc.left)))
			var p page
			_, ok, err := d.nextPageAt(base, &p)
			switch {
			case ok:
				t.Errorf("from %d, %d bytes of zeros to the end: found a page", base, tc.left)
			case tc.damage && (waxerr.CodeOf(err) != waxerr.CodeMalformedInput || err.Error() != giveUp):
				t.Errorf("from %d, %d bytes of zeros to the end: %v, want %q", base, tc.left, err, giveUp)
			case !tc.damage && err != nil:
				t.Errorf("from %d, %d bytes of zeros to the end: %v, want the end of the file", base, tc.left, err)
			}
		}
	}
}

// costStream is a long healthy stream the cost pins read.
type costStream struct {
	name string
	raw  []byte
	end  int64 // the stream's length in samples
}

// costStreams are Ogg FLAC of 40-byte pages and Opus of 831-byte and 6 KB
// pages, each over 200 KiB.
func costStreams(t *testing.T) []costStream {
	t.Helper()
	small, _, smallEnd := buildSyntheticOpus(312, 4, 2000, 200)
	large, _, largeEnd := buildSyntheticOpus(312, 50, 400, 120)
	return []costStream{
		{"Ogg FLAC of 40-byte pages", synthOggFLAC(t, 6000), 6000 * 4096},
		{"Opus of 831-byte pages", small, smallEnd - 312},
		{"Opus of 6 KB pages", large, largeEnd - 312},
	}
}

// plantedHeads is n zero bytes holding count page heads, evenly apart, each
// of version 0 with 255 segments of 255 and a checksum that fails.
func plantedHeads(n, count int) []byte {
	b := make([]byte, n)
	for i := range count {
		at := i * (n / count)
		copy(b[at:], "OggS")
		binary.LittleEndian.PutUint32(b[at+22:], 0xDEADBEEF^uint32(i))
		b[at+26] = 255
		copy(b[at+headerLen:], bytes.Repeat([]byte{255}, 255))
	}
	return b
}

// readAfterOpen opens src, then reads it to the end and returns what the read
// alone cost, the packets as text, and the warnings.
func readAfterOpen(t *testing.T, raw []byte) (src *testutil.CountingSource, d *Demuxer, packets []string) {
	t.Helper()
	src = &testutil.CountingSource{Src: container.BytesSource(raw), Log: true}
	d, err := NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.Reset()
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return src, d, packets
		}
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, fmt.Sprintf("%d+%d %08x", pkt.PTS, pkt.Dur, crc32(0, pkt.Data)))
	}
}

// TestResyncValidatesCandidatesFromTheScanWindow: a resync checks each
// candidate from the bytes it holds, so 512 KiB of capture patterns, and 500
// heads that parse and fail their checksum, cost the reads of 512 KiB of
// zeros within four, and land on the same page with the same single warning.
func TestResyncValidatesCandidatesFromTheScanWindow(t *testing.T) {
	const junk = 512 << 10
	raw, _, _ := buildSyntheticOpus(312, 4, 400, 512)
	at := pageStarts(t, raw)[100]
	want := []container.Warning{{Offset: int64(at), Kind: container.Damage, Msg: "damaged page, resyncing"}}
	zeros, zd, zp := readAfterOpen(t, testutil.Splice(raw, at, make([]byte, junk)))
	if !slices.Equal(zd.Warnings(), want) {
		t.Fatalf("zeros: warnings %v, want %v", zd.Warnings(), want)
	}
	for _, tc := range []struct {
		name string
		junk []byte
	}{
		{"512 KiB of capture patterns", bytes.Repeat([]byte("OggS"), junk/4)},
		{"500 planted heads in 512 KiB", plantedHeads(junk, 500)},
	} {
		src, d, packets := readAfterOpen(t, testutil.Splice(raw, at, tc.junk))
		if !slices.Equal(packets, zp) {
			t.Errorf("%s: %d packets, want the %d a read past zeros delivers", tc.name, len(packets), len(zp))
		}
		if !slices.Equal(d.Warnings(), want) {
			t.Errorf("%s: warnings %v, want %v", tc.name, d.Warnings(), want)
		}
		if src.Reads > zeros.Reads+4 || src.Bytes > zeros.Bytes+maxPageSize {
			t.Errorf("%s: %d reads of %d bytes, want at most the %d reads of %d bytes zeros cost, plus four and a page",
				tc.name, src.Reads, src.Bytes, zeros.Reads, zeros.Bytes)
		}
	}
}

// parsingCandidates counts the capture patterns in file[from:to] whose page
// structure fits the file: each one a resync takes a checksum of.
func parsingCandidates(file []byte, from, to int) int64 {
	var c int64
	for i := from; i < to; i++ {
		if i+headerLen > len(file) || string(file[i:i+4]) != "OggS" || file[i+4] != 0 {
			continue
		}
		end := i + headerLen + int(file[i+26])
		if end > len(file) {
			continue
		}
		for _, l := range file[i+headerLen : end] {
			end += int(l)
		}
		if end <= len(file) {
			c++
		}
	}
	return c
}

// TestResyncChecksumWorkIsBoundedByTheScan: the checksum a resync takes is
// the junk's bytes once, the damaged page's own sum and a page past the
// junk's end, and two partial strides and the stored field a candidate that
// parses, on heads and on the densest decoys the header parse takes.
func TestResyncChecksumWorkIsBoundedByTheScan(t *testing.T) {
	raw, _, _ := buildSyntheticOpus(312, 4, 400, 512)
	at := pageStarts(t, raw)[100]
	summed := func(file []byte) int64 {
		_, d, _ := readAfterOpen(t, file)
		return d.SummedForTest()
	}
	healthy := summed(raw)
	fives := make([]byte, 1<<20)
	for i := 0; i+5 <= len(fives); i += 5 {
		copy(fives[i:], "OggS\x00")
	}
	for _, tc := range []struct {
		name string
		junk []byte
	}{
		{"500 planted heads in 512 KiB", plantedHeads(512<<10, 500)},
		{"1 MiB of heads end to end", plantedHeads(1<<20, (1<<20)/(headerLen+255))},
		{"1 MiB of capture patterns five bytes apart", fives},
	} {
		file := testutil.Splice(raw, at, tc.junk)
		n := int64(len(tc.junk))
		c := parsingCandidates(file, at, at+len(tc.junk)+1) // and the page behind
		most := n + 2*maxPageSize + c*(2*(sumStride-1)+4)
		if got := summed(file) - healthy; got > most {
			t.Errorf("%s: the resync summed %d bytes over %d of junk and %d candidates that parse, want at most %d",
				tc.name, got, n, c, most)
		}
	}
}

// TestALinearReadCostsOneReadPerStep pins the packet path: a read reads each
// byte of a long stream once, a scan step at a time once its read-ahead has
// doubled up from a KiB, whatever the size of its pages.
func TestALinearReadCostsOneReadPerStep(t *testing.T) {
	for _, s := range costStreams(t) {
		src, _, _ := readAfterOpen(t, s.raw)
		n := int64(len(s.raw)) - src.Low
		if most := int(n/scanStep) + 7; src.Reads > most || src.Bytes > n {
			t.Errorf("%s: reading %d bytes took %d reads of %d bytes, want at most %d and each byte once",
				s.name, n, src.Reads, src.Bytes, most)
		}
	}
}

// TestTheWindowIsBounded pins what the read window holds, a scan step and a
// page at most, after a long read and after a resync to its horizon.
func TestTheWindowIsBounded(t *testing.T) {
	const most = scanStep + maxPageSize
	for _, s := range costStreams(t) {
		d, err := NewDemuxer(container.BytesSource(s.raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		var pkt container.Packet
		held := d.HeldForTest()
		for d.ReadPacket(&pkt) == nil {
			held = max(held, d.HeldForTest())
		}
		if held > most {
			t.Errorf("%s: the window held %d bytes over a read, want at most %d", s.name, held, most)
		}
	}
	pg := buildPage(0, 960, 7, 3, []byte("a page behind the junk"))
	for _, junk := range []int64{resyncHorizon, resyncHorizon + 1} {
		d := scanDemuxer(container.BytesSource(append(plantedHeads(int(junk), int(junk)/4096), pg...)))
		var p page
		d.nextPageAt(0, &p)
		if held := d.HeldForTest(); held > most || held < scanStep {
			t.Errorf("a resync over %d bytes of heads left the window holding %d bytes, want a step to %d", junk, held, most)
		}
	}
}

// TestTheOpenCostsAReadPerStep pins what an open reads: the head in a span,
// and the tail window a step and the spans doubling behind it, whatever the
// size of the pages.
func TestTheOpenCostsAReadPerStep(t *testing.T) {
	for _, s := range costStreams(t) {
		src := &testutil.CountingSource{Src: container.BytesSource(s.raw), Log: true}
		if _, err := NewDemuxer(src, nil); err != nil {
			t.Fatal(err)
		}
		if src.Reads > 8 {
			t.Errorf("%s: the open took %d reads, want at most 8; it read %v", s.name, src.Reads, src.Spans)
		}
	}
}

// TestAHealthySeekCostsAStepAProbe pins what a seek on a healthy stream reads:
// a step at each bisection probe the window does not hold, and the walk to
// the target in spans. The figures are those reads on these streams.
func TestAHealthySeekCostsAStepAProbe(t *testing.T) {
	// To 100, the middle and the last sample: reads, bytes.
	costs := [][3][2]int64{
		{{1, 1024}, {7, 130048}, {0, 0}},
		{{5, 263168}, {9, 325632}, {9, 300483}},
		{{7, 333757}, {8, 344318}, {8, 338107}},
	}
	for i, s := range costStreams(t) {
		for j, target := range []int64{100, s.end / 2, s.end - 1} {
			src := &testutil.CountingSource{Src: container.BytesSource(s.raw), Log: true}
			d, err := NewDemuxer(src, nil)
			if err != nil {
				t.Fatal(err)
			}
			src.Reset()
			if _, err := d.SeekSample(context.Background(), 0, target); err != nil {
				t.Fatal(err)
			}
			if most := costs[i][j]; int64(src.Reads) > most[0] || src.Bytes > most[1] {
				t.Errorf("%s: a seek to %d took %d reads of %d bytes, want at most %d of %d; it read %v",
					s.name, target, src.Reads, src.Bytes, most[0], most[1], src.Spans)
			}
		}
	}
}

// TestAProbeSteppingPagesReadsEachByteOnce: a bisection probe that steps over
// pages placing nothing, here 200 of another stream, takes each from the
// window, so it reads the span once in at most three reads.
func TestAProbeSteppingPagesReadsEachByteOnce(t *testing.T) {
	var stream []byte
	for i := range 200 {
		stream = append(stream, buildPage(0, int64(i+1)*960, 2, uint32(i), make([]byte, 300))...)
	}
	stream = append(stream, buildPage(0, 960, 1, 0, make([]byte, 300))...)
	src := &testutil.CountingSource{Src: container.BytesSource(stream), Log: true}
	d := scanDemuxer(src)
	d.serial, d.disowned = 1, -1
	at, granule, ok, err := d.granuleAt(0, int64(len(stream)))
	if err != nil || !ok || granule != 960 || at != int64(pageStarts(t, stream)[200]) {
		t.Fatalf("the probe found %d at %d (%v, %v), want the page of serial 1", granule, at, ok, err)
	}
	if src.Reads > 3 || src.Bytes > int64(len(stream)) {
		t.Errorf("a probe over %d bytes of pages took %d reads of %d bytes, want at most 3 and each byte once; it read %v",
			len(stream), src.Reads, src.Bytes, src.Spans)
	}
}
