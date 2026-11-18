package srcwin

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// planted is a source over real bytes that counts its reads, for the tests
// that plant a pattern: counted's bytes come from the offset alone. failAt
// fails that read and every later one, counted from 1.
type planted struct {
	data   []byte
	reads  int
	bytes  int64
	end    int64   // the furthest byte read
	ends   []int64 // where each read ended, in order
	failAt int
}

func (s *planted) Size() int64 { return int64(len(s.data)) }

func (s *planted) ReadAt(p []byte, off int64) (int, error) {
	if s.failAt != 0 && s.reads+1 >= s.failAt {
		return 0, errors.New("source is down")
	}
	s.reads++
	if off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[off:])
	s.bytes += int64(n)
	s.end = max(s.end, off+int64(n))
	s.ends = append(s.ends, off+int64(n))
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// filler is n bytes that vary with the offset and hold neither ASCII nor
// 0xFF, so a planted pattern is the only one and a view of the wrong offset
// is the wrong bytes.
func filler(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 0x80 + byte(i%0x7F)
	}
	return b
}

func skipAll(int64, []byte) Verdict { return Skip }

// TestScanReadsGrowWithLengthNotContent is the scan's cost: a range made of
// nothing but the pattern costs the reads of a range holding none, each byte
// read once, and a longer range costs more reads than a short one.
func TestScanReadsGrowWithLengthNotContent(t *testing.T) {
	pat := []byte("wvpk")
	scan := func(data []byte) (reads, offered int) {
		src := &planted{data: data}
		w := New(src, src.Size(), "test: reading")
		off, ok := w.Scan(0, src.Size(), pat, 32, func(int64, []byte) Verdict {
			offered++
			return Skip
		})
		if ok {
			t.Fatalf("a scan whose try takes nothing found %d", off)
		}
		if src.bytes != src.Size() {
			t.Errorf("scanning %d bytes read %d, want each byte once", src.Size(), src.bytes)
		}
		return src.reads, offered
	}
	const big = 512 << 10
	dense, offered := scan(bytes.Repeat(pat, big/len(pat)))
	if offered != big/len(pat) {
		t.Errorf("try was offered %d candidates, want each of the %d once", offered, big/len(pat))
	}
	none, _ := scan(make([]byte, big))
	if dense > 12 || dense != none {
		t.Errorf("512 KiB of the pattern cost %d reads and 512 KiB holding none cost %d, want the same and at most 12", dense, none)
	}
	if small, _ := scan(bytes.Repeat(pat, (4<<10)/len(pat))); small >= dense {
		t.Errorf("4 KiB cost %d reads and 512 KiB cost %d; the reads grow with the range", small, dense)
	}
}

// TestScanReadsWhatSpansOverCounts ties testutil.SpansOver, which the
// demuxers' read bounds are built on, to the spans a scan reads: over bytes
// with no candidate in them a scan of n bytes makes exactly that many reads.
func TestScanReadsWhatSpansOverCounts(t *testing.T) {
	for _, n := range []int64{1, 1023, 1024, 1025, 3072, 3073, 100 << 10, 255 << 10, 255<<10 + 1, 1 << 20, 3<<20 + 7} {
		for _, from := range []int64{0, 777} {
			// The view's fifteen bytes past the limit are there to read.
			src := &planted{data: filler(int(from+n) + 64)}
			w := New(src, src.Size(), "test: reading")
			if off, ok := w.Scan(from, from+n, []byte{0xFF}, 16, skipAll); ok {
				t.Fatalf("a scan of filler found %d", off)
			}
			if want := testutil.SpansOver(n, Chunk); src.reads != want {
				t.Errorf("a scan of %d bytes from %d took %d reads, SpansOver says %d", n, from, src.reads, want)
			}
		}
	}
}

// TestScanFindsAPatternAcrossEverySpanBoundary plants the pattern at every
// offset around each span's end: cut by it, whole with its view cut, and
// whole on either side. Each is offered once, with a whole view.
func TestScanFindsAPatternAcrossEverySpanBoundary(t *testing.T) {
	const size = 400 << 10
	for _, tc := range []struct {
		pat  []byte
		need int
		ends []int64 // where the spans end, stated for the one-byte pattern
	}{
		{[]byte{0xFF}, 4, []int64{1024, 3072, 7168, 15360, 31744, 64512, 130048, 261120, 392192}},
		{[]byte("wvpk"), 32, nil},
	} {
		data, clean := filler(size), filler(size)
		// A scan that finds nothing reads one span a read; the last read
		// ends with the data.
		empty := &planted{data: data}
		w := New(empty, size, "test: reading")
		w.Scan(0, size, tc.pat, tc.need, skipAll)
		ends := empty.ends[:len(empty.ends)-1]
		if len(ends) != 9 || (tc.ends != nil && !slices.Equal(ends, tc.ends)) {
			t.Fatalf("%q: the spans end at %v, want nine of them and %v for one byte", tc.pat, ends, tc.ends)
		}
		for _, end := range ends {
			for at := end - int64(tc.need) - 1; at <= end+1; at++ {
				copy(data[at:], tc.pat)
				src := &planted{data: data}
				w := New(src, size, "test: reading")
				offers := 0
				got, ok := w.Scan(0, size, tc.pat, tc.need, func(off int64, hdr []byte) Verdict {
					offers++
					if off != at || !bytes.Equal(hdr, data[at:at+int64(tc.need)]) {
						t.Errorf("%q at %d (span end %d): offered %d with %d bytes", tc.pat, at, end, off, len(hdr))
					}
					return Take
				})
				if !ok || got != at || offers != 1 {
					t.Errorf("%q at %d (span end %d): Scan = %d, %v after %d offers", tc.pat, at, end, got, ok, offers)
				}
				copy(data[at:], clean[at:at+int64(len(tc.pat))])
			}
		}
	}
}

// TestScanHonoursLimit: a pattern that starts at limit-1 is offered, with a
// view that runs past limit; one that starts at limit is not, and the scan
// reads no further than the last view it could offer.
func TestScanHonoursLimit(t *testing.T) {
	pat := []byte("MAC")
	const need = 6
	const size = 8 << 10
	// At the last limit the data ends inside the view of a pattern at it.
	for _, limit := range []int64{1, 500, 1024, 1025, 3072, 5000, size - 3} {
		for _, from := range []int64{0, limit - 1} {
			for _, at := range []int64{limit - 1, limit} {
				data := filler(size)
				copy(data[at:], pat)
				src := &planted{data: data}
				w := New(src, src.Size(), "test: reading")
				offers := 0
				got, ok := w.Scan(from, limit, pat, need, func(off int64, hdr []byte) Verdict {
					offers++
					if off != at || !bytes.Equal(hdr, data[at:min(at+need, size)]) {
						t.Errorf("limit %d from %d: offered %d with %q", limit, from, off, hdr)
					}
					return Take
				})
				if below := at < limit; ok != below || offers != btoi(below) || (below && got != at) {
					t.Errorf("limit %d from %d, pattern at %d: Scan = %d, %v after %d offers", limit, from, at, got, ok, offers)
				}
				if src.end > limit+need-1 {
					t.Errorf("limit %d from %d: read up to %d, past the last view below the limit", limit, from, src.end)
				}
			}
		}
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestScanShortViewAtEndOfData: a candidate in the last bytes of the data is
// offered with the bytes there are, and the data ends where the window was
// told it does, whatever the source holds behind it.
func TestScanShortViewAtEndOfData(t *testing.T) {
	pat := []byte{0xFF}
	const need = 4
	type offer struct {
		off  int64
		view string
	}
	for _, size := range []int{2, 1000, 1024, 1026, 3072, 3073, 5000} {
		for _, limit := range []int64{int64(size), int64(size) + 100} {
			// The pattern at each of the last six offsets, and more of it
			// behind the end of data.
			data := append(filler(size), bytes.Repeat(pat, 16)...)
			var want []offer
			for at := max(size-6, 0); at < size; at++ {
				data[at] = 0xFF
			}
			for at := max(size-6, 0); at < size; at++ {
				want = append(want, offer{int64(at), string(data[at:min(at+need, size)])})
			}
			src := &planted{data: data}
			w := New(src, int64(size), "test: reading")
			var got []offer
			if off, ok := w.Scan(0, limit, pat, need, func(off int64, hdr []byte) Verdict {
				got = append(got, offer{off, string(hdr)})
				return Skip
			}); ok {
				t.Fatalf("a scan whose try takes nothing found %d", off)
			}
			if !slices.Equal(got, want) {
				t.Errorf("%d bytes, limit %d: offered %x, want %x", size, limit, got, want)
			}
			if src.end > int64(size) {
				t.Errorf("%d bytes: read up to %d, past the end of data", size, src.end)
			}
		}
	}
}

// TestScanStops: Take returns the candidate it answers, Stop returns not
// found, and try is offered nothing after either.
func TestScanStops(t *testing.T) {
	data := bytes.Repeat([]byte{0, 0xFF}, 4<<10) // a candidate at every odd offset
	for _, tc := range []struct {
		name string
		at   int // the candidate that gets the verdict, counted from 0
		v    Verdict
	}{
		{"take the first", 0, Take},
		{"stop at the first", 0, Stop},
		{"take the fifth", 4, Take},
		{"stop at the fifth", 4, Stop},
		{"take one in the second span", 600, Take},
		{"stop at one in the second span", 600, Stop},
	} {
		src := &planted{data: data}
		w := New(src, src.Size(), "test: reading")
		offers := 0
		got, ok := w.Scan(0, src.Size(), []byte{0xFF}, 2, func(off int64, hdr []byte) Verdict {
			offers++
			if offers-1 == tc.at {
				return tc.v
			}
			return Skip
		})
		want := int64(0)
		if tc.v == Take {
			want = int64(2*tc.at + 1)
		}
		if ok != (tc.v == Take) || got != want || offers != tc.at+1 {
			t.Errorf("%s: Scan = %d, %v after %d offers, want %d, %v after %d", tc.name, got, ok, offers, want, tc.v == Take, tc.at+1)
		}
		if w.Err() != nil {
			t.Errorf("%s: Err = %v", tc.name, w.Err())
		}
	}

	// An empty pattern occurs nowhere.
	w := New(&planted{data: data}, int64(len(data)), "test: reading")
	if got, ok := w.Scan(0, int64(len(data)), nil, 2, func(off int64, hdr []byte) Verdict {
		t.Errorf("an empty pattern was offered at %d", off)
		return Stop
	}); ok || got != 0 {
		t.Errorf("an empty pattern: Scan = %d, %v", got, ok)
	}
}

// TestScanViewCannotScribbleOnTheWindow: the view try is handed is
// full-capacity, as BytesAt's is, so an append to it leaves the bytes behind
// it, and the candidates in them, alone.
func TestScanViewCannotScribbleOnTheWindow(t *testing.T) {
	data := bytes.Repeat([]byte{0xFF, 1, 2, 3}, 64)
	src := &planted{data: data}
	w := New(src, src.Size(), "test: reading")
	offers := 0
	w.Scan(0, src.Size(), []byte{0xFF}, 2, func(off int64, hdr []byte) Verdict {
		offers++
		_ = append(hdr, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA)
		return Skip
	})
	if offers != 64 || !bytes.Equal(w.win, data) {
		t.Errorf("a try that appends to its view was offered %d of 64 candidates; the window still holds its data: %v",
			offers, bytes.Equal(w.win, data))
	}
}

// TestAheadReadsTheFormula pins the one read an Ahead makes: the larger of
// what the request lacks and what the window holds, the second capped at a
// Chunk, the read clamped to the data end.
func TestAheadReadsTheFormula(t *testing.T) {
	const base = 1000
	for _, tc := range []struct {
		name string
		size int64 // the data end
		held int   // the window holds [base, base+held)
		off  int64 // from base
		n    int
		read int64
	}{
		{"resident", 10 * Chunk, 4096, 100, 3996, 0},
		{"a byte past a small window", 10 * Chunk, 100, 0, 101, 100},
		{"more missing than held", 10 * Chunk, 100, 50, 550, 500},
		{"a byte past 4 KiB", 10 * Chunk, 4096, 4000, 97, 4096},
		{"from the window's end", 10 * Chunk, 4096, 4096, 10, 4096},
		{"a byte past a Chunk", 10 * Chunk, Chunk, 0, Chunk + 1, Chunk},
		{"a byte past three Chunks", 10 * Chunk, 3 * Chunk, 3*Chunk - 1, 2, Chunk},
		{"more missing than a Chunk", 10 * Chunk, 3 * Chunk, 3 * Chunk, 2 * Chunk, 2 * Chunk},
		{"clamped to the data end", base + 4096 + 50, 4096, 4000, 200, 50},
		{"nothing left to read", base + 4096, 4096, 4000, 200, 0},
	} {
		src := &counted{size: tc.size}
		w := New(src, src.size, "test: reading")
		w.Peek(base, tc.held)
		src.reads, src.bytes = 0, 0

		n := int(min(int64(tc.n), tc.size-base-tc.off))
		if got := w.Ahead(base+tc.off, tc.n); !equal(got, want(base+tc.off, n)) {
			t.Errorf("%s: Ahead served %d wrong bytes, want %d", tc.name, len(got), n)
		}
		if src.bytes != tc.read || src.reads != btoi(tc.read > 0) {
			t.Errorf("%s: read %d bytes in %d reads, want %d in %d", tc.name, src.bytes, src.reads, tc.read, btoi(tc.read > 0))
		}
		if w.winOff != base || int64(len(w.win)) != int64(tc.held)+tc.read {
			t.Errorf("%s: the window holds %d bytes at %d, want %d at %d", tc.name, len(w.win), w.winOff, int64(tc.held)+tc.read, base)
		}
	}

	// A window that holds nothing reads what is asked for.
	src := &counted{size: 10 * Chunk}
	w := New(src, src.size, "test: reading")
	if got := w.Ahead(0, 300); !equal(got, want(0, 300)) || src.reads != 1 || src.bytes != 300 {
		t.Errorf("Ahead on an empty window read %d bytes in %d reads, want 300 in 1", src.bytes, src.reads)
	}

	// An off the window neither holds nor ends at is BytesAt's.
	for _, off := range []int64{base - 500, base + 4097, 5 * Chunk} {
		a, b := &counted{size: 10 * Chunk}, &counted{size: 10 * Chunk}
		wa, wb := New(a, a.size, "test: reading"), New(b, b.size, "test: reading")
		wa.Peek(base, 4096)
		wb.Peek(base, 4096)
		if got := wa.Ahead(off, 64); !equal(got, wb.BytesAt(off, 64)) || !equal(got, want(off, 64)) {
			t.Errorf("Ahead(%d) outside the window served the wrong bytes", off)
		}
		if a.reads != b.reads || a.bytes != b.bytes || wa.winOff != wb.winOff || len(wa.win) != len(wb.win) {
			t.Errorf("Ahead(%d) outside the window read %d bytes in %d reads and holds %d at %d; BytesAt read %d in %d and holds %d at %d",
				off, a.bytes, a.reads, len(wa.win), wa.winOff, b.bytes, b.reads, len(wb.win), wb.winOff)
		}
	}
}

// TestAheadCostsAReadPerDoubling is the confirm a scan's try makes, at its
// worst: every candidate asks, from itself, for a little more than the window
// holds. The reads double the window and then go a Chunk at a time, and the
// window stays where the scan began and within twice what was asked for.
func TestAheadCostsAReadPerDoubling(t *testing.T) {
	pat := []byte("wvpk")
	const size, ask = 512 << 10, 9000
	src := &planted{data: bytes.Repeat(pat, size/len(pat))}
	w := New(src, size, "test: reading")
	asked := int64(0)
	w.Scan(0, size, pat, 32, func(off int64, hdr []byte) Verdict {
		got := w.Ahead(off, ask)
		if len(got) != int(min(ask, size-off)) || !bytes.HasPrefix(got, pat) {
			t.Fatalf("Ahead(%d, %d) served %d bytes", off, ask, len(got))
		}
		asked = max(asked, off+int64(len(got)))
		if n, _ := w.Resident(); int64(n) > 2*asked {
			t.Fatalf("the window holds %d bytes with %d asked for, want at most twice", n, asked)
		}
		return Skip
	})
	if src.reads > 24 {
		t.Errorf("a confirm at every candidate of 512 KiB cost %d reads, want at most 24", src.reads)
	}
	if src.bytes != size {
		t.Errorf("read %d bytes of %d, want each byte once", src.bytes, size)
	}
	if n, _ := w.Resident(); w.winOff != 0 || n != size {
		t.Errorf("the window holds %d bytes at %d after the scan, want the %d scanned at 0", n, w.winOff, size)
	}
}

// TestScanOffersEveryCandidateWhenTryMovesTheWindow: a try that reads off
// either end of the window rebases the window there. The scan still offers
// each candidate once, ascending, with its own bytes, as a search finds them.
func TestScanOffersEveryCandidateWhenTryMovesTheWindow(t *testing.T) {
	pat := []byte("MAC")
	const size, from, need = 300 << 10, 777, 16
	data := filler(size)
	var want []int64
	for at := from; at+len(pat) <= size; at += 701 {
		copy(data[at:], pat)
		want = append(want, int64(at))
	}
	for _, tc := range []struct {
		name string
		far  func(w *Window, off int64, i int) int64 // where try reads
		take int                                     // the candidate try takes, from 0; -1 for none
	}{
		{"a step past the window's end", func(w *Window, _ int64, _ int) int64 {
			return w.winOff + int64(len(w.win)) + 5000
		}, -1},
		{"anywhere behind the candidate", func(_ *Window, off int64, i int) int64 {
			return off + 2000 + int64(i*7919%200000)
		}, -1},
		{"anywhere behind the candidate, and one is taken", func(_ *Window, off int64, i int) int64 {
			return off + 2000 + int64(i*7919%200000)
		}, 300},
	} {
		src := &planted{data: data}
		w := New(src, size, "test: reading")
		var got []int64
		moved := 0
		off, ok := w.Scan(from, size, pat, need, func(off int64, hdr []byte) Verdict {
			at, was := min(tc.far(&w, off, len(got)), size), w.winOff
			if b := w.Ahead(at, 32); !bytes.Equal(b, data[at:min(at+32, size)]) {
				t.Fatalf("%s: Ahead(%d) served the wrong bytes", tc.name, at)
			}
			if w.winOff != was {
				moved++
			}
			if !bytes.Equal(hdr, data[off:min(off+need, size)]) {
				t.Fatalf("%s: the view at %d holds the wrong bytes", tc.name, off)
			}
			got = append(got, off)
			if len(got)-1 == tc.take {
				return Take
			}
			return Skip
		})
		wantOff, offered := int64(0), want
		if tc.take >= 0 {
			wantOff, offered = want[tc.take], want[:tc.take+1]
		}
		if ok != (tc.take >= 0) || off != wantOff || !slices.Equal(got, offered) {
			t.Errorf("%s: Scan = %d, %v after %d offers, want %d, %v after %d", tc.name, off, ok, len(got), wantOff, tc.take >= 0, len(offered))
		}
		if moved < 10 || src.bytes <= size {
			t.Errorf("%s: the window moved %d times and %d bytes were read; the case no longer moves it", tc.name, moved, src.bytes)
		}
		if w.Err() != nil {
			t.Errorf("%s: Err = %v", tc.name, w.Err())
		}
	}
}

// TestScanNeverTrims: after a scan the window still starts where it did, at
// from for a window that held nothing there, and runs to the furthest byte
// read. Several Chunks, so a loop that trimmed would show.
func TestScanNeverTrims(t *testing.T) {
	pat := []byte("wvpk")
	const size, from = 6 * Chunk, 5000
	data := bytes.Repeat(pat, size/len(pat))
	for _, tc := range []struct {
		name  string
		held  int64 // the window holds [held, from+1000) before the scan; -1 for nothing
		ahead int   // what try asks Ahead for at each candidate
		take  int64 // the candidate try takes; 0 for none
	}{
		{"a fresh window", -1, 0, 0},
		{"a window that holds from", 100, 0, 0},
		{"a try that reads ahead", -1, 9000, 0},
		{"a scan that finds", -1, 9000, 3*Chunk + 4},
	} {
		src := &planted{data: data}
		w := New(src, size, "test: reading")
		start := int64(from)
		if tc.held >= 0 {
			start = tc.held
			w.Peek(tc.held, int(from+1000-tc.held))
		}
		got, ok := w.Scan(from, size, pat, 32, func(off int64, hdr []byte) Verdict {
			if tc.ahead > 0 {
				w.Ahead(off, tc.ahead)
			}
			if off == tc.take {
				return Take
			}
			return Skip
		})
		if ok != (tc.take != 0) || got != tc.take {
			t.Errorf("%s: Scan = %d, %v", tc.name, got, ok)
		}
		if tc.take == 0 && src.end != size {
			t.Errorf("%s: the scan read up to %d of %d", tc.name, src.end, size)
		}
		if n, _ := w.Resident(); w.winOff != start || w.winOff+int64(n) != src.end {
			t.Errorf("%s: the window holds [%d, %d) after the scan, want [%d, %d)", tc.name, w.winOff, w.winOff+int64(n), start, src.end)
		}
	}
}

// TestScanTryMayReadBackToFrom: everything from from to the candidate's view
// is resident when try runs, so reading it costs nothing, and the view try
// was handed still holds its bytes afterwards.
func TestScanTryMayReadBackToFrom(t *testing.T) {
	pat := []byte("MAC")
	const size, from, need = 300 << 10, 777, 16
	data := filler(size)
	for at := from; at < size; at += 701 {
		copy(data[at:], pat)
	}
	scan := func(back bool) (reads int, offs []int64) {
		src := &planted{data: data}
		w := New(src, size, "test: reading")
		w.Scan(from, size, pat, need, func(off int64, hdr []byte) Verdict {
			end := min(off+need, size)
			if back {
				if all := w.Ahead(from, int(end-from)); !bytes.Equal(all, data[from:end]) {
					t.Fatalf("at %d: reading back to from served the wrong bytes", off)
				}
			}
			if !bytes.Equal(hdr, data[off:end]) {
				t.Fatalf("at %d: the view holds the wrong bytes", off)
			}
			offs = append(offs, off)
			return Skip
		})
		if w.Err() != nil {
			t.Fatalf("Err = %v", w.Err())
		}
		return src.reads, offs
	}
	plainReads, plain := scan(false)
	backReads, back := scan(true)
	if len(plain) != (size-from+700)/701 || !slices.Equal(back, plain) {
		t.Errorf("a try that reads back was offered %d candidates, one that does not %d", len(back), len(plain))
	}
	if backReads != plainReads {
		t.Errorf("a try that reads back to from cost %d reads, one that does not %d", backReads, plainReads)
	}
}

// TestScanEndsOnReadFailure: a read that fails ends the scan as not found,
// whether the scan made it or try did and whatever try then answered, and the
// failure stays in Err.
func TestScanEndsOnReadFailure(t *testing.T) {
	data := bytes.Repeat([]byte{0, 0xFF}, 64<<10)
	for _, tc := range []struct {
		name   string
		failAt int     // the scan's read that fails; 0 for none
		inTry  bool    // try's own read fails, at the first candidate
		v      Verdict // what try answers
		offers int
	}{
		{"the first read", 1, false, Skip, 0},
		// The candidate at 3071 opens the third span, whose read fails.
		{"the third span", 3, false, Skip, 3072/2 - 1},
		{"a read try makes", 0, true, Skip, 1},
		{"a read try makes before it takes", 0, true, Take, 1},
		{"a read try makes before it stops", 0, true, Stop, 1},
	} {
		src := &planted{data: data, failAt: tc.failAt}
		w := New(src, src.Size(), "test: reading")
		offers := 0
		got, ok := w.Scan(0, src.Size(), []byte{0xFF}, 2, func(off int64, hdr []byte) Verdict {
			offers++
			if off >= src.end {
				t.Fatalf("%s: offered %d, past the %d bytes read", tc.name, off, src.end)
			}
			if tc.inTry {
				src.failAt = src.reads + 1
				if b := w.Ahead(off, 1<<20); b != nil {
					t.Fatalf("%s: a failed read served %d bytes", tc.name, len(b))
				}
			}
			return tc.v
		})
		if ok || got != 0 || offers != tc.offers {
			t.Errorf("%s: Scan = %d, %v after %d offers, want not found after %d", tc.name, got, ok, offers, tc.offers)
		}
		if code := waxerr.CodeOf(w.Err()); code != waxerr.CodeSourceUnreadable {
			t.Errorf("%s: Err = %v, want %q", tc.name, w.Err(), waxerr.CodeSourceUnreadable)
		}
	}
}
