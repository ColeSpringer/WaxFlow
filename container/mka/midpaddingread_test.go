package mka_test

// The delivery half of the mid-stream DiscardPadding fixture; the fixture and
// the demuxer's half are in midpadding_test.go, which cannot import format.

import (
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mka"
	"github.com/colespringer/waxflow/format"
)

// mustReadInts is readAll's integer half, named because every value the
// fixture delivers is its own raw index and a cell reads them as positions.
func mustReadInts(t *testing.T, med format.Media) []int32 {
	t.Helper()
	i32, _ := readAll(t, med)
	return i32
}

// TestMidStreamDiscardPaddingRead is what a read makes of a trim that is not
// at the end: the frames are dropped in place, so the delivered run is the
// gapless one with a hole where the padded block ended.
//
// The timeline the demuxer speaks excludes those frames too, which is what
// makes a seek across one exact. A landing cluster past the padded block
// pre-rolls through no trim and does not need to: its anchor is already short
// by the trim, so the target names the same sample it would have before the
// block was padded, and the raw-end cap built from the settled track ends the
// read where the linear read ends. A landing at or before the padded block
// decodes through the trim and lands exactly for the older reason.
func TestMidStreamDiscardPaddingRead(t *testing.T) {
	p := mka.BuildMidPadding(t)
	open := func() format.Media {
		med, err := format.Open(container.BytesSource(p.File), "", nil)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return med
	}

	med := open()
	defer med.Close()
	got := mustReadInts(t, med)
	if int64(len(got)) != p.Samples() {
		t.Fatalf("read delivered %d frames, want the gapless %d", len(got), p.Samples())
	}
	trim := p.MidTrimStart()
	for i, v := range got {
		want := int64(i)
		if int64(i) >= trim {
			want += p.MidClamped() // the hole the mid-stream trim left
		}
		if int64(v) != want {
			t.Fatalf("delivered frame %d = %d, want raw sample %d", i, v, want)
		}
	}

	// Three landings, one on either side of the trim inside the first cluster
	// and one in the second cluster, which is the one whose anchor the walk has
	// to have shortened: nothing on its pre-roll path crosses the padded block.
	const secondCluster = 200000
	if secondCluster <= trim {
		t.Fatalf("the seek target %d must lie past the mid-stream trim at %d", secondCluster, trim)
	}
	for _, target := range []int64{10000, 100000, secondCluster} {
		seeked := open()
		landed, err := seeked.SeekSample(target)
		if err != nil {
			seeked.Close()
			t.Fatalf("SeekSample(%d): %v", target, err)
		}
		if landed != target {
			seeked.Close()
			t.Fatalf("landed at %d, want the target %d", landed, target)
		}
		after := mustReadInts(t, seeked)
		seeked.Close()
		if len(after) == 0 {
			t.Fatalf("the seeked read to %d delivered nothing", target)
		}
		want := target
		if target >= trim {
			want += p.MidClamped()
		}
		if int64(after[0]) != want {
			t.Errorf("first value after a seek to %d = %d, want raw sample %d", target, after[0], want)
		}
	}

	// Settling the track installs the raw-end cap, which counts from the same
	// timeline and so ends the seeked read exactly where the linear read ends.
	// A fresh media for that, since the one above is drained: the walk has to
	// happen before the read it bounds.
	linear := open()
	defer linear.Close()
	tail := mustReadInts(t, linear)
	lastLinear := tail[len(tail)-1]

	seekedAgain := open()
	defer seekedAgain.Close()
	if _, err := seekedAgain.SeekSample(secondCluster); err != nil {
		t.Fatalf("SeekSample: %v", err)
	}
	w, ok := seekedAgain.(format.Walker)
	if !ok {
		t.Fatal("a Matroska Media must implement format.Walker")
	}
	if err := w.Walk(); err != nil {
		t.Fatal(err)
	}
	afterWalk := mustReadInts(t, seekedAgain)
	if len(afterWalk) == 0 {
		t.Fatal("the seeked read delivered nothing after the walk")
	}
	if got, want := afterWalk[len(afterWalk)-1], lastLinear; got != want {
		t.Errorf("last value from the seeked read = %d, want the linear read's %d", got, want)
	}
}

// TestOversizedFinalDiscardPaddingRead: a final trim larger than the frame it
// rides on is applied in full, but only once the walk has settled the track.
// The read alone can drop no more than the packet it is trimming produced; the
// rest comes off through the raw-end cap, which is the length the walk settled.
func TestOversizedFinalDiscardPaddingRead(t *testing.T) {
	p := mka.BuildMidPaddingShape(t, 48, 600)
	open := func() format.Media {
		med, err := format.Open(container.BytesSource(p.File), "", nil)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return med
	}

	med := open()
	defer med.Close()
	if got := int64(len(mustReadInts(t, med))); got != p.Delivered() {
		t.Errorf("an unwalked read delivered %d frames, want %d (the trim bounded by its own frame)",
			got, p.Delivered())
	}

	walked := open()
	defer walked.Close()
	w, ok := walked.(format.Walker)
	if !ok {
		t.Fatal("a Matroska Media must implement format.Walker")
	}
	if err := w.Walk(); err != nil {
		t.Fatal(err)
	}
	if got := int64(len(mustReadInts(t, walked))); got != p.Samples() {
		t.Errorf("a walked read delivered %d frames, want the settled %d", got, p.Samples())
	}
}
