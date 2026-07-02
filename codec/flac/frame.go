package flac

// Frame header parsing (RFC 9639 section 9.1). Exported because both
// native-FLAC and Ogg-FLAC containers need it: frame headers carry their
// own position (frame or sample number), which is what makes FLAC seeks
// verifiable at the container layer.

// Channel assignments (9.1.4). Values 0-7 are that many plus one
// independent channels; 8-10 are the stereo decorrelation modes.
const (
	assignLeftSide  = 8
	assignRightSide = 9
	assignMidSide   = 10
)

// MaxFrameHeaderLen bounds a frame header: 4 fixed bytes, up to 7 coded
// number bytes, up to 2 block size bytes, up to 2 sample rate bytes, and
// the CRC-8. ParseFrameHeader never needs more input than this.
const MaxFrameHeaderLen = 16

// FrameInfo is a parsed frame header.
type FrameInfo struct {
	// Variable is the blocking strategy: false means fixed block size
	// (Coded is a frame number), true means variable (Coded is the frame's
	// first sample number). It must not change within a stream.
	Variable bool
	// Coded is the frame number or sample number, per Variable.
	Coded uint64
	// BlockSize is the frame's sample count per channel.
	BlockSize int
	// Rate is the frame's sample rate in Hz, 0 when the header defers to
	// STREAMINFO.
	Rate int
	// Channels is the channel count implied by the assignment.
	Channels int
	// Bits is the frame's sample depth, 0 when deferred to STREAMINFO.
	Bits int

	assign int // raw channel assignment, decoder-internal
	hdrLen int // header length in bytes including CRC-8
}

// Numbering resolves frames' coded numbers to sample positions. The
// decision is a stream-level property, not a per-frame one, so it lives
// here rather than in each container: pre-1.0 "old format" streams
// (Flake) code sample numbers with the variable-blocksize bit clear, and
// unequal STREAMINFO block size bounds are how libFLAC detects them too.
// Containers latch it once from STREAMINFO and the first frame.
type Numbering struct {
	// SampleCoded means coded numbers are sample positions; otherwise
	// they are frame indexes at a constant block size.
	SampleCoded bool
	// ConstBlock is the fixed-strategy constant block size, from the
	// first frame.
	ConstBlock int
}

// Numbering latches the stream's coded-number semantics from the first
// frame.
func (si StreamInfo) Numbering(first FrameInfo) Numbering {
	return Numbering{
		SampleCoded: first.Variable || si.MinBlock != si.MaxBlock,
		ConstBlock:  first.BlockSize,
	}
}

// Start returns the frame's first sample position.
func (n Numbering) Start(fi FrameInfo) int64 {
	if n.SampleCoded {
		return int64(fi.Coded)
	}
	return int64(fi.Coded) * int64(n.ConstBlock)
}

// Next returns the coded number the frame following fi must carry, the
// invariant container/flacn confirms packet boundaries with.
func (n Numbering) Next(fi FrameInfo) uint64 {
	if n.SampleCoded {
		return fi.Coded + uint64(fi.BlockSize)
	}
	return fi.Coded + 1
}

// blockSizes maps 4-bit codes to fixed block sizes; 0 marks codes that
// are reserved (0) or read from the header end (6, 7).
var blockSizes = [16]int{
	0, 192, 576, 1152, 2304, 4608, 0, 0,
	256, 512, 1024, 2048, 4096, 8192, 16384, 32768,
}

// sampleRates maps 4-bit codes to rates in Hz; 0 marks STREAMINFO (0),
// header-supplied (12-14), and invalid (15) codes.
var sampleRates = [16]int{
	0, 88200, 176400, 192000, 8000, 16000, 22050, 24000,
	32000, 44100, 48000, 96000, 0, 0, 0, 0,
}

// sampleBits maps 3-bit codes to depths; 0 marks STREAMINFO, -1 reserved.
var sampleBits = [8]int{0, 8, 12, -1, 16, 20, 24, 32}

// SyncOK reports whether b begins with a frame sync sequence: the 15-bit
// code 0b111111111111100 followed by the blocking-strategy bit. It is the
// cheap first test in container resync scans.
func SyncOK(b []byte) bool {
	return len(b) >= 2 && b[0] == 0xFF && b[1]&0xFE == 0xF8
}

// ParseFrameHeader parses and CRC-checks the frame header at the start of
// b. It needs at most MaxFrameHeaderLen bytes; short input where the
// header is truncated is an error. Rate and Bits stay 0 when the header
// defers to STREAMINFO; the caller resolves them.
func ParseFrameHeader(b []byte) (FrameInfo, error) {
	var fi FrameInfo
	if len(b) < 5 {
		return fi, malformed("frame header truncated")
	}
	if !SyncOK(b) {
		return fi, malformed("bad frame sync")
	}
	fi.Variable = b[1]&0x01 != 0

	bsCode := int(b[2]) >> 4
	rateCode := int(b[2]) & 0xF
	fi.assign = int(b[3]) >> 4
	bitsCode := (int(b[3]) >> 1) & 0x7

	if bsCode == 0 {
		return fi, malformed("reserved block size code")
	}
	if rateCode == 15 {
		return fi, malformed("invalid sample rate code")
	}
	if b[3]&0x01 != 0 {
		return fi, malformed("reserved frame header bit set")
	}
	switch {
	case fi.assign <= 7:
		fi.Channels = fi.assign + 1
	case fi.assign <= 10:
		fi.Channels = 2
	default:
		return fi, malformed("reserved channel assignment %d", fi.assign)
	}
	if fi.Bits = sampleBits[bitsCode]; fi.Bits < 0 {
		return fi, malformed("reserved sample size code")
	}

	// Coded number: a UTF-8-like variable-length integer, up to 36 bits
	// in up to 7 bytes.
	pos := 4
	head := b[pos]
	pos++
	extra := 0
	switch {
	case head&0x80 == 0:
		fi.Coded = uint64(head)
	case head&0xC0 == 0x80, head == 0xFF:
		return fi, malformed("invalid coded number")
	default:
		for m := head; m&0x40 != 0; m <<= 1 {
			extra++
		}
		fi.Coded = uint64(head) & (0x3F >> extra)
	}
	if pos+extra > len(b) {
		return fi, malformed("frame header truncated")
	}
	for range extra {
		c := b[pos]
		if c&0xC0 != 0x80 {
			return fi, malformed("invalid coded number continuation")
		}
		fi.Coded = fi.Coded<<6 | uint64(c&0x3F)
		pos++
	}

	// Uncommon block size and sample rate follow the coded number.
	need := 0
	if bsCode == 6 {
		need++
	}
	if bsCode == 7 {
		need += 2
	}
	switch rateCode {
	case 12:
		need++
	case 13, 14:
		need += 2
	}
	if pos+need+1 > len(b) {
		return fi, malformed("frame header truncated")
	}
	switch bsCode {
	case 6:
		fi.BlockSize = int(b[pos]) + 1
		pos++
	case 7:
		fi.BlockSize = (int(b[pos])<<8 | int(b[pos+1])) + 1
		pos += 2
	default:
		fi.BlockSize = blockSizes[bsCode]
	}
	if fi.BlockSize > MaxBlockSize {
		return fi, malformed("block size %d exceeds %d", fi.BlockSize, MaxBlockSize)
	}
	switch rateCode {
	case 12:
		fi.Rate = int(b[pos]) * 1000
		pos++
	case 13:
		fi.Rate = int(b[pos])<<8 | int(b[pos+1])
		pos += 2
	case 14:
		fi.Rate = (int(b[pos])<<8 | int(b[pos+1])) * 10
		pos += 2
	default:
		fi.Rate = sampleRates[rateCode]
	}
	if fi.Rate == 0 && rateCode != 0 {
		return fi, malformed("frame sample rate 0")
	}

	if crc8(b[:pos]) != b[pos] {
		return fi, malformed("frame header CRC-8 mismatch")
	}
	fi.hdrLen = pos + 1
	return fi, nil
}
