package ogg

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// placed is a packet's position and the hole in front of it.
type placed struct{ pts, lost int64 }

// placedBy reads d to its end and returns where each packet was placed.
func placedBy(t *testing.T, d *Demuxer) []placed {
	t.Helper()
	var out []placed
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("the read ended %v", err)
		}
		out = append(out, placed{pkt.PTS, pkt.Lost})
	}
}

// TestAnOggFLACSeekIntoTheHeadHoleReadsAsTheOpenDoes: a seek into the hole
// the stream starts with lands at 0, and what the read behind it delivers
// is what a read from the open does: the frame due in front of the first,
// lying behind it, is read first with the hole owed.
func TestAnOggFLACSeekIntoTheHeadHoleReadsAsTheOpenDoes(t *testing.T) {
	const bs = 4096
	raw := testutil.DamageOggPage(t, oggFLACFrames(t, 8*bs, 0, 2, 1, 3, 4, 5, 6, 7), 2)
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	open := placedBy(t, d)
	if len(open) == 0 || open[0] != (placed{bs, bs}) {
		t.Fatalf("a read from the open placed %v, want frame 1 among them: this pins nothing", open)
	}
	for _, target := range []int64{0, bs + 5} {
		if landed, err := d.SeekSample(context.Background(), 0, target); err != nil || landed != 0 {
			t.Fatalf("a seek to %d landed at %d (%v), want 0", target, landed, err)
		}
		if got := placedBy(t, d); !slices.Equal(got, open) {
			t.Errorf("behind a seek to %d the read placed %v, from the open %v", target, got, open)
		}
	}
}

// TestAnOggFLACSeekLandsByItsFirstWalkWhereNoReadFromFurtherBackComesToIt:
// a walk that meets a hole behind its first frame starts further back, for
// the frame due there. Where that walk meets frames joined on in front of
// the target, with nothing lost between, or the same hole behind the same
// frame, no read from there comes to the target as the first walk does,
// and the seek lands by the first walk, as it always did.
func TestAnOggFLACSeekLandsByItsFirstWalkWhereNoReadFromFurtherBackComesToIt(t *testing.T) {
	const bs, n, far = 4096, 5000, 20000
	numbers := make([]uint64, n)
	for i := range numbers {
		numbers[i] = uint64(i)
	}
	plain := oggFLACFrames(t, n*bs, numbers...)
	offs := []int{0} // where each page ends
	for _, p := range testutil.OggPages(t, plain) {
		offs = append(offs, offs[len(offs)-1]+27+len(p.Lacing)+len(p.Body))
	}
	pd, err := NewDemuxer(container.BytesSource(plain), nil)
	if err != nil {
		t.Fatal(err)
	}
	// A c whose target, in frame c+2, the bisection hands the page that frame
	// starts on.
	c := -1
	for k := n / 2; k < n-8 && c < 0; k++ {
		if lo, err := pd.bisectFrames(t.Context(), int64(k+2)*bs+5); err == nil && lo == int64(offs[k+2+2]) && lo > pd.firstData {
			c = k
		}
	}
	if c < 0 {
		t.Fatal("no bisection lands on the target's own page")
	}
	// The frame met again lies where frame c+2 lay, with frame c+3's page
	// damaged, so the target lies in the hole behind it. In front of it lie
	// frames c and c+1 numbered far past the length, which a read joins on,
	// or the two out of order, which it reads in order: from further back
	// the hole lies behind the same frame.
	for name, front := range map[string][2]uint64{
		"joined on":    {far + uint64(c), far + uint64(c) + 1},
		"out of order": {uint64(c) + 1, uint64(c)},
	} {
		raw := slices.Clone(plain)
		for i, coded := range map[int]uint64{c: front[0], c + 1: front[1], c + 2: uint64(c) + 1} {
			pg := buildPage(0, int64(coded+1)*bs, 77, uint32(i+2), buildFrameBytes(t, coded, bs))
			if len(pg) != offs[i+3]-offs[i+2] {
				t.Fatalf("frame %d's page is %d bytes numbered %d, want %d", i, len(pg), coded, offs[i+3]-offs[i+2])
			}
			copy(raw[offs[i+2]:], pg)
		}
		raw = testutil.DamageOggPage(t, raw, c+3+2)
		d, err := NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		target := int64(c+2)*bs + 5
		if lo, err := d.bisectFrames(t.Context(), target); err != nil || lo != int64(offs[c+2+2]) {
			t.Fatalf("%s: the bisection lands at %d (%v), want the frame met again at %d: this pins nothing", name, lo, err, offs[c+2+2])
		}
		if landed, err := d.SeekSample(context.Background(), 0, target); err != nil || landed != int64(c+1)*bs {
			t.Fatalf("%s: a seek to %d landed at %d (%v), want the frame met again at %d", name, target, landed, err, int64(c+1)*bs)
		}
		var pkt container.Packet
		for _, want := range []placed{{int64(c+1) * bs, 0}, {int64(c+4) * bs, 2 * bs}, {int64(c+5) * bs, 0}} {
			if err := d.ReadPacket(&pkt); err != nil || (placed{pkt.PTS, pkt.Lost}) != want {
				t.Fatalf("%s: behind the seek the packet at %d owes %d (%v), want %+v", name, pkt.PTS, pkt.Lost, err, want)
			}
		}
	}
}

// TestAnOggFLACMeasureIsWhatAReadDelivers: the measure is the count of a
// read from the first frame, whatever a read has settled and wherever the
// read stands, and it lists nothing and moves nothing.
func TestAnOggFLACMeasureIsWhatAReadDelivers(t *testing.T) {
	const bs, n = 4096, 3000
	// A swapped pair, two frames joined on past the length, a damaged page,
	// and frames enough behind them that a second walk would have to read.
	numbers := []uint64{0, 1, 2, 3, 4, 5, 7, 6, 20008, 20009}
	for k := uint64(10); k < n; k++ {
		numbers = append(numbers, k)
	}
	raw := testutil.DamageOggPage(t, oggFLACFrames(t, n*bs, numbers...), 10+2)
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	const want = (n - 1) * bs
	m, ok := any(d).(container.Measurer)
	if !ok {
		t.Fatal("the demuxer measures nothing")
	}
	var pkt container.Packet
	for range 3 {
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatal(err)
		}
	}
	track, before := d.Tracks()[0], slices.Clone(d.Warnings())
	if n, err := m.MeasureSamples(context.Background(), 0); err != nil || n != want {
		t.Errorf("measured %d (%v), want %d", n, err, want)
	}
	if got := d.Tracks()[0]; got.Samples != track.Samples || got.SamplesExact != track.SamplesExact || !slices.Equal(d.Warnings(), before) {
		t.Errorf("the measure left the track at %d, exact %v, and listed %v", got.Samples, got.SamplesExact, d.Warnings()[len(before):])
	}
	rest := placedBy(t, d)
	if len(rest) != n-4 || rest[0] != (placed{3 * bs, 0}) || readTotal(rest) != want-3*bs {
		t.Errorf("behind the measure the read placed %d packets from %+v, want the %d behind the third", len(rest), rest[0], n-4)
	}
	if n, err := m.MeasureSamples(context.Background(), 0); err != nil || n != want {
		t.Errorf("behind a whole read measured %d (%v), want %d", n, err, want)
	}
	// The source has not changed, so the answer is kept: there is no walk
	// left for a canceled context to stop.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n, err := m.MeasureSamples(ctx, 0); err != nil || n != want {
		t.Errorf("measured again %d (%v), want the %d it kept", n, err, want)
	}
	// A canceled measure says so, in its own words.
	d, err = NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.MeasureSamples(ctx, 0); waxerr.CodeOf(err) != waxerr.CodeCanceled || !strings.Contains(err.Error(), "ogg: measure canceled") {
		t.Errorf("a canceled measure returned %v, want it canceled as a measure", err)
	}
}

// readTotal is what the packets hold: their frames, 4096 samples each, and
// the holes in front of them.
func readTotal(ps []placed) int64 {
	var n int64
	for _, p := range ps {
		n += 4096 + p.lost
	}
	return n
}
