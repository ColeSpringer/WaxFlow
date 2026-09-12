//go:build !race

package srcwin

import (
	"runtime"
	"testing"
)

// TestLinearReadAllocatesOnce is TestLinearReadReusesOneWindow's claim in the
// units that motivated it: bytes off the heap for a linear read, which must
// not scale with the read.
//
// Excluded under -race, where the detector's own accounting dominates: the
// same walk measures 0.4 MB here and 17 MB there, so the number says nothing
// about the window in that build. The capacity pin beside it is what holds in
// both, and this is what makes the size of the win concrete: before the
// window was reused, this walk allocated 33,159,096 bytes.
//
// Total bytes rather than testing.AllocsPerRun, which divides its count by the
// run count in INTEGER arithmetic and reports a clean zero for anything under
// one allocation per run. The first version of this test asserted exactly that
// zero and passed against the allocating implementation.
func TestLinearReadAllocatesOnce(t *testing.T) {
	const walk = 16 << 20
	src := &counted{size: 1 << 30}
	w := New(src, src.size, "test: reading")
	const step = 24576

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for off := int64(0); off < walk; off += step {
		w.Trim(off)
		if got := w.BytesAt(off, step); len(got) != step {
			t.Fatalf("view at %d is %d bytes", off, len(got))
		}
	}
	runtime.ReadMemStats(&after)

	// A generous bound, and still two orders under what the walk used to cost:
	// the window itself plus the growth that sizes it, and nothing that scales
	// with the 16 MiB walked.
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 4*Chunk {
		t.Errorf("a %d-byte linear read allocated %d bytes, want no more than %d",
			walk, grew, 4*Chunk)
	}
}
