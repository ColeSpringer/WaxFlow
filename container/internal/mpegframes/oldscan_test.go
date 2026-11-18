package mpegframes

import (
	"bytes"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/internal/testutil"
)

// scanShapes counts what the old loop met, so the comparison below can say
// it met each shape a scan takes.
type scanShapes struct {
	found, notFound int
	atFrom          int // the header at from confirmed
	limitCut        int // a sync byte at or past limit ended the scan
	straddle        int // a header cut by a span's end, retried from the next
	straddleFound   int // and one of those was the answer
	pastSpan        int // a confirm that read past what the span held
	quiet           int // a scan with no such confirm
	ended           int // the data ended inside a span
}

// oldNextCandidate is nextCandidate as it stood before srcwin.Scan: its own
// span loop, and exact confirming reads. It is the oracle the scan is held
// to, and it counts the shapes it meets.
func oldNextCandidate(w *Walker, from, limit int64, seen *scanShapes) (int64, mp3.Header, bool) {
	limit = min(limit, w.w.DataEnd())
	if from >= limit {
		return 0, mp3.Header{}, false
	}
	if h, ok := oldConfirmed(w, from, w.w.Peek(from, mp3.HeaderLen), seen); ok {
		seen.atFrom++
		return from, h, true
	}
	span := int64(1 << 10)
	retried := int64(-1)
	for off := from; off < limit; {
		// A candidate below limit may carry its header past it.
		n := min(span, limit-off+mp3.HeaderLen-1)
		buf := w.w.Peek(off, int(n))
		if len(buf) < mp3.HeaderLen {
			return 0, mp3.Header{}, false
		}
		next := off + int64(len(buf))
		for i := 0; i < len(buf); {
			j := bytes.IndexByte(buf[i:], 0xFF)
			if j < 0 {
				break
			}
			i += j
			cand := off + int64(i)
			if cand >= limit {
				seen.limitCut++
				return 0, mp3.Header{}, false
			}
			if i+mp3.HeaderLen > len(buf) {
				next = cand // straddles the span: the next span starts on it
				if int64(len(buf)) == n {
					seen.straddle++
					retried = cand
				}
				break
			}
			if h, ok := oldConfirmed(w, cand, buf[i:i+mp3.HeaderLen], seen); ok {
				if cand == retried {
					seen.straddleFound++
				}
				return cand, h, true
			}
			i++
		}
		if int64(len(buf)) < n {
			seen.ended++
			return 0, mp3.Header{}, false // the data ended inside the span
		}
		off = next
		span = min(span*2, srcwin.Chunk)
	}
	return 0, mp3.Header{}, false
}

// oldConfirmed is confirmed as it stood beside that loop.
func oldConfirmed(w *Walker, off int64, hdr []byte, seen *scanShapes) (mp3.Header, bool) {
	h, err := mp3.ParseHeader(hdr)
	if err != nil || h.Size() == 0 || (w.spf != 0 && !w.hdr.Kin(h)) {
		return mp3.Header{}, false
	}
	next := off + int64(h.Size())
	if next >= w.w.DataEnd() {
		return h, true // runs to the end: nothing to confirm against
	}
	held, _ := w.w.Resident()
	frame := w.w.Peek(off, h.Size()+mp3.HeaderLen)
	if now, _ := w.w.Resident(); now > held {
		seen.pastSpan++
	}
	if len(frame) < h.Size()+mp3.HeaderLen {
		return mp3.Header{}, false
	}
	if nh, err := mp3.ParseHeader(frame[h.Size():]); err != nil || !h.Kin(nh) {
		return mp3.Header{}, false
	}
	return h, true
}

// scanHeaders are what the random runs are built from: three frames of one
// stream, and two streams that are kin to neither it nor each other.
var scanHeaders = [][]byte{
	{0xFF, 0xFB, 0x90, 0x00}, // MPEG-1 stereo at 128 kbit/s: 417 bytes
	{0xFF, 0xFB, 0x92, 0x00}, // the same, padded: 418
	{0xFF, 0xFB, 0x10, 0x00}, // the same at 32 kbit/s: 104
	{0xFF, 0xF3, 0x80, 0x00}, // MPEG-2 at 64 kbit/s: 208
	{0xFF, 0xFB, 0x90, 0xC0}, // MPEG-1 mono
}

// scanFamilies groups scanHeaders by kinship: a run of frames draws from one.
var scanFamilies = [][]int{{0, 1, 2}, {3}, {4}}

// scanRun is a random run to scan: junk over sixteen byte values the case
// draws, some of them the sync byte, so headers that parse turn up by chance,
// then planted headers and runs of frames. marks are where those were planted.
func scanRun(t *testing.T, rng *rand.Rand) (data []byte, marks []int64) {
	n := rng.IntN(20 << 10)
	if rng.IntN(16) == 0 {
		n = rng.IntN(300 << 10)
	}
	other := []byte{0xFB, 0x90, 0x00, 0xF3, 0x11}
	syncs := []int{0, 1, 2, 8}[rng.IntN(4)]
	var values [16]byte
	for i := range values {
		values[i] = other[rng.IntN(len(other))]
		if i < syncs {
			values[i] = 0xFF
		}
	}
	data = make([]byte, n)
	for i := 0; i < n; i += 16 {
		r := rng.Uint64()
		for j := i; j < min(i+16, n); j++ {
			data[j] = values[r&15]
			r >>= 4
		}
	}
	for range rng.IntN(8) {
		at := rng.IntN(n + 1)
		marks = append(marks, int64(at))
		if rng.IntN(3) == 0 {
			copy(data[at:], scanHeaders[rng.IntN(len(scanHeaders))])
			continue
		}
		family := scanFamilies[rng.IntN(len(scanFamilies))]
		for range 1 + rng.IntN(3) {
			hdr := scanHeaders[family[rng.IntN(len(family))]]
			h, err := mp3.ParseHeader(hdr)
			if err != nil {
				t.Fatal(err)
			}
			if at > n {
				break
			}
			copy(data[at:], hdr)
			at += h.Size()
			marks = append(marks, int64(min(at, n)))
		}
	}
	return data, marks
}

// TestScanPortMatchesTheOldLoop holds nextCandidate to the loop it replaced,
// over random runs, ranges and reference headers: the same offset, header
// and answer, in no more reads, and in the same reads where no confirm read
// past a span. It fails too if the cases stop reaching a shape a scan takes.
func TestScanPortMatchesTheOldLoop(t *testing.T) {
	rng := rand.New(rand.NewPCG(2, 2))
	var seen scanShapes
	for i := range 4000 {
		data, marks := scanRun(t, rng)
		n := int64(len(data))
		end := n
		if rng.IntN(4) == 0 {
			end = testutil.ScanOffset(rng, marks, n) // a run its container bounds, with bytes behind it
		}
		from := testutil.ScanFrom(rng, marks, n, mp3.HeaderLen)
		var limit int64
		switch rng.IntN(4) {
		case 0:
			limit = from + maxResync
		case 1:
			limit = testutil.ScanOffset(rng, marks, n) // at or before from, some of the time
		default:
			limit = from + rng.Int64N(2*n+1)
		}
		var ref mp3.Header
		if rng.IntN(2) == 0 {
			ref, _ = mp3.ParseHeader(scanHeaders[rng.IntN(len(scanHeaders))])
		}
		walker := func() (*Walker, *testutil.CountingSource) {
			src := &testutil.CountingSource{Src: container.BytesSource(data), Log: true}
			w := New(src, end, Options{})
			if ref.Rate != 0 {
				w.hdr, w.spf = ref, int64(ref.SamplesPerFrame())
			}
			return w, src
		}

		old, oldSrc := walker()
		now, nowSrc := walker()
		confirms := seen.pastSpan
		wantOff, wantHdr, want := oldNextCandidate(old, from, limit, &seen)
		off, hdr, ok := now.nextCandidate(from, limit)
		if off != wantOff || hdr != wantHdr || ok != want {
			t.Fatalf("case %d: %d bytes ending at %d, scanning [%d, %d): nextCandidate = %d, %+v, %v; the old loop %d, %+v, %v",
				i, n, end, from, limit, off, hdr, ok, wantOff, wantHdr, want)
		}
		if old.Err() != nil || now.Err() != nil {
			t.Fatalf("case %d: Err = %v, the old loop's %v", i, now.Err(), old.Err())
		}
		if nowSrc.Reads > oldSrc.Reads {
			t.Fatalf("case %d: %d reads, the old loop %d", i, nowSrc.Reads, oldSrc.Reads)
		}
		if seen.pastSpan == confirms {
			seen.quiet++
			if !slices.Equal(nowSrc.Spans, oldSrc.Spans) {
				t.Fatalf("case %d: with no confirm reading past a span the reads are %v, the old loop's %v", i, nowSrc.Spans, oldSrc.Spans)
			}
		}
		if want {
			seen.found++
		} else {
			seen.notFound++
		}
	}
	for _, shape := range []struct {
		name string
		n    int
	}{
		{"found", seen.found}, {"not found", seen.notFound}, {"found at from", seen.atFrom},
		{"cut by the limit", seen.limitCut}, {"a header straddling a span", seen.straddle},
		{"found on a straddling header", seen.straddleFound}, {"a confirm reading past the span", seen.pastSpan},
		{"a scan with no such confirm", seen.quiet}, {"the data ending inside a span", seen.ended},
	} {
		if shape.n < 20 {
			t.Errorf("%d cases were %s, want at least 20: the generator no longer reaches it", shape.n, shape.name)
		}
	}
}

// TestJunkScanLeavesTheWindowAtItsRange pins what the longest scan leaves
// resident. A scan trims nothing, so that is the junk, and the confirming
// read may reach a frame and a Chunk past it.
func TestJunkScanLeavesTheWindowAtItsRange(t *testing.T) {
	const junk = maxResync - 1 // the furthest a scan takes a frame at
	for _, tc := range []struct {
		name string
		junk []byte
	}{
		{"junk that never parses", bytes.Repeat([]byte{0x11}, junk)},
		{"headers that parse", bytes.Repeat(frameHeader, maxResync/len(frameHeader))[:junk]},
	} {
		// Enough frames behind the junk that a read-ahead is not cut short.
		w, _ := countedWalk(t, append(tc.junk, frames(400)...))
		if w.FirstFrame() != junk {
			t.Fatalf("%s: first frame at %d, want %d", tc.name, w.FirstFrame(), junk)
		}
		if n, _ := w.w.Resident(); n > maxResync+frameLen+srcwin.Chunk {
			t.Errorf("%s: the window holds %d bytes after the scan, want at most the %d scanned, a frame and a Chunk",
				tc.name, n, maxResync)
		}
	}
}
