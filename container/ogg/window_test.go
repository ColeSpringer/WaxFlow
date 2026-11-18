package ogg

import (
	"encoding/binary"
	"math/rand/v2"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// TestCRCShiftIsZeroBytes: shifting a register by n is feeding it n zero
// bytes.
func TestCRCShiftIsZeroBytes(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 11))
	zeros := make([]byte, windowCap)
	ns := []int64{0, 1, 2, 3, 4, 26, 27, 255, 256, maxPageSize - 26, maxPageSize, windowCap}
	for range 200 {
		ns = append(ns, rng.Int64N(windowCap+1))
	}
	for _, n := range ns {
		c := rng.Uint32()
		if got, want := crcShift(c, n), crc32(c, zeros[:n]); got != want {
			t.Errorf("crcShift(%#08x, %d) = %#08x, want %#08x", c, n, got, want)
		}
	}
}

// directSum is the checksum readPage takes of pg: the bytes as they lie with
// the checksum field zeroed.
func directSum(pg []byte) uint32 {
	crc := crc32(0, pg[:22])
	crc = crc32(crc, []byte{0, 0, 0, 0})
	return crc32(crc, pg[26:])
}

// TestRangeSumsMatchTheDirectSum holds the window's checksum algebra to the
// direct sum: over random bytes, ranges and stored checksums, a page's sum by
// the running sums is the crc32 chain's, so it verifies by one exactly when
// by the other, while the window reads on, lets go and starts over.
func TestRangeSumsMatchTheDirectSum(t *testing.T) {
	rng := rand.New(rand.NewPCG(13, 13))
	var verified, failed, empty, largest, atFirst, atLast, afterDrop, jumps int
	for i := range 80 {
		data := make([]byte, 3*windowCap)
		for j := 0; j+8 <= len(data); j += 8 {
			binary.LittleEndian.PutUint64(data[j:], rng.Uint64())
		}
		// Pages to check, as a resync meets them: ascending a few bytes to a
		// few KiB apart, now and then elsewhere. Some have their checksum
		// planted so they verify.
		var pages [][2]int64
		off := rng.Int64N(windowCap)
		for range 100 {
			size := 27 + rng.Int64N(4096)
			switch rng.IntN(20) {
			case 0, 1:
				size = 27
			case 2:
				size = maxPageSize
			case 3, 4:
				size = 27 + rng.Int64N(maxPageSize-26)
			}
			if off += 1 + rng.Int64N(6000); rng.IntN(30) == 0 {
				off = rng.Int64N(int64(len(data)))
			}
			if off+size > int64(len(data)) {
				off = rng.Int64N(int64(len(data)) - size + 1)
			}
			if rng.IntN(2) == 0 {
				binary.LittleEndian.PutUint32(data[off+22:], directSum(data[off:off+size]))
			}
			pages = append(pages, [2]int64{off, size})
		}
		w := newWindow(container.BytesSource(data))
		check := func(off, size int64, what string) {
			want := directSum(data[off : off+size])
			got := w.pageSum(off, size)
			if got != want {
				t.Fatalf("case %d, %s: the page of %d bytes at %d sums to %#08x, the direct sum %#08x", i, what, size, off, got, want)
			}
			if want == binary.LittleEndian.Uint32(data[off+22:]) {
				verified++
			} else {
				failed++
			}
			if size == 27 {
				empty++
			}
			if size == maxPageSize {
				largest++
			}
		}
		for _, pg := range pages {
			off, size := pg[0], pg[1]
			before, end, summed := w.off, w.end(), len(w.sums) > 0
			if _, err := w.need(off, off+size, off, "need"); err != nil {
				t.Fatal(err)
			}
			switch {
			case off < before || off > end:
				jumps++
			case summed && len(w.sums) > 0 && w.off > before:
				afterDrop++ // sums kept over the bytes let go
			}
			check(off, size, "a page as the scan meets it")
			// The window's first byte and its last.
			if held := int64(len(w.buf)); held >= 27 {
				n := 27 + rng.Int64N(min(held, 4096)-26)
				check(w.off, n, "a range from the window's first byte")
				check(w.end()-n, n, "a range to the window's last byte")
				atFirst++
				atLast++
			}
		}
	}
	t.Logf("%d verified, %d did not, %d empty bodies, %d of the largest size, %d from the first byte, %d to the last, %d after letting go, %d after starting over",
		verified, failed, empty, largest, atFirst, atLast, afterDrop, jumps)
	testutil.ShapesReached(t,
		testutil.ScanShape{Name: "a page that verifies", N: verified},
		testutil.ScanShape{Name: "a page that does not", N: failed},
		testutil.ScanShape{Name: "a page with an empty body", N: empty},
		testutil.ScanShape{Name: "a page of the largest size", N: largest},
		testutil.ScanShape{Name: "a range from the window's first byte", N: atFirst},
		testutil.ScanShape{Name: "a range to the window's last byte", N: atLast},
		testutil.ScanShape{Name: "a page after the window let bytes go", N: afterDrop},
		testutil.ScanShape{Name: "a page after the window started over", N: jumps})
}

// TestAFailedWindowReadIsTheNeedsError: read-ahead that fails reads what the
// call needs alone, so a hole behind the bytes asked for fails nothing; a
// hole in them is the call's error in its own words, and the next call reads
// again.
func TestAFailedWindowReadIsTheNeedsError(t *testing.T) {
	data := make([]byte, 4*scanStep)
	const hole = 3000
	src := &testutil.FailingSource{Src: container.BytesSource(data), From: hole, To: hole + 1}
	w := newWindow(src)
	if _, err := w.need(0, 1000, 0, "ogg: reading page header"); err != nil {
		t.Fatalf("bytes in front of the hole: %v", err)
	}
	if _, err := w.need(1000, 2500, 0, "ogg: reading page body"); err != nil || src.Failed != 1 {
		t.Fatalf("bytes in front of the hole, read ahead over it: %v after %d failed reads, want the read-ahead's one", err, src.Failed)
	}
	if w.end() != 2500 {
		t.Fatalf("the window ends at %d after a read-ahead failed, want the 2500 asked for", w.end())
	}
	_, err := w.need(2400, hole+1, 2400, "ogg: reading page body")
	if waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable || err.Error() != "ogg: reading page body: source is down" {
		t.Fatalf("bytes over the hole: %v, want the read's error in the call's words", err)
	}
	src.From = int64(len(data)) // the source comes back
	if got, err := w.need(2400, hole+1, 2400, "ogg: reading page body"); err != nil || len(got) != hole+1-2400 {
		t.Fatalf("the same bytes once the source reads: %d bytes, %v", len(got), err)
	}
}

// TestTheWindowReadsForward: needs that move forward read each byte once, in
// spans doubling from a KiB to a step, and the window holds at most a step
// and a page.
func TestTheWindowReadsForward(t *testing.T) {
	data := make([]byte, 40*scanStep)
	src := &testutil.CountingSource{Src: container.BytesSource(data), Log: true}
	w := newWindow(src)
	rng := rand.New(rand.NewPCG(17, 17))
	for off := int64(0); off < int64(len(data)); {
		size := min(1+rng.Int64N(maxPageSize), int64(len(data))-off)
		if _, err := w.need(off, off+size, off, "need"); err != nil {
			t.Fatal(err)
		}
		if held := int64(len(w.buf)); held > windowCap {
			t.Fatalf("the window holds %d bytes, want at most %d", held, windowCap)
		}
		off += size
	}
	if src.Bytes != int64(len(data)) {
		t.Errorf("%d bytes read for %d needed, want each once", src.Bytes, len(data))
	}
	for i, s := range src.Spans[:min(7, len(src.Spans))] {
		if want := int64(firstSpan << i); i < 6 && s[1]-s[0] < want {
			t.Errorf("read %d spans %v, want at least %d", i, s, want)
		}
	}
	if most := len(data)/scanStep + 7; src.Reads > most {
		t.Errorf("%d reads for %d bytes, want at most %d", src.Reads, len(data), most)
	}
}
