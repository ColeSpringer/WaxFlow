package mka_test

// The delivery half of the laced-block fixture; the fixture and the
// demuxer's half are in lacedpadding_test.go, which cannot import format.

import (
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mka"
	"github.com/colespringer/waxflow/format"
)

// TestLacedBlockTrimRead: a trim spread over a laced block's laces comes out
// of the delivered stream as one hole at the block's end, whether the read
// walked first or not, and a seek to either side of it lands exactly. A
// final trim larger than its block is applied in full only once the walk has
// settled the track: the read alone drops what the laces hold, and the
// raw-end cap removes the rest.
func TestLacedBlockTrimRead(t *testing.T) {
	for _, tc := range []struct {
		name         string
		first, count int
		trim         int64
	}{
		{"inside the run", 100, 3, 600},
		{"ending the stream", 497, 3, 1000},
		{"ending the stream past its block", 497, 3, 1500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := mka.BuildLacedPadding(t, tc.first, tc.count, tc.trim)
			open := func() format.Media {
				med, err := format.Open(container.BytesSource(l.File), "", nil)
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				return med
			}
			rawOf := func(i int64) int64 {
				if i >= l.TrimStart() {
					return i + l.Honoured()
				}
				return i
			}

			med := open()
			defer med.Close()
			got := mustReadInts(t, med)
			if int64(len(got)) != l.Delivered() {
				t.Fatalf("read delivered %d frames, want %d", len(got), l.Delivered())
			}
			for i, v := range got {
				if int64(v) != rawOf(int64(i)) {
					t.Fatalf("delivered frame %d = %d, want raw sample %d", i, v, rawOf(int64(i)))
				}
			}

			// Before the laced block, inside it (a lace the trim reaches), and
			// past it where the timeline is short by the trim.
			targets := []int64{
				int64(l.First-3) * l.BlockDur,
				int64(l.First+1)*l.BlockDur + 7,
				int64(l.First+l.Count+5) * l.BlockDur,
			}
			for _, target := range targets {
				if target >= l.Delivered() {
					continue
				}
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
				if int64(after[0]) != rawOf(target) {
					t.Errorf("first value after a seek to %d = %d, want raw sample %d", target, after[0], rawOf(target))
				}
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
			if got := int64(len(mustReadInts(t, walked))); got != l.Samples() {
				t.Errorf("a walked read delivered %d frames, want the settled %d", got, l.Samples())
			}
		})
	}
}
