package ogg

import (
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

// mapping is the codec-specific half of an Ogg logical bitstream. The Demuxer
// owns page framing, packet reassembly, resync, and granule bisection; a
// mapping identifies its codec from the beginning-of-stream packet, consumes
// its header packets into a Track, and assigns each audio packet a timeline.
//
// Two timing models coexist. FLAC self-times: every frame header carries its
// absolute sample position, so packetTiming ignores the running cursor and
// SeekSample walks frames reading positions directly. Vorbis and Opus
// accumulate: a packet's position is the running sum of durations, and
// SeekSample anchors that sum to page granule positions (which are authoritative
// at page boundaries). selfTiming selects which SeekSample walk applies.
type mapping interface {
	// codecID is the codec this mapping decodes.
	codecID() codec.ID

	// parseID parses the BOS identification packet (already sniffed to this
	// mapping) and returns how many further header packets precede audio:
	// a non-negative count (Opus 1, Vorbis 2), or detectHeaders to stop at the
	// first packet isAudio accepts (FLAC's declared-or-detected count).
	parseID(pkt []byte) (extraHeaders int, err error)

	// parseHeader consumes one non-identification header packet.
	parseHeader(pkt []byte) error

	// isAudio reports whether pkt is an audio packet. In detect mode it finds
	// the first audio packet; otherwise it validates the header/audio boundary.
	isAudio(pkt []byte) bool

	// finalizeTrack builds the Track once headers are done. lastGranule lazily
	// yields the stream's final page granule (or -1 when unknown) via a
	// multi-MiB tail scan; a mapping calls it only when the length is not
	// otherwise known, so a FLAC stream with a declared total never pays for it.
	// The mapping maps the granule to Samples and, where the codec signals
	// gapless trims, Delay and Padding.
	finalizeTrack(lastGranule func() int64) (container.Track, error)

	// packetTiming returns a data packet's start sample and duration in the
	// codec's output timeline, whether it is a seekable sync point, and whether
	// it is a valid audio packet at all. running is the demuxer's accumulated
	// position; self-timing mappings ignore it, accumulating mappings return
	// pts == running.
	packetTiming(pkt []byte, running int64) (pts, dur int64, sync, ok bool)

	// selfTiming reports whether packetTiming reads absolute positions from the
	// packet (FLAC) rather than accumulating them.
	selfTiming() bool

	// preroll is how many samples before a seek target the demuxer should land
	// so the decoder reconverges: 0 for FLAC, a block for Vorbis, 80 ms for
	// Opus (RFC 7845).
	preroll() int64

	// granuleShift is how far the page granule timeline leads the decoder's
	// raw output timeline. Vorbis emits nothing for its priming packet, so its
	// output starts firstBlock/2 samples after granule 0; Opus and FLAC emit
	// from granule 0, so their shift is 0. finalizeTrack subtracts it from the
	// length, and accumulating seeks convert granule anchors to output
	// positions with it.
	granuleShift() int64

	// resetTiming clears any stateful per-packet timing (Vorbis block-size
	// tracking) after a seek restart.
	resetTiming()
}

// detectHeaders is parseID's sentinel for "consume header packets until the
// first audio packet, using isAudio" (FLAC with a zero declared count).
const detectHeaders = -1
