package testutil

import (
	"bytes"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// markScan is a JunkScan for a made-up format whose scan looks for 'M' and
// lands on "MK". It reads spans that start at first bytes and double, each
// one times over, and with perCandidate a byte more at every other 'M'.
func markScan(first int64, times int, perCandidate bool) JunkScan {
	return JunkScan{
		Pat:  []byte("M"),
		File: func(junk []byte) []byte { return append(append([]byte(nil), junk...), "MK"...) },
		Scan: func(t testing.TB, src container.Source, n int) {
			var off int64
			for span := first; off < src.Size(); span *= 2 {
				buf := make([]byte, min(span, src.Size()-off))
				for range times {
					if err := container.ReadFull(src, buf, off); err != nil {
						t.Fatal(err)
					}
				}
				for i, c := range buf {
					switch {
					case c != 'M':
					case off+int64(i) == int64(n):
						return
					case perCandidate:
						_, _ = src.ReadAt(make([]byte, 1), off+int64(i))
					}
				}
				off += int64(len(buf))
			}
			t.Fatalf("no mark behind %d bytes of junk", n)
		},
	}
}

func TestJunkScanBoundedBySpans(t *testing.T) {
	const content, growth, twice = "the reads must not depend on the content", "the reads grow with the junk's length", "want at most twice the junk"
	for _, tc := range []struct {
		name string
		scan JunkScan
		want []string // one in each error reported, in order
	}{
		{"a scan in spans", markScan(1<<10, 1, false), nil},
		// Short dense junk then costs more than long junk with nothing in it.
		{"a read per candidate", markScan(1<<10, 1, true), []string{content, growth}},
		{"the whole bound whatever the junk", markScan(1<<20, 1, false), []string{growth}},
		{"each span read three times", markScan(1<<10, 3, false), []string{twice}},
	} {
		rec := &errTB{TB: t}
		tc.scan.BoundedBySpans(rec, 2)
		ok := len(rec.errs) == len(tc.want)
		for i := 0; ok && i < len(tc.want); i++ {
			ok = strings.Contains(rec.errs[i], tc.want[i])
		}
		if !ok {
			t.Errorf("%s: reported %q, want %q", tc.name, rec.errs, tc.want)
		}
	}
}

func TestJunkHoldsItsPatternAndNothingElse(t *testing.T) {
	s := JunkScan{Pat: []byte("MAC")}
	if got := s.Junk(8, 0); !bytes.Equal(got, bytes.Repeat([]byte{0x11}, 8)) {
		t.Errorf("plain junk = %x", got)
	}
	if got, want := s.Junk(8, 4), []byte("MAC\x11MAC\x11"); !bytes.Equal(got, want) {
		t.Errorf("dense junk = %q, want %q", got, want)
	}
	// The last pattern keeps a junk byte behind it.
	if got, want := s.Junk(7, 4), []byte("MAC\x11\x11\x11\x11"); !bytes.Equal(got, want) {
		t.Errorf("dense junk cut at a pattern's end = %q, want %q", got, want)
	}
}

func TestSpansOverCountsDoublingSpans(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want int
	}{
		{0, 0}, {1, 1}, {1024, 1}, {1025, 2}, {3072, 2}, {3073, 3},
		// Eight spans reach 255 KiB at a 128 KiB chunk, and each one after is a chunk.
		{255 << 10, 8}, {255<<10 + 1, 9}, {383 << 10, 9}, {383<<10 + 1, 10},
	} {
		if got := SpansOver(tc.n, 128<<10); got != tc.want {
			t.Errorf("SpansOver(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
}

func TestRandomJunkDrawsFromItsValues(t *testing.T) {
	other, hot := []byte{1, 2, 3}, []byte("MAC")
	var long, withHot, without int
	for seed := range uint64(200) {
		rng := rand.New(rand.NewPCG(seed, 1))
		junk := RandomJunk(rng, other, hot, []int{0, 3})
		if again := RandomJunk(rand.New(rand.NewPCG(seed, 1)), other, hot, []int{0, 3}); !bytes.Equal(junk, again) {
			t.Fatalf("seed %d: two draws of one seed differ", seed)
		}
		if len(junk) >= 300<<10 {
			t.Fatalf("seed %d: %d bytes, want under 300 KiB", seed, len(junk))
		}
		if len(junk) >= 20<<10 {
			long++
		}
		if i := bytes.IndexFunc(junk, func(r rune) bool { return !bytes.ContainsRune(append(other, hot...), r) }); i >= 0 {
			t.Fatalf("seed %d: byte %#x at %d is neither a value nor the pattern's", seed, junk[i], i)
		}
		if bytes.ContainsAny(junk, "MAC") {
			withHot++
		} else {
			without++
		}
	}
	if long == 0 || withHot == 0 || without == 0 {
		t.Errorf("of 200 draws %d were long, %d held the pattern's bytes and %d did not, want some of each", long, withHot, without)
	}
}

func TestScanRangesAimAtWhatIsPlanted(t *testing.T) {
	const n, need, bound = 10000, 7, 1 << 20
	marks := []int64{4000, 9000}
	rng := rand.New(rand.NewPCG(1, 1))
	var atZero, onMark, acrossFirst, acrossSecond, bounded, empty, atFrom int
	for range 4000 {
		if off := ScanOffset(rng, marks, n); off < 0 || off > n {
			t.Fatalf("ScanOffset = %d, want within [0, %d]", off, n)
		}
		from := ScanFrom(rng, marks, n, need)
		if from < 0 || from > n {
			t.Fatalf("ScanFrom = %d, want within [0, %d]", from, n)
		}
		limit := ScanLimit(rng, marks, n, from, bound)
		for _, m := range marks {
			switch rel := m - from; {
			case rel == 0:
				onMark++
			case AcrossSpan(rel, need) == 1:
				acrossFirst++
			case AcrossSpan(rel, need) == 2:
				acrossSecond++
			}
		}
		switch {
		case from == 0:
			atZero++
		case limit == from+bound:
			bounded++
		case limit == from:
			atFrom++
		case limit < from:
			empty++
		}
	}
	ShapesReached(t,
		ScanShape{"a scan from 0", atZero}, ScanShape{"a scan from a mark", onMark},
		ScanShape{"a mark across the first span's end", acrossFirst}, ScanShape{"a mark across the second's", acrossSecond},
		ScanShape{"a limit at the bound", bounded}, ScanShape{"a limit at from", atFrom}, ScanShape{"a limit short of from", empty})
}

func TestAcrossSpanNamesTheSpanAViewRunsOver(t *testing.T) {
	for _, tc := range []struct {
		rel  int64
		need int
		want int
	}{
		{1017, 7, 0}, {1018, 7, 1}, {1023, 7, 1}, {1024, 7, 0},
		{3065, 7, 0}, {3066, 7, 2}, {3071, 7, 2}, {3072, 7, 0},
		{-1, 7, 0}, {1023, 1, 0}, {3071, 1, 0}, {1023, 2, 1},
	} {
		if got := AcrossSpan(tc.rel, tc.need); got != tc.want {
			t.Errorf("AcrossSpan(%d, %d) = %d, want %d", tc.rel, tc.need, got, tc.want)
		}
	}
}

// A view of one byte is across no span's end, so its scans start on a mark.
func TestScanFromTakesAViewOfOneByte(t *testing.T) {
	const n = 10000
	marks := []int64{4000, 9000}
	rng := rand.New(rand.NewPCG(1, 1))
	onMark := 0
	for range 400 {
		from := ScanFrom(rng, marks, n, 1)
		if from < 0 || from > n {
			t.Fatalf("ScanFrom = %d, want within [0, %d]", from, n)
		}
		if from == marks[0] || from == marks[1] {
			onMark++
		}
	}
	ShapesReached(t, ScanShape{"a scan from a mark", onMark})
}

func TestShapesReachedNamesTheShapeThatWasNot(t *testing.T) {
	rec := &errTB{TB: t}
	ShapesReached(rec, ScanShape{"found", 20}, ScanShape{"cut by the limit", 19})
	if len(rec.errs) != 1 || !strings.Contains(rec.errs[0], "19 cases were cut by the limit") {
		t.Errorf("reported %q, want one error naming the shape nineteen cases took", rec.errs)
	}
}

func TestFailingSourceFailsAReadThatTouchesItsRange(t *testing.T) {
	data := container.BytesSource([]byte("0123456789"))
	p := make([]byte, 3)
	for _, tc := range []struct {
		src    *FailingSource
		behind bool // a read at 8 fails
		failed int
	}{
		{&FailingSource{Src: data, From: 6}, true, 2}, // to the end of the source
		{&FailingSource{Src: data, From: 6, To: 8}, false, 1},
	} {
		src := tc.src
		if n, err := src.ReadAt(p, 3); err != nil || n != 3 || string(p) != "345" {
			t.Errorf("to %d: a read ending at the range = %d, %v, %q", src.To, n, err, p)
		}
		if _, err := src.ReadAt(p, 4); err != ErrSourceDown {
			t.Errorf("to %d: a read reaching into the range returned %v, want ErrSourceDown", src.To, err)
		}
		if _, err := src.ReadAt(p[:2], 8); (err != nil) != tc.behind {
			t.Errorf("to %d: a read behind the range returned %v", src.To, err)
		}
		if src.Failed != tc.failed || src.FailedAt != [2]int64{4, 7} {
			t.Errorf("to %d: Failed = %d and FailedAt = %v, want %d and the first failed read's [4 7]", src.To, src.Failed, src.FailedAt, tc.failed)
		}
		if src.Size() != 10 {
			t.Errorf("Size = %d, want the source's 10", src.Size())
		}
	}
	// From at the source's size holds no byte: a read that runs past the end
	// is the source's own short read.
	src := &FailingSource{Src: data, From: data.Size()}
	if n, err := src.ReadAt(p, 8); err == ErrSourceDown || n != 2 || src.Failed != 0 {
		t.Errorf("from the source's size: a read running past the end = %d, %v after %d failed reads, want the source's 2 bytes", n, err, src.Failed)
	}
}

func TestFailingSourceFailsEveryReadBehindItsCount(t *testing.T) {
	data := container.BytesSource([]byte("0123456789"))
	src := &FailingSource{Src: data, From: data.Size(), After: 2}
	p := make([]byte, 3)
	for i, off := range []int64{7, 0, 3, 0} {
		_, err := src.ReadAt(p, off)
		if i < 2 && err != nil || i >= 2 && err != ErrSourceDown {
			t.Errorf("read %d of a source that fails behind 2 returned %v", i+1, err)
		}
	}
	if src.Failed != 2 || src.FailedAt != [2]int64{3, 6} {
		t.Errorf("Failed = %d and FailedAt = %v, want 2 and the third read's [3 6]", src.Failed, src.FailedAt)
	}
}
