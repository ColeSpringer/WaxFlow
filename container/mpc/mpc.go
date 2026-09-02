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

import "github.com/colespringer/waxflow/codec/musepack"

// MatchNeed is the sniff window Match wants.
const MatchNeed = musepack.MatchNeed

// Match reports whether head begins with a Musepack stream. It is the format
// sniff-table entry.
func Match(head []byte) bool { return musepack.Match(head) }

// DemuxerOptions configures parsing.
type DemuxerOptions struct {
	// Strict turns tolerated damage (the Warnings list) into errors.
	Strict bool
}

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
	// maxWarnings caps the tolerated-damage list.
	maxWarnings = 64
	// checkpointFrames is the SV7 index granularity: the hop walk records
	// every this-many-th frame's position, and the seek scanner keeps the
	// decoder state at the same points.
	checkpointFrames = 32
	// synthWarmup is how many samples before a target a landing must sit so
	// the synthesis filter's cold history is fully inside the discarded
	// pre-roll: the filter reads 16 blocks of 64 V values, 512 output samples.
	synthWarmup = 512
)
