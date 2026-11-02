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
// sample position (counted here from the first frame's), so packetTiming
// ignores the running cursor and SeekSample walks frames reading positions
// directly. Vorbis and Opus accumulate: a packet's position is the running sum
// of durations, and SeekSample anchors that sum to page granule positions
// (which are authoritative at page boundaries). selfTiming selects which
// SeekSample walk applies.
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

	// finalizeTrack builds the Track once headers are done. first is the page
	// the first audio packet completed on, which says where the stream starts.
	// lastEnd lazily yields where the stream ends on the raw timeline (or -1
	// when unknown) via a tail scan, one window in the common case: the last
	// whole frame's end, off its header, for FLAC, and the final page granule
	// for the rest. The mapping maps it to Samples and, where the codec
	// signals gapless trims, Delay and Padding. r reports what the mapping made
	// of a declared total that disagrees with it: a shortfall is damage (and a
	// strict open refuses it), an overrun is the stream being internally
	// inconsistent in a way this build believes the stream about.
	finalizeTrack(first firstPage, lastEnd func() int64, r reporter) (container.Track, error)

	// packetTiming returns a data packet's start sample and duration in the
	// codec's output timeline, whether it is a seekable sync point, and whether
	// it is a valid audio packet at all. running is the demuxer's accumulated
	// position; self-timing mappings ignore it, accumulating mappings return
	// pts == running.
	packetTiming(pkt []byte, running int64) (pts, dur int64, sync, ok bool)

	// selfTiming reports whether packetTiming reads positions from the packet
	// itself (FLAC) rather than accumulating them.
	selfTiming() bool

	// preroll is how many samples before a seek target the demuxer should land
	// so the decoder reconverges: 0 for FLAC, a block for Vorbis, 80 ms for
	// Opus (RFC 7845).
	preroll() int64

	// resetTiming clears any stateful per-packet timing (Vorbis block-size
	// tracking) after a seek restart.
	resetTiming()
}

// reporter is the demuxer's warning pair, handed to a mapping that verifies a
// declared length against the pages: warn records tolerated damage and fails a
// strict open, note records a fact about a well-formed stream that Strict must
// not escalate.
type reporter interface {
	warn(off int64, format string, args ...any) error
	note(off int64, format string, args ...any)
}

// detectHeaders is parseID's sentinel for "consume header packets until the
// first audio packet, using isAudio" (FLAC with a zero declared count).
const detectHeaders = -1

// firstPage is the page the first audio packet completed on. A stream may
// start at a granule other than 0 (RFC 7845 section 4.5, Vorbis I section
// A.2), and this page is where it says so.
type firstPage struct {
	off     int64
	granule int64 // -1 when the page states none
	samples int64 // in the packets that complete on the page
	packets int   // the audio packets that complete on it
	eos     bool
	// start is the initial granule position the stream backs (see
	// Demuxer.startGranule): a page granule less start is a position on the
	// raw packet timeline, which starts at 0.
	start int64
}
