package mka

import (
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// vintWidth returns the byte length an EBML variable-length integer beginning
// with first occupies: one plus the number of leading zero bits. A zero first
// byte would need a width past eight (EBML's ceiling), so it returns 0 to mark
// the encoding invalid.
func vintWidth(first byte) int {
	for i := 0; i < 8; i++ {
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
// when unknownSize is set.
type element struct {
	id          uint32
	dataOff     int64
	size        int64
	unknownSize bool
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
	if off+2 > end {
		return element{}, malformed("element header at %d runs past %d", off, end)
	}
	var hdr [12]byte
	n := int64(len(hdr))
	if off+n > end {
		n = end - off
	}
	if err := container.ReadFull(d.src, hdr[:n], off); err != nil {
		return element{}, waxerr.Wrap(waxerr.CodeSourceUnreadable, "mka: reading element header", err)
	}
	buf := hdr[:n]
	id64, idLen, _, ok := parseVint(buf, true)
	if !ok || id64 > 0xFFFFFFFF {
		return element{}, malformed("invalid element ID at %d", off)
	}
	size, sizeLen, unknown, ok := parseVint(buf[idLen:], false)
	if !ok {
		return element{}, malformed("invalid element size at %d", off)
	}
	e := element{id: uint32(id64), dataOff: off + int64(idLen+sizeLen)}
	if unknown {
		e.unknownSize = true
		e.size = -1
		return e, nil
	}
	// size > end-dataOff rather than dataOff+size > end: a near-2^63 size
	// would overflow the sum and slip past the guard. dataOff <= end here.
	if e.dataOff > end || int64(size) > end-e.dataOff {
		return element{}, malformed("element %#x at %d (size %d) runs past %d", e.id, off, size, end)
	}
	e.size = int64(size)
	return e, nil
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
