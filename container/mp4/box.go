package mp4

import (
	"encoding/binary"

	"github.com/colespringer/waxflow/container"
)

// box is one parsed box header plus its payload offset within a parent
// buffer or the source. size is the whole box length including the header.
type box struct {
	typ    string
	off    int64 // box start
	hdrLen int64 // 8, or 16 with a 64-bit largesize
	size   int64 // whole box size; a size-0 box was resolved to reach end
	toEnd  bool  // the box declared size 0 (extends to end of its container)
}

// payloadOff is the offset where the box body begins.
func (b box) payloadOff() int64 { return b.off + b.hdrLen }

// payloadLen is the box body length in bytes.
func (b box) payloadLen() int64 { return b.size - b.hdrLen }

// readBox parses a box header at off from the source, bounded by end. A
// size-0 box resolves to reach end (the top-level mdat convention). The
// progress guarantee holds: a returned box always has size >= hdrLen >= 8,
// so callers advancing by size make progress.
func readBox(src container.Source, off, end int64) (box, error) {
	if off+8 > end {
		return box{}, malformed("box header at %d runs past end", off)
	}
	var hdr [16]byte
	if err := container.ReadFull(src, hdr[:8], off); err != nil {
		return box{}, err
	}
	b := box{typ: string(hdr[4:8]), off: off, hdrLen: 8}
	size := int64(be32(hdr[:]))
	switch size {
	case 1:
		if off+16 > end {
			return box{}, malformed("64-bit box size at %d runs past end", off)
		}
		if err := container.ReadFull(src, hdr[8:16], off+8); err != nil {
			return box{}, err
		}
		size = int64(be64(hdr[8:]))
		b.hdrLen = 16
	case 0:
		size = end - off
		b.toEnd = true
	}
	if size < b.hdrLen {
		return box{}, malformed("box %q size %d smaller than its header", b.typ, size)
	}
	// size > end-off, not off+size > end: a 64-bit largesize near 2^63 would
	// overflow the sum and slip past the guard. off <= end here (the header
	// reads above bounded it), so end-off does not underflow.
	if size > end-off {
		return box{}, malformed("box %q at %d (size %d) runs past end %d", b.typ, off, size, end)
	}
	b.size = size
	return b, nil
}

// walkBoxes iterates the child boxes packed in body, calling fn for each
// with its type and payload slice. It enforces the progress guarantee (a
// box shorter than its header ends the walk with an error) and validates
// every size against the remaining bytes before slicing. A size-0 child
// runs to the end of body.
func walkBoxes(body []byte, fn func(typ string, payload []byte) error) error {
	for len(body) >= 8 {
		size := int64(be32(body))
		typ := string(body[4:8])
		hdr := int64(8)
		switch size {
		case 1:
			if len(body) < 16 {
				return malformed("64-bit box %q size truncated", typ)
			}
			size = int64(be64(body[8:16]))
			hdr = 16
		case 0:
			size = int64(len(body))
		}
		if size < hdr {
			return malformed("box %q size %d smaller than its header", typ, size)
		}
		if size > int64(len(body)) {
			return malformed("box %q size %d exceeds %d remaining bytes", typ, size, len(body))
		}
		if err := fn(typ, body[hdr:size]); err != nil {
			return err
		}
		body = body[size:]
	}
	// Any trailing bytes too short for a box header are tolerated padding at
	// the tail of a container, not a hard error (real files pad).
	return nil
}

// fullBox splits a FullBox payload into its version byte and the content
// after the 4-byte version+flags prefix. It returns ok=false when the
// payload is too short to carry the prefix.
func fullBox(payload []byte) (version byte, flags uint32, rest []byte, ok bool) {
	if len(payload) < 4 {
		return 0, 0, nil, false
	}
	return payload[0], be32(payload[:4]) & 0xFFFFFF, payload[4:], true
}

// makeBox assembles a box: a 4-byte size, the 4-byte type, and the
// concatenated parts. Every box the muxer writes fits in 32 bits (fragments
// are bounded), so no 64-bit largesize form is needed.
func makeBox(typ string, parts ...[]byte) []byte {
	n := 8
	for _, p := range parts {
		n += len(p)
	}
	b := make([]byte, 8, n)
	binary.BigEndian.PutUint32(b, uint32(n))
	copy(b[4:], typ)
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

// makeFullBox assembles a FullBox: the version byte and 24-bit flags precede
// the body.
func makeFullBox(typ string, version byte, flags uint32, parts ...[]byte) []byte {
	head := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return makeBox(typ, append([][]byte{head}, parts...)...)
}

// u16, u32, u64 render big-endian integer fields for box assembly.
func u16(v uint16) []byte { return []byte{byte(v >> 8), byte(v)} }

func u32(v uint32) []byte {
	return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}

func u64(v uint64) []byte {
	return []byte{byte(v >> 56), byte(v >> 48), byte(v >> 40), byte(v >> 32),
		byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}
