package mka

import (
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/waxerr"
)

// frameLoc is one codec frame's byte range in the source.
type frameLoc struct {
	off  int64
	size int
}

// blockHeader is one parsed (Simple)Block: its track, cluster-relative
// timestamp, and the byte ranges of the frames it laces together.
type blockHeader struct {
	track   uint64
	relTime int64
	frames  []frameLoc
}

// lacing selectors, from bits 0x06 of the block flags byte.
const (
	laceNone  = 0
	laceXiph  = 1
	laceFixed = 2
	laceEBML  = 3
)

// parseBlock reads the (Simple)Block header at dataOff (spanning size bytes)
// through the window and resolves its laced frames into byte ranges. It reads
// only the header and lacing table; frame payloads stay on disk until a packet
// needs them. A read failure surfaces through the window's sticky error, which
// the caller checks.
func parseBlock(w *srcwin.Window, dataOff, size int64) (blockHeader, error) {
	var bh blockHeader
	if size < 4 {
		return bh, malformed("block of %d bytes too short", size)
	}
	if size > maxBlockData {
		return bh, malformed("block of %d bytes exceeds the %d cap", size, int64(maxBlockData))
	}
	end := dataOff + size

	// Track number: a VINT whose marker is stripped like a size.
	head := w.BytesAt(dataOff, int(min(8, size)))
	if len(head) == 0 {
		return bh, readErr(w, "block track number")
	}
	track, tlen, _, ok := parseVint(head, false)
	if !ok {
		return bh, malformed("block track number is not a valid vint")
	}
	pos := dataOff + int64(tlen)
	// Two-byte relative timestamp, then the flags byte.
	if pos+3 > end {
		return bh, malformed("block header truncated")
	}
	ht := w.BytesAt(pos, 3)
	if len(ht) < 3 {
		return bh, readErr(w, "block timestamp")
	}
	bh.track = track
	bh.relTime = int64(int16(uint16(ht[0])<<8 | uint16(ht[1])))
	flags := ht[2]
	pos += 3

	lacing := int(flags>>1) & 0x3
	if lacing == laceNone {
		bh.frames = []frameLoc{{off: pos, size: int(end - pos)}}
		return bh, nil
	}

	// Laced: a count byte (frames minus one) precedes the size table.
	cb := w.BytesAt(pos, 1)
	if len(cb) < 1 {
		return bh, readErr(w, "block lace count")
	}
	n := int(cb[0]) + 1
	pos++
	if n < 1 || n > maxLaceFrames {
		return bh, malformed("block laces %d frames", n)
	}

	sizes := make([]int, n)
	var err error
	switch lacing {
	case laceFixed:
		// No size table: every frame is the same length, and pos already
		// points at the frame data. The shared tail fills sizes[n-1].
		total := end - pos
		if total < 0 || total%int64(n) != 0 {
			return bh, malformed("fixed-lace block of %d bytes not divisible by %d frames", total, n)
		}
		each := int(total / int64(n))
		for i := 0; i < n-1; i++ {
			sizes[i] = each
		}
	case laceXiph:
		pos, err = readXiphSizes(w, pos, end, sizes)
	case laceEBML:
		pos, err = readEBMLSizes(w, pos, end, sizes)
	}
	if err != nil {
		return bh, err
	}

	// The last frame runs from the table's end to the block's end; the earlier
	// sizes must not overrun that span.
	used := int64(0)
	for i := 0; i < n-1; i++ {
		if sizes[i] < 0 {
			return bh, malformed("negative lace size")
		}
		used += int64(sizes[i])
	}
	if pos+used > end {
		return bh, malformed("laced frame sizes overrun the block")
	}
	sizes[n-1] = int(end - pos - used)

	bh.frames = make([]frameLoc, n)
	off := pos
	for i, s := range sizes {
		bh.frames[i] = frameLoc{off: off, size: s}
		off += int64(s)
	}
	return bh, nil
}

// readXiphSizes fills all but the last of sizes from a Xiph lacing table
// (each size is a run of 0xFF bytes plus a final byte below 255) and returns
// the offset just past the table.
func readXiphSizes(w *srcwin.Window, pos, end int64, sizes []int) (int64, error) {
	for i := 0; i < len(sizes)-1; i++ {
		total := 0
		for {
			if pos >= end {
				return 0, malformed("xiph lace size runs past block")
			}
			b := w.BytesAt(pos, 1)
			if len(b) < 1 {
				return 0, readErr(w, "xiph lace size")
			}
			pos++
			total += int(b[0])
			if b[0] < 255 {
				break
			}
		}
		sizes[i] = total
	}
	return pos, nil
}

// readEBMLSizes fills all but the last of sizes from an EBML lacing table: the
// first size is an unsigned VINT, each following is a signed VINT difference
// from its predecessor. It returns the offset just past the table.
func readEBMLSizes(w *srcwin.Window, pos, end int64, sizes []int) (int64, error) {
	prev := int64(0)
	for i := 0; i < len(sizes)-1; i++ {
		avail := int(min(8, end-pos))
		b := w.BytesAt(pos, avail)
		if len(b) == 0 {
			return 0, readErr(w, "ebml lace size")
		}
		val, vlen, _, ok := parseVint(b, false)
		if !ok {
			return 0, malformed("ebml lace size is not a valid vint")
		}
		pos += int64(vlen)
		if i == 0 {
			prev = int64(val)
		} else {
			// The stored value is biased by half its range to encode a signed
			// difference (EBML lacing, Matroska spec section on Block Lacing).
			bias := int64(1)<<(7*vlen-1) - 1
			prev += int64(val) - bias
		}
		if prev < 0 {
			return 0, malformed("ebml lace produced a negative frame size")
		}
		sizes[i] = int(prev)
	}
	return pos, nil
}

// readErr reports the window's sticky read failure, or an unexpected-EOF-style
// malformed error when the window simply ran short of data.
func readErr(w *srcwin.Window, what string) error {
	if err := w.Err(); err != nil {
		return err
	}
	return waxerr.New(waxerr.CodeUnsupportedFormat, "mka: "+what+" runs past end of data")
}
