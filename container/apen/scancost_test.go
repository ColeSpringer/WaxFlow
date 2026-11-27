package apen_test

import (
	"bytes"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/colespringer/waxflow/codec/ape"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/apen"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// longFile is a file of eight frames, over two windows, so no read behind
// junk in front of it is cut short by its end. It is encoded once.
func longFile(t testing.TB) []byte {
	t.Helper()
	longOnce.Do(func() {
		track, pkts := muxFrames(t, 8*ape.BlocksPerFrame, int64(8*ape.BlocksPerFrame))
		longRaw = writeAll(t, track, pkts, nil)
	})
	if len(longRaw) < 2*srcwin.Chunk {
		t.Fatalf("eight frames wrote %d bytes, want over two windows", len(longRaw))
	}
	return longRaw
}

var (
	longOnce sync.Once
	longRaw  []byte
)

// headerScan is the open's scan for the file header, with file behind the
// junk.
func headerScan(file []byte) testutil.JunkScan {
	return testutil.JunkScan{
		Pat:  ape.Magic,
		File: func(junk []byte) []byte { return append(append([]byte(nil), junk...), file...) },
		Scan: func(t testing.TB, src container.Source, n int) {
			t.Helper()
			openBehind(t, src, n)
		},
	}
}

func openBehind(t testing.TB, src container.Source, junk int) *apen.Demuxer {
	t.Helper()
	d, err := apen.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.BaseForTest(); got != int64(junk) {
		t.Fatalf("the header found at %d, want %d", got, junk)
	}
	return d
}

// decoyJunk is n bytes of junk holding count magics, evenly spaced, with
// nothing behind any of them that parses as a header.
func decoyJunk(n, count int) []byte {
	b := bytes.Repeat([]byte{0x11}, n)
	for i := range count {
		copy(b[n/count*i+n/count/2:], "MAC ")
	}
	return b
}

// cappedJunk is junk holding two more magics than a scan tries, and where
// the last one it tries is. The last six are just under a window apart and
// each a little short of a span's end, so each one's header is its own read.
func cappedJunk() (junk []byte, last int64) {
	b := bytes.Repeat([]byte{0x11}, 800<<10)
	for i := range 1020 {
		copy(b[8*i+4:], "MAC ")
	}
	for i := range 6 {
		copy(b[127<<10-2000+i*(srcwin.Chunk-8):], "MAC ")
	}
	return b, 127<<10 - 2000 + 3*(srcwin.Chunk-8)
}

// TestJunkScanIsBoundedBySpans pins the scan a file led by junk pays at
// open: the junk read in spans, whatever it holds.
func TestJunkScanIsBoundedBySpans(t *testing.T) {
	file := longFile(t)
	headerScan(file).BoundedBySpans(t, ape.MaxHeaderLen+1024)
}

// TestHeaderScanReadsAreBoundedBySpans is the same pin for junk that carries
// the magic a thousand times, each one asking for the header behind it.
func TestHeaderScanReadsAreBoundedBySpans(t *testing.T) {
	const big = 512 << 10
	s := headerScan(longFile(t))
	plain, _ := s.Cost(t, s.Junk(big, 0))
	if reads, _ := s.Cost(t, decoyJunk(big, 1000)); reads > 2*plain {
		t.Errorf("junk holding 1000 magics cost %d reads and plain junk %d, want at most twice", reads, plain)
	}
}

// TestConfirmBehindShortJunkReadsNoWindow: the header behind the junk is
// read in proportion to what the scan holds, so short junk before a long
// file costs about twice the junk and the header, and never a window.
func TestConfirmBehindShortJunkReadsNoWindow(t *testing.T) {
	s := headerScan(longFile(t))
	for _, n := range []int{1000, 3000, 7000} {
		if _, read := s.Cost(t, s.Junk(n, 0)); read > int64(2*n+ape.MaxHeaderLen+1024) {
			t.Errorf("%d bytes of junk before a long file read %d bytes, want about twice the junk and the header", n, read)
		}
	}
}

// TestScanLeavesTheWindowAtItsRange pins what the longest scan leaves
// resident. A scan trims nothing, so that is the junk, and the read of the
// header behind it may reach a Chunk past it.
func TestScanLeavesTheWindowAtItsRange(t *testing.T) {
	const junk = apen.MaxJunkForTest - 1 // the furthest a scan takes a header at
	s := headerScan(longFile(t))
	for _, tc := range []struct {
		name string
		junk []byte
	}{
		{"junk with no magic", s.Junk(junk, 0)},
		{"junk holding 1000 magics", decoyJunk(junk, 1000)},
	} {
		d := openBehind(t, container.BytesSource(s.File(tc.junk)), junk)
		n := d.ResidentForTest()
		if n < junk {
			t.Fatalf("%s: the window holds %d bytes, fewer than the %d scanned: this pins nothing", tc.name, n, junk)
		}
		if n > apen.MaxJunkForTest+ape.MaxHeaderLen+srcwin.Chunk {
			t.Errorf("%s: the window holds %d bytes after the scan, want at most the %d scanned, a header and a Chunk",
				tc.name, n, apen.MaxJunkForTest)
		}
	}
}

// TestOpenCost pins the reads a clean open makes: the tag probe and the rest
// of the header's 4 KiB in one read, or behind a tag the magic and then the
// header, and one read of the tail for every trailer probe. A file that never
// enters the scan pays no span.
func TestOpenCost(t *testing.T) {
	file := longFile(t)
	tag := make([]byte, 10+64)
	copy(tag, "ID3\x04\x00\x00")
	tag[9] = 64
	for _, tc := range []struct {
		name string
		raw  []byte
		want [][2]int64 // the reads in front of the trailer probe
	}{
		{"bare", file, [][2]int64{{0, 10}, {10, ape.MaxHeaderLen}}},
		{"behind a tag", append(tag, file...), [][2]int64{{0, 10}, {74, 78}, {78, 74 + ape.MaxHeaderLen}}},
	} {
		src := &testutil.CountingSource{Src: container.BytesSource(tc.raw), Log: true}
		openBehind(t, src, len(tc.raw)-len(file))
		size := int64(len(tc.raw))
		want := append(slices.Clone(tc.want), [2]int64{size - 128, size})
		if !slices.Equal(src.Spans, want) {
			t.Errorf("%s: the open read %v, want %v", tc.name, src.Spans, want)
		}
	}
}

// TestAReadThatFailsInAScanIsNeverARefusedFile: a read that fails while the
// scan is over junk is the open's error, never a file with no header or too
// many magics. A scan that ends before the failure answers as over a whole source.
func TestAReadThatFailsInAScanIsNeverARefusedFile(t *testing.T) {
	file := fixture(t, "seek.ape")
	capped, last := cappedJunk()
	for _, tc := range []struct {
		name  string
		junk  []byte
		step  int64
		whole string // what a source that reads refuses the file with, "" when it opens
		last  int64  // the magic whose header read must be seen to fail, -1 for none
	}{
		{"junk holding 1000 magics", decoyJunk(200<<10, 1000), 1021, "", -1},
		{"junk holding 1026 magics", capped, 2039, "ape: no Monkey's Audio header in the first 1024 candidates", last},
	} {
		raw := append(append([]byte(nil), tc.junk...), file...)
		_, whole := apen.NewDemuxer(container.BytesSource(raw), nil)
		if got := fmt.Sprint(whole); tc.whole != "" && got != tc.whole || tc.whole == "" && whole != nil {
			t.Fatalf("%s: a source that reads returned %q, want %q", tc.name, got, tc.whole)
		}
		// The junk cannot be read from cut on. The tail can, so the trailer
		// probes in front of the scan pass.
		onLast := 0
		for cut := int64(11); cut < int64(len(tc.junk)); cut += tc.step {
			// Strict has no say in the scan; every fourth cut holds it to that.
			strict := cut%4 == 0
			src := &testutil.FailingSource{Src: container.BytesSource(raw), From: cut, To: int64(len(tc.junk))}
			_, err := apen.NewDemuxer(src, &container.DemuxerOptions{Strict: strict})
			switch {
			case src.Failed > 0 && waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable:
				t.Fatalf("%s, no read from %d, strict %v: the open returned %v after %d failed reads, want the read's error",
					tc.name, cut, strict, err, src.Failed)
			case src.Failed == 0 && fmt.Sprint(err) != fmt.Sprint(whole):
				t.Fatalf("%s, no read from %d, strict %v: the open returned %v with no read failed; a source that reads returns %v",
					tc.name, cut, strict, err, whole)
			}
			// The read for that magic's header starts behind the magic, which
			// the scan already held, and inside the header.
			if at := src.FailedAt[0]; src.Failed > 0 && at > tc.last && at < tc.last+ape.MaxHeaderLen {
				onLast++
			}
		}
		if tc.last >= 0 && onLast == 0 {
			t.Errorf("%s: no cut failed the header read of the last magic the scan tries, at %d", tc.name, tc.last)
		}
	}
}
