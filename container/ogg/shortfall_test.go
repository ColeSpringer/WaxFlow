package ogg

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// oggFLACFrames is a stream of one-frame pages, 4096 samples each, numbered
// as given, declaring total.
func oggFLACFrames(t testing.TB, total int64, numbers ...uint64) []byte {
	t.Helper()
	const bs = 4096
	stream := buildPage(flagBOS, 0, 77, 0, flacBOSPacket())
	stream = append(stream, buildPage(0, 0, 77, 1, commentPacket())...)
	for i, n := range numbers {
		flags := byte(0)
		if i == len(numbers)-1 {
			flags = flagEOS
		}
		stream = append(stream, buildPage(flags, int64(n+1)*bs, 77, uint32(i+2), buildFrameBytes(t, n, bs))...)
	}
	return patchOggFLACTotalB(t, stream, total)
}

// lostToAHole is ten frames whose sixth page is damaged and whose last four
// are numbered 700 frames on: more than the damaged bytes could have held,
// so a read reports nothing lost there and ends short of the closing frame.
func lostToAHole(t testing.TB) (raw []byte, total, reached int64) {
	t.Helper()
	const bs = 4096
	raw = oggFLACFrames(t, 710*bs, 0, 1, 2, 3, 4, 5, 706, 707, 708, 709)
	return testutil.DamageOggPage(t, raw, 7), 710 * bs, 9 * bs
}

// readCount reads d to its end and returns what its packets hold: their
// durations and the samples holes took.
func readCount(t *testing.T, d *Demuxer) int64 {
	t.Helper()
	var n int64
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return n
		}
		if err != nil {
			t.Fatalf("the read ended %v", err)
		}
		n += pkt.Dur + pkt.Lost
	}
}

func exactAt(t *testing.T, d *Demuxer, samples int64) {
	t.Helper()
	if tr := d.Tracks()[0]; tr.Samples != samples || !tr.SamplesExact {
		t.Fatalf("the track says %d samples, exact %v, want %d exact", tr.Samples, tr.SamplesExact, samples)
	}
}

// settled fails unless d's track says samples and no longer calls them
// exact, counts truncated past them, and lists one Damage naming want.
func settled(t *testing.T, d *Demuxer, samples, truncated int64, want string) {
	t.Helper()
	if tr := d.Tracks()[0]; tr.Samples != samples || tr.SamplesExact {
		t.Fatalf("the track says %d samples, exact %v, want %d and not exact", tr.Samples, tr.SamplesExact, samples)
	}
	if got := container.TruncatedOf(d, 0); got != truncated {
		t.Errorf("%d samples counted truncated, want %d", got, truncated)
	}
	n := 0
	for _, w := range d.Warnings() {
		if strings.Contains(w.Msg, want) {
			n++
			if w.Kind != container.Damage || w.Offset != d.firstData {
				t.Errorf("the shortfall is %+v, want Damage at the first audio page, %d", w, d.firstData)
			}
		}
	}
	if n != 1 {
		t.Errorf("warnings %+v, want one naming %q", d.Warnings(), want)
	}
	si, err := flac.ParseStreamInfo(d.Tracks()[0].CodecConfig)
	if err != nil || si.Samples != samples {
		t.Errorf("the STREAMINFO handed on states %d samples (%v), want %d", si.Samples, err, samples)
	}
}

// TestAnOggFLACReadEndingShortOfTheLengthSettlesIt: a FLAC read whose clean
// end falls short of the exact length the open read off the closing frame
// reports the shortfall, once, adopts what the packets hold, counts the rest
// truncated, and no longer calls the length exact. A seek reaches as far as
// it did before the read.
func TestAnOggFLACReadEndingShortOfTheLengthSettlesIt(t *testing.T) {
	raw, total, reached := lostToAHole(t)
	open := func() *Demuxer {
		d, err := NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		exactAt(t, d, total)
		return d
	}
	want := fmt.Sprintf("STREAMINFO declares %d samples but the frames end at %d", total, reached)
	d := open()
	if n := readCount(t, d); n != reached {
		t.Fatalf("read %d samples, want %d", n, reached)
	}
	settled(t, d, reached, total-reached, want)
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != io.EOF {
		t.Fatalf("a read past the end returned %v", err)
	}
	settled(t, d, reached, total-reached, want)
	// The frames past the hole are where a seek found them before the read.
	const target = 708 * 4096
	if landed, err := d.SeekSample(context.Background(), 0, target); err != nil || landed != target {
		t.Fatalf("a seek to %d after the read landed at %d (%v)", target, landed, err)
	}
	if n := readCount(t, d); n != 2*4096 {
		t.Errorf("read %d samples from the seek, want the two frames behind it", n)
	}
	settled(t, d, reached, total-reached, want)

	// A read from a landing past the first packet judges nothing; one from
	// a landing on it settles the same.
	d = open()
	if landed, err := d.SeekSample(context.Background(), 0, 3*4096); err != nil || landed != 3*4096 {
		t.Fatalf("a seek landed at %d (%v)", landed, err)
	}
	if n := readCount(t, d); n != reached-3*4096 {
		t.Fatalf("read %d samples from the seek", n)
	}
	exactAt(t, d, total)
	if got := container.TruncatedOf(d, 0); got != 0 {
		t.Errorf("%d samples counted truncated by a read from a landing past the first packet, want none", got)
	}
	if landed, err := d.SeekSample(context.Background(), 0, 100); err != nil || landed != 0 {
		t.Fatalf("a seek landed at %d (%v)", landed, err)
	}
	if n := readCount(t, d); n != reached {
		t.Fatalf("read %d samples from the start", n)
	}
	settled(t, d, reached, total-reached, want)
}

// TestAnOggFLACReadPastTheLengthTakesTheClaimBack: frames that step back
// hold more than the closing frame's end says, so a whole read notes it and
// leaves the length as it was and no longer exact.
func TestAnOggFLACReadPastTheLengthTakesTheClaimBack(t *testing.T) {
	const bs = 4096
	d, err := NewDemuxer(container.BytesSource(oggFLACFrames(t, 4*bs, 0, 1, 2, 1, 2, 3)), nil)
	if err != nil {
		t.Fatal(err)
	}
	exactAt(t, d, 4*bs)
	if n := readCount(t, d); n != 6*bs {
		t.Fatalf("read %d samples, want %d", n, 6*bs)
	}
	want := []container.Warning{{Offset: d.firstData, Kind: container.Note, Msg: "STREAMINFO declares 16384 samples but the frames hold 24576"}}
	if tr := d.Tracks()[0]; tr.Samples != 4*bs || tr.SamplesExact || container.TruncatedOf(d, 0) != 0 || !slices.Equal(d.Warnings(), want) {
		t.Errorf("the track says %d samples, exact %v, %d truncated, warnings %v; want %d, not exact, none, and the overrun reported",
			tr.Samples, tr.SamplesExact, container.TruncatedOf(d, 0), d.Warnings(), 4*bs)
	}
	// Strict reads to the end as well, with the note.
	ds, err := NewDemuxer(container.BytesSource(oggFLACFrames(t, 4*bs, 0, 1, 2, 1, 2, 3)), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	for err == nil {
		err = ds.ReadPacket(&pkt)
	}
	if err != io.EOF || !slices.Equal(ds.Warnings(), want) {
		t.Errorf("a Strict read ended %v with %v, want the end and the note", err, ds.Warnings())
	}
}

// TestAnOggFLACSecondPassReadsAsTheFirst: a hole is held to the length the
// open called exact on every pass, so a read after one that took the claim
// back delivers what that one did.
func TestAnOggFLACSecondPassReadsAsTheFirst(t *testing.T) {
	const bs = 4096
	// Two frames numbered past the closing frame: joined on, not a hole.
	d, err := NewDemuxer(container.BytesSource(oggFLACFrames(t, 5*bs, 0, 1, 2, 500, 501, 3, 4)), nil)
	if err != nil {
		t.Fatal(err)
	}
	exactAt(t, d, 5*bs)
	for pass := range 2 {
		if n := readCount(t, d); n != 7*bs {
			t.Fatalf("pass %d read %d samples, want the %d its seven frames hold", pass, n, 7*bs)
		}
		if tr := d.Tracks()[0]; tr.Samples != 5*bs || tr.SamplesExact {
			t.Fatalf("pass %d: the track says %d samples, exact %v, want %d and not exact", pass, tr.Samples, tr.SamplesExact, 5*bs)
		}
		if landed, err := d.SeekSample(context.Background(), 0, 0); err != nil || landed != 0 {
			t.Fatalf("a seek to 0 landed at %d (%v)", landed, err)
		}
	}
}

// TestAWholeOggStreamSettlesNothing: a read to the end of an intact stream of
// any mapping, from its start or from a seek, leaves the track as it opened
// and warns nothing.
func TestAWholeOggStreamSettlesNothing(t *testing.T) {
	inputs := map[string][]byte{
		"sine-s16.oga":         fixture(t, "sine-s16.oga"),
		"noise-s24.oga":        fixture(t, "noise-s24.oga"),
		"a frame a page":       synthOggFLAC(t, 40),
		"numbered from past 0": synthOggFLACFrom(t, 40, 1000),
		"a hole inside":        testutil.DamageOggPage(t, synthOggFLAC(t, 40), 12),
		"a hole at the start":  testutil.DamageOggPage(t, synthOggFLAC(t, 40), 2),
		"an old-format stream": oldFormatStream(t),
		"vorbis":               fixtureLocal(t, "golden-vorbis.ogg"),
		"opus":                 fixtureLocal(t, "golden-opus.opus"),
	}
	for name, raw := range inputs {
		for _, from := range []int64{-1, 0, 5000} {
			d, err := NewDemuxer(container.BytesSource(raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			want, warned := d.Tracks()[0], len(d.Warnings())
			if from >= 0 {
				if _, err := d.SeekSample(context.Background(), 0, from); err != nil {
					t.Fatal(err)
				}
			}
			readCount(t, d)
			got := d.Tracks()[0]
			if got.Samples != want.Samples || got.SamplesExact != want.SamplesExact || string(got.CodecConfig) != string(want.CodecConfig) ||
				container.TruncatedOf(d, 0) != 0 || len(d.Warnings()) > warned+strings.Count(name, "hole") {
				t.Errorf("%s from %d: the track says %d samples, exact %v, %d truncated, warnings %v; it opened at %d, exact %v",
					name, from, got.Samples, got.SamplesExact, container.TruncatedOf(d, 0), d.Warnings(), want.Samples, want.SamplesExact)
			}
		}
	}
}

// oldFormatStream is three frames coding sample numbers with the
// variable-blocksize bit clear, under unequal STREAMINFO block bounds.
func oldFormatStream(t *testing.T) []byte {
	t.Helper()
	stream := buildPage(flagBOS, 0, 55, 0, flacBOSPacketBlocks(1000, 4096))
	stream = append(stream, buildPage(0, 0, 55, 1, commentPacket())...)
	for i, f := range []struct {
		coded uint64
		bs    int
	}{{0, 4096}, {4096, 2304}, {6400, 2304}} {
		flags := byte(0)
		if i == 2 {
			flags = flagEOS
		}
		stream = append(stream, buildPage(flags, int64(f.coded)+int64(f.bs), 55, uint32(i+2), buildFrameBytes(t, f.coded, f.bs))...)
	}
	return stream
}

// fixtureLocal reads a file of the package's own testdata.
func fixtureLocal(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// flakySource counts its reads and fails the failAt'th and the one behind
// it, which is the window's second try, and no other.
type flakySource struct {
	container.Source
	reads, failAt int
}

func (s *flakySource) ReadAt(p []byte, off int64) (int, error) {
	if s.reads++; s.failAt > 0 && (s.reads == s.failAt || s.reads == s.failAt+1) {
		return 0, testutil.ErrSourceDown
	}
	return s.Source.ReadAt(p, off)
}

// TestAFailedSeekLeavesTheLengthAlone: a seek that fails in its walk leaves
// the read somewhere no count describes, so the read behind it judges
// nothing.
func TestAFailedSeekLeavesTheLengthAlone(t *testing.T) {
	raw := synthOggFLAC(t, 12000)
	const target = 9000 * 4096
	src := &flakySource{Source: container.BytesSource(raw)}
	d, err := NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	opened := src.reads
	if _, err := d.SeekSample(context.Background(), 0, target); err != nil {
		t.Fatal(err)
	}
	// The same seek on a fresh open, its last read failing: the walk's.
	src = &flakySource{Source: container.BytesSource(raw), failAt: src.reads}
	if d, err = NewDemuxer(src, nil); err != nil || src.reads != opened {
		t.Fatalf("a second open read %d times (%v), the first %d", src.reads, err, opened)
	}
	want := d.Tracks()[0]
	if _, err := d.SeekSample(context.Background(), 0, target); waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
		t.Fatalf("the seek returned %v, want its failed read", err)
	}
	if n := readCount(t, d); n == 0 || n >= want.Samples {
		t.Fatalf("the read behind the failed seek delivered %d samples of %d: this holds nothing", n, want.Samples)
	}
	if got := d.Tracks()[0]; got.Samples != want.Samples || !got.SamplesExact || container.TruncatedOf(d, 0) != 0 {
		t.Errorf("after it the track says %d samples, exact %v, %d truncated, warnings %v; want it as it opened",
			got.Samples, got.SamplesExact, container.TruncatedOf(d, 0), d.Warnings())
	}
}

// TestAnOggFLACHolePastTheLengthIsJoined: a frame behind a damaged page that
// its own number places past the exact length is joined on with nothing
// reported lost, so no silence pushes the frames behind it past where a
// decode is cut.
func TestAnOggFLACHolePastTheLengthIsJoined(t *testing.T) {
	const bs = 4096
	raw := testutil.DamageOggPage(t, oggFLACFrames(t, 10*bs, 0, 1, 2, 3, 4, 50, 51, 7, 8, 9), 6)
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	exactAt(t, d, 10*bs)
	var pkt container.Packet
	var n, lost int64
	for d.ReadPacket(&pkt) == nil {
		n, lost = n+pkt.Dur, lost+pkt.Lost
	}
	if n != 9*bs || lost != 0 {
		t.Errorf("read %d samples and %d lost, want the nine frames' %d and none", n, lost, 9*bs)
	}
	settled(t, d, 9*bs, bs, fmt.Sprintf("STREAMINFO declares %d samples but the frames end at %d", 10*bs, 9*bs))
}

// TestAnOggFLACFirstFrameNumberedPastTheLengthIsNoHoleFromZero: a first
// frame read behind a damaged page whose number puts it at or past where
// the closing frame ends is not owed a hole from sample 0, whose silence a
// decode would be cut in. It is joined on at 0, from the open and from a
// seek, and the stream's own frames follow at their numbers.
func TestAnOggFLACFirstFrameNumberedPastTheLengthIsNoHoleFromZero(t *testing.T) {
	const bs = 4096
	raw := testutil.DamageOggPage(t, oggFLACFrames(t, 10*bs, 0, 51, 52, 3, 4, 5, 6, 7, 8, 9), 2)
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	exactAt(t, d, 10*bs)
	for pass := range 2 {
		var pkt container.Packet
		var got []timed
		for d.ReadPacket(&pkt) == nil {
			got = append(got, timed{pkt.PTS, pkt.Dur, pkt.Lost})
		}
		if len(got) != 9 || got[0] != (timed{0, bs, 0}) || got[1] != (timed{bs, bs, 0}) || got[2] != (timed{3 * bs, bs, bs}) {
			t.Fatalf("pass %d read %+v, want nine frames in order, the two joined on from 0", pass, got)
		}
		if landed, err := d.SeekSample(context.Background(), 0, 100); err != nil || landed != 0 {
			t.Fatalf("a seek to 100 landed at %d (%v), want the first frame", landed, err)
		}
	}
	found := false
	for _, w := range d.Warnings() {
		found = found || strings.Contains(w.Msg, "numbered past the frames that follow, joined")
		if strings.Contains(w.Msg, "took") {
			t.Errorf("listed %q: no hole was taken", w.Msg)
		}
	}
	if !found {
		t.Errorf("warnings %v, want the first frame reported as joined", d.Warnings())
	}
}

// TestAnOggFLACStrayFirstFrameDoesNotDisownTheStream: a first frame read
// behind a damaged page and numbered above the frames that follow is a
// stray, not where the stream starts: it is joined on at 0, and the frames
// numbered below it are the stream's, not dropped as another's.
func TestAnOggFLACStrayFirstFrameDoesNotDisownTheStream(t *testing.T) {
	const bs = 4096
	raw := testutil.DamageOggPage(t, oggFLACFrames(t, 10*bs, 0, 501, 502, 3, 4, 5, 6, 7, 8, 9), 2)
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	exactAt(t, d, 10*bs)
	var pkt container.Packet
	var got []timed
	for d.ReadPacket(&pkt) == nil {
		got = append(got, timed{pkt.PTS, pkt.Dur, pkt.Lost})
	}
	if len(got) != 9 || got[0] != (timed{0, bs, 0}) || got[2] != (timed{3 * bs, bs, bs}) || got[8] != (timed{9 * bs, bs, 0}) {
		t.Fatalf("read %+v, want nine frames in order, the two joined on from 0", got)
	}
	for _, w := range d.Warnings() {
		if strings.Contains(w.Msg, "is not a valid") {
			t.Errorf("listed %q: a frame of the stream was dropped", w.Msg)
		}
	}
	// With no damage in front of it, a first frame is where the stream
	// starts, and frames numbered below it are not its own, as before.
	clean, err := NewDemuxer(container.BytesSource(oggFLACFrames(t, 10*bs, 501, 502, 3, 4)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := readCount(t, clean); n != 2*bs {
		t.Errorf("an undamaged stream that starts at frame 501 read %d samples, want its two frames' %d", n, 2*bs)
	}
}

// TestAnOggFLACFrameOutOfOrderIsNoHole: two frames the other way round lose
// nothing, so no silence stands in for either, which a decode cut at the
// exact length would pay for with the stream's last frames. They are read
// in order, and a seek into either lands on it with the other not repeated.
func TestAnOggFLACFrameOutOfOrderIsNoHole(t *testing.T) {
	const bs = 4096
	d, err := NewDemuxer(container.BytesSource(oggFLACFrames(t, 7*bs, 0, 1, 2, 4, 3, 5, 6)), nil)
	if err != nil {
		t.Fatal(err)
	}
	exactAt(t, d, 7*bs)
	var pkt container.Packet
	var n, lost int64
	var order []int64
	for d.ReadPacket(&pkt) == nil {
		n, lost, order = n+pkt.Dur, lost+pkt.Lost, append(order, pkt.PTS/bs)
		if want := buildFrameBytes(t, uint64(pkt.PTS/bs), bs); string(pkt.Data) != string(want) {
			t.Errorf("the packet at %d is not its frame's bytes", pkt.PTS)
		}
	}
	if n != 7*bs || lost != 0 || !slices.Equal(order, []int64{0, 1, 2, 3, 4, 5, 6}) {
		t.Errorf("read %d samples, %d lost, frames %v; want all seven in order and nothing lost", n, lost, order)
	}
	exactAt(t, d, 7*bs)
	if container.DamageOf(d) != 1 {
		t.Errorf("findings %v, want the one frame out of order", d.Warnings())
	}
	for _, tc := range []struct {
		target int64
		want   []int64
	}{{3*bs + 5, []int64{2, 3, 4, 5, 6}}, {4*bs + 5, []int64{4, 5, 6}}, {2*bs + 5, []int64{2, 3, 4, 5, 6}}, {5*bs + 5, []int64{5, 6}}} {
		landed, err := d.SeekSample(context.Background(), 0, tc.target)
		if err != nil || landed != tc.want[0]*bs {
			t.Fatalf("a seek to %d landed at %d (%v), want %d", tc.target, landed, err, tc.want[0]*bs)
		}
		order = order[:0]
		for d.ReadPacket(&pkt) == nil {
			if order = append(order, pkt.PTS/bs); pkt.Lost != 0 {
				t.Errorf("behind a seek to %d the frame at %d owes %d samples, want none", tc.target, pkt.PTS, pkt.Lost)
			}
		}
		if !slices.Equal(order, tc.want) {
			t.Errorf("behind a seek to %d the read holds frames %v, want %v", tc.target, order, tc.want)
		}
	}
	// A frame gone without its number is still a hole.
	d, err = NewDemuxer(container.BytesSource(oggFLACFrames(t, 7*bs, 0, 1, 2, 4, 5, 6)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := readCount(t, d); got != 7*bs {
		t.Errorf("with a frame gone a read holds %d samples, want %d with the hole", got, 7*bs)
	}
}

// TestASeekWalkBehindASwapStartsBeforeIt: where a bisection hands the walk
// the page of the frame due before the one ahead of it, the frame the
// target lies in is behind the walk's start, so the walk starts further back
// and lands on it, and nothing behind the landing is owed.
func TestASeekWalkBehindASwapStartsBeforeIt(t *testing.T) {
	const bs, n = 4096, 5000
	numbers := make([]uint64, n)
	for i := range numbers {
		numbers[i] = uint64(i)
	}
	plain, err := NewDemuxer(container.BytesSource(oggFLACFrames(t, n*bs, numbers...)), nil)
	if err != nil {
		t.Fatal(err)
	}
	// A k whose target, in frame k+1, the bisection hands the page frame k+1
	// starts on; swapped, that page holds frame k, and frame k+1 is before it.
	k := -1
	for c := n / 2; c < n-4 && k < 0; c++ {
		lo, err := plain.bisectFrames(t.Context(), int64(c+1)*bs+5)
		if err != nil {
			t.Fatal(err)
		}
		if off, pts, ok, err := plain.frameAt(lo, plain.totalSize); err == nil && ok && off == lo && pts == int64(c+1)*bs && lo > plain.firstData {
			k = c
		}
	}
	if k < 0 {
		t.Fatal("no bisection lands on the target's own page")
	}
	numbers[k], numbers[k+1] = numbers[k+1], numbers[k]
	d, err := NewDemuxer(container.BytesSource(oggFLACFrames(t, n*bs, numbers...)), nil)
	if err != nil {
		t.Fatal(err)
	}
	target := int64(k+1)*bs + 5
	if landed, err := d.SeekSample(context.Background(), 0, target); err != nil || landed != int64(k+1)*bs {
		t.Fatalf("a seek to %d landed at %d (%v), want %d", target, landed, err, int64(k+1)*bs)
	}
	var pkt container.Packet
	for i := int64(k + 1); i < int64(k+4); i++ {
		if err := d.ReadPacket(&pkt); err != nil || pkt.PTS != i*bs || pkt.Lost != 0 {
			t.Fatalf("behind the seek the packet at %d owes %d (%v), want frame %d owing nothing", pkt.PTS, pkt.Lost, err, i)
		}
	}
}

// TestAnOggFLACSwapBehindDamageIsReadInOrder: two frames out of order behind
// bytes that are no page are read in order too: the frame due is the next
// packet, so nothing is lost, whatever came before it.
func TestAnOggFLACSwapBehindDamageIsReadInOrder(t *testing.T) {
	const bs = 4096
	raw := oggFLACFrames(t, 7*bs, 0, 1, 2, 4, 3, 5, 6)
	at := 0 // frame 4's page, the sixth
	for range 5 {
		at += bytes.Index(raw[at+1:], []byte("OggS")) + 1
	}
	raw = slices.Concat(raw[:at], bytes.Repeat([]byte{0x11}, 200), raw[at:])
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	var order []int64
	var lost int64
	for d.ReadPacket(&pkt) == nil {
		order, lost = append(order, pkt.PTS/bs), lost+pkt.Lost
	}
	if !slices.Equal(order, []int64{0, 1, 2, 3, 4, 5, 6}) || lost != 0 {
		t.Errorf("read frames %v, %d lost; want all seven in order and nothing lost", order, lost)
	}
}
