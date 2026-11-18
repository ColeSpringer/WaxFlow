package adts_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/adts"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// The synthetic stream the scan tests read: AAC-LC, 44.1 kHz stereo, no CRC,
// 264 bytes a frame, the payload zeros. Nothing decodes it, and the open's
// SBR probe reads it as AAC-LC.
var lcHeader = []byte{0xFF, 0xF1, 0x50, 0x80, 0x21, 0x1F, 0xFC}

const (
	lcFrameLen = 264
	// maxConfirm is the longest read a candidate asks for: the 13-bit length
	// field's largest frame and the header behind it.
	maxConfirm = 8191 + 9
)

func lcFrames(n int) []byte {
	f := make([]byte, lcFrameLen)
	copy(f, lcHeader)
	return bytes.Repeat(f, n)
}

// headerJunk is n bytes of mono headers stating frames of frameLen bytes, no
// multiple of seven: each parses, its length points into the middle of
// another, and none is kin to the stereo stream. Seven bytes of junk lead it.
func headerJunk(n, frameLen int) []byte {
	hdr := []byte{0xFF, 0xF1, 0x50, 0x40 | byte(frameLen>>11), byte(frameLen >> 3), byte(frameLen&7)<<5 | 0x1F, 0xFC}
	pad := bytes.Repeat([]byte{0x11}, len(hdr)+n%len(hdr))
	return append(append(pad[:len(hdr)], bytes.Repeat(hdr, n/len(hdr)-1)...), pad[len(hdr):]...)
}

// headScan is the open's scan for the first frame, with frames frames behind
// the junk.
func headScan(frames int) testutil.JunkScan {
	return testutil.JunkScan{
		Pat:  []byte{0xFF},
		File: func(junk []byte) []byte { return append(append([]byte(nil), junk...), lcFrames(frames)...) },
		Scan: func(t testing.TB, src container.Source, n int) {
			t.Helper()
			openBehind(t, src, n)
		},
	}
}

func openBehind(t testing.TB, src container.Source, junk int) *adts.Demuxer {
	t.Helper()
	d, err := adts.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.FirstFrameForTest(); got != int64(junk) {
		t.Fatalf("first frame at %d, want %d", got, junk)
	}
	return d
}

// midScan is the walk's resync over junk between before frames and three
// more.
func midScan(before int) testutil.JunkScan {
	return testutil.JunkScan{
		Pat: []byte{0xFF},
		File: func(junk []byte) []byte {
			return append(append(lcFrames(before), junk...), lcFrames(3)...)
		},
		Scan: func(t testing.TB, src container.Source, n int) {
			t.Helper()
			d := openBehind(t, src, 0)
			if err := d.Walk(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := container.Warning{Offset: int64(before * lcFrameLen), Kind: container.Damage,
				Msg: fmt.Sprintf("%d unparsable bytes skipped", n)}
			if !slices.Contains(d.Warnings(), want) {
				t.Fatalf("warnings = %v, want %v", d.Warnings(), want)
			}
		},
	}
}

// TestJunkScanIsBoundedBySpans pins the scan a stream led by junk pays at
// open: the junk read in spans, whatever it holds.
func TestJunkScanIsBoundedBySpans(t *testing.T) {
	headScan(3).BoundedBySpans(t, 3*lcFrameLen)
}

// TestJunkOfParseableHeadersIsBoundedBySpans is the same pin for junk whose
// candidates parse and never confirm, each asking for the frame behind it,
// up to the longest a header can state.
func TestJunkOfParseableHeadersIsBoundedBySpans(t *testing.T) {
	const big = 512 << 10
	// A tail longer than the longest frame, or a header near the end of the
	// junk would run to the end of data and be taken.
	s := headScan(40)
	plain, _ := s.Cost(t, s.Junk(big, 0))
	for _, frameLen := range []int{lcFrameLen, 8191} {
		if reads, _ := s.Cost(t, headerJunk(big, frameLen)); reads > 2*plain {
			t.Errorf("junk of headers stating %d-byte frames cost %d reads and plain junk %d, want at most twice", frameLen, reads, plain)
		}
	}
}

// TestConfirmBehindShortJunkReadsNoWindow: the frame behind the junk is
// confirmed by a read in proportion to what the scan holds, so short junk
// before a long stream costs about twice the junk and never a window.
func TestConfirmBehindShortJunkReadsNoWindow(t *testing.T) {
	s := headScan(2000)
	// Each junk ends within a frame of a span's end, so the confirm reads.
	for _, n := range []int{1000, 3000, 7000} {
		if _, read := s.Cost(t, s.Junk(n, 0)); read > int64(2*n+1024) {
			t.Errorf("%d bytes of junk before a long stream read %d bytes, want about twice the junk", n, read)
		}
	}
}

// TestMidStreamResyncIsBoundedBySpans is the pin for junk between frames,
// which the walk scans past: junk dense with sync bytes costs the reads of
// junk with none.
func TestMidStreamResyncIsBoundedBySpans(t *testing.T) {
	const big = 512 << 10
	s := midScan(5)
	plain, read := s.Cost(t, s.Junk(big, 0))
	dense, _ := s.Cost(t, s.Junk(big, 100))
	if dense != plain {
		t.Errorf("junk dense with sync bytes cost %d reads, junk with none %d; the reads must not depend on the content", dense, plain)
	}
	if read > 2*big+8*lcFrameLen {
		t.Errorf("resyncing past %d bytes of junk read %d bytes, want at most twice the junk and the frames", big, read)
	}
}

// TestScanLeavesTheWindowAtItsRange pins what the longest scan leaves
// resident. A scan trims nothing, so that is the junk, and a confirming read
// may reach its frame and a Chunk past it.
func TestScanLeavesTheWindowAtItsRange(t *testing.T) {
	const junk = adts.MaxResyncForTest - 1 // the furthest a scan takes a frame at
	// Enough frames behind the junk that a read-ahead is not cut short.
	s := headScan(600)
	for _, tc := range []struct {
		name string
		junk []byte
	}{
		{"junk that never parses", s.Junk(junk, 0)},
		{"headers that parse", headerJunk(junk, 8191)},
	} {
		d := openBehind(t, container.BytesSource(s.File(tc.junk)), junk)
		n := d.ResidentForTest()
		if n < junk {
			t.Fatalf("%s: the window holds %d bytes, fewer than the %d scanned: this pins nothing", tc.name, n, junk)
		}
		if n > adts.MaxResyncForTest+maxConfirm+srcwin.Chunk {
			t.Errorf("%s: the window holds %d bytes after the scan, want at most the %d scanned, a confirming read and a Chunk",
				tc.name, n, adts.MaxResyncForTest)
		}
	}
}

// TestOpenCost pins what opening a clean stream reads: the tag probes, then
// one window from the last probe's end, which holds the frames the SBR probe
// walks. A stream that never enters the scan pays no span.
func TestOpenCost(t *testing.T) {
	fixture := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	tagged := append(make([]byte, 10+20), lcFrames(2000)...)
	copy(tagged, "ID3\x04\x00\x00")
	tagged[9] = 20
	for _, tc := range []struct {
		name  string
		raw   []byte
		first int
		reads int
	}{
		{"stereo.aac", fixture("stereo.aac"), 0, 2},
		{"mono.aac", fixture("mono.aac"), 0, 2},
		{"golden-heaac-implicit.aac, with SBR", fixture("golden-heaac-implicit.aac"), 0, 2},
		{"2000 frames", lcFrames(2000), 0, 2},
		// A tag costs its own probe and the one behind it.
		{"2000 frames behind a tag", tagged, 30, 3},
	} {
		src := &testutil.CountingSource{Src: container.BytesSource(tc.raw)}
		openBehind(t, src, tc.first)
		want := min(int64(len(tc.raw)), int64(10*(tc.reads-1)+srcwin.Chunk))
		if src.Bytes != want || src.Reads != tc.reads {
			t.Errorf("%s: the open read %d bytes in %d reads, want %d in %d", tc.name, src.Bytes, src.Reads, want, tc.reads)
		}
	}
}

// TestAReadThatFailsInAScanIsNeverAFinishedWalk: a read that fails while a
// scan is over junk is the read's error, at open and in the walk, tolerant
// and strict. The junk is never reported as where the stream ends.
func TestAReadThatFailsInAScanIsNeverAFinishedWalk(t *testing.T) {
	junk := headScan(0).Junk(200<<10, 100)

	// At open, behind the tag probe: no cut short of the first frame's
	// header lets a scan find it.
	head := headScan(3).File(junk)
	for _, cut := range []int64{10, 11, 1024, 1025, 3072, 100 << 10, int64(len(junk)), int64(len(junk)) + 6} {
		for _, strict := range []bool{false, true} {
			_, err := adts.NewDemuxer(&testutil.FailingSource{Src: container.BytesSource(head), From: cut},
				&container.DemuxerOptions{Strict: strict})
			if waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
				t.Errorf("reads failing from %d, strict %v: the open returned %v, want the read's error", cut, strict, err)
			}
		}
	}

	// In the walk, with the junk between frames. A cut inside the junk, or
	// short of the header behind it, leaves the resync nothing to find.
	const before = 5
	mid := midScan(before).File(junk)
	start := int64(before * lcFrameLen)
	end := start + int64(len(junk))
	for _, cut := range []int64{start + 1, start + 1024, srcwin.Chunk, srcwin.Chunk + 1024, 150 << 10, end - 1, end, end + 6} {
		for _, strict := range []bool{false, true} {
			name := fmt.Sprintf("reads failing from %d, strict %v", cut, strict)
			src := &testutil.FailingSource{Src: container.BytesSource(mid), From: cut}
			d, err := adts.NewDemuxer(src, &container.DemuxerOptions{Strict: strict})
			if err != nil {
				// The cut fell inside what the open reads.
				if waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
					t.Errorf("%s: the open returned %v, want the read's error", name, err)
				}
				continue
			}
			err = d.Walk(context.Background())
			if waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable || d.Walked() || len(d.Warnings()) != 0 || d.Tracks()[0].SamplesExact {
				t.Errorf("%s: Walk = %v, Walked = %v, warnings %v, length exact %v; want the read's error, a walk not finished and nothing settled",
					name, err, d.Walked(), d.Warnings(), d.Tracks()[0].SamplesExact)
			}

			// A read meets the same failure.
			r, err := adts.NewDemuxer(src, &container.DemuxerOptions{Strict: strict})
			if err != nil {
				t.Fatalf("%s: the second open failed: %v", name, err)
			}
			var pkt container.Packet
			for err == nil {
				err = r.ReadPacket(&pkt)
			}
			if err == io.EOF || waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable || len(r.Warnings()) != 0 {
				t.Errorf("%s: the read ended with %v and warnings %v, want the read's error and none", name, err, r.Warnings())
			}
		}
	}
}
