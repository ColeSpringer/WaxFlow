package srcwin

import (
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// TestBackReadsOnlyTheBytesInFront: a range that starts in front of the window
// and reaches it reads what lies in front, and the window then holds both.
func TestBackReadsOnlyTheBytesInFront(t *testing.T) {
	for _, n := range []int{Chunk, Chunk + 16, 2 * Chunk} {
		src := &counted{size: 10 * Chunk}
		w := New(src, src.size, "test: reading")
		w.Peek(5*Chunk, Chunk)
		src.reads, src.bytes = 0, 0
		if got := w.Back(4*Chunk, n); !equal(got, want(4*Chunk, n)) {
			t.Fatalf("Back(4*Chunk, %d) served the wrong bytes", n)
		}
		if src.reads != 1 || src.bytes != Chunk {
			t.Errorf("Back(4*Chunk, %d) read %d bytes in %d reads, want the %d in front in 1", n, src.bytes, src.reads, Chunk)
		}
		if !w.Holds(4*Chunk, 2*Chunk) || len(w.win) != 2*Chunk {
			t.Errorf("Back(4*Chunk, %d): the window holds %d bytes at %d, want [4*Chunk, 6*Chunk)", n, len(w.win), w.winOff)
		}
	}
}

// TestBackKeepsEarlierViews: the bytes in front go into fresh storage, so a
// view taken before a Back, of the window or of an earlier Back, keeps its
// bytes.
func TestBackKeepsEarlierViews(t *testing.T) {
	src := &counted{size: 10 * Chunk}
	w := New(src, src.size, "test: reading")
	first := w.Peek(5*Chunk, 4096)
	second := w.Back(4*Chunk, Chunk+64)
	w.Back(3*Chunk, Chunk+64)
	if !equal(first, want(5*Chunk, 4096)) || !equal(second, want(4*Chunk, Chunk+64)) {
		t.Error("a Back rewrote the bytes an earlier view was serving")
	}
	if got := w.BytesAt(3*Chunk, 3*Chunk); !equal(got, want(3*Chunk, 3*Chunk)) {
		t.Error("the window after two Backs served the wrong bytes")
	}
}

// TestBackAnywhereElseIsPeek: a range that ends short of the window, or that
// starts in it or past it, is read as Peek reads it.
func TestBackAnywhereElseIsPeek(t *testing.T) {
	for _, tc := range []struct {
		name      string
		off       int64
		n         int
		bytes     int64
		holdOff   int64
		holdBytes int
	}{
		{"a range a byte short of the window", 5*Chunk - 100, 99, 99, 5*Chunk - 100, 99},
		{"a range inside the window", 5*Chunk + 10, 100, 0, 5 * Chunk, Chunk},
		{"a range from inside past the end", 5*Chunk + 10, Chunk, 10, 5 * Chunk, Chunk + 10},
		{"a range past the end", 7 * Chunk, 100, 100, 7 * Chunk, 100},
	} {
		src := &counted{size: 10 * Chunk}
		w := New(src, src.size, "test: reading")
		w.Peek(5*Chunk, Chunk)
		src.reads, src.bytes = 0, 0
		if got := w.Back(tc.off, tc.n); !equal(got, want(tc.off, tc.n)) {
			t.Fatalf("%s: Back served the wrong bytes", tc.name)
		}
		if src.bytes != tc.bytes || w.winOff != tc.holdOff || len(w.win) != tc.holdBytes {
			t.Errorf("%s: Back read %d bytes and holds %d at %d, want %d read and %d held at %d",
				tc.name, src.bytes, len(w.win), w.winOff, tc.bytes, tc.holdBytes, tc.holdOff)
		}
	}
}

// TestBackPastTheWindowsEndExtendsIt: a range that starts in front of the
// window and runs past its end reads the bytes in front, then what the end
// lacks, and the window holds the range.
func TestBackPastTheWindowsEndExtendsIt(t *testing.T) {
	src := &counted{size: 10 * Chunk}
	w := New(src, src.size, "test: reading")
	w.Peek(5*Chunk, 100)
	src.reads, src.bytes = 0, 0
	if got := w.Back(4*Chunk, 2*Chunk); !equal(got, want(4*Chunk, 2*Chunk)) {
		t.Fatal("Back past the window's end served the wrong bytes")
	}
	if src.reads != 2 || src.bytes != 2*Chunk-100 || w.winOff != 4*Chunk || len(w.win) != 2*Chunk {
		t.Errorf("Back past the window's end read %d bytes in %d reads and holds %d at %d, want %d in 2 holding [4*Chunk, 6*Chunk)",
			src.bytes, src.reads, len(w.win), w.winOff, 2*Chunk-100)
	}
}

// TestBackClampsToDataEnd: the data end clamps a Back as it clamps Peek,
// before it decides whether the range reaches the window.
func TestBackClampsToDataEnd(t *testing.T) {
	src := &counted{size: 6 * Chunk}
	w := New(src, src.size, "test: reading")
	w.Peek(5*Chunk, Chunk)
	src.reads, src.bytes = 0, 0
	if got := w.Back(4*Chunk, 4*Chunk); len(got) != 2*Chunk || !equal(got, want(4*Chunk, 2*Chunk)) {
		t.Errorf("Back over the data end returned %d bytes, want the %d there are", len(got), 2*Chunk)
	}
	if src.bytes != Chunk {
		t.Errorf("Back over the data end read %d bytes, want the %d in front", src.bytes, Chunk)
	}

	// A data end in front of the window: the range cannot reach it.
	w = New(src, src.size, "test: reading")
	w.Peek(5*Chunk, Chunk)
	w.SetDataEnd(5*Chunk - 10)
	src.reads, src.bytes = 0, 0
	if got := w.Back(4*Chunk, Chunk); len(got) != Chunk-10 || !equal(got, want(4*Chunk, Chunk-10)) {
		t.Errorf("Back below a data end in front of the window returned %d bytes, want %d", len(got), Chunk-10)
	}
	if src.bytes != Chunk-10 || w.winOff != 4*Chunk || len(w.win) != Chunk-10 {
		t.Errorf("Back below a data end in front of the window read %d bytes and holds %d at %d, want Peek's rebase of %d",
			src.bytes, len(w.win), w.winOff, Chunk-10)
	}
	if w.Back(5*Chunk, 10) != nil {
		t.Error("Back at the data end served bytes")
	}
}

// TestBackFailureIsSticky: a read that fails in front of the window is the
// window's error, and nothing is served after it.
func TestBackFailureIsSticky(t *testing.T) {
	src := &counted{size: 10 * Chunk}
	w := New(src, src.size, "test: reading")
	w.Peek(5*Chunk, Chunk)
	src.fail = true
	if got := w.Back(4*Chunk, Chunk); got != nil {
		t.Fatal("a Back whose read failed served bytes")
	}
	if waxerr.CodeOf(w.Err()) != waxerr.CodeSourceUnreadable {
		t.Fatalf("Err = %v after a Back whose read failed, want source-unreadable", w.Err())
	}
	src.fail = false
	if w.Back(4*Chunk, Chunk) != nil || w.Peek(5*Chunk, 10) != nil {
		t.Error("the window served bytes after a failed Back; the error is meant to stick")
	}
}

// TestBackFailurePastTheWindowIsSticky: a range whose bytes in front read
// and whose extension past the window's end fails is the window's error, and
// nothing is served after it.
func TestBackFailurePastTheWindowIsSticky(t *testing.T) {
	data := filler(10 * Chunk)
	src := &testutil.FailingSource{Src: container.BytesSource(data), From: 5*Chunk + 100}
	w := New(src, int64(len(data)), "test: reading")
	w.Peek(5*Chunk, 100)
	if got := w.Back(4*Chunk, 2*Chunk); got != nil {
		t.Fatal("a Back whose extension failed served bytes")
	}
	if want := [2]int64{5*Chunk + 100, 6 * Chunk}; src.Failed != 1 || src.FailedAt != want {
		t.Fatalf("%d reads failed, the first %v, want the extension's alone, %v", src.Failed, src.FailedAt, want)
	}
	if waxerr.CodeOf(w.Err()) != waxerr.CodeSourceUnreadable {
		t.Errorf("Err = %v after a Back whose extension failed, want source-unreadable", w.Err())
	}
	if w.Back(4*Chunk, 10) != nil || w.Peek(5*Chunk, 10) != nil {
		t.Error("the window served bytes after a failed extension; the error is meant to stick")
	}
}

// TestBackOfNothingReadsNothing: a Back of no bytes serves nothing and reads
// nothing, in front of the window, at its start, or past it.
func TestBackOfNothingReadsNothing(t *testing.T) {
	src := &counted{size: 10 * Chunk}
	w := New(src, src.size, "test: reading")
	w.Peek(5*Chunk, Chunk)
	src.reads = 0
	for _, off := range []int64{4 * Chunk, 5*Chunk - 1, 5 * Chunk, 7 * Chunk} {
		for _, n := range []int{0, -1} {
			if got := w.Back(off, n); got != nil {
				t.Errorf("Back(%d, %d) served %d bytes", off, n, len(got))
			}
		}
	}
	if src.reads != 0 || w.winOff != 5*Chunk || len(w.win) != Chunk {
		t.Errorf("Backs of no bytes read %d times and left [%d, +%d), want no read and the window as it was", src.reads, w.winOff, len(w.win))
	}
}

// TestBackStepsReadEachByteOnce: thirty-two steps of a Chunk back from the
// end of 4 MiB, each view reaching three bytes into the last, read the range
// once, a read a step, and leave it all resident.
func TestBackStepsReadEachByteOnce(t *testing.T) {
	const size = 32 * Chunk
	src := &counted{size: size + 4096}
	w := New(src, src.size, "test: reading")
	w.Peek(0, 4096) // the window starts at the head, as an open leaves it
	src.reads, src.bytes = 0, 0
	end := src.size
	for from, hi := end-Chunk, end; ; hi, from = from+3, from-Chunk {
		if got := w.Back(from, int(hi-from)); !equal(got, want(from, int(hi-from))) {
			t.Fatalf("Back(%d, %d) served the wrong bytes", from, hi-from)
		}
		if from == end-size {
			break
		}
	}
	if src.reads != 32 || src.bytes != size {
		t.Errorf("32 steps back over %d bytes read %d bytes in %d reads, want each byte once, a read a step", size, src.bytes, src.reads)
	}
	if !w.Holds(end-size, size) {
		t.Errorf("after 32 steps back the window holds %d bytes at %d, want [%d, %d)", len(w.win), w.winOff, end-size, end)
	}
}
