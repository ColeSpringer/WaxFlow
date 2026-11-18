package testutil

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// JunkScan is one demuxer's scan over junk, as the cost tests of its package
// drive it.
type JunkScan struct {
	// Pat is what the scan looks for.
	Pat []byte
	// File is junk with what the scan must find behind it.
	File func(junk []byte) []byte
	// Scan runs the scan over src and fails t unless it lands behind n bytes
	// of junk.
	Scan func(t testing.TB, src container.Source, n int)
}

// Junk returns n bytes that hold no candidate, and with every above zero
// s.Pat at each multiple of it: a candidate the byte behind it rules out.
func (s JunkScan) Junk(n, every int) []byte {
	b := bytes.Repeat([]byte{0x11}, n)
	for i := 0; every > 0 && i+len(s.Pat) < n; i += every {
		copy(b[i:], s.Pat)
	}
	return b
}

// Cost is the reads the scan takes over s.File(junk), and the bytes they read.
func (s JunkScan) Cost(t testing.TB, junk []byte) (reads int, read int64) {
	t.Helper()
	src := &CountingSource{Src: container.BytesSource(s.File(junk))}
	s.Scan(t, src, len(junk))
	return src.Reads, src.Bytes
}

// BoundedBySpans holds the scan to reading its junk in spans: junk dense with
// candidates costs the reads of junk with none, the bytes stay within twice
// the junk and past (what it may read behind it), and reads grow with length.
func (s JunkScan) BoundedBySpans(t testing.TB, past int64) {
	t.Helper()
	// The long junk is several windows, so a scan that reads on at each
	// candidate cannot find them all resident.
	const big, small = 512 << 10, 4 << 10
	plainReads, plainBytes := s.Cost(t, s.Junk(big, 0))
	denseReads, _ := s.Cost(t, s.Junk(big, 100))
	if plainBytes > 2*big+past {
		t.Errorf("scanning %d bytes of junk read %d bytes, want at most twice the junk and %d", big, plainBytes, past)
	}
	if denseReads != plainReads {
		t.Errorf("junk dense with candidates cost %d reads, junk with none %d; the reads must not depend on the content", denseReads, plainReads)
	}
	smallReads, _ := s.Cost(t, s.Junk(small, 100))
	if smallReads >= plainReads {
		t.Errorf("%d bytes of junk cost %d reads and %d bytes cost %d; the reads grow with the junk's length", small, smallReads, big, plainReads)
	}
}

// SpansOver is how many of a scan's spans, 1 KiB doubling up to chunk, cover
// n bytes: the reads a scan of n bytes makes when nothing in it asks for
// more.
func SpansOver(n, chunk int64) int {
	spans := 0
	for span := int64(1 << 10); n > 0; span = min(2*span, chunk) {
		n -= span
		spans++
	}
	return spans
}

// RandomJunk is the junk one seeded case scans: under 20 KiB, now and then up
// to 300 KiB, over sixteen byte values drawn from other. One of hots says how
// many of them are hot's bytes instead, so candidates turn up by chance.
func RandomJunk(rng *rand.Rand, other, hot []byte, hots []int) []byte {
	n := rng.IntN(20 << 10)
	if rng.IntN(16) == 0 {
		n = rng.IntN(300 << 10)
	}
	k := hots[rng.IntN(len(hots))]
	var values [16]byte
	for i := range values {
		values[i] = other[rng.IntN(len(other))]
		if i < k {
			values[i] = hot[i%len(hot)]
		}
	}
	data := make([]byte, n)
	for i := 0; i < n; i += 16 {
		r := rng.Uint64()
		for j := i; j < min(i+16, n); j++ {
			data[j] = values[r&15]
			r >>= 4
		}
	}
	return data
}

// ScanOffset is an offset worth aiming a scan at in n bytes: within four
// bytes of one of marks, or anywhere.
func ScanOffset(rng *rand.Rand, marks []int64, n int64) int64 {
	if len(marks) > 0 && rng.IntN(2) == 0 {
		return min(max(marks[rng.IntN(len(marks))]+int64(rng.IntN(9))-4, 0), n)
	}
	return rng.Int64N(n + 1)
}

// spanEnds are where a scan's first span and its second end, from its start.
var spanEnds = [2]int64{1 << 10, 3 << 10}

// AcrossSpan is which of a scan's first two spans a view of need bytes runs
// across the end of, rel bytes into the scan: 1, 2, or 0 for neither.
func AcrossSpan(rel int64, need int) int {
	for i, end := range spanEnds {
		if rel < end && rel+int64(need) > end {
			return i + 1
		}
	}
	return 0
}

// ScanFrom is where one case starts its scan: at 0, on one of marks, with one
// of them across the end of the scan's first span or its second, need being
// the view a candidate takes, or at a ScanOffset. A view of one byte runs
// across nothing, and starts on the mark.
func ScanFrom(rng *rand.Rand, marks []int64, n int64, need int) int64 {
	from := ScanOffset(rng, marks, n)
	switch aim := rng.IntN(8); {
	case aim < 2:
		from = 0
	case aim < 5 && len(marks) > 0:
		back := int64(0)
		if need > 1 {
			across := 1 + rng.Int64N(int64(need)-1) // how much of the view the span holds
			back = []int64{0, spanEnds[0] - across, spanEnds[1] - across}[rng.IntN(3)]
		}
		from = max(marks[rng.IntN(len(marks))]-back, 0)
	}
	return from
}

// ScanLimit is where one case ends its scan: bound past from, at from or a
// few bytes short of it, at a ScanOffset, or anywhere up to twice the bytes
// past from.
func ScanLimit(rng *rand.Rand, marks []int64, n, from, bound int64) int64 {
	switch rng.IntN(8) {
	case 0, 1:
		return from + bound
	case 2:
		return from - rng.Int64N(3)
	case 3:
		return ScanOffset(rng, marks, n)
	}
	return from + rng.Int64N(2*n+1)
}

// ScanShape is a shape a scan can take, and how many cases took it.
type ScanShape struct {
	Name string
	N    int
}

// ShapesReached fails t for each shape fewer than twenty cases took: a
// comparison that no longer meets a shape says nothing about it.
func ShapesReached(t testing.TB, shapes ...ScanShape) {
	t.Helper()
	for _, s := range shapes {
		if s.N < 20 {
			t.Errorf("%d cases were %s, want at least 20: the generator no longer reaches it", s.N, s.Name)
		}
	}
}

// ErrSourceDown is what a FailingSource read returns.
var ErrSourceDown = errors.New("source is down")

// FailingSource is Src with a part that cannot be read: a read that touches
// [From, To) fails, To at zero being the end of the source, and with After
// above zero so does every read behind that many (From at the source's size
// fails nothing by range, so only the count applies). Failed counts the reads
// that failed, and FailedAt is the first one's range.
type FailingSource struct {
	Src      container.Source
	From, To int64
	After    int
	Failed   int
	FailedAt [2]int64

	reads int
}

func (s *FailingSource) ReadAt(p []byte, off int64) (int, error) {
	to := s.To
	if to == 0 {
		to = s.Src.Size()
	}
	s.reads++
	// A read touches only the bytes the source has.
	end := min(off+int64(len(p)), s.Src.Size())
	if end > s.From && off < to || s.After > 0 && s.reads > s.After {
		if s.Failed++; s.Failed == 1 {
			s.FailedAt = [2]int64{off, off + int64(len(p))}
		}
		return 0, ErrSourceDown
	}
	return s.Src.ReadAt(p, off)
}

func (s *FailingSource) Size() int64 { return s.Src.Size() }
