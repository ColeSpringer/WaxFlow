package wavpack

import (
	"bytes"
	"encoding/binary"
)

// The block checksum is the reference's integrity field over a block's own
// coded bytes: a fold stored in an ID_BLOCK_CHECKSUM sub-block and flagged in
// the header. It catches damage the header CRC cannot, since that one covers
// the samples that come back out, and it is what `wvunpack -v` refuses a block
// over. Nothing in the decode path checks it, which is where libwavpack keeps
// it too: verifying is a caller's to ask for.

// checksumBytes is the payload width this package writes. The reference also
// writes a narrower two-byte form, for hybrid and multichannel streams, so
// both are read and restated.
const checksumBytes = 4

// blockChecksum folds b as little-endian 16-bit words, the reference's own
// arithmetic. Callers pass the bytes ahead of the checksum sub-block's header,
// which is what the reference covers: the block header, so a patched length
// invalidates it, and every sub-block before this one.
func blockChecksum(b []byte) uint32 {
	sum := uint32(0xffffffff)
	for i := 0; i+1 < len(b); i += 2 {
		sum = sum*3 + uint32(b[i]) + uint32(b[i+1])<<8
	}
	return sum
}

// checksumOffset reports where a block's stored checksum sits and how wide it
// is, or (-1, 0) when the block carries none in a form the reference would
// verify: an odd size, or a width other than two or four, is left alone rather
// than rewritten into something the reference reads differently.
//
// The walk is bounded by the block's own declared length and keeps the first
// match, so a caller holding more than one block cannot have a later block's
// bytes taken for this one's checksum.
func checksumOffset(block []byte) (off, width int) {
	h, err := ParseBlockHeader(block)
	if err != nil || h.Size > int64(len(block)) {
		return -1, 0
	}
	off, width = -1, 0
	// The walk fails only when its callback does, and this one cannot.
	_ = walkMeta(block[:h.Size], func(id byte, data []byte, at int) error {
		if n := len(data); off < 0 && id == idBlockChecksum && (n == 2 || n == 4) {
			off, width = at, n
		}
		return nil
	})
	return off, width
}

// checksumFor renders the value the payload at off must hold, in the width it
// has: the fold of everything ahead of the sub-block's own two-byte header, or
// for the narrow form that value folded onto itself. The two bytes before the
// payload are excluded whatever the width of the header they fall in, which is
// what the reference folds.
func checksumFor(block []byte, off, width int) []byte {
	sum := blockChecksum(block[:off-2])
	if width == 4 {
		return binary.LittleEndian.AppendUint32(nil, sum)
	}
	return binary.LittleEndian.AppendUint16(nil, uint16(sum^(sum>>16)))
}

// writeChecksum stores the block's own checksum in the payload at off.
func writeChecksum(block []byte, off, width int) {
	copy(block[off:], checksumFor(block, off, width))
}

// UpdateBlockChecksum recomputes the checksum a block stores over its own
// bytes and reports whether it found one to update. A caller that edits a
// finished block's header must call this before the block goes out, since the
// fold covers the header.
func UpdateBlockChecksum(block []byte) bool {
	off, width := checksumOffset(block)
	if off < 0 {
		return false
	}
	writeChecksum(block, off, width)
	return true
}

// VerifyBlockChecksum reports whether the checksum a block stores matches the
// bytes it covers, and whether the block carries one at all. A block that
// carries none is not damaged: streams below version 0x410 have no such
// sub-block, and neither did anything written before WavPack 5.
func VerifyBlockChecksum(block []byte) (ok, present bool) {
	off, width := checksumOffset(block)
	if off < 0 {
		return false, false
	}
	return bytes.Equal(block[off:off+width], checksumFor(block, off, width)), true
}

// SetTotalSamples rewrites a finished block's stream-length field and restates
// the checksum covering it. It is the muxer's seam: no encoder knows the
// length while it is still encoding, so every block carries the escape and the
// first one, which is the copy readers consult, is corrected on the way past
// and again once the count is final.
//
// A negative total, or one past what the field can state, writes the escape
// every reader must handle anyway. A block carrying no checksum needs none
// restated: streams below version 0x410 have none, and a form this package
// declines to rewrite is one the reference declines to verify.
func SetTotalSamples(block []byte, total int64) error {
	h, err := ParseBlockHeader(block)
	if err != nil {
		return err
	}
	if h.Size > int64(len(block)) {
		return malformed("block declares %d bytes, holding %d", h.Size, len(block))
	}
	if total > MaxSamples {
		total = -1
	}
	putTotal(block[11:16], total)
	UpdateBlockChecksum(block)
	return nil
}

// putTotal renders the five-byte total-samples field: its high byte followed
// by its low word, the order they sit in at bytes 11 through 15 of a block
// header. A negative total writes the all-ones escape.
//
// The halves are not simply the high and low words of the count. The reference
// skips every value whose low word would collide with the escape and
// ParseBlockHeader subtracts the high byte back off, which is what keeps a
// length of exactly 2^32-1 distinguishable from "unknown".
func putTotal(field []byte, total int64) {
	if total < 0 {
		field[0] = 0
		binary.LittleEndian.PutUint32(field[1:], 0xffffffff)
		return
	}
	total += total / 0xffffffff
	field[0] = byte(total >> 32)
	binary.LittleEndian.PutUint32(field[1:], uint32(total))
}
