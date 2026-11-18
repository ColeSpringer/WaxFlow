package flacn_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// firstFrameScan is the open's scan for the first frame, with junk between
// the metadata blocks of raw and its frames.
func firstFrameScan(t testing.TB, raw []byte) testutil.JunkScan {
	t.Helper()
	first := frameSpans(t, raw)[0][0]
	return testutil.JunkScan{
		Pat: []byte{0xFF},
		File: func(junk []byte) []byte {
			return append(append(append([]byte(nil), raw[:first]...), junk...), raw[first:]...)
		},
		Scan: func(t testing.TB, src container.Source, n int) {
			t.Helper()
			d, err := flacn.NewDemuxer(src, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := d.FirstFrameForTest(); got != int64(first+n) {
				t.Fatalf("first frame at %d, want %d", got, first+n)
			}
		},
	}
}

// noiseStream is n samples of stereo noise as this tree writes them at its
// default level to a destination it cannot seek: no SEEKTABLE, and frames
// that do not compress.
func noiseStream(t *testing.T, n int) []byte {
	t.Helper()
	src := audio.Get(muxFmt(44100, 2, 16), n)
	defer audio.Put(src)
	src.N = n
	r := rand.New(rand.NewPCG(uint64(n), 7))
	for c := range src.Fmt.Channels {
		s := src.ChanI(c)
		for j := range n {
			s[j] = int32(r.IntN(1<<16)) - 1<<15
		}
	}
	var buf bytes.Buffer
	encodeStream(t, src, flac.DefaultEncoderLevel, &buf, int64(n))
	return buf.Bytes()
}

// TestJunkScanIsBoundedBySpans pins the scan a stream pays at open for junk
// in front of its first frame: the junk read in spans, whatever it holds.
func TestJunkScanIsBoundedBySpans(t *testing.T) {
	raw := fixture(t, "noise-s16.flac")
	firstFrameScan(t, raw).BoundedBySpans(t, int64(len(raw)))
}

// TestScanLeavesTheWindowAtItsRange pins what a scan leaves resident. The
// open's scan trims nothing and is bounded, so that is its junk. A seek's
// probe has no bound, so it scans in pieces of that bound and holds one.
func TestScanLeavesTheWindowAtItsRange(t *testing.T) {
	const junk = flacn.MaxResyncForTest - 1 // the furthest a scan takes a frame at
	const most = flacn.MaxResyncForTest + flac.MaxFrameHeaderLen + srcwin.Chunk
	raw := fixture(t, "noise-s16.flac")
	s := firstFrameScan(t, raw)
	for _, tc := range []struct {
		name string
		junk []byte
	}{
		{"junk with no sync byte", s.Junk(junk, 0)},
		{"junk dense with sync bytes", s.Junk(junk, 100)},
	} {
		// The tail read behind the scan adds the stream, which is under a Chunk.
		d, err := flacn.NewDemuxer(container.BytesSource(s.File(tc.junk)), nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		n := d.ResidentForTest()
		if n < junk {
			t.Fatalf("%s: the window holds %d bytes, fewer than the %d scanned: this pins nothing", tc.name, n, junk)
		}
		if n > most {
			t.Errorf("%s: the window holds %d bytes after the scan, want at most the %d scanned, a header and a Chunk",
				tc.name, n, flacn.MaxResyncForTest)
		}
	}

	// A seek to the end of a stream with 8 MiB that hold no frame in its
	// middle. The tail confirms, so the open leaves the window there, and
	// the bisection's one probe starts halfway into the hole.
	spans := frameSpans(t, raw)
	mid := spans[len(spans)/2][0]
	for _, tc := range []struct {
		name string
		hole []byte
	}{
		{"a hole with no sync byte", s.Junk(8<<20, 0)},
		{"a hole dense with sync bytes", s.Junk(8<<20, 100)},
	} {
		file := append(append(append([]byte(nil), raw[:mid]...), tc.hole...), raw[mid:]...)
		src := &testutil.CountingSource{Src: container.BytesSource(file)}
		d, err := flacn.NewDemuxer(src, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		src.Reset()
		off, ok := d.BisectForTest(d.Tracks()[0].Samples - 1)
		if !ok || off != int64(mid+len(tc.hole)) || src.Bytes < 3<<20 {
			t.Fatalf("%s: the bisection found %d (%v) and read %d bytes, want the frame behind the hole at %d and half the hole read: this pins nothing",
				tc.name, off, ok, src.Bytes, mid+len(tc.hole))
		}
		if n := d.ResidentForTest(); n > most {
			t.Errorf("%s: the window holds %d bytes after a probe over %d, want at most a piece of %d, a header and a Chunk",
				tc.name, n, src.Bytes, flacn.MaxResyncForTest)
		}
		// A piece behind the first reads on a window at a time.
		if src.Reads > int(10*(src.Bytes>>20+1)) {
			t.Errorf("%s: the probe took %d reads over %d bytes, want at most ten a mebibyte", tc.name, src.Reads, src.Bytes)
		}
	}

	// A second probe that starts inside what the first left. The first scans
	// most of a piece of junk to frame 2 and its last span reads a window on;
	// the second starts in that window and scans the junk in front of frame 3.
	// A probe reads the largest frame's bytes and a header, as much again,
	// and then spans that double from where 127 KiB of them would end.
	const kib = 1 << 10
	si, err := flac.ParseStreamInfo(raw[8 : 8+flac.StreamInfoLen])
	if err != nil || si.MaxFrame == 0 {
		t.Fatalf("STREAMINFO states a largest frame of %d bytes (%v), want one: this pins nothing", si.MaxFrame, err)
	}
	first := int64(spans[0][0])
	total := first + 2*1146*kib
	at := first + 1146*kib                                   // where the first probe starts
	found := at + int64(si.MaxFrame) + 1 + (127+5*128+1)*kib // frame 2, a kibibyte into the span the scan reads last
	next := found + 125*kib                                  // where the second probe starts
	file := append([]byte(nil), raw[:spans[2][0]]...)
	file = append(file, s.Junk(int(found)-len(file), 0)...)
	file = append(file, raw[spans[2][0]:spans[2][1]]...)
	file = append(file, s.Junk(int(total)-len(file)-(len(raw)-spans[3][0]), 0)...)
	file = append(file, raw[spans[3][0]:]...)
	src := &testutil.CountingSource{Src: container.BytesSource(file), Log: true}
	d, err := flacn.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.Reset()
	off, ok := d.BisectForTest(d.Tracks()[0].Samples - 1)
	inside := slices.ContainsFunc(src.Spans, func(r [2]int64) bool { return r[0] < next && next < r[1] }) &&
		!slices.ContainsFunc(src.Spans, func(r [2]int64) bool { return r[0] == next })
	if want := total - int64(len(raw)-spans[3][0]); !ok || off != want || !inside {
		t.Fatalf("the bisection found %d (%v) reading %v, want frame 3 at %d and a probe at %d that starts inside the last one's reads: this pins nothing",
			off, ok, src.Spans, want, next)
	}
	if n, most := int64(d.ResidentForTest()), total-next+flac.MaxFrameHeaderLen+srcwin.Chunk; n > most {
		t.Errorf("the window holds %d bytes after a probe that started inside the last one's, want at most %d: its own %d, a header and a Chunk",
			n, most, total-next)
	}
}

// TestOpenCost pins the read a clean open makes at its first frame: the
// sixteen bytes a header can take, exactly. A stream that never enters the
// scan pays no span.
func TestOpenCost(t *testing.T) {
	for _, name := range []string{"noise-s16.flac", "sine-s16.flac", "sine-s24.flac"} {
		raw := fixture(t, name)
		first := int64(frameSpans(t, raw)[0][0])
		src := &testutil.CountingSource{Src: container.BytesSource(raw), Log: true}
		if _, err := flacn.NewDemuxer(src, nil); err != nil {
			t.Fatal(err)
		}
		at := slices.IndexFunc(src.Spans, func(s [2]int64) bool { return s[0] <= first && first < s[1] })
		if want := [2]int64{first, first + flac.MaxFrameHeaderLen}; at < 0 || src.Spans[at] != want {
			t.Errorf("%s: the open read %v, want the first frame's header in a read of its own, %v", name, src.Spans, want)
		}
	}
}

// TestSeekProbesAreOneReadEach pins what a seek pays on a stream with no
// SEEKTABLE: the first frame's header read exactly, each probe one read of
// the largest frame and a header, where a frame must start, and the walk
// from where they land. The largest frame is the one STREAMINFO states or,
// stating none, what its largest block codes verbatim.
func TestSeekProbesAreOneReadEach(t *testing.T) {
	const n = 1 << 20
	raw := noiseStream(t, n)
	first := int64(frameSpans(t, raw)[0][0])
	// The stream was written unseekable and states no frame sizes: the
	// stated one is given the bounds its frames have.
	spans := frameSpans(t, raw)
	least, largest := len(raw), 0
	for _, f := range spans {
		least, largest = min(least, f[1]-f[0]), max(largest, f[1]-f[0])
	}
	stated := bytes.Clone(raw)
	for i, v := range []int{least, largest} {
		stated[12+3*i], stated[13+3*i], stated[14+3*i] = byte(v>>16), byte(v>>8), byte(v)
	}
	overstated := bytes.Clone(raw)
	overstated[15], overstated[16], overstated[17] = 0xFF, 0xFF, 0xFF
	if si, err := flac.ParseStreamInfo(raw[8 : 8+flac.StreamInfoLen]); err != nil || si.MaxFrame != 0 {
		t.Fatalf("the stream as written states a largest frame of %d bytes (%v), want none: this pins nothing", si.MaxFrame, err)
	}
	for _, tc := range []struct {
		name   string
		raw    []byte
		probed int64 // the most bytes a probe reads
	}{
		{"a largest frame stated", stated, int64(largest) + flac.MaxFrameHeaderLen},
		{"none stated", raw, 2*flac.MaxFrameHeaderLen + 2 + 2*(1+17*4096/8)},
		{"a largest frame stated that no frame of its blocks reaches", overstated, 2*flac.MaxFrameHeaderLen + 2 + 2*(1+17*4096/8)},
	} {
		// A probe is one read, and a seek the header's, its probes' and
		// two of the walk.
		seekCost(t, tc.name, tc.raw, first, n, 1, tc.probed, 8)
	}
}

// seekCost holds each bisection probe on raw to reads reads and probed
// bytes, and a whole seek to seek reads.
func seekCost(t *testing.T, name string, raw []byte, first, n int64, reads int, most int64, seek int) {
	t.Helper()
	open := func() (*flacn.Demuxer, *testutil.CountingSource) {
		src := &testutil.CountingSource{Src: container.BytesSource(raw), Log: true}
		d, err := flacn.NewDemuxer(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		src.Reset()
		return d, src
	}
	for _, target := range []int64{n / 7, n / 2, n * 9 / 10, n - 1} {
		// The bisection alone: its check of the first frame, then the probes,
		// each beginning with a read that does not continue the last.
		d, src := open()
		if _, ok := d.BisectForTest(target); !ok {
			t.Fatalf("%s: seek to %d: the bisection found no frame", name, target)
		}
		if want := [2]int64{first, first + flac.MaxFrameHeaderLen}; len(src.Spans) == 0 || src.Spans[0] != want {
			t.Fatalf("%s: seek to %d: the bisection read %v, want the first frame's header first, %v", name, target, src.Spans, want)
		}
		var probes [][][2]int64
		for i, s := range src.Spans[1:] {
			switch {
			case s[0] != src.Spans[i][1]:
				probes = append(probes, nil)
			case len(probes) == 0:
				t.Fatalf("%s: seek to %d: the read at %d continues the header's, want a probe's", name, target, s[0])
			}
			probes[len(probes)-1] = append(probes[len(probes)-1], s)
		}
		if len(probes) < 4 {
			t.Fatalf("%s: seek to %d: %d probes over %d bytes, want a stream long enough to bisect", name, target, len(probes), len(raw))
		}
		var probed int64
		for i, p := range probes {
			if len(p) > reads {
				t.Errorf("%s: seek to %d: probe %d at %d took %d reads, want at most %d", name, target, i, p[0][0], len(p), reads)
			}
			probed += p[len(p)-1][1] - p[0][0]
		}
		if most := int64(len(probes)) * most; probed > most {
			t.Errorf("%s: seek to %d: %d probes read %d bytes, want at most %d", name, target, len(probes), probed, most)
		}

		// The seek whole. Its walk reads a window from the frame the
		// bisection hands it and, stepping past that frame, one window on.
		d, src = open()
		landed, err := d.SeekSample(context.Background(), 0, target)
		if err != nil || landed > target || target-landed >= 4096 {
			t.Fatalf("%s: seek to %d landed at %d (%v), want the frame holding it", name, target, landed, err)
		}
		if most := flac.MaxFrameHeaderLen + probed + 2*srcwin.Chunk; src.Bytes > most {
			t.Errorf("%s: seek to %d read %d bytes in %d reads, want at most %d: the first frame's %d-byte header, the probes' %d and two windows of walk",
				name, target, src.Bytes, src.Reads, most, flac.MaxFrameHeaderLen, probed)
		}
		t.Logf("%s: seek to %d: %d reads, %d probes", name, target, src.Reads, len(probes))
		if src.Reads > seek {
			t.Errorf("%s: seek to %d took %d reads, want at most %d", name, target, src.Reads, seek)
		}
	}
}

// TestASeekUnderTwoWindowsReadsNoByteTwice pins a seek on a stream with no
// SEEKTABLE and under two windows of frames: the window at the first frame
// serves the one probe and a landing there, and the walk reads the rest once.
// The open that has to measure such a stream pays the same on top of an open.
func TestASeekUnderTwoWindowsReadsNoByteTwice(t *testing.T) {
	const n = 50000
	raw := noiseStream(t, n)
	first := int64(frameSpans(t, raw)[0][0])
	frames := int64(len(raw)) - first
	if frames < srcwin.Chunk*5/4 || frames > srcwin.Chunk*7/4 {
		t.Fatalf("%d bytes of frames, want about a window and a half", frames)
	}
	open := func(raw []byte) (*flacn.Demuxer, *testutil.CountingSource) {
		src := &testutil.CountingSource{Src: container.BytesSource(raw), Log: true}
		d, err := flacn.NewDemuxer(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		return d, src
	}
	for _, tc := range []struct {
		name   string
		target int64
		reads  int
		bytes  int64
	}{
		{"its start", 0, 1, srcwin.Chunk},
		{"its middle", n / 2, 2, frames},
		{"near its end", n * 9 / 10, 2, frames},
	} {
		d, src := open(raw)
		src.Reset()
		landed, err := d.SeekSample(context.Background(), 0, tc.target)
		if err != nil || landed > tc.target || tc.target-landed >= 4096 {
			t.Fatalf("a seek to %s landed at %d (%v), want the frame holding %d", tc.name, landed, err, tc.target)
		}
		if src.Reads > tc.reads || src.Bytes > tc.bytes {
			t.Errorf("a seek to %s read %d bytes in %d reads, want at most %d in %d; it read %v",
				tc.name, src.Bytes, src.Reads, tc.bytes, tc.reads, src.Spans)
		}
	}

	// Cut short, the tail confirms nothing and the open bisects to measure.
	_, whole := open(raw)
	cut := raw[:len(raw)-100]
	d, src := open(cut)
	if tr := d.Tracks()[0]; tr.Samples <= 0 || tr.Samples >= n || len(d.Warnings()) == 0 {
		t.Fatalf("the stream cut short opened at %d samples with warnings %v, want a measured length short of %d", tr.Samples, d.Warnings(), n)
	}
	if mostReads, mostBytes := whole.Reads+2, whole.Bytes+frames-100; src.Reads > mostReads || src.Bytes > mostBytes {
		t.Errorf("the open that measures read %d bytes in %d reads, want at most %d in %d, a whole stream's open and the frames once; it read %v",
			src.Bytes, src.Reads, mostBytes, mostReads, src.Spans)
	}

	// At two windows the probe starts where a window from the first frame
	// ends, which would serve it nothing: there the header is read alone.
	for _, tc := range []struct {
		name string
		size int64
		read int64
	}{{"a byte under two windows", 2*srcwin.Chunk - 1, srcwin.Chunk}, {"two windows", 2 * srcwin.Chunk, flac.MaxFrameHeaderLen}} {
		d, src := open(append(append([]byte(nil), raw...), bytes.Repeat([]byte{0x11}, int(tc.size-frames))...))
		src.Reset()
		if _, err := d.SeekSample(context.Background(), 0, 0); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if want := [2]int64{first, first + tc.read}; src.Spans[0] != want {
			t.Errorf("%s of frames and junk: the seek read %v, want %v first", tc.name, src.Spans, want)
		}
	}
}

// TestALinearReadCostsOneReadPerWindow pins the packet path: a read of many
// windows costs about a read a window and reads no byte twice, and the window
// stays bounded whatever the stream's length.
func TestALinearReadCostsOneReadPerWindow(t *testing.T) {
	for _, n := range []int{1 << 19, 1 << 20} {
		raw := noiseStream(t, n)
		src := &testutil.CountingSource{Src: container.BytesSource(raw)}
		d, err := flacn.NewDemuxer(src, nil)
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
		if windows < 16 || packets < 4*windows {
			t.Fatalf("%d packets over %d windows, want a long stream of several packets a window: this pins nothing", packets, windows)
		}
		if src.Reads > windows+2 || src.Bytes > int64(len(raw)) {
			t.Errorf("%d packets over %d windows took %d reads of %d bytes, want about a read a window and the stream's %d bytes once",
				packets, windows, src.Reads, src.Bytes, len(raw))
		}
		// A view reaches a window past its frame's start and the read behind
		// it at most a window further; Trim leaves under a window in front.
		if held > 3*srcwin.Chunk {
			t.Errorf("the window held %d bytes over %d windows of stream, want at most three windows", held, windows)
		}
	}
}

// TestAReadThatFailsInAScanIsNeverAnAnswer: a read that fails while a scan
// looks for a frame is the read's error, at open and in a seek. The junk is
// never a stream with no frames, and the bisection finds nothing.
func TestAReadThatFailsInAScanIsNeverAnAnswer(t *testing.T) {
	// At open: no cut short of the first frame's header lets a scan find it.
	raw := fixture(t, "noise-s16.flac")
	s := firstFrameScan(t, raw)
	first := int64(frameSpans(t, raw)[0][0])
	junk := s.Junk(200<<10, 100)
	file := s.File(junk)
	for _, cut := range []int64{first + 17, first + 1024, first + 1025, first + 3072, first + 100<<10, first + srcwin.Chunk + 1, first + int64(len(junk)), first + int64(len(junk)) + 4} {
		for _, strict := range []bool{false, true} {
			src := &testutil.FailingSource{Src: container.BytesSource(file), From: cut}
			_, err := flacn.NewDemuxer(src, &container.DemuxerOptions{Strict: strict})
			if waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
				t.Errorf("reads failing from %d, strict %v: the open returned %v, want the read's error", cut, strict, err)
			}
		}
	}

	// In a probe: the head and the tail read and a part in the middle does
	// not, where the first probe lands or just behind it.
	const n = 1 << 18
	long := noiseStream(t, n)
	mid := int64(len(long) / 2)
	var failed struct{ seeks, bisections, opens int } // how many of each met a read that failed
	for _, bad := range [][2]int64{{mid - 100, mid + 100}, {mid + 1000, mid + 1100}, {mid + 3000, mid + 3100}} {
		for _, strict := range []bool{false, true} {
			name := fmt.Sprintf("no read in [%d, %d), strict %v", bad[0], bad[1], strict)
			open := func(raw []byte) (*flacn.Demuxer, *testutil.FailingSource, error) {
				src := &testutil.FailingSource{Src: container.BytesSource(raw), From: bad[0], To: bad[1]}
				d, err := flacn.NewDemuxer(src, &container.DemuxerOptions{Strict: strict})
				return d, src, err
			}
			d, src, err := open(long)
			if err != nil || src.Failed != 0 {
				t.Fatalf("%s: the open returned %v after %d failed reads, want a stream that opens", name, err, src.Failed)
			}
			// The seek's probe or its walk reaches the part, every time.
			landed, err := d.SeekSample(context.Background(), 0, n/2)
			if src.Failed == 0 {
				t.Fatalf("%s: the seek read nothing of the part that cannot be read", name)
			}
			failed.seeks++
			if waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable || len(d.Warnings()) != 0 {
				t.Errorf("%s: the seek landed at %d with %v and warnings %v, want the read's error and none", name, landed, err, d.Warnings())
			}

			// The bisection behind that seek, alone.
			d, src, _ = open(long)
			if off, ok := d.BisectForTest(n / 2); src.Failed > 0 {
				failed.bisections++
				if ok {
					t.Errorf("%s: the bisection found a frame at %d after %d failed reads, want nothing found", name, off, src.Failed)
				}
			}

			// An open whose tail does not confirm, which bisects to measure.
			_, src, err = open(long[:len(long)-100])
			if src.Failed > 0 {
				failed.opens++
				if waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
					t.Errorf("%s: opening the stream cut short returned %v after %d failed reads, want the read's error", name, err, src.Failed)
				}
			}
		}
	}
	if failed.seeks == 0 || failed.bisections == 0 || failed.opens == 0 {
		t.Errorf("reads failed in %+v, want at least one of each", failed)
	}
}

// TestAFrameEndIsFoundAsFarAsTheScanReaches: a frame with one behind it is
// delivered whole when it ends on either side of the seam where the scan's
// first view ends, a Chunk in, or on the last byte of the scan's reach, the
// frame bound from its fifth byte; a byte longer is refused.
func TestAFrameEndIsFoundAsFarAsTheScanReaches(t *testing.T) {
	raw := fixture(t, "noise-s16.flac")
	spans := frameSpans(t, raw)
	// Frame 1 grown with junk, its CRC-16 made to check where frame 2 starts.
	grown := func(n int) []byte {
		file := append([]byte(nil), raw[:spans[1][1]-2]...)
		file = append(file, bytes.Repeat([]byte{0x11}, spans[1][0]+n-2-len(file))...)
		crc := flac.UpdateCRC16(0, file[spans[1][0]:])
		return append(append(file, byte(crc>>8), byte(crc)), raw[spans[2][0]:]...)
	}
	const reach = flacn.MaxFrameLenForTest + 4
	for _, tc := range []struct {
		name    string
		n       int
		refused bool
	}{
		{"the next frame on the first view's last byte", srcwin.Chunk - 1, false},
		{"the next frame on the second view's first byte", srcwin.Chunk, false},
		{"the next frame on the second view's fourth byte", srcwin.Chunk + 3, false},
		{"the next frame on the second view's fifth byte", srcwin.Chunk + 4, false},
		{"a frame that ends on the last byte scanned", reach - 1, false},
		{"a frame a byte longer", reach, true},
	} {
		d, err := flacn.NewDemuxer(container.BytesSource(grown(tc.n)), nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var pkt container.Packet
		var sizes []int
		for err = d.ReadPacket(&pkt); err == nil; err = d.ReadPacket(&pkt) {
			sizes = append(sizes, len(pkt.Data))
		}
		if tc.refused {
			if len(sizes) != 1 || waxerr.CodeOf(err) != waxerr.CodeMalformedInput || !strings.Contains(err.Error(), "exceeds") {
				t.Errorf("%s: packets of %v bytes, then %v, want the first frame and the bound's refusal", tc.name, sizes, err)
			}
			continue
		}
		want := make([]int, len(spans))
		for i, s := range spans {
			want[i] = s[1] - s[0]
		}
		want[1] = tc.n
		if err != io.EOF || !slices.Equal(sizes, want) || len(d.Warnings()) != 0 {
			t.Errorf("%s: packets of %v bytes, then %v, with warnings %v, want %v and none",
				tc.name, sizes, err, d.Warnings(), want)
		}
	}
}
