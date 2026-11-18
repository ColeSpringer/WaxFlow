package ogg

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// oggFLACBlocks is a stream of one-frame pages under the given STREAMINFO
// block size bounds, each frame of the size and number its entry gives.
func oggFLACBlocks(t *testing.T, minBlock, maxBlock int, frames ...[2]uint64) []byte {
	t.Helper()
	stream := buildPage(flagBOS, 0, 77, 0, flacBOSPacketBlocks(minBlock, maxBlock))
	stream = append(stream, buildPage(0, 0, 77, 1, commentPacket())...)
	end := int64(0)
	for i, f := range frames {
		flags := byte(0)
		if i == len(frames)-1 {
			flags = flagEOS
		}
		end += int64(f[1])
		stream = append(stream, buildPage(flags, end, 77, uint32(i+2), buildFrameBytes(t, f[0], int(f[1])))...)
	}
	return stream
}

// fourFrames is frames numbered 0 to 3, three of 4096 samples and a short
// last one.
var fourFrames = [][2]uint64{{0, 4096}, {1, 4096}, {2, 4096}, {3, 314}}

type timed struct{ pts, dur, lost int64 }

// packetsOf opens raw and reads it to the end.
func packetsOf(t *testing.T, raw []byte) (*Demuxer, container.Track, []timed) {
	t.Helper()
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := d.Tracks()[0]
	var out []timed
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return d, tr, out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, timed{pkt.PTS, pkt.Dur, pkt.Lost})
	}
}

// TestOggFLACDamagedBlockBoundsDoNotRenumberTheFrames: block size bounds no
// stream may state, unequal ones over frames that count up by one, and a
// stated block size the frames contradict, all leave a fixed-blocksize
// stream the frames it is.
func TestOggFLACDamagedBlockBoundsDoNotRenumberTheFrames(t *testing.T) {
	want := []timed{{0, 4096, 0}, {4096, 4096, 0}, {8192, 4096, 0}, {12288, 314, 0}}
	for _, b := range [][2]int{{4608, 256}, {8, 4096}, {2048, 4096}, {4096, 8192}, {256, 256}, {8192, 8192}, {4096, 4096}} {
		d, tr, got := packetsOf(t, oggFLACBlocks(t, b[0], b[1], fourFrames...))
		if tr.Samples != 12602 || !tr.SamplesExact || len(d.Warnings()) != 0 || len(got) != 4 {
			t.Errorf("bounds %d..%d: %d samples, exact %v, %d packets, warnings %v; want 12602 exact in 4 and none",
				b[0], b[1], tr.Samples, tr.SamplesExact, len(got), d.Warnings())
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("bounds %d..%d: packet %d is %+v, want %+v", b[0], b[1], i, got[i], want[i])
			}
		}
		if landed, err := d.SeekSample(context.Background(), 0, 9000); err != nil || landed != 8192 {
			t.Errorf("bounds %d..%d: a seek to 9000 landed at %d (%v), want 8192", b[0], b[1], landed, err)
		}
	}
}

// TestOggFLACNumberingSettledByTheSecondFrameMovesTheOrigin: positions run
// from the first frame under the numbering the second frame settled, not
// the one the first alone suggested.
func TestOggFLACNumberingSettledByTheSecondFrameMovesTheOrigin(t *testing.T) {
	d, tr, got := packetsOf(t, oggFLACBlocks(t, 2048, 4096, [2]uint64{5, 4096}, [2]uint64{6, 4096}, [2]uint64{7, 4096}, [2]uint64{8, 314}))
	want := []timed{{0, 4096, 0}, {4096, 4096, 0}, {8192, 4096, 0}, {12288, 314, 0}}
	// It notes the late start, and nothing is damaged.
	if tr.Samples != 12602 || !tr.SamplesExact || len(got) != 4 || container.DamageOf(d) != 0 {
		t.Fatalf("%d samples, exact %v, %d packets, warnings %v; want 12602 exact in 4", tr.Samples, tr.SamplesExact, len(got), d.Warnings())
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("packet %d is %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestOggFLACHeadDamageLeavesTheShortLastFrameWhereItBelongs: with the pages
// in front of the short last frame damaged, that frame sits where
// STREAMINFO's block size puts its number and owes the samples in front.
func TestOggFLACHeadDamageLeavesTheShortLastFrameWhereItBelongs(t *testing.T) {
	raw := oggFLACBlocks(t, 4096, 4096, fourFrames...)
	raw = patchOggFLACTotalB(t, raw, 12602)
	for page := 2; page < 5; page++ {
		raw = testutil.DamageOggPage(t, raw, page)
	}
	d, tr, got := packetsOf(t, raw)
	if tr.Samples != 12602 || !tr.SamplesExact || container.TruncatedOf(d, 0) != 0 {
		t.Errorf("the track says %d samples, exact %v, %d truncated; want the stream's 12602 exact", tr.Samples, tr.SamplesExact, container.TruncatedOf(d, 0))
	}
	if len(got) != 1 || got[0] != (timed{12288, 314, 12288}) {
		t.Fatalf("read %+v, want the closing frame at 12288 owing 12288", got)
	}
	for _, c := range [][2]int64{{0, 0}, {6000, 0}, {12288, 12288}, {12601, 12288}} {
		if landed, err := d.SeekSample(context.Background(), 0, c[0]); err != nil || landed != c[1] {
			t.Errorf("a seek to %d landed at %d (%v), want %d", c[0], landed, err, c[1])
		}
	}

	// A short frame with a damaged page behind it is not known to be the
	// last, so it is placed in its own size, as it was.
	// So is one whose successor lies past what the look reads: pages of
	// another stream, more than the look's reach of them, in between.
	raw = buildPage(flagBOS, 0, 77, 0, flacBOSPacket())
	raw = append(raw, buildPage(0, 0, 77, 1, commentPacket())...)
	for i, f := range [][2]uint64{{0, 4096}, {1, 4096}, {2, 314}} {
		raw = append(raw, buildPage(0, int64(i+1)*4096, 77, uint32(i+2), buildFrameBytes(t, f[0], int(f[1])))...)
	}
	raw = testutil.DamageOggPage(t, testutil.DamageOggPage(t, raw, 2), 3)
	for i := range 3 {
		raw = append(raw, buildPage(0, 0, 99, uint32(i), make([]byte, 60000))...)
	}
	raw = append(raw, buildPage(flagEOS, 4*4096, 77, 5, buildFrameBytes(t, 3, 4096))...)
	if _, _, got = packetsOf(t, raw); len(got) == 0 || got[0].pts != 2*314 || got[0].dur != 314 {
		t.Errorf("with its successor past the look's reach the short frame reads %+v, want it at 628 as it was", got)
	}
	for _, behind := range [][][2]uint64{{{3, 4096}}, {{3, 4096}, {4, 4096}}} {
		raw = oggFLACBlocks(t, 4096, 4096, append([][2]uint64{{0, 4096}, {1, 4096}, {2, 314}}, behind...)...)
		for _, page := range []int{2, 3, 5} {
			raw = testutil.DamageOggPage(t, raw, page)
		}
		if _, _, got = packetsOf(t, raw); len(got) == 0 || got[0].pts != 2*314 || got[0].dur != 314 {
			t.Errorf("with a damaged page and %d more behind it the short frame reads %+v, want it at 628 as it was", len(behind)-1, got)
		}
	}
}

// manyOldFormat is n one-frame pages of an old-format stream: sample
// numbers under the fixed blocking strategy bit and unequal bounds, in
// frames of 16 samples.
func manyOldFormat(t *testing.T, n int) []byte {
	t.Helper()
	frames := make([][2]uint64, n)
	for i := range frames {
		frames[i] = [2]uint64{uint64(i) * 16, 16}
	}
	return oggFLACBlocks(t, 16, 4096, frames...)
}

// TestTheOggNumberingLookLeavesNothingBehind: settling the numbering by the
// frame behind the first reads one packet ahead and puts the read back, so
// an old-format stream reads from its first frame, and the open pays for
// no more than the page that packet completes on.
func TestTheOggNumberingLookLeavesNothingBehind(t *testing.T) {
	const n = 4000
	cost := func(raw []byte) int64 {
		src := &testutil.CountingSource{Src: container.BytesSource(raw)}
		if _, err := NewDemuxer(src, nil); err != nil {
			t.Fatal(err)
		}
		return src.Bytes
	}
	old, plain := manyOldFormat(t, n), synthOggFLAC(t, n)
	if len(old) < maxPageSize+64<<10+4096 {
		t.Fatalf("%d bytes, want a stream past the tail scan's window", len(old))
	}
	if a, b := cost(old), cost(plain); a < b-4096 || a > b+firstSpan+4096 {
		t.Errorf("an old-format stream opened reading %d bytes, a plain one %d: want at most a read-ahead more", a, b)
	}
	// The first packet is the first frame's bytes, whatever the look read.
	first, err := NewDemuxer(container.BytesSource(old), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	if err := first.ReadPacket(&pkt); err != nil || !bytes.Equal(pkt.Data, buildFrameBytes(t, 0, 16)) {
		t.Fatalf("the first packet is % x (%v), want the first frame", pkt.Data, err)
	}
	d, tr, got := packetsOf(t, old)
	if tr.Samples != n*16 || !tr.SamplesExact || len(got) != n || len(d.Warnings()) != 0 {
		t.Fatalf("%d samples, exact %v, %d packets, warnings %v", tr.Samples, tr.SamplesExact, len(got), d.Warnings())
	}
	for i, p := range got {
		if p != (timed{int64(i) * 16, 16, 0}) {
			t.Fatalf("packet %d is %+v", i, p)
		}
	}
}

// TestAReadThatFailsInTheOggNumberingLookIsNotTheOpensError: the look reads
// where an open read nothing before, so a read that fails there is no
// answer, and the stream opens and seeks as it did.
func TestAReadThatFailsInTheOggNumberingLookIsNotTheOpensError(t *testing.T) {
	const n = 4000
	raw := manyOldFormat(t, n)
	pages := pageStarts(t, raw)
	// The second audio page, which the look reads and the open did not.
	src := &testutil.FailingSource{Src: container.BytesSource(raw), From: int64(pages[3]), To: int64(pages[4])}
	d, err := NewDemuxer(src, nil)
	if err != nil {
		t.Fatalf("the open returned %v after %d failed reads", err, src.Failed)
	}
	if src.Failed == 0 {
		t.Fatal("no read failed at open: this holds nothing")
	}
	if tr := d.Tracks()[0]; tr.Samples != n*16 || !tr.SamplesExact {
		t.Errorf("opened at %d samples, exact %v", tr.Samples, tr.SamplesExact)
	}
	if landed, err := d.SeekSample(context.Background(), 0, 3000*16); err != nil || landed != 3000*16 {
		t.Fatalf("a seek past the unreadable bytes landed at %d (%v)", landed, err)
	}
	if got := readCount(t, d); got != 1000*16 {
		t.Errorf("read %d samples from the seek, want %d", got, 1000*16)
	}
	// A read from the start meets the unreadable bytes itself.
	d, err = NewDemuxer(&testutil.FailingSource{Src: container.BytesSource(raw), From: int64(pages[3]), To: int64(pages[4])}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil {
		t.Fatalf("the first packet: %v", err)
	}
	if err := d.ReadPacket(&pkt); waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
		t.Errorf("the second packet returned %v, want the failed read", err)
	}
}

// TestEveryOggTrackNamesTheDemuxerRevision: a FLAC, a Vorbis and an Opus
// track each carry DemuxerVersion, which is what puts the revision in every
// cache key over an Ogg source, and a read does not drop it.
func TestEveryOggTrackNamesTheDemuxerRevision(t *testing.T) {
	short, _, _ := lostToAHole(t)
	for name, raw := range map[string][]byte{
		"sine-s16.oga":           fixture(t, "sine-s16.oga"),
		"golden-flac.oga":        fixtureLocal(t, "golden-flac.oga"),
		"a read that ends short": short,
		"golden-vorbis.ogg":      fixtureLocal(t, "golden-vorbis.ogg"),
		"golden-opus.opus":       fixtureLocal(t, "golden-opus.opus"),
	} {
		d, err := NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		const want = "ogg-demux-2"
		if got := d.Tracks()[0].DemuxerVersion; got != want || want != DemuxerVersion {
			t.Errorf("%s: the %s track names demuxer revision %q, want %q", name, d.Tracks()[0].Codec, got, want)
		}
		readCount(t, d)
		if got := d.Tracks()[0].DemuxerVersion; got != want {
			t.Errorf("%s: after a read the track names %q, want %q", name, got, want)
		}
	}
}

// TestAnOggFLACLengthBehindAHeadHoleIsHeldToTheBytesTheHoleTook: the samples
// a hole at the head took are in the length, so what the bytes can hold
// counts the pages that hole took, and the length is exact.
func TestAnOggFLACLengthBehindAHeadHoleIsHeldToTheBytesTheHoleTook(t *testing.T) {
	const n = 60
	frames := make([][2]uint64, n)
	for i := range frames {
		frames[i] = [2]uint64{uint64(1000 + i), 4096}
	}
	frames[n-1][1] = 314
	raw := oggFLACBlocks(t, 4096, 4096, frames...)
	for page := 2; page < 2+n-1; page++ {
		raw = testutil.DamageOggPage(t, raw, page)
	}
	const at = (1000 + n - 1) * 4096
	d, tr, got := packetsOf(t, raw)
	if tr.Samples != at+314 || !tr.SamplesExact {
		t.Errorf("the track says %d samples, exact %v, warnings %v; want %d exact", tr.Samples, tr.SamplesExact, d.Warnings(), at+314)
	}
	if len(got) != 1 || got[0] != (timed{at, 314, at}) {
		t.Errorf("read %+v, want the closing frame at %d owing as much", got, at)
	}
}
