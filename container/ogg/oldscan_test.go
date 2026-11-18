package ogg

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// scanShapes counts what the old loop met, so the comparisons below can say
// they met each shape a scan takes.
type scanShapes struct {
	found, notFound, gaveUp int
	failed                  map[string]int // reads that failed, by the call's words
	noRoom                  int            // a candidate with no room for a header
	version                 int            // a candidate of an unknown structure version
	lacingPast, bodyPast    int            // a lacing table, a body, past the end of data
	mismatch                int            // a candidate that parses and fails its checksum
	headerAcross            int            // a candidate whose header runs past its scan window
	patternAcross           int            // a capture pattern across a step's end
	laterStep               int            // a page found in a step after the first
	endsData                int            // a page found that ends the data
}

// oldScanner is the page reads as they stood before the window: readPage,
// nextPageAt and granuleAt, each reading the source for itself. It is the
// oracle the window is held to, and it counts the shapes it meets.
type oldScanner struct {
	src  container.Source
	size int64
	scan []byte
	seen *scanShapes
}

func (o *oldScanner) fail(err error, msg string) error {
	o.seen.failed[msg]++
	return waxerr.Wrap(waxerr.CodeSourceUnreadable, msg, err)
}

func (o *oldScanner) readPage(off int64, p *page) (bool, error) {
	if off+headerLen > o.size {
		o.seen.noRoom++
		return false, nil
	}
	var hdr [headerLen + 255]byte
	if err := container.ReadFull(o.src, hdr[:headerLen], off); err != nil {
		return false, o.fail(err, "ogg: reading page header")
	}
	if !Match(hdr[:]) {
		return false, malformed("no page capture pattern at offset %d", off)
	}
	if hdr[4] != 0 {
		o.seen.version++
		return false, malformed("unknown page structure version %d", hdr[4])
	}
	nsegs := int(hdr[26])
	if off+headerLen+int64(nsegs) > o.size {
		o.seen.lacingPast++
		return false, malformed("page lacing table extends past end of file")
	}
	if err := container.ReadFull(o.src, hdr[headerLen:headerLen+nsegs], off+headerLen); err != nil {
		return false, o.fail(err, "ogg: reading lacing table")
	}
	bodyLen := 0
	for _, l := range hdr[headerLen : headerLen+nsegs] {
		bodyLen += int(l)
	}
	if off+headerLen+int64(nsegs)+int64(bodyLen) > o.size {
		o.seen.bodyPast++
		return false, malformed("page body extends past end of file")
	}

	p.off = off
	p.flags = hdr[5]
	p.granule = int64(binary.LittleEndian.Uint64(hdr[6:]))
	p.serial = binary.LittleEndian.Uint32(hdr[14:])
	p.size = int64(headerLen + nsegs + bodyLen)
	p.lacing = append(p.lacing[:0], hdr[headerLen:headerLen+nsegs]...)
	if cap(p.body) < bodyLen {
		p.body = make([]byte, bodyLen)
	}
	p.body = p.body[:bodyLen]
	if err := container.ReadFull(o.src, p.body, off+headerLen+int64(nsegs)); err != nil {
		return false, o.fail(err, "ogg: reading page body")
	}

	crc := crc32(0, hdr[:22])
	crc = crc32(crc, []byte{0, 0, 0, 0})
	crc = crc32(crc, hdr[26:headerLen+nsegs])
	crc = crc32(crc, p.body)
	if want := binary.LittleEndian.Uint32(hdr[22:]); crc != want {
		o.seen.mismatch++
		return false, malformed("page checksum mismatch at offset %d", off)
	}
	return true, nil
}

func (o *oldScanner) nextPageAt(off int64, p *page) (int64, bool, error) {
	base := off
	if o.scan == nil {
		o.scan = make([]byte, 64<<10)
	}
	for off < o.size {
		if off-base > maxResync {
			o.seen.gaveUp++
			return 0, false, malformed("no page capture pattern within %d bytes", int64(maxResync))
		}
		n := min(int64(len(o.scan)), o.size-off)
		if n < 4 {
			o.seen.notFound++
			return 0, false, nil
		}
		buf := o.scan[:n]
		if err := container.ReadFull(o.src, buf, off); err != nil {
			return 0, false, o.fail(err, "ogg: scanning for page")
		}
		rel := 0
		for {
			i := bytes.Index(buf[rel:], []byte("OggS"))
			if i < 0 {
				break
			}
			cand := off + int64(rel+i)
			rel += i + 1
			if cand+headerLen > off+n && cand+headerLen <= o.size {
				o.seen.headerAcross++
			}
			if off > base && cand-off < 3 {
				o.seen.patternAcross++
			}
			ok, err := o.readPage(cand, p)
			if err == nil && ok {
				o.seen.found++
				if off > base {
					o.seen.laterStep++
				}
				if cand+p.size == o.size {
					o.seen.endsData++
				}
				return cand, true, nil
			}
			if err != nil && waxerr.CodeOf(err) == waxerr.CodeSourceUnreadable {
				return 0, false, err
			}
		}
		off += n - 3 // a capture pattern may straddle the boundary
	}
	o.seen.notFound++
	return 0, false, nil
}

func (o *oldScanner) granuleAt(off, limit int64, serial uint32, disowned int64) (int64, int64, bool, error) {
	var p page
	for off < limit {
		at, ok, err := o.nextPageAt(off, &p)
		if err != nil || !ok || at >= limit {
			return 0, 0, false, err
		}
		if p.serial == serial && p.granule >= 0 && p.flags&flagBOS == 0 && at != disowned {
			return at, p.granule, true, nil
		}
		off = at + p.size
	}
	return 0, 0, false, nil
}

// sameErr says whether two answers failed alike: both nil, or of one code
// and the same words.
func sameErr(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return waxerr.CodeOf(a) == waxerr.CodeOf(b) && a.Error() == b.Error()
}

// samePage says whether two pages read alike.
func samePage(a, b *page) bool {
	return a.off == b.off && a.size == b.size && a.flags == b.flags && a.granule == b.granule &&
		a.serial == b.serial && bytes.Equal(a.lacing, b.lacing) && bytes.Equal(a.body, b.body)
}

// randomPage is a whole page of a random body, now and then a large one.
func randomPage(rng *rand.Rand) []byte {
	n := rng.IntN(600)
	switch rng.IntN(12) {
	case 0:
		n = 0
	case 1:
		n = rng.IntN(255 * 255)
	}
	body := make([]byte, n)
	fillBody(rng, body)
	return buildPage(byte(rng.IntN(8)), rng.Int64N(1<<20)-1, uint32(rng.IntN(3)), uint32(rng.IntN(100)), body)
}

// fillBody fills b with random bytes, eight a draw.
func fillBody(rng *rand.Rand, b []byte) {
	for i := 0; i < len(b); i += 8 {
		v := rng.Uint64()
		for j := i; j < min(i+8, len(b)); j++ {
			b[j], v = byte(v), v>>8
		}
	}
}

// decoyHead is a page head that parses and fails its checksum: version 0 and
// a lacing table of mostly small values, now and then all 255.
func decoyHead(rng *rand.Rand) []byte {
	nsegs := rng.IntN(256)
	h := make([]byte, headerLen+nsegs)
	copy(h, "OggS")
	binary.LittleEndian.PutUint32(h[22:], rng.Uint32())
	h[26] = byte(nsegs)
	for i := range nsegs {
		h[headerLen+i] = byte(rng.IntN(40))
		if nsegs > 200 && rng.IntN(2) == 0 {
			h[headerLen+i] = 255
		}
	}
	return h
}

// denseJunk is junk where a capture pattern starts about every few bytes.
func denseJunk(rng *rand.Rand) []byte {
	var b []byte
	for n := rng.IntN(2 << 10); len(b) < n; {
		b = append(b, "OggS"...)
		for range rng.IntN(4) {
			b = append(b, []byte{0, 0x4f, 0x67, 0x53, 0xff}[rng.IntN(5)])
		}
	}
	return b
}

// scanRun is a run of bytes to scan, built of what a resync meets: junk plain
// and dense, decoys, whole pages, then now and then a page or a lacing table
// cut by the end of data. marks are where heads and pages start.
func scanRun(rng *rand.Rand) (data []byte, marks []int64) {
	switch rng.IntN(12) {
	case 0:
		// Past the horizon: zeros, now and then with pages planted near it.
		data = make([]byte, resyncHorizon+1<<16, resyncHorizon+1<<16+1<<20)
	case 1, 2, 3:
		// Steps of zeros, so a scan from in front of what follows them meets
		// it across a step's end.
		n := 70<<10 + rng.IntN(300<<10)
		data = make([]byte, n, n+1<<20)
	}
	for range 1 + rng.IntN(10) {
		at := int64(len(data))
		switch rng.IntN(6) {
		case 0:
			data = append(data, testutil.RandomJunk(rng, []byte{0x00, 0x11, 0xff, 0x4f, 0x67}, []byte("OggS"), []int{0})...)
		case 1:
			data = append(data, denseJunk(rng)...)
		case 2:
			data = append(data, decoyHead(rng)...)
			marks = append(marks, at)
		default:
			data = append(data, randomPage(rng)...)
			marks = append(marks, at)
		}
	}
	switch rng.IntN(6) {
	case 0: // a page cut by the end of data
		pg := randomPage(rng)
		marks = append(marks, int64(len(data)))
		data = append(data, pg[:1+rng.IntN(len(pg)-1)]...)
	case 1: // a lacing table past the end
		h := decoyHead(rng)
		h[26] = 255
		marks = append(marks, int64(len(data)))
		data = append(data, h[:min(len(h), headerLen+rng.IntN(255))]...)
	}
	if len(data) > resyncHorizon && rng.IntN(2) == 0 {
		// Plant around the horizon of a scan from a few bytes in.
		for range 4 {
			at := resyncHorizon - 40 + rng.IntN(80)
			copy(data[at:], randomPage(rng))
			marks = append(marks, int64(at))
		}
	}
	return data, marks
}

// scanBase is where a case starts its scan: steps in front of a mark, so the
// mark's capture pattern or header falls across a step's end, or anywhere.
func scanBase(rng *rand.Rand, marks []int64, n int64) int64 {
	if len(marks) > 0 && rng.IntN(2) == 0 {
		m := marks[rng.IntN(len(marks))]
		// The mark is this far into a step: across the last's end by up to
		// three bytes, or its header across it.
		into := rng.Int64N(3)
		if rng.IntN(2) == 0 {
			into = -1 - rng.Int64N(30)
		}
		if back := (1+rng.Int64N(17))*(scanStep-3) + into; m >= back {
			return m - back
		}
	}
	if n > resyncHorizon && rng.IntN(2) == 0 {
		return rng.Int64N(8)
	}
	return testutil.ScanOffset(rng, marks, n)
}

// scanSource is data as a case reads it: whole, or with a hole near where
// the scan goes. holed is the mark a hole was put in a head of, or -1.
func scanSource(rng *rand.Rand, data []byte, marks []int64, base int64) (src func() container.Source, holed int64) {
	if rng.IntN(4) != 0 {
		return func() container.Source { return &testutil.CountingSource{Src: container.BytesSource(data)} }, -1
	}
	from, holed := testutil.ScanOffset(rng, marks, int64(len(data))), int64(-1)
	switch rng.IntN(3) {
	case 0:
		from = base + rng.Int64N(17)*(scanStep-3) + scanStep - 8 + rng.Int64N(16)
	case 1:
		if len(marks) > 0 {
			// A head, its lacing table, the head of a body.
			holed = marks[rng.IntN(len(marks))]
			from = holed + []int64{0, headerLen, headerLen + 40}[rng.IntN(3)] + rng.Int64N(40)
		}
	}
	to := from + 1 + rng.Int64N(200)
	return func() container.Source {
		return &testutil.FailingSource{Src: container.BytesSource(data), From: from, To: to}
	}, holed
}

// TestScanPortMatchesTheOldLoop holds the window's page reads to the loops
// they replaced: over random runs, from random starts, on sources whole and
// holed, a resync and the page reads behind it give the same offsets,
// answers, errors in the same words and pages, call after call on one
// demuxer, with every shape a scan takes reached.
func TestScanPortMatchesTheOldLoop(t *testing.T) {
	rng := rand.New(rand.NewPCG(19, 19))
	seen := scanShapes{failed: map[string]int{}}
	var calls, again int
	for i := range 4000 {
		data, marks := scanRun(rng)
		n := int64(len(data))
		base := scanBase(rng, marks, n)
		src, holed := scanSource(rng, data, marks, base)
		old := &oldScanner{src: src(), size: n, seen: &seen}
		now := scanDemuxer(src())
		// A resync or a page read, then what a reader does after one: read
		// the page behind the one found, or resync again from a later byte.
		off, resync := base, true
		switch {
		case holed >= 0 && rng.IntN(2) == 0:
			off, resync = holed, false
		case len(marks) > 0 && rng.IntN(3) == 0:
			off, resync = marks[rng.IntN(len(marks))], false
		}
		for step := range 4 {
			var wp, gp page
			var wantAt, at int64
			var want, got bool
			var wantErr, err error
			what := "resync"
			if resync {
				wantAt, want, wantErr = old.nextPageAt(off, &wp)
				at, got, err = now.nextPageAt(off, &gp)
			} else {
				what = "page read"
				want, wantErr = old.readPage(off, &wp)
				got, err = now.readPage(off, &gp)
				wantAt, at = off, off
			}
			calls++
			if step > 0 {
				again++
			}
			if wantAt != at || want != got || !sameErr(wantErr, err) || want && !samePage(&wp, &gp) {
				t.Fatalf("case %d, call %d, a %s from %d over %d bytes: %d, %v, %v; the old loop %d, %v, %v",
					i, step, what, off, n, at, got, err, wantAt, want, wantErr)
			}
			switch {
			case err != nil && waxerr.CodeOf(err) == waxerr.CodeSourceUnreadable:
				off = off - 1 + rng.Int64N(3)
			case want:
				off, resync = at+wp.size, rng.IntN(3) == 0
			default:
				off, resync = min(off+1+rng.Int64N(70<<10), n), true
			}
		}
		// The calls read no more often than the old loop did, and no more
		// bytes than it and a step of read-ahead a call.
		if was, ok := old.src.(*testutil.CountingSource); ok {
			is := now.src.(*testutil.CountingSource)
			if is.Reads > was.Reads || is.Bytes > was.Bytes+4*scanStep {
				t.Fatalf("case %d: %d reads of %d bytes, the old loop %d of %d", i, is.Reads, is.Bytes, was.Reads, was.Bytes)
			}
		}
	}
	t.Logf("%d calls, %d on a demuxer that had read; the old loop met %+v", calls, again, seen)
	testutil.ShapesReached(t,
		testutil.ScanShape{Name: "found", N: seen.found},
		testutil.ScanShape{Name: "not found by the end of data", N: seen.notFound},
		testutil.ScanShape{Name: "given up past maxResync", N: seen.gaveUp},
		testutil.ScanShape{Name: "a failed scan read", N: seen.failed["ogg: scanning for page"]},
		testutil.ScanShape{Name: "a failed header read", N: seen.failed["ogg: reading page header"]},
		testutil.ScanShape{Name: "a failed lacing read", N: seen.failed["ogg: reading lacing table"]},
		testutil.ScanShape{Name: "a failed body read", N: seen.failed["ogg: reading page body"]},
		testutil.ScanShape{Name: "a candidate with no room for a header", N: seen.noRoom},
		testutil.ScanShape{Name: "a candidate of an unknown version", N: seen.version},
		testutil.ScanShape{Name: "a lacing table past the end", N: seen.lacingPast},
		testutil.ScanShape{Name: "a body past the end", N: seen.bodyPast},
		testutil.ScanShape{Name: "a decoy that fails its checksum", N: seen.mismatch},
		testutil.ScanShape{Name: "a header across a scan window's end", N: seen.headerAcross},
		testutil.ScanShape{Name: "a capture pattern across a step's end", N: seen.patternAcross},
		testutil.ScanShape{Name: "a page found a step or more on", N: seen.laterStep},
		testutil.ScanShape{Name: "a page found that ends the data", N: seen.endsData})
}

// granuleRun is a paged stream for granuleAt: pages of serial 1 that state
// granules or do not, of serial 2, and beginning-of-stream pages, with junk,
// decoys and damaged pages between them. pages are where they start.
func granuleRun(rng *rand.Rand) (data []byte, pages []int64) {
	granule := int64(0)
	for range 20 + rng.IntN(300) {
		at := int64(len(data))
		switch rng.IntN(14) {
		case 0:
			data = append(data, testutil.RandomJunk(rng, []byte{0x00, 0x11, 0x4f, 0x67}, []byte("OggS"), []int{0, 4})...)
			continue
		case 1:
			data = append(data, decoyHead(rng)...)
			continue
		}
		serial, flags := uint32(1), byte(0)
		switch rng.IntN(8) {
		case 0:
			serial = 2
		case 1:
			flags = flagBOS
		}
		g := int64(-1)
		if rng.IntN(3) != 0 {
			granule += 960
			g = granule
		}
		body := make([]byte, rng.IntN(1000))
		fillBody(rng, body)
		pg := buildPage(flags, g, serial, uint32(len(pages)), body)
		if rng.IntN(20) == 0 {
			pg[headerLen+int(pg[26])/2] ^= 0x55 // damaged
		}
		data = append(data, pg...)
		pages = append(pages, at)
	}
	return data, pages
}

// TestGranuleAtMatchesTheOldLoop holds granuleAt, whose steps from page to
// page the window serves from what it holds, to the loop it was: the same
// answers and errors, probe after probe on one demuxer, as a bisection makes
// them.
func TestGranuleAtMatchesTheOldLoop(t *testing.T) {
	rng := rand.New(rand.NewPCG(23, 23))
	seen := scanShapes{failed: map[string]int{}}
	var found, past, none, failed int
	for i := range 250 {
		data, pages := granuleRun(rng)
		n := int64(len(data))
		disowned := int64(-1)
		if rng.IntN(2) == 0 {
			disowned = pages[rng.IntN(len(pages))]
		}
		src, _ := scanSource(rng, data, pages, 0)
		old := &oldScanner{src: src(), size: n, seen: &seen}
		now := scanDemuxer(src())
		now.serial, now.disowned = 1, disowned
		lo, hi := int64(0), n
		for probe := range 12 {
			mid := lo + (hi-lo)/2
			if probe%4 == 3 {
				mid = testutil.ScanOffset(rng, pages, n)
			}
			wantAt, wantG, want, wantErr := old.granuleAt(mid, hi, 1, disowned)
			at, g, ok, err := now.granuleAt(mid, hi)
			if at != wantAt || g != wantG || ok != want || !sameErr(err, wantErr) {
				t.Fatalf("case %d, probe %d of [%d, %d) at %d: %d, %d, %v, %v; the old loop %d, %d, %v, %v",
					i, probe, lo, hi, mid, at, g, ok, err, wantAt, wantG, want, wantErr)
			}
			switch {
			case err != nil:
				failed++
			case !ok:
				none++
				hi = mid
			case rng.IntN(2) == 0:
				found++
				lo = at
			default:
				past++
				hi = mid
			}
			if hi-lo < 64 {
				lo, hi = 0, n
			}
		}
	}
	t.Logf("%d found, %d passed, %d none, %d failed; the old loop met %+v", found, past, none, failed, seen)
	testutil.ShapesReached(t,
		testutil.ScanShape{Name: "a granule found", N: found},
		testutil.ScanShape{Name: "no granule before the limit", N: none},
		testutil.ScanShape{Name: "a probe that failed", N: failed},
		testutil.ScanShape{Name: "a decoy that fails its checksum", N: seen.mismatch})
}
