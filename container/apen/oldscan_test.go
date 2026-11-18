package apen

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/codec/ape"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/internal/testutil"
)

// scanShapes counts what the old loop met, so the comparison below can say
// it met each shape a scan takes.
type scanShapes struct {
	found, noMagic int
	atFrom         int // the header at from parsed
	pastDecoys     int // a header found behind a magic that did not parse
	kept           int // no header parsed, and the first parse error is the answer
	capped         int // the candidate bound ended the scan
	lastAtLimit    int // the bound's last candidate was the range's last byte, so the first error is the answer
	word           int // the magic's three letters with another byte behind them
	limitCut       int // a magic at or past limit
	cutHeader      int // a header the data's end cut short
	acrossSpan     int // a header found with its magic across a span's end
	empty          int // a range that holds nothing: limit or the data's end at from or before it
	emptyOnMagic   int // and one of those with a magic at from
}

// oldHeaderIn is the header search as it stood before srcwin.Scan, over a
// range it is given: a window read at every magic. It is the oracle the scan
// is held to, and it counts the shapes it meets.
func oldHeaderIn(d *Demuxer, from, limit int64, seen *scanShapes) (int64, ape.Header, error) {
	limit = min(limit, d.w.DataEnd())
	var firstErr error
	tries := 0
	for off := from; off < limit; tries++ {
		if tries >= maxHeaderCandidates {
			seen.capped++
			return 0, ape.Header{}, malformed("no Monkey's Audio header in the first %d candidates", tries)
		}
		cand, ok := oldNextMagic(d, off, limit, seen)
		if !ok {
			break
		}
		b := d.w.Peek(cand, ape.MaxHeaderLen)
		if h, err := ape.ParseHeader(b); err == nil {
			if tries > 0 {
				seen.pastDecoys++
			}
			return cand, h, nil
		} else if firstErr == nil {
			firstErr = err
		}
		if len(b) < len(planted) && bytes.HasPrefix(planted, b) {
			seen.cutHeader++
		}
		off = cand + 1
	}
	if d.w.Err() != nil {
		return 0, ape.Header{}, d.w.Err()
	}
	if firstErr != nil {
		seen.kept++
		if tries >= maxHeaderCandidates {
			seen.lastAtLimit++
		}
		return 0, ape.Header{}, firstErr
	}
	seen.noMagic++
	return 0, ape.Header{}, malformed("no Monkey's Audio header in the first %d bytes", limit-from)
}

// oldNextMagic is nextMagic as it stood beside that loop.
func oldNextMagic(d *Demuxer, from, limit int64, seen *scanShapes) (int64, bool) {
	if b := d.w.Peek(from, ape.MatchNeed); len(b) == ape.MatchNeed && ape.Match(b) {
		return from, true
	}
	for off := from; off < limit; {
		b := d.w.BytesAt(off, srcwin.Chunk)
		if len(b) < ape.MatchNeed {
			return 0, false
		}
		for rel := 0; rel+ape.MatchNeed <= len(b); {
			i := bytes.Index(b[rel:], ape.Magic)
			if i < 0 {
				break
			}
			rel += i
			cand := off + int64(rel)
			if cand < limit && ape.Match(b[rel:]) {
				return cand, true
			}
			switch {
			case cand >= limit && ape.Match(b[rel:]):
				seen.limitCut++
			case len(b[rel:]) >= ape.MatchNeed:
				seen.word++
			}
			rel++
		}
		off += int64(len(b)) - int64(ape.MatchNeed) + 1
	}
	return 0, false
}

// planted is a header that parses: the descriptor and header a fixture
// starts with.
var planted = func() []byte {
	raw, err := os.ReadFile(filepath.Join("testdata", "seek.ape"))
	if err != nil {
		panic(err)
	}
	h, err := ape.ParseHeader(raw[:ape.MaxHeaderLen])
	if err != nil {
		panic(err)
	}
	return raw[:h.SeekTableOffset]
}()

// decoy is a magic with nothing behind it that parses.
var decoy = []byte{'M', 'A', 'C', ' ', 0, 0, 0, 0}

// scanRun is a random run to scan: junk that spells words and magics by
// chance, planted headers (at marks), magics that are no header, and now and
// then about as many of those as a scan tries, in a row from field (else -1).
func scanRun(rng *rand.Rand) (data []byte, marks []int64, field int64) {
	data = testutil.RandomJunk(rng, []byte{' ', 'F', 0x00, 0x11, 0x8E, 0x0F, 'X'}, ape.Magic, []int{0, 3, 6, 9})
	n := len(data)
	field = -1
	if rng.IntN(5) == 0 {
		// Chance magics would add to the count, so the field stands alone.
		data = bytes.Repeat([]byte{0x11}, max(n, 12<<10))
		n = len(data)
		field = int64(rng.IntN(n - 9<<10))
		for i := range maxHeaderCandidates - 1 + rng.IntN(4) {
			copy(data[field+int64(i*len(decoy)):], decoy)
		}
	}
	for range rng.IntN(6) {
		at := rng.IntN(n + 1)
		switch rng.IntN(4) {
		case 0:
			copy(data[at:], decoy)
		case 1:
			copy(data[at:], "MACF") // the float magic, and whatever follows
		default:
			copy(data[at:], planted)
			marks = append(marks, int64(at))
		}
	}
	return data, marks, field
}

// TestScanPortMatchesTheOldLoop holds headerIn to the loop it replaced, over
// random runs and ranges: the same offset, header and error, in reads the
// range bounds, with every shape a scan takes reached.
func TestScanPortMatchesTheOldLoop(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	var seen scanShapes
	for i := range 2500 {
		data, marks, field := scanRun(rng)
		n := int64(len(data))
		end := n
		switch aim := rng.IntN(8); {
		case aim < 2:
			end = testutil.ScanOffset(rng, marks, n) // a file cut short, with bytes behind the cut
		case aim == 2 && len(marks) > 0:
			end = min(marks[rng.IntN(len(marks))]+4+rng.Int64N(int64(len(planted))), n) // inside a header
		}
		from := testutil.ScanFrom(rng, marks, n, ape.MatchNeed)
		limit := testutil.ScanLimit(rng, marks, n, from, maxJunk)
		if field >= 0 && rng.IntN(4) > 0 {
			// The field from its start, and a limit at the magic the bound
			// counts last, a byte past it, or two.
			from = max(field-rng.Int64N(64), 0)
			limit = field + int64((maxHeaderCandidates-1)*len(decoy)) + rng.Int64N(3)
		}
		demuxer := func() (*Demuxer, *testutil.CountingSource) {
			src := &testutil.CountingSource{Src: container.BytesSource(data)}
			return &Demuxer{src: src, w: srcwin.New(src, end, "ape: reading the file header")}, src
		}

		old, _ := demuxer()
		now, nowSrc := demuxer()
		wantOff, wantHdr, wantErr := oldHeaderIn(old, from, limit, &seen)
		off, hdr, err := now.headerIn(from, limit)
		if off != wantOff || hdr != wantHdr || fmt.Sprint(err) != fmt.Sprint(wantErr) {
			t.Fatalf("case %d: %d bytes ending at %d, scanning [%d, %d): headerIn = %d, %+v, %v; the old loop %d, %+v, %v",
				i, n, end, from, limit, off, hdr, err, wantOff, wantHdr, wantErr)
		}
		if old.w.Err() != nil || now.w.Err() != nil {
			t.Fatalf("case %d: Err = %v, the old loop's %v", i, now.w.Err(), old.w.Err())
		}
		// The two reads at from, the range's spans, and a read of a header
		// for each doubling of what the scan holds. An empty range reads
		// nothing.
		scanned := max(min(limit, end)-from, 0)
		most := 3 + testutil.SpansOver(scanned+3, srcwin.Chunk) + testutil.SpansOver(scanned+ape.MaxHeaderLen, srcwin.Chunk)
		if scanned == 0 {
			most = 0
			seen.empty++
			if ape.Match(data[from:max(from, end)]) {
				seen.emptyOnMagic++
			}
		}
		if nowSrc.Reads > most {
			t.Fatalf("case %d: %d bytes ending at %d, scanning [%d, %d): %d reads, want at most %d whatever the range holds",
				i, n, end, from, limit, nowSrc.Reads, most)
		}
		if wantErr != nil {
			continue
		}
		seen.found++
		if off == from {
			seen.atFrom++
		}
		if testutil.AcrossSpan(off-from, ape.MatchNeed) > 0 {
			seen.acrossSpan++
		}
	}
	t.Logf("the old loop met %+v", seen)
	testutil.ShapesReached(t,
		testutil.ScanShape{Name: "found", N: seen.found}, testutil.ScanShape{Name: "without a magic", N: seen.noMagic},
		testutil.ScanShape{Name: "found at from", N: seen.atFrom},
		testutil.ScanShape{Name: "an empty range", N: seen.empty},
		testutil.ScanShape{Name: "an empty range that starts on a magic", N: seen.emptyOnMagic},
		testutil.ScanShape{Name: "found behind a magic that did not parse", N: seen.pastDecoys},
		testutil.ScanShape{Name: "answered with the first parse error", N: seen.kept},
		testutil.ScanShape{Name: "ended by the candidate bound", N: seen.capped},
		testutil.ScanShape{Name: "answered with the first error after the bound's last candidate", N: seen.lastAtLimit},
		testutil.ScanShape{Name: "the magic's letters and another byte", N: seen.word},
		testutil.ScanShape{Name: "a magic at or past the limit", N: seen.limitCut},
		testutil.ScanShape{Name: "a header cut by the end of data", N: seen.cutHeader},
		testutil.ScanShape{Name: "found on a magic across a span's end", N: seen.acrossSpan})
}
