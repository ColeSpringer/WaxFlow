package mpc

import (
	"bytes"
	"math/rand/v2"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/internal/testutil"
)

// scanShapes counts what the old loop met, so the comparison below can say
// it met each shape a scan takes.
type scanShapes struct {
	found, notFound int
	atFrom          int // the magic at from
	sv8, sv7        int // "MPCK" found, and "MP+" with a version found
	limitCut        int // a candidate at or past limit ended the scan
	cutMagic        int // a candidate the data's end left fewer than four bytes of
	decoy           int // an "MP" that is no magic
	underLimit      int // a magic in the last three bytes under limit the old loop reached
	acrossSpan      int // a magic found across a span's end
	empty           int // a range that holds nothing: limit or the data's end at from or before it
	emptyOnMagic    int // and one of those with a magic at from

	// A magic in the last three bytes under limit the old loop stopped short
	// of, by what its last step was.
	shortAtFrom, shortBehindCandidate, shortAtWindowEnd int
}

// The old loop's last step before its condition ended it.
const (
	noStep        = iota // from was within three bytes of limit
	pastCandidate        // a candidate that failed
	pastWindow           // a window with no candidate in it
)

// oldMagicIn is the scan for the magic as it stood before srcwin.Scan, over a
// range it is given: a window read at every candidate, and a loop that stops
// four bytes short of limit. It is the oracle the scan is held to.
func oldMagicIn(d *Demuxer, from, limit int64, seen *scanShapes) (at int64, ok bool, step int) {
	limit = min(limit, d.w.DataEnd())
	for off := from; off+4 <= limit; {
		b := d.w.BytesAt(off, srcwin.Chunk)
		if len(b) < 4 {
			break
		}
		i := bytes.Index(b, []byte("MP"))
		if i < 0 {
			off += int64(len(b)) - 3
			step = pastWindow
			continue
		}
		cand := off + int64(i)
		if h := d.w.BytesAt(cand, 4); len(h) == 4 && cand < limit &&
			(string(h) == "MPCK" || (string(h[:3]) == "MP+" && h[3]&0x0F >= 4 && h[3]&0x0F <= 7)) {
			return cand, true, step
		}
		if cand >= limit {
			seen.limitCut++
		} else {
			seen.decoy++
		}
		off = cand + 1
		step = pastCandidate
	}
	return 0, false, step
}

// firstMagic is the first magic that starts in [from, limit) of data, a byte
// at a time.
func firstMagic(data []byte, from, limit int64) (int64, bool) {
	for p := from; p < min(limit, int64(len(data))) && p+4 <= int64(len(data)); p++ {
		switch b := data[p : p+4]; {
		case string(b) == "MPCK", string(b[:3]) == "MP+" && b[3]&0x0F >= 4 && b[3]&0x0F <= 7:
			return p, true
		}
	}
	return 0, false
}

// scanRun is a random run to scan: junk in which candidates turn up by
// chance, then planted magics and things that are nearly one. marks are where
// the planted magics start, behind the ones a candidate ends at or just before.
func scanRun(rng *rand.Rand) (data []byte, marks, behind []int64) {
	data = testutil.RandomJunk(rng, []byte{'P', '+', 'C', 'K', 0x07, 0x17, 0x03, 0x08, 0x00, 'X'}, []byte{'M'}, []int{0, 1, 2, 8})
	n := len(data)
	// SV8, SV7 plain and with its PNS bit, and a version the decoder refuses
	// by name.
	magics := []string{"MPCK", "MP+\x07", "MP+\x17", "MP+\x04"}
	// Nearly one, and candidates that end where a magic starts.
	fronts := []string{"", "", "MP", "MP\x00", "MP+\x03", "MP+\x08", "MPCX"}
	for range rng.IntN(8) {
		front := fronts[rng.IntN(len(fronts))]
		at := rng.IntN(n + 1)
		copy(data[at:], front)
		if rng.IntN(4) > 0 {
			at += len(front)
			copy(data[min(at, n):], magics[rng.IntN(len(magics))])
			marks = append(marks, int64(min(at, n)))
			if front == "MP" || front == "MP\x00" {
				behind = append(behind, int64(min(at, n)))
			}
		}
	}
	return data, marks, behind
}

// TestScanPortMatchesTheOldLoop holds magicIn to a byte-at-a-time search and
// to the loop it replaced: the first magic in the range, in reads the range
// bounds. The old loop may miss one in the last three bytes under limit.
func TestScanPortMatchesTheOldLoop(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 5))
	var seen scanShapes
	for i := range 4000 {
		data, marks, behind := scanRun(rng)
		n := int64(len(data))
		end := n
		switch aim := rng.IntN(8); {
		case aim < 2:
			end = testutil.ScanOffset(rng, marks, n) // a file cut short, with bytes behind the cut
		case aim == 2 && len(marks) > 0:
			end = min(marks[rng.IntN(len(marks))]+2+rng.Int64N(2), n) // inside a magic
		}
		from := testutil.ScanFrom(rng, marks, n, 4)
		limit := testutil.ScanLimit(rng, marks, n, from, maxJunk)
		switch aim := rng.IntN(16); {
		case aim < 3 && len(marks) > 0:
			// A magic in the last three bytes under the limit, or at it.
			limit = marks[rng.IntN(len(marks))] + rng.Int64N(4)
		case aim == 3 && len(marks) > 0:
			// And the scan starting on it.
			from = marks[rng.IntN(len(marks))]
			limit = from + 1 + rng.Int64N(3)
		case aim < 6 && len(behind) > 0:
			// And a candidate ending at it or a byte before, the scan
			// starting in front of both.
			limit = behind[rng.IntN(len(behind))] + 1
			from = max(limit-8-rng.Int64N(2000), 0)
		case aim == 6:
			// And a window from the scan's start ending on its first byte,
			// with no candidate in the window.
			data = bytes.Repeat([]byte{0x11}, srcwin.Chunk+rng.IntN(4<<10)+8)
			n, end = int64(len(data)), int64(len(data))
			limit = srcwin.Chunk + rng.Int64N(n-srcwin.Chunk-4)
			from = limit - srcwin.Chunk
			copy(data[limit-1:], "MPCK")
		}
		demuxer := func() (*Demuxer, *testutil.CountingSource) {
			src := &testutil.CountingSource{Src: container.BytesSource(data)}
			return &Demuxer{w: srcwin.New(src, end, "musepack: reading stream data")}, src
		}

		old, _ := demuxer()
		now, nowSrc := demuxer()
		oldOff, oldOK, step := oldMagicIn(old, from, limit, &seen)
		off, ok := now.magicIn(from, limit)
		wantOff, want := firstMagic(data[:end], from, limit)
		if off != wantOff || ok != want {
			t.Fatalf("case %d: %d bytes ending at %d, scanning [%d, %d): magicIn = %d, %v; the first magic is %d, %v",
				i, n, end, from, limit, off, ok, wantOff, want)
		}
		if old.w.Err() != nil || now.w.Err() != nil {
			t.Fatalf("case %d: Err = %v, the old loop's %v", i, now.w.Err(), old.w.Err())
		}
		// The check at from, and the range's spans. An empty range reads
		// nothing.
		scanned := max(min(limit, end)-from, 0)
		most := 1 + testutil.SpansOver(scanned+3, srcwin.Chunk)
		if scanned == 0 {
			most = 0
			seen.empty++
			if _, ok := firstMagic(data[:end], from, from+1); ok {
				seen.emptyOnMagic++
			}
		}
		if nowSrc.Reads > most {
			t.Fatalf("case %d: %d bytes ending at %d, scanning [%d, %d): %d reads, want at most %d whatever the range holds",
				i, n, end, from, limit, nowSrc.Reads, most)
		}
		under := want && off+4 > min(limit, end) // in the last three bytes under limit
		switch {
		case oldOK == ok && oldOff == off:
			if under {
				seen.underLimit++
			}
		case oldOK || !under:
			t.Fatalf("case %d: %d bytes ending at %d, scanning [%d, %d): magicIn = %d, %v; the old loop %d, %v",
				i, n, end, from, limit, off, ok, oldOff, oldOK)
		case step == noStep:
			seen.shortAtFrom++
		case step == pastCandidate:
			seen.shortBehindCandidate++
		default:
			seen.shortAtWindowEnd++
		}
		if !want {
			seen.notFound++
			if tail := max(from, end-3); tail < min(limit, end) && bytes.Contains(data[tail:end], []byte("MP")) {
				seen.cutMagic++
			}
			continue
		}
		seen.found++
		if off == from {
			seen.atFrom++
		}
		if data[off+2] == 'C' {
			seen.sv8++
		} else {
			seen.sv7++
		}
		if testutil.AcrossSpan(off-from, 4) > 0 {
			seen.acrossSpan++
		}
	}
	t.Logf("the old loop met %+v", seen)
	testutil.ShapesReached(t,
		testutil.ScanShape{Name: "found", N: seen.found}, testutil.ScanShape{Name: "not found", N: seen.notFound},
		testutil.ScanShape{Name: "found at from", N: seen.atFrom},
		testutil.ScanShape{Name: "an empty range", N: seen.empty},
		testutil.ScanShape{Name: "an empty range that starts on a magic", N: seen.emptyOnMagic},
		testutil.ScanShape{Name: "an SV8 magic", N: seen.sv8}, testutil.ScanShape{Name: "an SV7 magic", N: seen.sv7},
		testutil.ScanShape{Name: "cut by the limit", N: seen.limitCut},
		testutil.ScanShape{Name: "a candidate cut by the end of data", N: seen.cutMagic},
		testutil.ScanShape{Name: "a candidate that is no magic", N: seen.decoy},
		testutil.ScanShape{Name: "a magic under the limit the old loop reached", N: seen.underLimit},
		testutil.ScanShape{Name: "a magic under the limit at from, where the old loop never started", N: seen.shortAtFrom},
		testutil.ScanShape{Name: "a magic under the limit behind a candidate, where the old loop stopped", N: seen.shortBehindCandidate},
		testutil.ScanShape{Name: "a magic under the limit at a window's end, where the old loop stopped", N: seen.shortAtWindowEnd},
		testutil.ScanShape{Name: "found on a magic across a span's end", N: seen.acrossSpan})
}
