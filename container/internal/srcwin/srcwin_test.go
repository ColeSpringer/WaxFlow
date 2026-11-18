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

// TestViewSurvivesARebase: a view taken before a rebase still reads its bytes
// after it, whether the read that rebased was behind the window or past it.
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

// TestALinearReadCostsOneReadPerChunk: a packet path that trims to each
// packet and takes it through BytesAt reads the stream once, a read per Chunk,
// whatever the packet's size, and holds a bounded window.
func TestALinearReadCostsOneReadPerChunk(t *testing.T) {
	const size = 64 * Chunk
	for _, step := range []int{16, 417, 33880, 67126, Chunk - 1, Chunk} {
		src := &counted{size: size}
		w := New(src, src.size, "test: reading")
		held := 0
		for off := int64(0); off < size; off += int64(step) {
			w.Trim(off)
			n := int(min(int64(step), size-off))
			if got := w.BytesAt(off, n); len(got) != n {
				t.Fatalf("step %d: view at %d is %d bytes, want %d", step, off, len(got), n)
			}
			held = max(held, len(w.win))
		}
		if src.reads > size/Chunk || src.bytes != size {
			t.Errorf("step %d: a linear read of %d Chunks took %d reads of %d bytes, want one a Chunk and each byte once",
				step, size/Chunk, src.reads, src.bytes)
		}
		if held > 3*Chunk {
			t.Errorf("step %d: the window held %d bytes, want at most three Chunks", step, held)
		}
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

// TestPeekLoadsExactly is the second entry point's contract: a Peek makes
// resident exactly the bytes it asks for, whether that is a forward extension
// or a rebase, and the read-ahead round-up stays BytesAt's alone. It is what
// lets an open path read a magic or a header without paying a window for it.
func TestPeekLoadsExactly(t *testing.T) {
	src := &counted{size: 10 * Chunk}
	w := New(src, src.size, "test: reading")

	if got := w.Peek(0, 4); !equal(got, want(0, 4)) {
		t.Fatal("Peek(0, 4) served the wrong bytes")
	}
	if src.reads != 1 || src.bytes != 4 || len(w.win) != 4 {
		t.Errorf("Peek(0, 4) read %d bytes in %d reads and holds %d, want 4 in 1 holding 4", src.bytes, src.reads, len(w.win))
	}

	// A forward Peek extends by exactly the bytes past the window's end.
	src.reads, src.bytes = 0, 0
	if got := w.Peek(4, 417); !equal(got, want(4, 417)) {
		t.Fatal("Peek(4, 417) served the wrong bytes")
	}
	if src.reads != 1 || src.bytes != 417 || len(w.win) != 421 {
		t.Errorf("a forward Peek read %d bytes in %d reads and holds %d, want 417 in 1 holding 421", src.bytes, src.reads, len(w.win))
	}
	// A Peek inside the resident bytes reads nothing.
	src.reads = 0
	if got := w.Peek(2, 100); !equal(got, want(2, 100)) || src.reads != 0 {
		t.Errorf("a resident Peek read %d times", src.reads)
	}

	// A backward Peek rebases into exactly n bytes.
	src.reads, src.bytes = 0, 0
	if got := w.Peek(5*Chunk, 16); !equal(got, want(5*Chunk, 16)) {
		t.Fatal("Peek(5*Chunk, 16) served the wrong bytes")
	}
	if src.reads != 1 || src.bytes != 16 || len(w.win) != 16 || w.winOff != 5*Chunk {
		t.Errorf("a rebasing Peek read %d bytes in %d reads and holds %d at %d, want 16 in 1 holding 16 at %d",
			src.bytes, src.reads, len(w.win), w.winOff, 5*Chunk)
	}

	// A BytesAt on the same region then costs one read of a window: read-ahead
	// resumes where the exact read left off.
	src.reads, src.bytes = 0, 0
	if got := w.BytesAt(5*Chunk, 4096); !equal(got, want(5*Chunk, 4096)) {
		t.Fatal("BytesAt after a Peek served the wrong bytes")
	}
	if src.reads != 1 || src.bytes != Chunk {
		t.Errorf("BytesAt after a Peek read %d bytes in %d reads, want %d in 1", src.bytes, src.reads, Chunk)
	}

	// The data end clamps a Peek the way it clamps BytesAt.
	if got := w.Peek(src.size-2, 10); len(got) != 2 || !equal(got, want(src.size-2, 2)) {
		t.Errorf("Peek past the end returned %d bytes, want 2", len(got))
	}
	if w.Err() != nil {
		t.Fatalf("Err = %v after clean reads", w.Err())
	}
}

// TestPeekThenLinearReadSettles is the trace an MP3 open leaves: two exact
// reads for the head, then the packet walk. The walk must settle the window
// at the same bounded size a walk with no Peek before it does, so the cost
// pins above hold for every owner that opens through Peek.
func TestPeekThenLinearReadSettles(t *testing.T) {
	src := &counted{size: 1 << 30}
	w := New(src, src.size, "test: reading")
	w.Peek(0, 4)
	w.Peek(4, 417)
	const step = 24576

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
		t.Errorf("the window was resized %d times while walking 16 MiB after a Peek", changes)
	}
	if settled > 4*Chunk {
		t.Errorf("the window settled at %d bytes, want it bounded near %d", settled, Chunk)
	}
}

// TestHoldsReportsWhatIsResident: Holds says whether a range would be served
// from the window, and reads nothing to say it.
func TestHoldsReportsWhatIsResident(t *testing.T) {
	src := &counted{size: 10 * Chunk}
	w := New(src, src.size, "test: reading")
	if w.Holds(0, 1) {
		t.Error("an empty window holds a byte")
	}
	w.Peek(1000, 100)
	src.reads = 0
	for _, tc := range []struct {
		off  int64
		n    int
		want bool
	}{
		{1000, 100, true}, {1050, 50, true}, {1099, 1, true},
		{1000, 101, false}, {999, 2, false}, {1100, 1, false}, {0, 1, false},
	} {
		if got := w.Holds(tc.off, tc.n); got != tc.want {
			t.Errorf("Holds(%d, %d) = %v with [1000, 1100) resident, want %v", tc.off, tc.n, got, tc.want)
		}
	}
	if src.reads != 0 {
		t.Errorf("Holds read %d times with [1000, 1100) resident", src.reads)
	}
	// The last byte of the data, as a trailer probe asks for it: in a window
	// loaded to the data end, then with the data end pulled below the
	// window's end, then below its start.
	end := src.size
	w.BytesAt(end-100, 100)
	src.reads = 0
	for _, tc := range []struct {
		name    string
		dataEnd int64
		want    bool
	}{
		{"a window loaded to the data end", end, true},
		{"a data end below the window's end", end - 40, true},
		{"a data end at the window's start", end - 100, false},
	} {
		w.SetDataEnd(tc.dataEnd)
		if got := w.Holds(w.DataEnd()-1, 1); got != tc.want {
			t.Errorf("%s: Holds of the last byte = %v with [%d, %d) resident, want %v", tc.name, got, end-100, end, tc.want)
		}
	}
	if w.Holds(end-1, 2) {
		t.Error("a window loaded to the data end holds a byte behind it")
	}
	if src.reads != 0 {
		t.Errorf("Holds read %d times", src.reads)
	}
}

// TestAsideLeavesTheWindowAsItWas: a window set aside starts with what the
// window holds, at no read, and nothing done through it, a trim, a growth or
// a failed read, changes the window's bytes, what it holds or its error.
func TestAsideLeavesTheWindowAsItWas(t *testing.T) {
	src := &counted{size: 10 * Chunk}
	w := New(src, src.size, "test: reading")
	view := w.BytesAt(0, 2*Chunk)
	held, _ := w.Resident()
	src.reads = 0

	a := w.aside()
	if got := a.BytesAt(100, 64); !equal(got, want(100, 64)) || src.reads != 0 {
		t.Fatalf("the window set aside served the wrong bytes or read %d times for bytes the window holds", src.reads)
	}
	// Off a multiple of 256, which the pattern repeats at.
	a.Trim(Chunk + 13)
	if got := a.BytesAt(Chunk+13, 3*Chunk); !equal(got, want(Chunk+13, 3*Chunk)) {
		t.Fatal("the window set aside served the wrong bytes behind a trim and a growth")
	}
	a.Trim(3*Chunk + 29)
	if got := a.BytesAt(3*Chunk+29, 64); !equal(got, want(3*Chunk+29, 64)) {
		t.Fatal("the window set aside served the wrong bytes behind a second trim")
	}
	src.fail = true
	if a.BytesAt(9*Chunk, 64) != nil || a.Err() == nil {
		t.Fatal("a failed read through the window set aside did not stick on it")
	}
	src.fail = false

	if !equal(view, want(0, 2*Chunk)) {
		t.Error("the window's view changed under a window set aside")
	}
	if n, _ := w.Resident(); n != held || !w.Holds(0, 2*Chunk) || w.Err() != nil {
		t.Errorf("the window holds %d bytes with error %v, want the %d it held and none", n, w.Err(), held)
	}
	src.reads = 0
	if got := w.BytesAt(0, 2*Chunk); !equal(got, want(0, 2*Chunk)) || src.reads != 0 {
		t.Errorf("the window served the wrong bytes or read %d times for bytes it held", src.reads)
	}
}

// TestAnAsideHasNoRoomToGrowIntoAndNoError: a window that trimmed in place
// has room past its bytes, which an aside must not grow into, or its next
// trim moves the window's bytes; and an aside of a failed window can read.
func TestAnAsideHasNoRoomToGrowIntoAndNoError(t *testing.T) {
	src := &counted{size: 20 * Chunk}
	w := New(src, src.size, "test: reading")
	w.BytesAt(0, 4*Chunk)
	w.Trim(2 * Chunk)
	n, c := w.Resident()
	if c <= n {
		t.Fatalf("the window has no room to spare: %d of %d", n, c)
	}
	end := int64(2*Chunk + n)
	view := w.BytesAt(2*Chunk, n)
	a := w.aside()
	if _, ac := a.Resident(); ac != n {
		t.Errorf("the aside has room for %d, want its length %d", ac, n)
	}
	if got := a.BytesAt(end-8, 64); !equal(got, want(end-8, 64)) {
		t.Fatal("the aside served the wrong bytes over the window's end")
	}
	a.Trim(end - 16)
	if got := a.BytesAt(end-16, 128); !equal(got, want(end-16, 128)) {
		t.Fatal("the aside served the wrong bytes behind its trim")
	}
	if !equal(view, want(2*Chunk, n)) {
		t.Error("the window's bytes moved under an aside that grew and trimmed")
	}

	src.fail = true
	if w.BytesAt(15*Chunk, 8) != nil || w.Err() == nil {
		t.Fatal("a failed read did not stick on the window")
	}
	src.fail = false
	b := w.aside()
	if b.Err() != nil {
		t.Error("an aside of a failed window starts with its error")
	}
	if got := b.BytesAt(15*Chunk, 8); !equal(got, want(15*Chunk, 8)) {
		t.Error("an aside of a failed window does not read")
	}
	if w.Err() == nil {
		t.Error("the window's error went away")
	}
}

// TestTryKeepsWhatTheLookRead: a look that is kept leaves the window holding
// what it read, at no read again, whatever it answered.
func TestTryKeepsWhatTheLookRead(t *testing.T) {
	for _, answer := range []bool{true, false} {
		src := &counted{size: 10 * Chunk}
		w := New(src, src.size, "test: reading")
		w.BytesAt(0, 64)
		ok, err := w.Try(true, func() bool {
			w.BytesAt(8*Chunk, 64)
			return answer
		})
		if ok != answer || err != nil {
			t.Fatalf("Try answered %v, %v, want %v and no error", ok, err, answer)
		}
		src.reads = 0
		if got := w.BytesAt(8*Chunk, 64); !equal(got, want(8*Chunk, 64)) || src.reads != 0 || w.Err() != nil {
			t.Errorf("answering %v, the window read %d times for bytes the kept look read", answer, src.reads)
		}
	}
}

// TestTryDropsWhatTheLookRead: a look that is not kept leaves the window
// holding what it held, its views intact, whatever the look read or trimmed.
func TestTryDropsWhatTheLookRead(t *testing.T) {
	src := &counted{size: 10 * Chunk}
	w := New(src, src.size, "test: reading")
	view := w.BytesAt(0, 2*Chunk)
	held, _ := w.Resident()
	ok, err := w.Try(false, func() bool {
		w.BytesAt(Chunk, 3*Chunk)
		w.Trim(2 * Chunk)
		w.BytesAt(8*Chunk, 64)
		return true
	})
	if !ok || err != nil {
		t.Fatalf("Try answered %v, %v, want true and no error", ok, err)
	}
	if !equal(view, want(0, 2*Chunk)) {
		t.Error("the window's view changed under a dropped look")
	}
	src.reads = 0
	if n, _ := w.Resident(); n != held || !w.Holds(0, 2*Chunk) || w.Holds(8*Chunk, 64) || src.reads != 0 {
		t.Errorf("the window holds %d bytes, want the %d it held and none the look read", n, held)
	}
}

// TestAFailedReadInATryIsReturnedAndSticksNowhere: whichever way the look
// was to go, a read that fails in it comes back from Try beside false, and
// the window is what it held, with no error.
func TestAFailedReadInATryIsReturnedAndSticksNowhere(t *testing.T) {
	for _, keep := range []bool{true, false} {
		src := &counted{size: 10 * Chunk}
		w := New(src, src.size, "test: reading")
		w.BytesAt(0, 64)
		held, _ := w.Resident()
		ok, err := w.Try(keep, func() bool {
			src.fail = true
			w.BytesAt(8*Chunk, 64)
			src.fail = false
			return true
		})
		if ok || waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
			t.Errorf("keep %v: Try answered %v, %v, want false and the read's error", keep, ok, err)
		}
		if n, _ := w.Resident(); n != held || w.Holds(8*Chunk, 64) || w.Err() != nil {
			t.Errorf("keep %v: the window holds %d bytes with error %v, want the %d it held and none", keep, n, w.Err(), held)
		}
	}
}

// TestATryKeepsAFailureStuckBefore: a look reads through a clean aside, and
// the window's own failure is still stuck behind it, kept or not.
func TestATryKeepsAFailureStuckBefore(t *testing.T) {
	for _, keep := range []bool{true, false} {
		src := &counted{size: 10 * Chunk, fail: true}
		w := New(src, src.size, "test: reading")
		w.BytesAt(0, 64)
		stuck := w.Err()
		src.fail = false
		ok, err := w.Try(keep, func() bool { return len(w.BytesAt(Chunk, 64)) == 64 })
		if !ok || err != nil {
			t.Errorf("keep %v: Try answered %v, %v, want a look that reads", keep, ok, err)
		}
		if w.Err() != stuck {
			t.Errorf("keep %v: the window's error is %v, want the one stuck before", keep, w.Err())
		}
	}
}
