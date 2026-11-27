package mpc_test

import (
	"bytes"
	"fmt"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/container/mpc"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// magicScan is the scan for the stream magic behind junk at the head. Two
// windows of zeros follow the magic, so no read is cut short by the file's
// end.
var magicScan = testutil.JunkScan{
	Pat: []byte("MP"),
	File: func(junk []byte) []byte {
		return append(append(append([]byte(nil), junk...), "MPCK"...), make([]byte, 2*srcwin.Chunk)...)
	},
	Scan: func(t testing.TB, src container.Source, n int) {
		t.Helper()
		base, _, err := mpc.FindMagicForTest(src)
		if err != nil {
			t.Fatal(err)
		}
		if base != int64(n) {
			t.Fatalf("the magic found at %d, want %d", base, n)
		}
	},
}

// TestJunkScanIsBoundedBySpans pins the scan a stream led by junk pays at
// open: the junk read in spans, whatever it holds.
func TestJunkScanIsBoundedBySpans(t *testing.T) {
	magicScan.BoundedBySpans(t, 4)
}

// TestScanLeavesTheWindowAtItsRange pins what the longest scan leaves
// resident. A scan trims nothing, so that is the junk, and its last span
// ends with the magic.
func TestScanLeavesTheWindowAtItsRange(t *testing.T) {
	const junk = mpc.MaxJunkForTest - 1 // the furthest a scan takes a magic at
	for _, tc := range []struct {
		name string
		junk []byte
	}{
		{"junk with no candidate", magicScan.Junk(junk, 0)},
		{"junk dense with candidates", magicScan.Junk(junk, 100)},
	} {
		base, n, err := mpc.FindMagicForTest(container.BytesSource(magicScan.File(tc.junk)))
		if err != nil || base != junk {
			t.Fatalf("%s: the magic found at %d (%v), want %d", tc.name, base, err, junk)
		}
		if n < junk {
			t.Fatalf("%s: the window holds %d bytes, fewer than the %d scanned: this pins nothing", tc.name, n, junk)
		}
		if n > mpc.MaxJunkForTest+4+srcwin.Chunk {
			t.Errorf("%s: the window holds %d bytes after the scan, want at most the %d scanned, the magic and a Chunk",
				tc.name, n, mpc.MaxJunkForTest)
		}
	}
}

// TestMagicAtTheJunkLimit: a magic that starts anywhere under the junk bound
// is found, whatever stands in front of it, and one at the bound is not.
func TestMagicAtTheJunkLimit(t *testing.T) {
	stream := fixture(t, "seek-pns.mpc")
	want := readAll(t, open(t, stream, false))
	for _, tc := range []struct {
		name  string
		under int // how far under the bound the magic starts
		cand  int // how far under the bound an "MP" that is no magic starts, 0 for none
		found bool
	}{
		{"three under, a candidate ending a byte before it", 3, 6, true},
		{"two under, a candidate ending at it", 2, 4, true},
		{"one under, a candidate ending at it", 1, 3, true},
		{"one under, a candidate ending a byte before it", 1, 4, true},
		// A window read from behind the candidate ends on the magic's first byte.
		{"one under, a candidate a window before it", 1, srcwin.Chunk + 1, true},
		{"one under, no candidate in front of it", 1, 0, true},
		{"at the bound", 0, 0, false},
	} {
		junk := magicScan.Junk(mpc.MaxJunkForTest-tc.under, 0)
		if tc.cand > 0 {
			copy(junk[mpc.MaxJunkForTest-tc.cand:], "MP")
		}
		raw := append(junk, stream...)
		d, err := mpc.NewDemuxer(container.BytesSource(raw), nil)
		if !tc.found {
			if waxerr.CodeOf(err) != waxerr.CodeMalformedInput {
				t.Errorf("%s: the open returned %v, want a refusal", tc.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		got := readAll(t, d)
		if len(got) != len(want) {
			t.Errorf("%s: %d packets, the stream alone reads %d", tc.name, len(got), len(want))
			continue
		}
		for i := range got {
			if string(got[i].Data) != string(want[i].Data) || got[i].PTS != want[i].PTS {
				t.Errorf("%s: packet %d differs from the stream's own", tc.name, i)
				break
			}
		}
	}
}

// TestShortJunkReadsSpansAndNoWindow: short junk in front of the magic costs
// about twice the junk, in spans, and never a window.
func TestShortJunkReadsSpansAndNoWindow(t *testing.T) {
	for _, n := range []int{1000, 3000, 7000} {
		if _, read := magicScan.Cost(t, magicScan.Junk(n, 0)); read > int64(2*n+1024) {
			t.Errorf("%d bytes of junk in front of the magic read %d bytes, want about twice the junk", n, read)
		}
	}
}

// namedStream is a stream and what a failure calls it.
type namedStream struct {
	name string
	raw  []byte
}

// longStreams are an SV8 stream and an SV7 stream of several windows each.
func longStreams() []namedStream {
	packets := [][]byte{sv8Packet("SH", shPayload(sv8Header{count: 1200 * 1152, maxBand: 20, channels: 2}))}
	frames := make([]sv7Frame, 1200)
	for i := range frames {
		packets = append(packets, sv8Packet("AP", apBlock(400)))
		frames[i] = randomFrame(i, 3200)
	}
	return []namedStream{
		{"1200 SV8 blocks", buildSV8(append(packets, sv8Packet("SE", nil))...)},
		{"1200 SV7 frames", buildSV7(sv7Header{frames: 1200, maxBand: 20, gapless: true, last: 700}, frames, 700, nil)},
	}
}

// TestOpenCost pins what opening a clean stream reads: the tag probe, one
// window from the magic, then its walk, each byte once. No span is paid, and
// the trailer probes of a stream longer than a window do not move the window.
func TestOpenCost(t *testing.T) {
	tag := append([]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 1, 20}, make([]byte, 148)...)
	for _, name := range []string{"seek.mpc", "seek-sv7.mpc", "tagged.mpc", "gapless-sv7.mpc", "chapters.mpc", "seek-pns.mpc"} {
		raw := fixture(t, name)
		for _, tc := range []struct {
			name string
			raw  []byte
		}{
			{name, raw},
			{name + " behind a tag", append(append([]byte(nil), tag...), raw...)},
		} {
			src := &testutil.CountingSource{Src: container.BytesSource(tc.raw)}
			if _, err := mpc.NewDemuxer(src, nil); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			// Each fixture fits the window, so the walk reads nothing more.
			want := int64(len(raw))
			if len(tc.raw) > len(raw) {
				want += 10 // the probe, read again with nothing behind a tag
			}
			if src.Bytes != want || src.Reads != 2 {
				t.Errorf("%s: the open read %d bytes in %d reads, want %d in 2", tc.name, src.Bytes, src.Reads, want)
			}
		}
	}

	// Several windows: the tag probe, the window at the magic, the trailer
	// probe, and the walk's three windows behind the first.
	for _, s := range longStreams() {
		for _, raw := range [][]byte{s.raw, append(append([]byte(nil), tag...), s.raw...)} {
			src := &testutil.CountingSource{Src: container.BytesSource(raw), Log: true}
			if _, err := mpc.NewDemuxer(src, nil); err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			// The trailer probe's bytes are the walk's last window's too.
			if most := int64(len(raw)) + 128; src.Reads != 6 || src.Bytes > most {
				t.Errorf("%s, %d bytes: the open read %d bytes in %d reads, want at most %d in 6: %v",
					s.name, len(raw), src.Bytes, src.Reads, most, src.Spans)
				continue
			}
			readsTheHeadOnce(t, s.name, src.Spans)
		}
	}
}

// readsTheHeadOnce fails where a read behind the window at the magic, the
// open's second read, starts inside it.
func readsTheHeadOnce(t *testing.T, name string, spans [][2]int64) {
	t.Helper()
	for _, s := range spans[2:] {
		if head := spans[1]; s[0] < head[1] {
			t.Errorf("%s: the read at %d starts inside the head window %v, read already: %v", name, s[0], head, spans)
		}
	}
}

// TestATrailerBehindALongStreamIsPeeledOffTheHeadWindow: behind a stream of
// several windows the trailer probes read through a window of their own. It
// reads the tags and takes them off the stream, and the head is read once.
func TestATrailerBehindALongStreamIsPeeledOffTheHeadWindow(t *testing.T) {
	tagged := fixture(t, "tagged.mpc")
	trailer := tagged[bytes.Index(tagged, []byte("APETAGEX")):] // an APEv2 block, then ID3v1
	for _, s := range longStreams() {
		counted := func(raw []byte) (*mpc.Demuxer, *testutil.CountingSource) {
			src := &testutil.CountingSource{Src: container.BytesSource(raw), Log: true}
			d, err := mpc.NewDemuxer(src, nil)
			if err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			return d, src
		}
		bare, bareSrc := counted(s.raw)
		raw := append(append([]byte(nil), s.raw...), trailer...)
		d, src := counted(raw)
		tags := d.Tags()
		for key, want := range map[string]string{"ARTIST": "Wax Test", "ALBUM": "Fixtures", "TITLE": "Tagged", "TRACKNUMBER": "3"} {
			if got := tags[key]; len(got) != 1 || got[0] != want {
				t.Errorf("%s: %s = %v, want %q", s.name, key, got, want)
			}
		}
		if got, want := d.Tracks()[0], bare.Tracks()[0]; got.Samples != want.Samples || got.SamplesExact != want.SamplesExact ||
			!slices.Equal(d.Warnings(), bare.Warnings()) {
			t.Errorf("%s: %d samples (exact %v) and warnings %v behind the trailer, want the stream's own %d (exact %v) and %v",
				s.name, got.Samples, got.SamplesExact, d.Warnings(), want.Samples, want.SamplesExact, bare.Warnings())
		}
		// Four reads more than the stream alone: a probe for each tag, and
		// the APEv2 block's header and its fields.
		if most := int64(len(raw)) + 128; src.Reads != bareSrc.Reads+4 || src.Bytes > most {
			t.Errorf("%s: the open read %d bytes in %d reads, want at most %d in %d: %v",
				s.name, src.Bytes, src.Reads, most, bareSrc.Reads+4, src.Spans)
		}
		readsTheHeadOnce(t, s.name, src.Spans)
		if got, want := len(readAll(t, d)), len(readAll(t, bare)); got != want {
			t.Errorf("%s: %d packets behind the trailer, the stream alone reads %d", s.name, got, want)
		}
	}
}

// TestAFailedTrailerProbeFailsTheOpen: a trailer probe whose read fails is
// the open's error. Behind each stream are more bytes than a window, so the
// walk, which ends at the stream's own end, never reads where the probes do.
func TestAFailedTrailerProbeFailsTheOpen(t *testing.T) {
	for _, s := range longStreams() {
		raw := append(s.raw, make([]byte, 200<<10)...)
		src := &testutil.FailingSource{Src: container.BytesSource(raw), From: int64(len(raw)) - 8}
		if _, err := mpc.NewDemuxer(src, nil); waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable || src.Failed == 0 {
			t.Errorf("%s: the open returned %v after %d failed reads, want the read's error", s.name, err, src.Failed)
		}
	}
}

// TestAReadThatFailsInAScanIsNeverARefusedStream: a read that fails while
// the scan is over junk ends in the read's error. The junk is never called a
// file that is not Musepack when the source did not let the scan finish.
func TestAReadThatFailsInAScanIsNeverARefusedStream(t *testing.T) {
	junk := magicScan.Junk(200<<10, 100)
	raw := append(junk, fixture(t, "seek-pns.mpc")...)
	// No cut short of the magic's four bytes lets a scan find it.
	for _, cut := range []int64{10, 11, 1024, 1025, 3072, 100 << 10, srcwin.Chunk, srcwin.Chunk + 1, int64(len(junk)), int64(len(junk)) + 3} {
		for _, strict := range []bool{false, true} {
			name := fmt.Sprintf("reads failing from %d, strict %v", cut, strict)
			src := &testutil.FailingSource{Src: container.BytesSource(raw), From: cut}
			if _, err := mpc.NewDemuxer(src, &container.DemuxerOptions{Strict: strict}); waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
				t.Errorf("%s: the open returned %v, want the read's error", name, err)
			}
			if _, _, err := mpc.FindMagicForTest(src); waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
				t.Errorf("%s: the scan returned %v, want the read's error", name, err)
			}
		}
	}
}
