package flacn

import (
	"bytes"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/internal/testutil"
)

// scanShapes counts what the old loop met, so the comparison below can say
// it met each shape a scan takes.
type scanShapes struct {
	found, notFound int
	atFrom          int // the header at from
	limitCut        int // a sync byte at or past limit ended the scan
	lastByte        int // one byte was left
	longest         int // a header of all sixteen bytes found
	shortView       int // a header found in a view the data's end cut short
	cutHeader       int // a header the data's end cut, rejected
	noSync          int // 0xFF with no sync code behind it
	unparsed        int // a sync code whose header does not parse
	inconsistent    int // a header that parses and disagrees with STREAMINFO
	acrossSpan      int // a header found across a span's end
	empty           int // a range that holds nothing: limit or the data's end at from or before it
}

// oldNextCandidate is nextCandidate as it stood before srcwin.Scan: a window
// read at every sync byte. It is the oracle the scan is held to, and it
// counts the shapes it meets.
func oldNextCandidate(d *Demuxer, from, limit int64, seen *scanShapes) (int64, flac.FrameInfo, bool) {
	limit = min(limit, d.w.DataEnd())
	for off := from; off < limit; {
		buf := d.w.BytesAt(off, srcwin.Chunk)
		if len(buf) < 2 {
			seen.lastByte++
			return 0, flac.FrameInfo{}, false
		}
		i := bytes.IndexByte(buf, 0xFF)
		if i < 0 {
			off += int64(len(buf))
			continue
		}
		cand := off + int64(i)
		if cand >= limit {
			seen.limitCut++
			return 0, flac.FrameInfo{}, false
		}
		hdr := d.w.BytesAt(cand, flac.MaxFrameHeaderLen)
		if flac.SyncOK(hdr) {
			switch fi, err := flac.ParseFrameHeader(hdr); {
			case err != nil:
				seen.unparsed++
				if len(hdr) < flac.MaxFrameHeaderLen {
					seen.cutHeader++
				}
			case !d.consistent(fi):
				seen.inconsistent++
			default:
				if len(hdr) < flac.MaxFrameHeaderLen {
					seen.shortView++
				}
				return cand, fi, true
			}
		} else {
			seen.noSync++
		}
		off = cand + 1
	}
	return 0, flac.FrameInfo{}, false
}

// scanInfo is the stream every random run is scanned as.
var scanInfo = flac.StreamInfo{Rate: 44100, Bits: 16, Channels: 2}

// scanHeader is a frame header with its CRC-8: the block-size and rate codes,
// the channel assignment, the sample-size code and the coded number. A block
// size or a rate the codes put behind the number is written there.
func scanHeader(variable bool, bs, rate, assign, bits int, coded uint64) []byte {
	h := []byte{0xFF, 0xF8, byte(bs<<4 | rate), byte(assign<<4 | bits<<1)}
	if variable {
		h[1] = 0xF9
	}
	switch {
	case coded < 0x80:
		h = append(h, byte(coded))
	default:
		// The longest form: 0xFE and six continuation bytes, 36 bits.
		h = append(h, 0xFE)
		for shift := 30; shift >= 0; shift -= 6 {
			h = append(h, 0x80|byte(coded>>shift)&0x3F)
		}
	}
	switch bs {
	case 6:
		h = append(h, 0xFF) // 256 samples
	case 7:
		h = append(h, 0x0F, 0xFF) // 4096
	}
	switch rate {
	case 13:
		h = append(h, 0xAC, 0x44) // 44100 Hz
	case 14:
		h = append(h, 0x11, 0x3A) // 4410 tens of Hz
	}
	crc := byte(0)
	for _, b := range h {
		crc ^= b
		for range 8 {
			if crc&0x80 != 0 {
				crc = crc<<1 ^ 0x07
			} else {
				crc <<= 1
			}
		}
	}
	return append(h, crc)
}

// scanRun is a random run to scan: junk that spells sync codes by chance,
// then planted headers: the stream's own (at marks, lens long), another
// stream's, and ones with a byte flipped.
func scanRun(rng *rand.Rand) (data []byte, marks []int64, lens []int) {
	data = testutil.RandomJunk(rng, []byte{0xF8, 0xF9, 0xC9, 0x18, 0x00, 0x7F, 0x42, 0xC0, 0x12}, []byte{0xFF}, []int{0, 1, 2, 8})
	for range rng.IntN(8) {
		h, taken := scanPlant(rng)
		at := rng.IntN(len(data) + 1)
		copy(data[at:], h)
		if taken {
			marks = append(marks, int64(at))
			lens = append(lens, len(h))
		}
	}
	return data, marks, lens
}

// scanPlant is one header to plant, and whether the stream takes it.
func scanPlant(rng *rand.Rand) (h []byte, taken bool) {
	coded := uint64(rng.IntN(100))
	if rng.IntN(3) == 0 {
		coded = rng.Uint64N(1 << 36)
	}
	variable := rng.IntN(2) == 0
	switch kind := rng.IntN(10); {
	case kind < 4:
		// A code for every field, or STREAMINFO's rate and depth.
		return scanHeader(variable, 12, []int{9, 0}[rng.IntN(2)], []int{1, 8, 10}[rng.IntN(3)], []int{4, 0}[rng.IntN(2)], coded), true
	case kind < 6:
		// The longest header: a block size and a rate of two bytes each
		// behind the longest number.
		return scanHeader(variable, 7, 13+rng.IntN(2), 1, 4, coded|1<<35), true
	case kind < 8:
		// Another stream's: 48 kHz, 24 bits, or mono.
		return [][]byte{scanHeader(variable, 12, 10, 1, 4, coded), scanHeader(variable, 12, 9, 1, 6, coded),
			scanHeader(variable, 12, 9, 0, 4, coded)}[rng.IntN(3)], false
	}
	h = scanHeader(variable, 12, 9, 1, 4, coded)
	h[2+rng.IntN(len(h)-2)] ^= 0x10 // the CRC-8 no longer closes it
	return h, false
}

// scanDemuxer is a demuxer to scan data with, the data ending at end.
func scanDemuxer(data []byte, end int64) (*Demuxer, *testutil.CountingSource) {
	src := &testutil.CountingSource{Src: container.BytesSource(data)}
	return &Demuxer{src: src, si: scanInfo, w: srcwin.New(src, end, "flac: reading frame data")}, src
}

// TestScanPortMatchesTheOldLoop holds nextCandidate to the loop it replaced,
// over random runs and ranges: the same offset, header and answer, in reads
// the range bounds, with every shape a scan takes reached.
func TestScanPortMatchesTheOldLoop(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 11))
	var seen scanShapes
	for i := range 4000 {
		data, marks, lens := scanRun(rng)
		n := int64(len(data))
		end, near := n, int64(-1) // near is the header the end was aimed at
		switch aim := rng.IntN(8); {
		case aim < 2:
			end = testutil.ScanOffset(rng, marks, n) // a stream cut short, with bytes behind the cut
		case aim < 4 && len(marks) > 0:
			// Inside a header, at its end, or a few bytes behind it.
			k := rng.IntN(len(marks))
			near, end = marks[k], min(marks[k]+int64(lens[k])-2+rng.Int64N(6), n)
		}
		from := testutil.ScanFrom(rng, marks, n, flac.MaxFrameHeaderLen)
		if near >= 0 && rng.IntN(2) == 0 {
			from = max(near-rng.Int64N(64), 0)
		}
		limit := testutil.ScanLimit(rng, marks, n, from, maxResync)

		old, _ := scanDemuxer(data, end)
		now, nowSrc := scanDemuxer(data, end)
		wantOff, wantFi, want := oldNextCandidate(old, from, limit, &seen)
		off, fi, ok := now.nextCandidate(from, limit)
		if off != wantOff || fi != wantFi || ok != want {
			t.Fatalf("case %d: %d bytes ending at %d, scanning [%d, %d): nextCandidate = %d, %+v, %v; the old loop %d, %+v, %v",
				i, n, end, from, limit, off, fi, ok, wantOff, wantFi, want)
		}
		if old.w.Err() != nil || now.w.Err() != nil {
			t.Fatalf("case %d: Err = %v, the old loop's %v", i, now.w.Err(), old.w.Err())
		}
		// The range's spans, and one more for the bytes a header cut by a
		// span's end is read again in. A candidate is judged from its view,
		// and an empty range reads nothing.
		scanned := max(min(limit, end)-from, 0)
		most := 1 + testutil.SpansOver(scanned+flac.MaxFrameHeaderLen-1, srcwin.Chunk)
		if scanned == 0 {
			most = 0
			seen.empty++
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
		if off == from {
			seen.atFrom++
		}
		if data[off+2]>>4 == 7 {
			seen.longest++ // the block size of two bytes is the longest header's alone
		}
		if testutil.AcrossSpan(off-from, flac.MaxFrameHeaderLen) > 0 {
			seen.acrossSpan++
		}
	}
	t.Logf("the old loop met %+v", seen)
	testutil.ShapesReached(t,
		testutil.ScanShape{Name: "found", N: seen.found}, testutil.ScanShape{Name: "not found", N: seen.notFound},
		testutil.ScanShape{Name: "found at from", N: seen.atFrom}, testutil.ScanShape{Name: "an empty range", N: seen.empty},
		testutil.ScanShape{Name: "cut by the limit", N: seen.limitCut},
		testutil.ScanShape{Name: "down to the last byte", N: seen.lastByte},
		testutil.ScanShape{Name: "found on a header of sixteen bytes", N: seen.longest},
		testutil.ScanShape{Name: "found in a view the data's end cut short", N: seen.shortView},
		testutil.ScanShape{Name: "a header the data's end cut", N: seen.cutHeader},
		testutil.ScanShape{Name: "a sync byte with no sync code", N: seen.noSync},
		testutil.ScanShape{Name: "a sync code with no header", N: seen.unparsed},
		testutil.ScanShape{Name: "a header of another stream", N: seen.inconsistent},
		testutil.ScanShape{Name: "found on a header across a span's end", N: seen.acrossSpan})
}

// pieceShapes counts what the probes below met at the seams between pieces.
type pieceShapes struct {
	found, notFound int
	later           int // found in a piece behind the first
	acrossSeam      int // found on a header a seam cuts
	atSeam          int // found on a piece's first byte
	beforeSeam      int // found on a piece's last byte
	rejectedAtSeam  int // a header of another stream, or a broken one, across a seam before the answer
}

// TestProbeInPiecesMatchesTheOldLoop holds probe, which scans a range a
// piece at a time, to the old loop's one scan: the same offset, header and
// answer wherever the seams fall. Each case aims a seam at a planted header.
func TestProbeInPiecesMatchesTheOldLoop(t *testing.T) {
	rng := rand.New(rand.NewPCG(13, 13))
	var seen pieceShapes
	for i := range 2500 {
		data, marks, lens := scanRun(rng)
		n := int64(len(data))
		piece := 32 + rng.Int64N(4<<10)
		from := testutil.ScanFrom(rng, marks, n, flac.MaxFrameHeaderLen)
		// A seam somewhere from sixteen bytes in front of a header to its
		// last byte, or one of another stream's on it.
		seam := int64(-1)
		if len(marks) > 0 && rng.IntN(4) > 0 {
			k := rng.IntN(len(marks))
			seam = marks[k] - 16 + rng.Int64N(int64(lens[k])+16)
			if rng.IntN(3) == 0 {
				seam = marks[k] + rng.Int64N(2) // the header a piece's first byte, or its last
			}
		} else if rng.IntN(2) == 0 {
			h, _ := scanPlant(rng)
			seam = rng.Int64N(n + 1)
			copy(data[max(seam-rng.Int64N(int64(len(h))), 0):], h)
		}
		if f := seam - (1+rng.Int64N(4))*piece; f >= 0 {
			from = f
		}
		limit := from + rng.Int64N(2*n+1)
		end := n
		if rng.IntN(4) == 0 {
			end = testutil.ScanOffset(rng, marks, n)
		}

		old, _ := scanDemuxer(data, end)
		now, _ := scanDemuxer(data, end)
		var unused scanShapes
		wantOff, wantFi, want := oldNextCandidate(old, from, limit, &unused)
		off, fi, ok := now.probe(from, limit, piece)
		if off != wantOff || fi != wantFi || ok != want {
			t.Fatalf("case %d: %d bytes ending at %d, probing [%d, %d) in pieces of %d: probe = %d, %+v, %v; the old loop %d, %+v, %v",
				i, n, end, from, limit, piece, off, fi, ok, wantOff, wantFi, want)
		}
		if old.w.Err() != nil || now.w.Err() != nil {
			t.Fatalf("case %d: Err = %v, the old loop's %v", i, now.w.Err(), old.w.Err())
		}
		// A header the stream does not take, lying across a seam the probe
		// crossed.
		reach := min(limit, end)
		if want {
			reach = off
		}
		for s := from + piece; s < reach; s += piece {
			if hdr := data[max(s-8, from):min(s+8, end)]; bytes.Contains(hdr, []byte{0xFF, 0xF8}) || bytes.Contains(hdr, []byte{0xFF, 0xF9}) {
				seen.rejectedAtSeam++
				break
			}
		}
		if !want {
			seen.notFound++
			continue
		}
		seen.found++
		rel := off - from
		if rel >= piece {
			seen.later++
		}
		// The header's length, where it is one that was planted.
		if k := slices.Index(marks, off); k >= 0 && rel/piece != (rel+int64(lens[k])-1)/piece {
			seen.acrossSeam++
		}
		if rel > 0 && rel%piece == 0 {
			seen.atSeam++
		}
		if (rel+1)%piece == 0 {
			seen.beforeSeam++
		}
	}
	t.Logf("the probes met %+v", seen)
	testutil.ShapesReached(t,
		testutil.ScanShape{Name: "found", N: seen.found}, testutil.ScanShape{Name: "not found", N: seen.notFound},
		testutil.ScanShape{Name: "found in a piece behind the first", N: seen.later},
		testutil.ScanShape{Name: "found on a header a seam cuts", N: seen.acrossSeam},
		testutil.ScanShape{Name: "found on a piece's first byte", N: seen.atSeam},
		testutil.ScanShape{Name: "found on a piece's last byte", N: seen.beforeSeam},
		testutil.ScanShape{Name: "a sync code at a seam in front of the answer", N: seen.rejectedAtSeam})

	// The pieces a seek's probe scans in: 3 MiB with a header at each seam,
	// one byte in front of it and fifteen in front of it, taken one at a time.
	for _, back := range []int64{0, 1, 15} {
		for _, k := range []int64{1, 2} {
			data := bytes.Repeat([]byte{0x11}, 3<<20)
			from := int64(4321)
			at := from + k*maxResync - back
			copy(data[at:], scanHeader(true, 12, 9, 1, 4, 7))
			old, _ := scanDemuxer(data, int64(len(data)))
			now, _ := scanDemuxer(data, int64(len(data)))
			var unused scanShapes
			wantOff, wantFi, want := oldNextCandidate(old, from, int64(len(data)), &unused)
			off, fi, ok := now.probe(from, int64(len(data)), maxResync)
			if off != wantOff || fi != wantFi || ok != want || off != at {
				t.Errorf("a header %d bytes in front of seam %d: probe = %d, %+v, %v; the old loop %d, %+v, %v", back, k, off, fi, ok, wantOff, wantFi, want)
			}
		}
	}
}
