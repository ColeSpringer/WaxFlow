package ogg

import (
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestOggFLACHoleTellsTheReaderWhatItLost: Ogg FLAC frames say where they
// are, so the packet after a lost page carries the samples the page took, and
// a reader keeps its positions where the frames say.
func TestOggFLACHoleTellsTheReaderWhatItLost(t *testing.T) {
	const bs = 4096
	raw := synthOggFLACPages(t, []int{5, 5, 5, 5, 5}, 0)
	first := testutil.OggFirstAudioPage(t, raw, 2)
	d, err := NewDemuxer(container.BytesSource(testutil.DamageOggPage(t, raw, first+2)), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	var prevEnd, total int64
	for i := 0; ; i++ {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if want := pkt.PTS - prevEnd; i > 0 && pkt.Lost != want {
			t.Errorf("packet at %d: lost %d, want %d", pkt.PTS, pkt.Lost, want)
		}
		total += pkt.Lost
		prevEnd = pkt.PTS + pkt.Dur
	}
	if total != 5*bs {
		t.Fatalf("packets say %d samples were lost, want the page's %d", total, 5*bs)
	}
}

// TestOggDamageWarnsOnce: a read that crosses the same damaged page again,
// after a seek back over it, records nothing new, so repeated seeks on a
// damaged file do not grow its warnings.
func TestOggDamageWarnsOnce(t *testing.T) {
	const bs = 4096
	raw := synthOggFLACPages(t, fiveToAPage(40), 0)
	first := testutil.OggFirstAudioPage(t, raw, 2)
	d, err := NewDemuxer(container.BytesSource(testutil.DamageOggPage(t, raw, first+20)), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	crossDamage := func() {
		if _, err := d.SeekSample(0, 15*5*bs); err != nil {
			t.Fatal(err)
		}
		for range 10 * 5 {
			if err := d.ReadPacket(&pkt); err != nil {
				t.Fatal(err)
			}
		}
	}
	crossDamage()
	n := len(d.Warnings())
	if n == 0 {
		t.Fatal("the damaged page raised no warning")
	}
	for range 5 {
		crossDamage()
	}
	if got := len(d.Warnings()); got != n {
		t.Fatalf("%d warnings after crossing the damage six times, want the first crossing's %d", got, n)
	}
}

// oggFLACNumbered is an Ogg FLAC of one frame per page, numbered as given,
// with every page and every frame checking.
func oggFLACNumbered(t *testing.T, numbers ...uint64) []byte {
	t.Helper()
	const bs = 4096
	stream := buildPage(flagBOS, 0, 77, 0, flacBOSPacket())
	stream = append(stream, buildPage(0, 0, 77, 1, commentPacket())...)
	for i, n := range numbers {
		flags := byte(0)
		if i == len(numbers)-1 {
			flags = flagEOS
		}
		stream = append(stream, buildPage(flags, int64(i+1)*bs, 77, uint32(i+2), buildFrameBytes(t, n, bs))...)
	}
	return patchOggFLACTotalB(t, stream, int64(len(numbers))*bs)
}

// TestOggFLACCleanJumpIsAHole: frame numbers that skip with every page and
// frame whole are frames gone without their numbers, a hole the stream's
// measured length vouches for: the samples skipped are lost, with a warning,
// as container/flacn reads it, and a strict read refuses it.
func TestOggFLACCleanJumpIsAHole(t *testing.T) {
	const bs = 4096
	raw := oggFLACNumbered(t, 0, 1, 5, 6)
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; !tr.SamplesExact || tr.Samples != 7*bs {
		t.Fatalf("%d samples (exact %v), want the frames' %d exact", tr.Samples, tr.SamplesExact, 7*bs)
	}
	pkts := walkPackets(t, d)
	want := []struct{ pts, lost int64 }{{0, 0}, {bs, 0}, {5 * bs, 3 * bs}, {6 * bs, 0}}
	if len(pkts) != len(want) {
		t.Fatalf("%d packets, want %d", len(pkts), len(want))
	}
	for i, p := range pkts {
		if p.PTS != want[i].pts || p.Lost != want[i].lost {
			t.Errorf("packet %d at %d lost %d, want at %d lost %d", i, p.PTS, p.Lost, want[i].pts, want[i].lost)
		}
	}
	var damage []string
	for _, w := range d.Warnings() {
		if w.Kind == container.Damage {
			damage = append(damage, w.Msg)
		}
	}
	if len(damage) != 1 || !strings.Contains(damage[0], "skip") {
		t.Errorf("damage %v, want one warning that the frames skip", damage)
	}
	sd, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	for err == nil {
		err = sd.ReadPacket(&pkt)
	}
	if err == io.EOF {
		t.Error("a strict read went past the jump")
	}
}

// TestOggFLACCleanJumpUnderAnInexactLengthIsAHole: a jump the bytes could
// hold is a hole whether or not the length is exact, so a read places what
// follows where a seek does, as container/flacn reads it.
func TestOggFLACCleanJumpUnderAnInexactLengthIsAHole(t *testing.T) {
	d := &Demuxer{track: container.Track{Samples: 4096}, running: 2 * 4096, totalSize: 1 << 20, firstData: 100}
	if lost, err := d.gap(5 * 4096); err != nil || lost != 3*4096 {
		t.Fatalf("lost %d (%v), want the 12288 the numbers skip", lost, err)
	}
}

// TestOggFLACCleanJumpPastTheLengthJoins: frame numbers past what the bytes
// could hold make no exact length and vouch for nothing, so the jump is not
// paid out as silence: the frames are joined on.
func TestOggFLACCleanJumpPastTheLengthJoins(t *testing.T) {
	raw := oggFLACNumbered(t, 0, 1, 1<<30, 1<<30+1)
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.SamplesExact && tr.Samples > 4*4096 {
		t.Errorf("opens as %d samples, exact, from frame numbers no file this size holds", tr.Samples)
	}
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if pkt.Lost != 0 {
			t.Fatalf("packet at %d says %d samples were lost past the stream's length", pkt.PTS, pkt.Lost)
		}
	}
	if len(d.Warnings()) == 0 {
		t.Error("the jump raised no warning")
	}
}

// TestOggFLACDamagedFirstPageIsAHoleFromZero: a first audio page lost to
// damage is a hole at the stream's start: positions still start at 0, the
// first frame read owes the samples it took, and a seek inside it lands at 0.
func TestOggFLACDamagedFirstPageIsAHoleFromZero(t *testing.T) {
	const bs = 4096
	raw := testutil.DamageOggPage(t, oggFLACNumbered(t, 0, 1, 2, 3), 2)
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.Samples != 4*bs {
		t.Errorf("%d samples, want the stream's %d from 0", tr.Samples, 4*bs)
	}
	for _, w := range d.Warnings() {
		if w.Kind == container.Note {
			t.Errorf("note %q: the damage reads as a stream-copy cut", w.Msg)
		}
	}
	if pkts := walkPackets(t, d); len(pkts) != 3 || pkts[0].PTS != bs || pkts[0].Lost != bs {
		t.Fatalf("%d packets, the first at %d owing %d; want 3 from %d owing %d", len(pkts), pkts[0].PTS, pkts[0].Lost, bs, bs)
	}
	if landed, err := d.SeekSample(0, bs/2); err != nil || landed != 0 {
		t.Fatalf("a seek into the hole landed at %d (%v), want 0", landed, err)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil || pkt.PTS != bs || pkt.Lost != bs {
		t.Errorf("after the seek: packet at %d owing %d (%v), want %d owing %d", pkt.PTS, pkt.Lost, err, bs, bs)
	}
}

// TestOggDamageCountIgnoresTheListCap: whether a look ahead met damage is a
// count, which the warning list's cap and de-duplication cannot hide.
func TestOggDamageCountIgnoresTheListCap(t *testing.T) {
	d := &Demuxer{}
	for i := range maxWarnings + 5 {
		if err := d.warn(int64(i), "damaged page, resyncing"); err != nil {
			t.Fatal(err)
		}
	}
	n := d.damage
	if err := d.warn(int64(maxWarnings+5), "damaged page, resyncing"); err != nil {
		t.Fatal(err)
	}
	if !d.damagedSince(n) {
		t.Fatal("damage past a full warning list went unseen")
	}
}

// TestOggFLACSeekIntoAHoleLandsBefore: a target a lost page took lands on the
// last frame before the hole, at or before it, as container.Seeker wants; the
// read then fills the hole as a read from the start does.
func TestOggFLACSeekIntoAHoleLandsBefore(t *testing.T) {
	const bs = 4096
	raw := synthOggFLACPages(t, []int{5, 5, 5, 5, 5}, 0)
	first := testutil.OggFirstAudioPage(t, raw, 2)
	bad := testutil.DamageOggPage(t, raw, first+2) // frames [10, 15) lost
	for _, target := range []int64{10 * bs, 12*bs + 7, 15*bs - 1} {
		d, err := NewDemuxer(container.BytesSource(bad), nil)
		if err != nil {
			t.Fatal(err)
		}
		landed, err := d.SeekSample(0, target)
		if err != nil {
			t.Fatal(err)
		}
		if landed > target {
			t.Fatalf("a seek to %d, in the hole, landed at %d, past it", target, landed)
		}
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil || pkt.PTS != landed {
			t.Fatalf("after landing at %d the next packet is at %d (%v)", landed, pkt.PTS, err)
		}
		if err := d.ReadPacket(&pkt); err != nil || pkt.PTS != 15*bs || pkt.Lost != 5*bs {
			t.Fatalf("the packet after the hole: at %d with %d lost (%v), want %d with %d", pkt.PTS, pkt.Lost, err, 15*bs, 5*bs)
		}
	}
}
