package adts

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
	atFrom          int // the frame at from confirmed
	limitCut        int // a sync byte at or past limit ended the scan
	ended           int // fewer bytes than a header were left
	toEnd           int // a frame taken because it runs to the end of data
	shortCRC        int // and one of those a CRC header short of its nine bytes
	behind          int // a frame taken on the header behind it
	cutConfirm      int // a candidate whose confirming header the data's end cut
	notKin          int // a header that parses and is not the stream's
	acrossSpan      int // a frame taken with its header across a span's end
	empty           int // a range that holds nothing: limit or the data's end at from or before it
	emptyOnHeader   int // and one of those with a header at from
}

// oldNextFrame is nextFrame as it stood before srcwin.Scan: a window read at
// every candidate. It is the oracle the scan is held to, and it counts the
// shapes it meets.
func oldNextFrame(d *Demuxer, from, limit int64, seen *scanShapes) (int64, header, bool) {
	limit = min(limit, d.w.DataEnd())
	for off := from; off < limit; {
		buf := d.w.BytesAt(off, srcwin.Chunk)
		if len(buf) < headerLen {
			seen.ended++
			return 0, header{}, false
		}
		i := bytes.IndexByte(buf, 0xFF)
		if i < 0 {
			off += int64(len(buf)) - headerLen + 1
			continue
		}
		cand := off + int64(i)
		if cand >= limit {
			seen.limitCut++
			return 0, header{}, false
		}
		if h, ok := oldFrameAt(d, cand, seen); ok {
			if cand == from {
				seen.atFrom++
			}
			return cand, h, true
		}
		off = cand + 1
	}
	return 0, header{}, false
}

// oldFrameAt is frameAt as it stood beside that loop.
func oldFrameAt(d *Demuxer, off int64, seen *scanShapes) (header, bool) {
	hdr := d.w.BytesAt(off, 9)
	h, ok := parseHeader(hdr)
	if !ok {
		return header{}, false
	}
	if d.haveRef && !d.ref.kin(h) {
		seen.notKin++
		return header{}, false
	}
	next := off + int64(h.frameLen)
	if next >= d.w.DataEnd() {
		seen.toEnd++
		if len(hdr) < h.hdrLen {
			seen.shortCRC++
		}
		return h, true // runs to the end: nothing to confirm against
	}
	b := d.w.BytesAt(off, h.frameLen+9)
	if len(b) < h.frameLen+9 {
		seen.cutConfirm++
		return header{}, false
	}
	if nh, ok := parseHeader(b[h.frameLen:]); ok && h.kin(nh) {
		seen.behind++
		return h, true
	}
	return header{}, false
}

// scanFamilies are three streams no two of which are kin: the profile, the
// sampling index and the channel configuration of each.
var scanFamilies = [][3]int{{1, 4, 2}, {1, 3, 1}, {0, 4, 2}}

// scanHeader is a header of one family stating a frame of frameLen bytes,
// with the CRC's two bytes behind it or without.
func scanHeader(family, frameLen int, crc bool) []byte {
	f := scanFamilies[family]
	b1 := byte(0xF1)
	if crc {
		b1 = 0xF0
	}
	return []byte{0xFF, b1, byte(f[0]<<6 | f[1]<<2 | f[2]>>2), byte(f[2]<<6 | frameLen>>11),
		byte(frameLen >> 3), byte(frameLen&7)<<5 | 0x1F, 0xFC}
}

// scanRun is a random run to scan: junk in which headers parse by chance,
// then planted headers and runs of frames. marks are where those were
// planted, crcs the ones with a CRC, nexts where the frame behind each starts.
func scanRun(rng *rand.Rand) (data []byte, marks, crcs, nexts []int64) {
	data = testutil.RandomJunk(rng, []byte{0xF1, 0xF0, 0x50, 0x4C, 0x80, 0x21, 0x1F, 0xFC, 0x00}, []byte{0xFF}, []int{0, 1, 2, 8})
	n := len(data)
	// A length a frame states: short, so the header behind it is in reach,
	// and now and then anything the field holds, below a header's own too.
	frameLen := func() int {
		if rng.IntN(8) == 0 {
			return rng.IntN(1 << 13)
		}
		return 7 + rng.IntN(600)
	}
	for range rng.IntN(8) {
		at := rng.IntN(n + 1)
		family := rng.IntN(len(scanFamilies))
		for range 1 + rng.IntN(3) {
			if at > n {
				break
			}
			size, crc := frameLen(), rng.IntN(4) == 0
			copy(data[at:], scanHeader(family, size, crc))
			marks = append(marks, int64(at))
			if crc {
				crcs = append(crcs, int64(at))
			}
			at += size
			nexts = append(nexts, int64(at))
		}
	}
	return data, marks, crcs, nexts
}

// TestScanPortMatchesTheOldLoop holds nextFrame to the loop it replaced, over
// random runs, ranges and reference headers: the same offset, header and
// answer, in reads the range bounds, with every shape a scan takes reached.
func TestScanPortMatchesTheOldLoop(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 3))
	var seen scanShapes
	for i := range 4000 {
		data, marks, crcs, nexts := scanRun(rng)
		n := int64(len(data))
		end, near := n, int64(-1) // near is the header the end was aimed at
		switch aim := rng.IntN(8); {
		case aim < 2:
			end = testutil.ScanOffset(rng, marks, n) // a stream cut short, with bytes behind the cut
		case aim == 2 && len(crcs) > 0:
			// Inside a CRC header's two last bytes, or just behind them.
			near = crcs[rng.IntN(len(crcs))]
			end = min(near+7+rng.Int64N(3), n)
		case aim == 3 && len(nexts) > 0:
			// Inside the header behind a planted frame.
			k := rng.IntN(len(nexts))
			near, end = marks[k], min(nexts[k]+1+rng.Int64N(8), n)
		}
		from := testutil.ScanFrom(rng, marks, n, headerLen)
		if near >= 0 && rng.IntN(2) == 0 {
			from = max(near-rng.Int64N(64), 0)
		}
		limit := testutil.ScanLimit(rng, marks, n, from, maxResync)
		ref, haveRef := header{}, rng.IntN(2) == 0
		if haveRef {
			ref, _ = parseHeader(scanHeader(rng.IntN(len(scanFamilies)), 100, false))
		}
		demuxer := func() (*Demuxer, *testutil.CountingSource) {
			src := &testutil.CountingSource{Src: container.BytesSource(data)}
			return &Demuxer{src: src, w: srcwin.New(src, end, "adts: reading frame data"), ref: ref, haveRef: haveRef}, src
		}

		old, _ := demuxer()
		now, nowSrc := demuxer()
		wantOff, wantHdr, want := oldNextFrame(old, from, limit, &seen)
		off, hdr, ok := now.nextFrame(from, limit)
		if off != wantOff || hdr != wantHdr || ok != want {
			t.Fatalf("case %d: %d bytes ending at %d, scanning [%d, %d): nextFrame = %d, %+v, %v; the old loop %d, %+v, %v",
				i, n, end, from, limit, off, hdr, ok, wantOff, wantHdr, want)
		}
		if old.w.Err() != nil || now.w.Err() != nil {
			t.Fatalf("case %d: Err = %v, the old loop's %v", i, now.w.Err(), old.w.Err())
		}
		// The check at from and its confirm, the range's spans, and a
		// confirming read for each doubling of what the scan holds. An
		// empty range reads nothing.
		scanned := max(min(limit, end)-from, 0)
		most := 3 + testutil.SpansOver(scanned, srcwin.Chunk) + testutil.SpansOver(scanned+8191+9, srcwin.Chunk)
		if scanned == 0 {
			most = 0
			seen.empty++
			if _, ok := parseHeader(data[from:max(from, end)]); ok {
				seen.emptyOnHeader++
			}
		}
		if nowSrc.Reads > most {
			t.Fatalf("case %d: %d bytes ending at %d, scanning [%d, %d): %d reads, want at most %d whatever the range holds",
				i, n, end, from, limit, nowSrc.Reads, most)
		}
		if !want {
			seen.notFound++
			continue
		}
		seen.found++
		if testutil.AcrossSpan(off-from, headerLen) > 0 {
			seen.acrossSpan++
		}
	}
	t.Logf("the old loop met %+v", seen)
	testutil.ShapesReached(t,
		testutil.ScanShape{Name: "found", N: seen.found}, testutil.ScanShape{Name: "not found", N: seen.notFound},
		testutil.ScanShape{Name: "found at from", N: seen.atFrom},
		testutil.ScanShape{Name: "an empty range", N: seen.empty},
		testutil.ScanShape{Name: "an empty range that starts on a header", N: seen.emptyOnHeader},
		testutil.ScanShape{Name: "cut by the limit", N: seen.limitCut},
		testutil.ScanShape{Name: "out of bytes for a header", N: seen.ended},
		testutil.ScanShape{Name: "a frame running to the end of data", N: seen.toEnd},
		testutil.ScanShape{Name: "a CRC header short of its nine bytes", N: seen.shortCRC},
		testutil.ScanShape{Name: "a frame confirmed by the header behind it", N: seen.behind},
		testutil.ScanShape{Name: "a confirming header the data's end cut", N: seen.cutConfirm},
		testutil.ScanShape{Name: "a header of another stream", N: seen.notKin},
		testutil.ScanShape{Name: "found on a header across a span's end", N: seen.acrossSpan})
}
