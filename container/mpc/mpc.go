// Package mpc demuxes Musepack streams, both framings the format has had: the
// SV7 stream mppenc writes (a 28-byte header, then bit-packed frames each
// preceded by its 20-bit length, as little-endian words read MSB-first) and
// the SV8 packet stream mpcenc writes ("MPCK", then two-letter keyed packets:
// the stream header, replay gain, encoder info, a seek table, chapters, audio
// blocks of 4^n frames, an end marker). Either may carry an APEv2 tag after
// the audio and an ID3v2 tag in front.
//
// SV7 frames are realigned here into byte-aligned packets, one frame each, so
// the codec reads one bit order; SV8 blocks pass through whole. Neither
// version's frames are sync points on their own (SV7 chains scalefactors
// across frames forever, both versions carry a noise generator no frame
// resets), so a seek landing delivers the decoder state in front of its first
// packet, scanned from the stream and cached through container.Indexer.
//
// The package is named mpc after the file extension rather than musepack
// because the registry imports codec/musepack alongside it (the flacn
// precedent). The public container name and error prefix are "musepack".
package mpc

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"

	"github.com/colespringer/waxflow/codec/musepack"
)

// MatchNeed is the sniff window Match wants.
const MatchNeed = musepack.MatchNeed

// Match reports whether head begins with a Musepack stream. It is the format
// sniff-table entry.
func Match(head []byte) bool { return musepack.Match(head) }

// Hostile-input caps (ADR-0005 invariants), sized to the legitimate maximum
// rather than to the reference's buffer hints.
const (
	// maxFrameBits bounds one frame. The syntax's worst case is every band of
	// both channels at the widest raw resolution: a 13-bit escaped resolution
	// code, a mid/side bit, a 3-bit scalefactor pattern, three escaped
	// scalefactors of 10 bits and 36 samples of 16 bits per band and channel,
	// 623 bits times 64, just under 40000. The reference's own 4352-byte frame
	// buffer is smaller than that, which says how far real frames sit below it.
	maxFrameBits = 40960
	// maxBlockFrames is the most frames an SV8 block can hold: the header's
	// exponent runs to 14, and `mpcenc --num_frames 7` writes exactly that,
	// which is a 7.1-minute block at 44.1 kHz.
	maxBlockFrames = 1 << musepack.MaxBlockPwr
	// maxBlockBytes bounds one SV8 audio block: the frame maximum times the
	// block maximum. A block is buffered whole, so the bound is the legitimate
	// worst case rather than a guess.
	maxBlockBytes = maxBlockFrames * (maxFrameBits / 8)
	// maxHeaderPacket bounds a non-audio packet, the reference's own limit on
	// what it will stage before the first audio block.
	maxHeaderPacket = 61173
	// maxSeekEntriesKept is a retention cap, not a refusal: the reference
	// halves its seek table's density until it fits this many entries, and so
	// does the block index here.
	maxSeekEntriesKept = 65536
	// maxChapters bounds the chapter list.
	maxChapters = 1024
	// maxJunk bounds the scan for the magic behind a leading ID3v2 tag. The
	// reference reads the magic at the tag's end and nowhere else; the scan is
	// tolerance for a tagger that left padding.
	maxJunk = 1 << 20
	// checkpointFrames is the SV7 index granularity: the hop walk records
	// every this-many-th frame's position, and the seek scanner keeps the
	// decoder state at the same points.
	checkpointFrames = 32
	// synthWarmup is how many samples before a target a landing must sit so
	// the synthesis filter's cold history is fully inside the discarded
	// pre-roll: the filter reads 16 blocks of 64 V values, 512 output samples.
	synthWarmup = 512
)

// FindStart returns the offset of the first stream start window holds, each
// confirmed the way the demuxer reads it: "MPCK" with the stream header
// packet behind it, whose checksum holds, or an SV7 magic whose header states
// frames, a band count and a last-frame length the format allows and whose
// first two frame lengths are ones a frame can have. It is how a stream
// behind stray bytes is told from the same letters in them.
func FindStart(window []byte) (int, bool) {
	for i := 0; ; i++ {
		j := bytes.Index(window[i:], []byte("MP"))
		if j < 0 {
			return 0, false
		}
		i += j
		if b := window[i:]; len(b) >= 4 && (string(b[:4]) == "MPCK" && startsSV8(b[4:]) || string(b[:3]) == "MP+" && startsSV7(b)) {
			return i, true
		}
	}
}

// startsSV8 reports whether p, the bytes behind the magic, open with a
// stream header packet whose checksum holds.
func startsSV8(p []byte) bool {
	if len(p) < 3 || string(p[:2]) != "SH" {
		return false
	}
	size, n, ok := musepack.ReadVarint(p[2:])
	if !ok || size < uint64(2+n+4) || size > maxHeaderPacket || size > uint64(len(p)) {
		return false
	}
	payload := p[2+n : size]
	return binary.BigEndian.Uint32(payload) == crc32.ChecksumIEEE(payload[4:])
}

// startsSV7 reports whether b opens with an SV7 header and frames behind it.
func startsSV7(b []byte) bool {
	// The stream version, alone or under the one flag the byte carries.
	if len(b) < sv7HeaderLen || b[3] != 0x07 && b[3] != 0x17 {
		return false
	}
	frames := binary.LittleEndian.Uint32(b[4:])
	maxBand := binary.LittleEndian.Uint32(b[8:]) >> 24 & 0x3F
	last := binary.LittleEndian.Uint32(b[20:]) >> 20 & 0x7FF
	if frames == 0 || maxBand < 1 || maxBand > musepack.MaxBands-1 || last > musepack.FrameLength {
		return false
	}
	pos := int64(sv7FirstFrame)
	for range min(frames, 2) {
		n, ok := sv7BitsIn(b, pos, 20)
		if !ok || n < minFrameBits || n > maxFrameBits {
			return false
		}
		pos += 20 + int64(n)
	}
	return true
}

// sv7BitsIn reads n <= 32 bits at bit position pos of the word-swapped stream
// b holds from its magic, or false when b does not hold the words they lie
// in.
func sv7BitsIn(b []byte, pos int64, n int) (uint32, bool) {
	m0 := pos >> 3
	w0 := m0 &^ 3
	if w0+8 > int64(len(b)) {
		return 0, false
	}
	var acc uint64
	for j := range int64(5) {
		acc = acc<<8 | uint64(b[w0+(m0+j-w0)^3])
	}
	return uint32(acc << (24 + pos&7) >> (64 - n)), true
}
