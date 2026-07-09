// Package mka demuxes Matroska and WebM (ISO/IEC 14496 EBML) audio: the
// .mka/.mkv/.webm family carrying Opus, Vorbis, FLAC, AAC-LC, or PCM. It
// parses the Segment's SeekHead, Info, and Tracks, selects the audio track,
// and streams block frames from the clusters as codec packets.
//
// Seeking uses a frame-counted cluster index rather than the container's
// Cues: a block timestamp is stored at millisecond granularity and so cannot
// name a sample position, whereas format.Media's decode-and-discard pre-roll
// needs the exact decoder-output position of the cluster it restarts on. The
// index (offset to cumulative sample count) is built by walking the blocks
// once, sample-exact; Cues and other metadata elements are tolerated and
// skipped. That same walk yields the exact decoder-output total gapless needs.
//
// Gapless trims come from the track's CodecDelay (front) and the last
// block's DiscardPadding (end), mapped onto Track.Delay/Padding so
// format.Media delivers the trimmed timeline. These are the Opus-in-WebM
// gapless mechanism; other codecs rarely signal them.
//
// EBML is a nesting attack surface, so the parser holds to the hostile-input
// invariants: a fixed, shallow descent that never recurses on attacker-chosen
// depth (each master element is walked by a specific parser, and unknown
// masters are treated as leaves, not descended), every element size validated
// against remaining input before any allocation, table and frame-count caps,
// and a progress guarantee on every parse loop. The Segment header (SeekHead,
// Info, Tracks) is read into bounded buffers and parsed from memory; cluster
// block data stays on disk and only the bytes a packet needs are read.
package mka

import (
	"fmt"
	"math"

	"github.com/colespringer/waxflow/waxerr"
)

// Hostile-input caps.
const (
	// maxHeaderElement bounds an in-memory master element (SeekHead, Info,
	// Tracks, Cues). Cues for a day-long file are a few MiB (one entry per
	// cluster); cover art does not live here, so this is generous.
	maxHeaderElement = 64 << 20
	// maxTracks bounds the track count parsed from one segment.
	maxTracks = 1 << 10
	// maxCodecPrivate bounds a CodecPrivate blob (setup headers, magic
	// cookies): larger than any real one, smaller than a memory hazard.
	maxCodecPrivate = 1 << 20
	// maxLaceFrames is the hard ceiling on frames in one laced block: the
	// lace count field is a byte, so a block never holds more than 256.
	maxLaceFrames = 256
	// maxBlockData bounds one block's payload before it is split into frames.
	maxBlockData = 1 << 26
	// maxTopLevelElements bounds the Segment-child scan that finds the first
	// cluster, so a stream of tiny void elements cannot spin the header walk.
	maxTopLevelElements = 1 << 20
	// maxClusters bounds the seek index so a file of many tiny clusters cannot
	// grow it without limit. A real file clusters every few seconds, so this
	// clears months of audio; past it, seeks land on the last indexed cluster
	// and format.Media pre-rolls the rest.
	maxClusters = 1 << 20
	// maxFrames bounds the whole-stream frame walk (the seek index and gapless
	// raw-total pass), the backstop behind the segment-size bound. It clears a
	// day of the finest Opus frames (2.5 ms) with headroom.
	maxFrames = 1 << 26
)

// Matroska element IDs, as they appear on the wire (the length-descriptor
// marker bits are part of the ID). IDs are 1 to 4 bytes, so they fit in a
// uint32.
const (
	idEBML    = 0x1A45DFA3
	idDocType = 0x4282
	idSegment = 0x18538067

	idSeekHead     = 0x114D9B74
	idSeek         = 0x4DBB
	idSeekID       = 0x53AB
	idSeekPosition = 0x53AC

	idInfo           = 0x1549A966
	idTimestampScale = 0x2AD7B1
	idDuration       = 0x4489

	idTracks       = 0x1654AE6B
	idTrackEntry   = 0xAE
	idTrackNumber  = 0xD7
	idTrackType    = 0x83
	idFlagDefault  = 0x88
	idCodecID      = 0x86
	idCodecPrivate = 0x63A2
	idCodecDelay   = 0x56AA
	idSeekPreRoll  = 0x56BB
	idAudio        = 0xE1
	idSamplingFreq = 0xB5
	idChannels     = 0x9F
	idBitDepth     = 0x6264

	idCues = 0x1C53BB6B // parsed only as a top-level boundary; see the doc

	idCluster        = 0x1F43B675
	idTimestamp      = 0xE7 // Cluster Timestamp (a.k.a. Timecode)
	idSimpleBlock    = 0xA3
	idBlockGroup     = 0xA0
	idBlock          = 0xA1
	idDiscardPadding = 0x75A2
)

// trackTypeAudio is the TrackType value for an audio track.
const trackTypeAudio = 2

// defaultTimestampScale is the TimestampScale Matroska assumes when the Info
// element omits it: one million nanoseconds (1 ms) per tick.
const defaultTimestampScale = 1_000_000

// ebmlMagic is the four-byte EBML signature every Matroska/WebM file opens
// with; Match keys on it.
var ebmlMagic = [4]byte{0x1A, 0x45, 0xDF, 0xA3}

// Match reports whether head begins with the EBML signature. DocType
// (matroska/webm) is validated at parse time; the four magic bytes are the
// sniff-table gate.
func Match(head []byte) bool {
	return len(head) >= 4 &&
		head[0] == ebmlMagic[0] && head[1] == ebmlMagic[1] &&
		head[2] == ebmlMagic[2] && head[3] == ebmlMagic[3]
}

// MatchNeed is how many leading bytes Match inspects.
const MatchNeed = 4

func malformed(format string, args ...any) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, "mka: "+fmt.Sprintf(format, args...))
}

// beUint reads a big-endian unsigned integer from a 0-to-8-byte field.
func beUint(b []byte) uint64 {
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}

// beInt reads a big-endian two's-complement signed integer, sign-extended
// from its width. DiscardPadding is signed.
func beInt(b []byte) int64 {
	if len(b) == 0 {
		return 0
	}
	v := int64(0)
	if b[0]&0x80 != 0 {
		v = -1
	}
	for _, x := range b {
		v = v<<8 | int64(x)
	}
	return v
}

// beFloat reads an EBML float: a 4- or 8-byte IEEE 754 value, or 0 for the
// empty (default) encoding. ok is false for any other width.
func beFloat(b []byte) (v float64, ok bool) {
	switch len(b) {
	case 0:
		return 0, true
	case 4:
		return float64(math.Float32frombits(uint32(beUint(b)))), true
	case 8:
		return math.Float64frombits(beUint(b)), true
	default:
		return 0, false
	}
}

// nsToSamples converts a nanosecond duration to a sample count at rate,
// rounded to nearest. Gapless trims (CodecDelay, DiscardPadding) round-trip
// through nanoseconds, and round-to-nearest recovers the exact sample count
// a muxer wrote (312 samples at 48 kHz is 6_500_000 ns exactly, and the
// inverse rounds back to 312).
//
// It splits into whole seconds plus a sub-second remainder so a long duration
// (or a crafted huge Duration or DiscardPadding) cannot overflow the ns*rate
// product: a full day at 48 kHz already pushes a naive ns*rate near int64's
// ceiling.
func nsToSamples(ns int64, rate int) int64 {
	if ns <= 0 || rate <= 0 {
		return 0
	}
	sec := ns / 1_000_000_000
	rem := ns % 1_000_000_000
	return sec*int64(rate) + (rem*int64(rate)+500_000_000)/1_000_000_000
}
