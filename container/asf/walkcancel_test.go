package asf_test

import (
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// TestWalkStopsWithItsContext is container.Walker's cancel contract on the
// WMA v1/v2 frame count (testutil.WalkPauses).
func TestWalkStopsWithItsContext(t *testing.T) {
	testutil.WalkPauses(t, readFixture(t, windowsFixture), 1, func(src container.Source) (testutil.Walkable, error) {
		return asf.NewDemuxer(src, nil)
	})
}

// twoHoleStream is eight single-object packets with a continuation of an
// object nothing started in the third: two findings a walk reports.
func twoHoleStream(t *testing.T) []byte {
	t.Helper()
	b := reservoirBuilder()
	b.playHNS = b.prerollMS*10_000 + 10_000_000
	pres := uint32(b.prerollMS)
	for i := range 8 {
		if i == 2 {
			b.packet(uint32(i*20), byte(i+1), 8, 12, pres+uint32(i*20), []byte{9, 9, 9, 9})
			continue
		}
		b.packet(uint32(i*20), byte(i+1), 0, 4, pres+uint32(i*20), []byte{1, byte(i), 0, 0})
	}
	return b.build()
}

// TestWalkReportsEachFindingOnce: what a walk found is reported through the
// warn path once, however many times Walk is called, so the damage count
// says how much damage the file holds rather than how often it was asked.
func TestWalkReportsEachFindingOnce(t *testing.T) {
	d, err := asf.NewDemuxer(container.BytesSource(twoHoleStream(t)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Walk(t.Context()); err != nil {
		t.Fatal(err)
	}
	damage, listed := container.DamageOf(d), len(d.Warnings())
	if damage == 0 {
		t.Fatal("the walk found no damage in a stream with a hole")
	}
	for range 3 {
		if err := d.Walk(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := container.DamageOf(d); got != damage {
		t.Errorf("DamageOf = %d after four walks, %d after one", got, damage)
	}
	if got := len(d.Warnings()); got != listed {
		t.Errorf("%d warnings after four walks, %d after one", got, listed)
	}
}

// TestWalkStopsBetweenOtherStreamsPackets: the context is read per packet,
// not per object of the selected stream, so a walk over packets that carry
// only another stream stops within a packet of the cancel.
func TestWalkStopsBetweenOtherStreamsPackets(t *testing.T) {
	b := reservoirBuilder()
	b.playHNS = b.prerollMS*10_000 + 10_000_000
	pres := uint32(b.prerollMS)
	b.streamNum = 2
	for i := range 1000 {
		b.packet(uint32(i*20), byte(i+1), 0, 4, pres+uint32(i*20), []byte{1, byte(i), 0, 0})
	}
	b.streamNum = 1
	src := &testutil.CountingSource{Src: container.BytesSource(b.build())}
	d, err := asf.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	opened := src.Reads
	if err := d.Walk(testutil.CancelAfterChecks(t, 2)); waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("Walk = %v, want canceled", err)
	}
	if n := src.Reads - opened; n > 1 {
		t.Errorf("a walk canceled on its second check read %d times, want one packet's window at most", n)
	}
	if err := d.Walk(t.Context()); err != nil || !d.Walked() {
		t.Fatalf("Walk after the cancel = %v, Walked %v", err, d.Walked())
	}
}

// TestASeekStopsWithItsContext: the count a seek takes to its target ends
// with the seek's context, and a seek with a live one then lands where a
// fresh demuxer lands.
func TestASeekStopsWithItsContext(t *testing.T) {
	raw := readFixture(t, windowsFixture)
	fresh, err := asf.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	target := fresh.Tracks()[0].Samples * 3 / 4
	want, err := fresh.SeekSample(t.Context(), 0, target)
	if err != nil {
		t.Fatal(err)
	}
	d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SeekSample(testutil.CancelAfterChecks(t, 2), 0, target); waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("a seek whose context ended returned %v, want canceled", err)
	}
	if got, err := d.SeekSample(t.Context(), 0, target); err != nil || got != want {
		t.Errorf("the seek after the cancel landed at %d (%v), a fresh demuxer at %d", got, err, want)
	}
}

// losslessFormat is a WMA Lossless WAVEFORMATEX: the fixed fields, then the
// eighteen extra bytes the codec reads its depth and channel mask from.
func losslessFormat(rate, channels, bits int, mask uint32) []byte {
	b := make([]byte, 36)
	le.PutUint16(b, 0x0163)
	le.PutUint16(b[2:], uint16(channels))
	le.PutUint32(b[4:], uint32(rate))
	le.PutUint32(b[8:], 144000)
	le.PutUint16(b[12:], 13375)
	le.PutUint16(b[14:], 16)
	le.PutUint16(b[16:], 18)
	le.PutUint16(b[18:], uint16(bits))
	le.PutUint32(b[20:], mask)
	le.PutUint16(b[32:], 0x01a1)
	return b
}

// losslessStream is a WMA Lossless stream of n single-object packets, 20 ms
// apart, of which only the first is a sync point: a seek anywhere past it
// scans back to it, one packet at a time.
func losslessStream(t *testing.T, n int) []byte {
	t.Helper()
	b := newBuilder()
	b.wfx = losslessFormat(b.rate, b.channels, 16, 0x03)
	b.playHNS = b.prerollMS*10_000 + uint64(n)*20*10_000
	pres := uint32(b.prerollMS)
	for i := range n {
		data := []byte{0, byte(i), 0, 0}
		if i == 0 {
			data[0] = 1 << 3 // the sync bit wmalossless.PacketIsSync reads
		}
		b.packet(uint32(i*20), byte(i+1), 0, 4, pres+uint32(i*20), data)
	}
	return b.build()
}

// TestAStatedTimeSeekStopsWithItsContext: the packet scan a WMA Lossless seek
// runs back from its target to a sync object ends with the seek's context,
// and a seek with a live one then lands where a fresh demuxer lands. Pro and
// Voice land on a stated time and scan nothing.
func TestAStatedTimeSeekStopsWithItsContext(t *testing.T) {
	raw := losslessStream(t, 64)
	fresh, err := asf.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	target := fresh.Tracks()[0].Samples * 3 / 4
	want, err := fresh.SeekSample(t.Context(), 0, target)
	if err != nil {
		t.Fatal(err)
	}
	d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SeekSample(testutil.CancelAfterChecks(t, 1), 0, target); waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("a seek whose context ended returned %v, want canceled", err)
	}
	if got, err := d.SeekSample(t.Context(), 0, target); err != nil || got != want {
		t.Errorf("the seek after the cancel landed at %d (%v), a fresh demuxer at %d", got, err, want)
	}
}
