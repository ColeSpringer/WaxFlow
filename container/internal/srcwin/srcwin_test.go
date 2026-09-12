package srcwin

import (
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// counted is a source that answers from a byte pattern and counts the reads it
// served, so a test can say what the window asked the source for and not only
// what it handed back.
type counted struct {
	size  int64
	reads int
	bytes int64
	fail  bool
}

func (s *counted) Size() int64 { return s.size }

func (s *counted) ReadAt(p []byte, off int64) (int, error) {
	if s.fail {
		return 0, errors.New("source is down")
	}
	s.reads++
	if off >= s.size {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > s.size-off {
		n = int(s.size - off)
	}
	for i := range n {
		// A position-derived pattern, so a view that reads the wrong offset
		// is a wrong value rather than plausible bytes.
		p[i] = byte((off + int64(i)) * 7)
	}
	s.bytes += int64(n)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func want(off int64, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((off + int64(i)) * 7)
	}
	return b
}

func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestBytesAtServesTheSourcesBytes is the base contract: whatever the window
// does internally, a view holds the source's bytes at the offset asked for.
func TestBytesAtServesTheSourcesBytes(t *testing.T) {
	src := &counted{size: 10 * Chunk}
	w := New(src, src.size, "test: reading")
	for _, off := range []int64{0, 1, Chunk - 1, Chunk, 3*Chunk + 17, 9 * Chunk} {
		got := w.BytesAt(off, 64)
		if !equal(got, want(off, 64)) {
			t.Fatalf("BytesAt(%d, 64) served the wrong bytes", off)
		}
	}
	if w.Err() != nil {
		t.Fatalf("Err = %v after clean reads", w.Err())
	}
}

// TestForwardExtensionKeepsEarlierBytesAddressable pins the aliasing guarantee
// load's grow path exists for, and the one every demuxer that reads a header
// and then its body depends on: the view taken before the extension still
// holds the bytes it held, at the same values, after the window has grown
// under it.
//
// It is the reason the grow path is not simply a rebase, and the reason it
// must not become one: flacn, mka and wv all take a header view, discover a
// length from it, extend the window to cover the body, and then use both.
func TestForwardExtensionKeepsEarlierBytesAddressable(t *testing.T) {
	src := &counted{size: 10 * Chunk}
	w := New(src, src.size, "test: reading")

	head := w.BytesAt(0, 16)
	if !equal(head, want(0, 16)) {
		t.Fatal("the header view served the wrong bytes")
	}
	// Extend well past the first chunk, several times, so the window both
	// grows within its capacity and outgrows it.
	for _, n := range []int{Chunk + 1, 2 * Chunk, 5 * Chunk} {
		body := w.BytesAt(0, n)
		if !equal(body, want(0, n)) {
			t.Fatalf("the %d-byte view served the wrong bytes", n)
		}
		if !equal(head, want(0, 16)) {
			t.Fatal("extending the window changed the bytes an earlier view held")
		}
	}
}

// TestViewSurvivesARebase pins the aliasing a rebase must not break, and it is
// not a theoretical one: flacn's frame-boundary scan holds a Chunk-sized view
// while it reads a CRC range BEHIND it for each sync candidate it tests, and a
// backward read is exactly what rebases the window. The bytes the held view
// serves have to keep serving.
//
// This is the reason the rebase path allocates where everything else was made
// to reuse. Reading the new range correctly is the easy half; leaving the old
// one readable is the half a demuxer depends on.
func TestViewSurvivesARebase(t *testing.T) {
	src := &counted{size: 10 * Chunk}
	w := New(src, src.size, "test: reading")

	held := w.BytesAt(5*Chunk, 4096)
	if !equal(held, want(5*Chunk, 4096)) {
		t.Fatal("the held view served the wrong bytes")
	}
	// Backwards, and far enough forward to leave a hole: both rebase.
	for _, off := range []int64{0, 9 * Chunk, 2 * Chunk} {
		if got := w.BytesAt(off, 32); !equal(got, want(off, 32)) {
			t.Fatalf("view at %d served the wrong bytes after a rebase", off)
		}
		if !equal(held, want(5*Chunk, 4096)) {
			t.Fatalf("a read at %d rewrote the bytes a held view was serving", off)
		}
	}
}

// TestBytesAtClampsToDataEnd covers the owner-shrinkable end of data: a
// trailing tag peeled off the stream must not be readable through the window,
// and a request that straddles the end comes back short rather than reading
// past it.
func TestBytesAtClampsToDataEnd(t *testing.T) {
	src := &counted{size: 4096}
	w := New(src, src.size, "test: reading")
	if got := w.BytesAt(4090, 64); len(got) != 6 {
		t.Errorf("view at the end is %d bytes, want the 6 that exist", len(got))
	}
	w.SetDataEnd(2048)
	if got := w.DataEnd(); got != 2048 {
		t.Errorf("DataEnd = %d, want 2048", got)
	}
	if got := w.BytesAt(2040, 64); len(got) != 8 {
		t.Errorf("view at the shortened end is %d bytes, want 8", len(got))
	}
	if got := w.BytesAt(2048, 64); got != nil {
		t.Errorf("view past the shortened end served %d bytes", len(got))
	}
	if got := w.BytesAt(0, 0); got != nil {
		t.Error("a zero-length view served bytes")
	}
}

// TestReadFailureIsSticky pins what a demuxer surfaces on its packet path: the
// first failure is kept and every later view is empty, so a caller that checks
// Err once at the end learns about a read that failed in the middle.
func TestReadFailureIsSticky(t *testing.T) {
	src := &counted{size: 10 * Chunk, fail: true}
	w := New(src, src.size, "test: reading")
	if got := w.BytesAt(0, 32); got != nil {
		t.Fatal("a failing source served bytes")
	}
	err := w.Err()
	if err == nil {
		t.Fatal("Err is nil after a failed read")
	}
	if code := waxerr.CodeOf(err); code != waxerr.CodeSourceUnreadable {
		t.Errorf("code = %q, want %q", code, waxerr.CodeSourceUnreadable)
	}
	src.fail = false
	if got := w.BytesAt(0, 32); got != nil {
		t.Error("the window served bytes after a failure; the error is meant to stick")
	}
	if w.Err() != err {
		t.Error("the sticky error was replaced by a later read")
	}
}

// TestTrimKeepsTheBytesAtAndAfterTheOffset pins Trim's contract: bytes before
// the offset are dropped, and a view taken after the Trim still reads the
// stream correctly from it. Trim is advisory about memory, never about
// content.
func TestTrimKeepsTheBytesAtAndAfterTheOffset(t *testing.T) {
	src := &counted{size: 10 * Chunk}
	w := New(src, src.size, "test: reading")
	// Fill a window well past one chunk so Trim has something to drop.
	if got := w.BytesAt(0, 3*Chunk); !equal(got, want(0, 3*Chunk)) {
		t.Fatal("the initial view served the wrong bytes")
	}
	for _, off := range []int64{0, Chunk, 2 * Chunk, 2*Chunk + 123} {
		w.Trim(off)
		if got := w.BytesAt(off, 64); !equal(got, want(off, 64)) {
			t.Fatalf("after Trim(%d) the view at that offset served the wrong bytes", off)
		}
	}
	// And past the window entirely, which resets it.
	w.Trim(8 * Chunk)
	if got := w.BytesAt(8*Chunk, 64); !equal(got, want(8*Chunk, 64)) {
		t.Fatal("after a Trim past the window the view served the wrong bytes")
	}
}

// TestLinearReadReusesOneWindow is the cost pin, over the access pattern every
// demuxer has: walk forward, trim behind, take a view.
//
// The property is that a linear read costs the window and nothing per packet,
// so reading a gigabyte allocates what reading a megabyte does. It was not
// true: trimming copied the kept bytes into fresh storage and threw the old
// storage away, so the extension that followed had no capacity to grow into
// and allocated again, and a forward walk spent about two window-sized
// allocations for every window of stream. Measured on a 16 MiB walk, that was
// 33 MB of garbage against the 0.4 MB it is now (TestLinearReadAllocatesOnce
// keeps that number).
//
// Asserted on the window's own capacity rather than on the runtime's
// allocation counters, because this has to hold under `go test -race` too, and
// there the detector's accounting swamps any measurement of the thing being
// measured. Capacity is the same property stated exactly: the backing array
// settles and is then reused, and the old implementation reshaped it on every
// Trim.
func TestLinearReadReusesOneWindow(t *testing.T) {
	src := &counted{size: 1 << 30}
	w := New(src, src.size, "test: reading")
	const step = 24576 // a 4096-frame packet of 24-bit stereo

	// Warm up past a few windows so the measurement is steady state.
	off := int64(0)
	for ; off < 8*Chunk; off += step {
		w.Trim(off)
		w.BytesAt(off, step)
	}
	settled := cap(w.win)
	changes := 0
	for end := off + 16<<20; off < end; off += step {
		w.Trim(off)
		if got := w.BytesAt(off, step); len(got) != step {
			t.Fatalf("view at %d is %d bytes", off, len(got))
		}
		if cap(w.win) != settled {
			changes++
			settled = cap(w.win)
		}
	}
	if changes != 0 {
		t.Errorf("the window was resized %d times while walking 16 MiB; a settled window is reused", changes)
	}
	if settled > 4*Chunk {
		t.Errorf("the window settled at %d bytes, want it bounded near %d", settled, Chunk)
	}
}

// TestWindowDoesNotAccreteTheWholeStream is the other half of the same
// property: Trim is what keeps a long forward walk from holding the file.
func TestWindowDoesNotAccreteTheWholeStream(t *testing.T) {
	src := &counted{size: 1 << 26}
	w := New(src, src.size, "test: reading")
	const step = 24576
	for off := int64(0); off < 1<<24; off += step {
		w.Trim(off)
		w.BytesAt(off, step)
	}
	if n := cap(w.win); n > 4*Chunk {
		t.Errorf("the window holds %d bytes after walking 16 MB, want it bounded near %d", n, Chunk)
	}
}

// TestBytesAtViewCannotScribbleOnItsNeighbours pins the full-capacity slicing:
// a caller that appends to a view must not write into the bytes after it,
// which is what a three-index slice prevents.
func TestBytesAtViewCannotScribbleOnItsNeighbours(t *testing.T) {
	src := &counted{size: 4 * Chunk}
	w := New(src, src.size, "test: reading")
	view := w.BytesAt(0, 16)
	if cap(view) != 16 {
		t.Fatalf("view capacity is %d, want its length 16", cap(view))
	}
	_ = append(view, 0xFF)
	if got := w.BytesAt(16, 16); !equal(got, want(16, 16)) {
		t.Error("appending to a view overwrote the bytes after it")
	}
}

var _ container.Source = (*counted)(nil)
