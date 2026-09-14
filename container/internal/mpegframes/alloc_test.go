//go:build !race

package mpegframes

import (
	"runtime"
	"testing"

	"github.com/colespringer/waxflow/container/internal/srcwin"
)

// TestMeasurePassAllocatesOneWindow is the daemon's measure pass in the units
// that matter for it: a seek to the end walks the whole index without reading
// a packet, and the window must slide behind that walk rather than be rebuilt
// at every window boundary it crosses. Before the walk trimmed behind itself
// and read the next header with the frame ahead of it, a frame straddling the
// window's end made every boundary a rebase into fresh storage: one window
// allocated per window walked, the whole file's worth over a long stream.
//
// Excluded under -race for the reason srcwin's own allocation pin is: the
// detector's accounting swamps the measurement.
func TestMeasurePassAllocatesOneWindow(t *testing.T) {
	const n = 32000 // 13 MB, over a hundred windows
	w, _ := countedWalk(t, frames(n))

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if _, err := w.Landing(n - 10); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)

	// The index itself is n offsets the walk must build, and append grows a
	// large slice by a quarter at a time, so its total allocation is about
	// five times its final size; six is the budget. What must not appear on
	// top of that is a window per window, in bytes or in allocations.
	windows := uint64(n * frameLen / srcwin.Chunk)
	index := uint64(6 * 8 * n)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 4*srcwin.Chunk+index {
		t.Errorf("a landing over %d windows allocated %d bytes, want no more than %d plus the index's %d",
			windows, grew, 4*srcwin.Chunk, index)
	}
	if mallocs := after.Mallocs - before.Mallocs; mallocs > windows/2 {
		t.Errorf("a landing over %d windows made %d allocations, want fewer than one per window", windows, mallocs)
	}
}
