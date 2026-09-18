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
// It also pins the limitation, so a fix will be noticed rather than silently
// breaking this cell. The raw timeline still counts the trimmed frames, and a
// seek speaks it: a landing cluster past the padded block pre-rolls through no
// trim, so the position is stamped at the target while the content delivered
// there is the trim's worth earlier than it should be, and once the track is
// settled the raw-end cap ends that read the same worth short. A landing at or
// before the padded block decodes through the trim and lands exactly.
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
			want += p.MidPad // the hole the mid-stream trim left
		}
		if int64(v) != want {
			t.Fatalf("delivered frame %d = %d, want raw sample %d", i, v, want)
		}
	}

	// The limitation, as it stands. The target is inside the second cluster,
	// which is past the padded block, so nothing on the pre-roll path trims.
	const target = 200000
	if target <= trim {
		t.Fatalf("the seek target %d must lie past the mid-stream trim at %d", target, trim)
	}
	seeked := open()
	defer seeked.Close()
	landed, err := seeked.SeekSample(target)
	if err != nil {
		t.Fatalf("SeekSample: %v", err)
	}
	if landed != target {
		t.Fatalf("landed at %d, want the target %d", landed, target)
	}
	after := mustReadInts(t, seeked)
	if len(after) == 0 {
		t.Fatal("the seeked read delivered nothing")
	}
	if int64(after[0]) != target {
		t.Errorf("first value after the seek = %d, want the target's raw index %d "+
			"(an exact landing would give %d; see the deferred-work entry)",
			after[0], target, target+p.MidPad)
	}
	// Settling the track installs the raw-end cap, which counts from the same
	// overstated position and so stops the same worth short of the real end.
	// A fresh media for that, since the one above is drained: the walk has to
	// happen before the read it bounds.
	linear := open()
	defer linear.Close()
	tail := mustReadInts(t, linear)
	lastLinear := tail[len(tail)-1]

	seekedAgain := open()
	defer seekedAgain.Close()
	if _, err := seekedAgain.SeekSample(target); err != nil {
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
	if got, want := afterWalk[len(afterWalk)-1], lastLinear-int32(p.MidPad); got != want {
		t.Errorf("last value from the seeked read = %d, want %d: the cap ends it the "+
			"mid-stream trim's worth short of the linear read's %d", got, want, lastLinear)
	}
}
