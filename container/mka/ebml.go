package mka

import (
	"encoding/binary"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// vintWidth returns the byte length an EBML variable-length integer beginning
// with first occupies: one plus the number of leading zero bits. A zero first
// byte would need a width past eight (EBML's ceiling), so it returns 0 to mark
// the encoding invalid.
func vintWidth(first byte) int {
	for i := range 8 {
		if first&(0x80>>i) != 0 {
			return i + 1
		}
	}
	return 0
}

// parseVint decodes one variable-length integer at the start of b. For an
// element ID keepMarker is true and the returned value includes the
// length-descriptor bits (IDs are compared as their on-wire form); for a size
// or value it is false and the marker is stripped. unknown reports the
// all-ones size that marks an element running to the end of its parent. ok is
// false when b is too short for the encoded width.
func parseVint(b []byte, keepMarker bool) (val uint64, n int, unknown bool, ok bool) {
	if len(b) == 0 {
		return 0, 0, false, false
	}
	w := vintWidth(b[0])
	if w == 0 || w > len(b) {
		return 0, 0, false, false
	}
	if keepMarker {
		return beUint(b[:w]), w, false, true
	}
	// Strip the marker bit from the first byte; the value is the remaining
	// 7*w - (w-1) bits. Track whether every value bit is set (unknown size).
	first := b[0] & (0xFF >> w)
	allOnes := first == (0xFF >> w)
	val = uint64(first)
	for _, x := range b[1:w] {
		val = val<<8 | uint64(x)
		if x != 0xFF {
			allOnes = false
		}
	}
	return val, w, allOnes, true
}

// element is one parsed EBML element header: its ID, the byte offset of its
// data, and the data length (unknownSize marks the all-ones size). size is -1
// when unknownSize is set. clamped marks an element whose declared size runs
// past the end it was read against, over bytes past it; its size is then cut
// to that end.
type element struct {
	id          uint32
	dataOff     int64
	size        int64
	unknownSize bool
	clamped     bool
	over        int64
}

// dataEnd is the offset one past the element's data, valid only for a
// known-size element.
func (e element) dataEnd() int64 { return e.dataOff + e.size }

// readElement reads the element header at off from src, bounded by end. It
// validates the ID and size vints and that a known-size element's data fits
// within end before returning. The progress guarantee holds: dataOff is
// always strictly greater than off (a header is at least two bytes), so a
// caller advancing to dataEnd makes progress.
func (d *Demuxer) readElement(off, end int64) (element, error) {
	e, _, err := d.readHeader(off, end, false)
	return e, err
}

// readHeader is readElement with two answers more. With clamp set, a known
// size that runs past end comes back clamped to it rather than refused. short
// says end splits the header itself, so the bytes ran out rather than being
// wrong.
func (d *Demuxer) readHeader(off, end int64, clamp bool) (element, bool, error) {
	if off+2 > end {
		return element{}, true, malformed("element header at %d runs past %d", d.log.At(off), d.log.At(end))
	}
	var hdr [12]byte
	n := min(int64(len(hdr)), end-off)
	if err := container.ReadFull(d.src, hdr[:n], off); err != nil {
		return element{}, false, waxerr.Wrap(waxerr.CodeSourceUnreadable, "mka: reading element header", err)
	}
	return d.parseHeader(hdr[:n], off, end, clamp)
}

// parseHeader is readHeader's parse of the header at off, from buf, which
// holds the min(12, end-off) bytes there.
func (d *Demuxer) parseHeader(buf []byte, off, end int64, clamp bool) (element, bool, error) {
	// An ID is at most four bytes; a wider one is no ID, however many bytes
	// follow. Twelve bytes always hold both vints, so a vint that does not fit
	// is one end cut short.
	if w := vintWidth(buf[0]); w == 0 || w > 4 {
		return element{}, false, malformed("invalid element ID at %d", d.log.At(off))
	} else if w > len(buf) {
		return element{}, true, malformed("invalid element ID at %d", d.log.At(off))
	}
	id64, idLen, _, _ := parseVint(buf, true)
	rest := buf[idLen:]
	if len(rest) == 0 {
		return element{}, true, malformed("invalid element size at %d", d.log.At(off))
	}
	if w := vintWidth(rest[0]); w == 0 {
		return element{}, false, malformed("invalid element size at %d", d.log.At(off))
	} else if w > len(rest) {
		return element{}, true, malformed("invalid element size at %d", d.log.At(off))
	}
	size, sizeLen, unknown, _ := parseVint(rest, false)
	e := element{id: uint32(id64), dataOff: off + int64(idLen+sizeLen)}
	if unknown {
		e.unknownSize = true
		e.size = -1
		return e, false, nil
	}
	// size > end-dataOff rather than dataOff+size > end: a near-2^63 size
	// would overflow the sum and slip past the guard. dataOff <= end here,
	// and a size is below 2^56, so over cannot wrap.
	if int64(size) > end-e.dataOff {
		if !clamp {
			return element{}, false, malformed("element %#x at %d (size %d) runs past %d", e.id, d.log.At(off), size, d.log.At(end))
		}
		e.clamped, e.over = true, int64(size)-(end-e.dataOff)
		e.size = end - e.dataOff
		return e, false, nil
	}
	e.size = int64(size)
	return e, false, nil
}

// childKind is what readChild found at an offset.
type childKind int

const (
	childOK   childKind = iota // an element, clamped where the file ends inside it
	childCut                   // the file's end splits the element's header
	childJunk                  // bytes that head no element that fits
)

// readChild reads a Segment or Cluster child's header. Where the Segment's
// end is the file's (it states no size, or one past the file), an element the
// file ends inside is not damage of its own: one whose header the end splits
// is childCut, and one whose data runs past it comes back clamped. Anything
// else readElement refuses is childJunk. Only a read that fails is an error.
func (d *Demuxer) readChild(off, end int64) (element, childKind, error) {
	atEOF := d.openEnd && end == d.size
	e, short, err := d.readHeader(off, end, atEOF)
	return classifyChild(e, short, err, atEOF)
}

// classifyChild sorts a header read into readChild's answers.
func classifyChild(e element, short bool, err error, atEOF bool) (element, childKind, error) {
	switch {
	case err == nil:
		return e, childOK, nil
	case waxerr.CodeOf(err) == waxerr.CodeSourceUnreadable:
		return element{}, childOK, err
	case short && atEOF:
		return element{}, childCut, nil
	}
	return element{}, childJunk, nil
}

// maxJunk is how far a resync looks past bytes that head no element, and so
// the longest run of them it steps over. waxlabel and ffmpeg look as far, so
// the readers agree on where a damaged file's audio ends.
const maxJunk = 64 << 10

// resync returns where the element after the junk at off begins: the first
// offset past off, at most maxJunk on, where a Segment-level ID heads an
// element that fits end. Only a Cluster may state no size, since a false ID
// in junk followed by an all-ones byte would otherwise swallow the rest. Where
// the Segment ends with the file, any ID followed by a large size reads as
// cut short, so such a candidate is taken at once only when it is a Cluster
// whose first child is a cluster's, and otherwise only when no candidate in
// the window fits. It returns end when no candidate stands before end, and
// -1 when maxJunk bytes pass first. The bytes are read in spans that grow from 1 KiB, so a
// short run costs a short read, and a candidate's header is parsed from them
// where they hold it.
func (d *Demuxer) resync(off, end int64) (int64, error) {
	avail := end - off
	want := min(avail, maxJunk+4)
	span := min(want, 1<<10)
	i := int64(1)
	clamped := int64(-1) // the first candidate the file's end cuts short
	for {
		if have := int64(len(d.junkBuf)); have < span {
			d.junkBuf = append(d.junkBuf, make([]byte, span-have)...)
			if err := container.ReadFull(d.src, d.junkBuf[have:], off+have); err != nil {
				d.junkBuf = d.junkBuf[:0]
				return -1, waxerr.Wrap(waxerr.CodeSourceUnreadable, "mka: reading past damaged bytes", err)
			}
		}
		for ; i+4 <= span; i++ {
			if !isSegmentLevel(binary.BigEndian.Uint32(d.junkBuf[i:])) {
				continue
			}
			e, kind, err := d.junkChild(off, off+i, end)
			if err != nil {
				d.junkBuf = d.junkBuf[:0]
				return -1, err
			}
			if kind != childOK || (e.unknownSize && e.id != idCluster) {
				continue
			}
			take := !e.clamped
			if !take && e.id == idCluster {
				// A cluster the end cuts short is real when its first child
				// is one a cluster holds; a false ID in junk is followed by
				// more junk.
				c, ck, err := d.junkChild(off, e.dataOff, end)
				if err != nil {
					d.junkBuf = d.junkBuf[:0]
					return -1, err
				}
				take = e.dataOff < end && ck == childOK && isClusterChild(c.id)
			}
			if take {
				d.junkBuf = d.junkBuf[:0]
				return off + i, nil
			}
			if clamped < 0 {
				clamped = off + i
			}
		}
		if span == want {
			break
		}
		span = min(span*2, want)
	}
	d.junkBuf = d.junkBuf[:0]
	switch {
	case clamped >= 0:
		return clamped, nil
	case want < avail:
		return -1, nil
	}
	return end, nil
}

// junkChild reads the header at pos for resync, from the bytes it has
// scanned from off where they hold it.
func (d *Demuxer) junkChild(off, pos, end int64) (element, childKind, error) {
	i, have := pos-off, int64(len(d.junkBuf))
	if i >= 0 && pos+2 <= end && (i+12 <= have || off+have == end) {
		atEOF := d.openEnd && end == d.size
		h, short, err := d.parseHeader(d.junkBuf[i:min(i+12, have)], pos, end, atEOF)
		return classifyChild(h, short, err, atEOF)
	}
	return d.readChild(pos, end)
}

// isClusterChild reports whether id is one a Cluster holds.
func isClusterChild(id uint32) bool {
	switch id {
	case idTimestamp, idSimpleBlock, idBlockGroup, idVoid, idCRC32, idPosition, idPrevSize:
		return true
	}
	return false
}

// walkElements iterates the child elements packed in buf, calling fn with each
// child's ID and data slice. It enforces the progress guarantee and validates
// every size against the remaining bytes before slicing. An unknown-size child
// (or one whose size overruns) runs to the end of buf, the in-memory analog of
// a master element with no declared length. Trailing bytes too short for a
// header are tolerated as padding.
func walkElements(buf []byte, fn func(id uint32, data []byte) error) error {
	for len(buf) >= 2 {
		id64, idLen, _, ok := parseVint(buf, true)
		if !ok || id64 > 0xFFFFFFFF {
			return malformed("invalid element ID in master body")
		}
		size, sizeLen, unknown, ok := parseVint(buf[idLen:], false)
		if !ok {
			return malformed("invalid element size in master body")
		}
		hdr := idLen + sizeLen
		body := buf[hdr:]
		// Clamp to the remaining bytes with an unsigned compare: a bare
		// int(size) would wrap negative on a 32-bit build when a crafted size
		// has its low 32 bits set, panicking the slice below.
		n := len(body)
		if !unknown && size < uint64(n) {
			n = int(size)
		}
		if err := fn(uint32(id64), body[:n]); err != nil {
			return err
		}
		buf = body[n:]
	}
	return nil
}
