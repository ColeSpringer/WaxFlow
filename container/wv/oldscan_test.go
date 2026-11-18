package wv

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"testing"

	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/internal/testutil"
)

// scanShapes counts what the old loop met, so the comparison below can say
// it met each shape a scan takes.
type scanShapes struct {
	found, notFound int
	atFrom          int // the block at from taken
	limitCut        int // a magic at or past limit ended the scan
	ended           int // fewer bytes than a header were left
	noSync          int // a magic with no header behind it
	tooLong         int // a header whose block runs past the data end
	otherShape      int // an audio header of another stream
	toEnd           int // a block taken because it ends at the data end
	behind          int // a block taken on the header behind it
	cutBehind       int // a candidate whose header behind the data end cut
	sampleFree      int // a block without samples taken
	wide            int // a candidate stating a block over a Chunk, confirmed or not
	acrossSpan      int // a block taken with its header across a span's end
	empty           int // a range that holds nothing
}

// oldNextCandidate is nextCandidate as it stood before srcwin.Scan: a window
// read at every candidate, and a read of the header behind wherever it lies.
// It is the oracle the scan is held to, and it counts the shapes it meets.
func oldNextCandidate(d *Demuxer, from, limit int64, seen *scanShapes) (int64, wavpack.BlockHeader, bool) {
	limit = min(limit, d.w.DataEnd())
	for off := from; off < limit; {
		buf := d.w.BytesAt(off, srcwin.Chunk)
		if len(buf) < wavpack.BlockHeaderLen {
			seen.ended++
			return 0, wavpack.BlockHeader{}, false
		}
		i := bytes.Index(buf, magic)
		if i < 0 {
			off += int64(len(buf) - len(magic) + 1)
			continue
		}
		cand := off + int64(i)
		if cand >= limit {
			seen.limitCut++
			return 0, wavpack.BlockHeader{}, false
		}
		if h, ok := oldConfirm(d, cand, seen); ok {
			if cand == from {
				seen.atFrom++
			}
			if !h.Audio() {
				seen.sampleFree++
			}
			return cand, h, true
		}
		off = cand + 1
	}
	return 0, wavpack.BlockHeader{}, false
}

// oldConfirm is confirm as it stood beside that loop, on blockAt's reads.
func oldConfirm(d *Demuxer, off int64, seen *scanShapes) (wavpack.BlockHeader, bool) {
	hdr := d.w.BytesAt(off, wavpack.BlockHeaderLen)
	h, ok := d.block(off, hdr)
	if !ok {
		if len(hdr) == wavpack.BlockHeaderLen && !wavpack.SyncOK(hdr) {
			seen.noSync++
		} else if p, err := wavpack.ParseBlockHeader(hdr); err == nil && off+p.Size > d.w.DataEnd() {
			seen.tooLong++
		}
		return wavpack.BlockHeader{}, false
	}
	if h.Size > srcwin.Chunk {
		seen.wide++
	}
	if d.cfg.Channels != 0 && h.Audio() &&
		(h.Channels() != d.cfg.Channels || h.BytesPerSample()*8 != d.cfg.BitDepth) {
		seen.otherShape++
		return wavpack.BlockHeader{}, false
	}
	end := off + h.Size
	if end == d.w.DataEnd() {
		seen.toEnd++
		return h, true
	}
	next := d.w.BytesAt(end, wavpack.BlockHeaderLen)
	if _, ok := d.block(end, next); !ok {
		if len(next) < wavpack.BlockHeaderLen {
			seen.cutBehind++
		}
		return wavpack.BlockHeader{}, false
	}
	seen.behind++
	return h, true
}

// scanCfg is the stream the scans check a header's shape against: stereo,
// 16-bit.
var scanCfg = wavpack.Config{Rate: 44100, Channels: 2, BitDepth: 16}

// Header flags for the shapes planted: the stream's, two others, and one the
// parse refuses (mono and false stereo both).
var scanFlags = []uint32{1, 1 | 4, 2, 4 | 0x40000000}

// scanHeader is a block header stating size bytes, samples frames at index,
// with flags and a stream version.
func scanHeader(size, samples int, index int64, flags uint32, version uint16) []byte {
	h := make([]byte, wavpack.BlockHeaderLen)
	copy(h, "wvpk")
	binary.LittleEndian.PutUint32(h[4:], uint32(size-8))
	binary.LittleEndian.PutUint16(h[8:], version)
	binary.LittleEndian.PutUint32(h[12:], 0xffffffff)
	binary.LittleEndian.PutUint32(h[16:], uint32(index))
	binary.LittleEndian.PutUint32(h[20:], uint32(samples))
	binary.LittleEndian.PutUint32(h[24:], flags)
	return h
}

// scanSize is a length a planted header states: short, so the header behind
// it is in reach, now and then long, and rarely past what a header may state
// or odd.
func scanSize(rng *rand.Rand) int {
	switch rng.IntN(16) {
	case 0:
		return 32 + 2*rng.IntN(1<<19) // up to the largest block
	case 1:
		return 32 + 2*rng.IntN(40000)
	case 2:
		return []int{24, 33, 1<<20 + 8, 1<<20 + 7}[rng.IntN(4)] // SyncOK refuses each
	}
	return 32 + 2*rng.IntN(600)
}

// scanRun is a random run to scan: junk that spells the magic by chance, then
// planted headers, alone or as runs of blocks of one shape. marks are where
// they were planted, nexts where the block behind each starts.
func scanRun(rng *rand.Rand) (data []byte, marks, nexts []int64) {
	data = testutil.RandomJunk(rng, []byte{0x11, 0x00, 0x01, 0x04, 0x10, 0x18, 0x20, 0xFF}, []byte("wvpk"), []int{0, 4, 8})
	// Now and then room for blocks over a Chunk, and runs of them.
	long := rng.IntN(24) == 0
	if long {
		data = append(data, bytes.Repeat([]byte{0x11}, 1<<20+rng.IntN(300<<10))...)
	}
	n := len(data)
	for range rng.IntN(8) {
		at := rng.IntN(n + 1)
		flags := scanFlags[rng.IntN(len(scanFlags))]
		version := uint16(0x410)
		if rng.IntN(16) == 0 {
			version = 0x401
		}
		size := scanSize
		if long && rng.IntN(2) == 0 {
			at = rng.IntN(n / 4)
			size = func(*rand.Rand) int { return srcwin.Chunk + 2*rng.IntN(150<<10) }
		}
		for range 1 + rng.IntN(3) {
			if at >= n {
				break
			}
			size, samples := size(rng), 1+rng.IntN(8192)
			if rng.IntN(4) == 0 {
				samples = 0
			}
			copy(data[at:], scanHeader(size, samples, int64(at), flags, version))
			marks = append(marks, int64(at))
			at += size
			nexts = append(nexts, int64(at))
		}
	}
	return data, marks, nexts
}

// TestScanPortMatchesTheOldLoop holds nextCandidate to the loop it replaced,
// over random runs, ranges and tracks: the same offset, header and answer, in
// reads the range bounds, with every shape a scan takes reached.
func TestScanPortMatchesTheOldLoop(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 5))
	var seen scanShapes
	for i := range 4000 {
		data, marks, nexts := scanRun(rng)
		n := int64(len(data))
		end := n
		switch aim := rng.IntN(8); {
		case aim < 2:
			end = testutil.ScanOffset(rng, marks, n) // a stream cut short, with bytes behind the cut
		case aim < 4 && len(nexts) > 0:
			// Where a planted block ends, inside the header behind it, or just past.
			end = min(max(nexts[rng.IntN(len(nexts))]+rng.Int64N(40)-4, 0), n)
		case aim < 5 && len(nexts) > 0:
			end = min(nexts[rng.IntN(len(nexts))], n) // exactly where one ends
		}
		from := testutil.ScanFrom(rng, marks, n, wavpack.BlockHeaderLen)
		limit := testutil.ScanLimit(rng, marks, n, from, maxResync)
		cfg := wavpack.Config{}
		if rng.IntN(2) == 0 {
			cfg = scanCfg
		}
		demuxer := func() (*Demuxer, *testutil.CountingSource) {
			src := &testutil.CountingSource{Src: container.BytesSource(data)}
			return &Demuxer{cfg: cfg, w: srcwin.New(src, end, "wavpack: reading block data")}, src
		}

		old, _ := demuxer()
		now, nowSrc := demuxer()
		wantOff, wantHdr, want := oldNextCandidate(old, from, limit, &seen)
		off, hdr, ok := now.nextCandidate(from, limit)
		if off != wantOff || hdr != wantHdr || ok != want {
			t.Fatalf("case %d: %d bytes ending at %d, scanning [%d, %d): nextCandidate = %d, %+v, %v; the old loop %d, %+v, %v",
				i, n, end, from, limit, off, hdr, ok, wantOff, wantHdr, want)
		}
		if old.w.Err() != nil || now.w.Err() != nil {
			t.Fatalf("case %d: Err = %v, the old loop's %v", i, now.w.Err(), old.w.Err())
		}
		// The range's spans, and the confirming reads: one a doubling of what
		// the scan holds, then one a Chunk, up to a confirm's reach past the
		// range. An empty range reads nothing.
		scanned := max(min(limit, end)-from, 0)
		reach := min(scanned+wavpack.BlockHeaderLen+1<<20+6, end-from)
		most := 1 + testutil.SpansOver(scanned+wavpack.BlockHeaderLen-1, srcwin.Chunk) + testutil.SpansOver(reach, srcwin.Chunk)
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
		if testutil.AcrossSpan(off-from, wavpack.BlockHeaderLen) > 0 {
			seen.acrossSpan++
		}
	}
	t.Logf("the old loop met %+v", seen)
	testutil.ShapesReached(t,
		testutil.ScanShape{Name: "found", N: seen.found}, testutil.ScanShape{Name: "not found", N: seen.notFound},
		testutil.ScanShape{Name: "found at from", N: seen.atFrom},
		testutil.ScanShape{Name: "an empty range", N: seen.empty},
		testutil.ScanShape{Name: "cut by the limit", N: seen.limitCut},
		testutil.ScanShape{Name: "out of bytes for a header", N: seen.ended},
		testutil.ScanShape{Name: "a magic with no header", N: seen.noSync},
		testutil.ScanShape{Name: "a block running past the data end", N: seen.tooLong},
		testutil.ScanShape{Name: "a header of another stream", N: seen.otherShape},
		testutil.ScanShape{Name: "a block taken because it ends at the data end", N: seen.toEnd},
		testutil.ScanShape{Name: "a block confirmed by the header behind it", N: seen.behind},
		testutil.ScanShape{Name: "a header behind that the data end cut", N: seen.cutBehind},
		testutil.ScanShape{Name: "a block without samples taken", N: seen.sampleFree},
		testutil.ScanShape{Name: "a header stating a block over a Chunk", N: seen.wide},
		testutil.ScanShape{Name: "found on a header across a span's end", N: seen.acrossSpan})
}

// tailShapes counts what the old tail scan met.
type tailShapes struct {
	found, notFound int
	firstWindow     int // the last block found in the tail's last Chunk
	further         int // found further back
	hops            int // found across sample-free blocks behind it
	tooManyHops     int // a block whose run to the end holds more than maxMetaBlocks
	sampleFree      int // a sample-free candidate passed over
	atSeam          int // a candidate whose magic ends within three bytes of a Chunk step
	long            int // a range that maxTailScan cuts, not the first block
	empty           int // a range that holds nothing
	seeded          int // a scan from a window that already holds bytes
	otherShape      int // a block that tiles to the end passed over for its shape
}

// oldScanTail is scanTail as it stood before the window held its range: a
// window read at each step, and the hops to the end read wherever they lie.
func oldScanTail(d *Demuxer, seen *tailShapes) (int64, bool) {
	audioEnd := d.w.DataEnd()
	lo := max(d.firstBlock, audioEnd-maxTailScan)
	if lo > d.firstBlock {
		seen.long++
	}
	if lo >= audioEnd {
		seen.empty++
	}
	for hi := audioEnd; hi > lo; {
		from := max(lo, hi-srcwin.Chunk)
		buf := d.w.BytesAt(from, int(hi-from))
		if len(buf) == 0 {
			return 0, false
		}
		for rel := len(buf); ; {
			i := bytes.LastIndex(buf[:rel], magic)
			if i < 0 {
				break
			}
			rel = i + len(magic) - 1
			cand := from + int64(i)
			// scanTail steps at a Chunk's multiples from the end.
			if d := audioEnd - cand; d > srcwin.Chunk && d%srcwin.Chunk >= 1 && d%srcwin.Chunk <= 3 {
				seen.atSeam++
			}
			h, ok := d.block(cand, d.w.BytesAt(cand, wavpack.BlockHeaderLen))
			if ok && !h.Audio() {
				seen.sampleFree++
			}
			if !ok || !h.Audio() {
				continue
			}
			hops, tiles := oldTilesToEnd(d, cand+h.Size, audioEnd)
			if !tiles {
				if hops > maxMetaBlocks {
					seen.tooManyHops++
				}
				continue
			}
			// The one difference since: a block of another shape, which the
			// read refuses, gives no length.
			if d.cfg.Channels != 0 && (h.Channels() != d.cfg.Channels || h.BytesPerSample()*8 != d.cfg.BitDepth) {
				seen.otherShape++
				continue
			}
			if cand >= audioEnd-srcwin.Chunk {
				seen.firstWindow++
			} else {
				seen.further++
			}
			if hops > 0 {
				seen.hops++
			}
			return h.BlockIndex + int64(h.BlockSamples), true
		}
		if from == lo {
			break
		}
		hi = from + int64(len(magic)) - 1
	}
	return 0, false
}

// oldTilesToEnd is tilesToEnd as it stood beside that scan, on blockAt's
// reads, and says how many blocks it hopped.
func oldTilesToEnd(d *Demuxer, off, end int64) (int, bool) {
	for i := range maxMetaBlocks + 1 {
		switch {
		case off == end:
			return i, true
		case off > end:
			return i, false
		}
		h, ok := d.block(off, d.w.BytesAt(off, wavpack.BlockHeaderLen))
		if !ok {
			return i, false
		}
		off += h.Size
	}
	return maxMetaBlocks + 1, false
}

// tailRun is a random stream to scan from its end: bytes in front of the
// first block, a run of blocks, now and then sample-free ones at its end, and
// then junk, a cut, or nothing. Headers that lie are planted in the payloads,
// some across the seams the scan steps at. firstBlock is where the run
// starts. A long run is of long blocks, so the old scan's hops stay cheap.
func tailRun(rng *rand.Rand) (data []byte, firstBlock int64) {
	data = testutil.RandomJunk(rng, []byte{0x11, 0x00, 0x04, 0x18}, []byte("wvpk"), []int{0, 4})
	if rng.IntN(2) == 0 {
		data = data[:0]
	}
	firstBlock = int64(len(data))
	total, longest := 20<<10, 2000
	switch rng.IntN(48) {
	case 0:
		total, longest = maxTailScan+rng.IntN(1<<20), 1<<19 // past what the scan reaches
	case 1, 2, 3, 4:
		total, longest = 400<<10, 40000
	case 5:
		total = 0 // no blocks
	}
	var blocks []int64
	for index := int64(0); len(data)-int(firstBlock) < total; {
		n, samples := 32+2*rng.IntN(longest), 1+rng.IntN(8192)
		blocks = append(blocks, int64(len(data)))
		data = append(data, scanHeader(n, samples, index, 1, 0x410)...)
		data = append(data, bytes.Repeat([]byte{0x11}, n-wavpack.BlockHeaderLen)...)
		index += int64(samples)
	}
	// Sample-free blocks a stream signs off with, at times more than a scan
	// hops.
	if len(blocks) > 0 && rng.IntN(3) == 0 {
		for range 1 + rng.IntN(20) {
			blocks = append(blocks, int64(len(data)))
			data = append(data, scanHeader(32, 0, 0, 1, 0x410)...)
		}
	}
	switch rng.IntN(4) {
	case 0:
		data = data[:max(int64(len(data))-1-rng.Int64N(200), firstBlock)] // cut inside the last block
	case 1:
		data = append(data, bytes.Repeat([]byte{0x22}, 1+rng.IntN(300))...) // junk behind
	}
	// Headers that lie: audio, stating the end of the data, a block start or
	// anywhere, at random or across a seam the scan steps at.
	end := int64(len(data))
	for range rng.IntN(6) {
		at := firstBlock + rng.Int64N(end-firstBlock+1)
		if seams := (end - firstBlock) / srcwin.Chunk; seams > 0 && rng.IntN(2) == 0 {
			at = end - (1+rng.Int64N(seams))*srcwin.Chunk - 1 - rng.Int64N(3)
		}
		if at < firstBlock || at+wavpack.BlockHeaderLen > end {
			continue
		}
		var target int64
		switch rng.IntN(3) {
		case 0:
			target = end
		case 1:
			if len(blocks) > 0 {
				target = blocks[rng.IntN(len(blocks))]
			}
		default:
			target = at + 32 + 2*rng.Int64N(end/2+1)
		}
		if n := target - at; n >= 32 && n%2 == 0 && n < 1<<20 {
			copy(data[at:], scanHeader(int(n), 1+rng.IntN(100), 7, scanFlags[rng.IntN(len(scanFlags))], 0x410))
		}
	}
	return data, firstBlock
}

// TestTailScanPortMatchesTheOldLoop holds scanTail, which steps back through
// what it holds, to the scan it replaced: the same answer on random streams,
// in a read a step and each byte once, with every shape a tail takes reached.
func TestTailScanPortMatchesTheOldLoop(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	var seen tailShapes
	for i := range 3000 {
		data, firstBlock := tailRun(rng)
		// Half the scans start from a window that holds bytes: the first
		// block's exact read, as an open leaves it, or the tail's last bytes,
		// as a trailer probe leaves them.
		n := int64(len(data))
		seed := [2]int64{}
		switch rng.IntN(4) {
		case 0:
			seed = [2]int64{firstBlock, min(firstBlock+32+rng.Int64N(70<<10), n)}
		case 1:
			seed = [2]int64{max(n-1-rng.Int64N(128), firstBlock), n}
		}
		if seed[1] > seed[0] {
			seen.seeded++
		}
		// Half the scans know the stream's shape, the blocks' own.
		cfg := wavpack.Config{}
		if rng.IntN(2) == 0 {
			cfg = scanCfg
		}
		demuxer := func() (*Demuxer, *testutil.CountingSource) {
			src := &testutil.CountingSource{Src: container.BytesSource(data)}
			d := &Demuxer{cfg: cfg, firstBlock: firstBlock, w: srcwin.New(src, n, "wavpack: reading block data")}
			d.w.Peek(seed[0], int(seed[1]-seed[0]))
			src.Reset()
			return d, src
		}
		old, _ := demuxer()
		now, nowSrc := demuxer()
		passed := seen.otherShape
		wantEnd, want := oldScanTail(old, &seen)
		got, ok := now.scanTail()
		if got.end != wantEnd || ok != want {
			t.Fatalf("case %d: %d bytes, the first block at %d: scanTail = %d, %v; the old scan %d, %v",
				i, len(data), firstBlock, got.end, ok, wantEnd, want)
		}
		// Sure unless the old scan passed over a block of another shape.
		if got.sure != (seen.otherShape == passed) {
			t.Fatalf("case %d: scanTail is sure %v of its answer; the old scan passed over %d blocks of another shape",
				i, got.sure, seen.otherShape-passed)
		}
		if old.w.Err() != nil || now.w.Err() != nil {
			t.Fatalf("case %d: Err = %v, the old scan's %v", i, now.w.Err(), old.w.Err())
		}
		span := max(n-max(firstBlock, n-maxTailScan), 0)
		if steps := int((span + srcwin.Chunk - 1) / srcwin.Chunk); nowSrc.Reads > steps || nowSrc.Bytes > span {
			t.Fatalf("case %d: a tail scan over %d bytes took %d reads of %d bytes, want at most %d, each byte once",
				i, span, nowSrc.Reads, nowSrc.Bytes, steps)
		}
		if want {
			seen.found++
		} else {
			seen.notFound++
		}
	}
	t.Logf("the old scan met %+v", seen)
	testutil.ShapesReached(t,
		testutil.ScanShape{Name: "found", N: seen.found}, testutil.ScanShape{Name: "not found", N: seen.notFound},
		testutil.ScanShape{Name: "found in the last Chunk", N: seen.firstWindow},
		testutil.ScanShape{Name: "found further back", N: seen.further},
		testutil.ScanShape{Name: "found across sample-free blocks", N: seen.hops},
		testutil.ScanShape{Name: "a run to the end of more blocks than a scan hops", N: seen.tooManyHops},
		testutil.ScanShape{Name: "a sample-free candidate passed over", N: seen.sampleFree},
		testutil.ScanShape{Name: "a candidate at a step's seam", N: seen.atSeam},
		testutil.ScanShape{Name: "a range maxTailScan cuts", N: seen.long},
		testutil.ScanShape{Name: "an empty range", N: seen.empty},
		testutil.ScanShape{Name: "a scan from a window that holds bytes", N: seen.seeded},
		testutil.ScanShape{Name: "a block of another shape that tiles to the end passed over", N: seen.otherShape})
}
