// Package ape implements a Monkey's Audio (APE) decoder. It is a clean-room
// port of the reference Monkey's Audio SDK decoder (see THIRD-PARTY-NOTICES.md):
// the range coder and its two models, the cascaded neural filters, and the
// adaptive predictor, so decodes are bit-exact.
//
// Scope is stream versions 3.95 and later (3950..3990, which is every version
// the format has had since 2001 and what every encoder in circulation writes)
// in 8, 16, and 24-bit integer, mono and stereo, at all five compression
// levels. Anything else is refused by name: 32-bit and floating-point streams,
// more than two channels, and the pre-3950 bitstreams, which are a different
// codec sharing the same magic.
//
// A frame is the unit of decoding and a sync point: it re-primes the range
// coder, flushes the predictors and filters, and ends with a CRC over the
// samples it produced. Frames are large by design (73728 blocks, times four at
// extra high and times sixteen at insane), so one packet is one frame and a
// seek lands at frame granularity.
//
// The frame's own byte extent and block count live in the file's header and
// seek table rather than in the frame, so the container prepends both to the
// packet as a small frame header this package defines (PutFrameHeader).
package ape

import (
	"encoding/binary"
	"fmt"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

// The committed .ape fixtures are regenerated with the reference encoder; see
// fixturegen_test.go.
//go:generate go test -tags apefixtures -run ^TestGenerateFixtures$ -count=1

// Version is the decoder's cache-key version constant (ADR-0004): bump on any
// change that alters decoded samples.
const Version = "ape-dec-1"

// Stream version bounds. Below 3950 the predictor is a different design and
// the entropy coder older still; 3990 has been the current version since 2003
// and the descriptor carries no room to grow past it.
const (
	MinFileVersion = 3950
	MaxFileVersion = 3990
)

// Fixed structure sizes.
const (
	// descriptorLen is the 3980-and-later APE_DESCRIPTOR: the magic, two
	// version numbers, the seven section lengths, and the file MD5.
	descriptorLen = 52
	// headerLen is the 3980-and-later APE_HEADER that follows it.
	headerLen = 24
	// oldHeaderLen is the pre-3980 header, which is the whole thing: magic,
	// version, format, and frame counts in one 32-byte struct.
	oldHeaderLen = 32
	// MaxHeaderLen is the most bytes ParseHeader can consume. A descriptor
	// may declare itself longer than the 52 bytes this version defines, so
	// the bound is what a caller must have on hand rather than what a current
	// file uses.
	MaxHeaderLen = 4 << 10
)

// Compression levels. The level chooses the filter cascade and the frame
// length; every one of them decodes losslessly.
const (
	LevelFast      = 1000
	LevelNormal    = 2000
	LevelHigh      = 3000
	LevelExtraHigh = 4000
	LevelInsane    = 5000
)

// Format flags. Most are marked obsolete in the reference and survive only in
// old files; the ones that matter here say what the samples are.
const (
	flag8Bit            = 1 << 0  // pre-3980 way of saying 8-bit
	flagHasPeakLevel    = 1 << 2  // a uint32 peak follows the old header
	flag24Bit           = 1 << 3  // pre-3980 way of saying 24-bit
	flagHasSeekElements = 1 << 4  // a uint32 seek-table length follows
	flagCreateWAVHeader = 1 << 5  // no source header stored; synthesize on decode
	flagFloatingPoint   = 1 << 12 // IEEE float samples
)

// Special frame codes, read from the frame header when the encoder found a
// whole frame it could describe instead of code.
const (
	specialMonoSilence  = 1
	specialLeftSilence  = 1
	specialRightSilence = 2
	specialPseudoStereo = 4
)

// Hostile-input caps (ADR-0005 invariants).
const (
	// maxBlocksPerFrame is the longest frame any encoder writes: the format's
	// 73728-block frame, times the sixteen the insane level multiplies it by.
	// The reference's own bound is ten million, but a frame is decoded whole
	// before its CRC is checked, so the cap is sized to the legitimate
	// maximum rather than to the reference's headroom.
	maxBlocksPerFrame = 73728 * 16
	// maxShallowBlocksPerFrame is the same bound below the insane level,
	// where the reference caps at one million.
	maxShallowBlocksPerFrame = 1000 * 1000
	// maxWAVHeaderBytes bounds the source file's stored header and trailer,
	// matching the reference's eight megabytes.
	maxWAVHeaderBytes = 8 << 20
	// maxChannels is this decoder's scope, not the format's (which allows 32).
	maxChannels = 2
)

func malformed(format string, args ...any) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, "ape: "+fmt.Sprintf(format, args...))
}

// unsupported names a stream shape this decoder deliberately does not cover.
// It is the same error as malformed, since a caller can act on neither; the
// two names exist so the message can say whether the file is broken or merely
// outside our scope.
func unsupported(format string, args ...any) error {
	return malformed(format, args...)
}

// MatchNeed is how many bytes Match wants: the file magic.
const MatchNeed = 4

// Match reports whether b begins with a Monkey's Audio file. It is the format
// sniff-table entry; a leading ID3v2 tag is the registry's business, since it
// rebases the source past one before any driver is asked.
func Match(b []byte) bool {
	return len(b) >= 4 && (string(b[:4]) == "MAC " || string(b[:4]) == "MACF")
}

// Magic is the file header's marker, without the fourth byte that says
// whether the samples are integers ("MAC ") or floats ("MACF"). A scan for a
// header looks for this and asks Match about the byte behind it.
var Magic = []byte("MAC")

// Header is a parsed APE file header: the stream's shape plus where the rest
// of the file's sections start. Both header forms parse into it, so nothing
// downstream has to know which one the file used.
//
// Every offset is measured from the first byte of the magic, which is where
// the container found the header and not necessarily the start of the file.
type Header struct {
	FileVersion      int
	ProgramVersion   int
	CompressionLevel int
	FormatFlags      int
	BlocksPerFrame   int
	FinalFrameBlocks int
	TotalFrames      int
	BitsPerSample    int
	Channels         int
	Rate             int

	// SeekTableOffset and SeekTableEntries locate the table of per-frame
	// file offsets. The table is what makes a frame addressable: a frame
	// carries no length of its own.
	SeekTableOffset  int64
	SeekTableEntries int
	// FrameDataOffset is where the first frame's bytes begin. The seek
	// table's first entry says the same thing, and a stream where the two
	// disagree is rejected rather than reconciled.
	FrameDataOffset int64
	// FrameDataBytes is the descriptor's own count of the coded frame data,
	// -1 when the header form does not carry one (pre-3980).
	FrameDataBytes int64
	// TerminatingBytes is the source file's trailing data (a WAV file's
	// chunks after the samples), stored after the last frame.
	TerminatingBytes int64
}

// ParseHeader parses the file header at the head of b, which must start at the
// magic. b must hold the whole fixed header; MaxHeaderLen is the most it can
// need.
func ParseHeader(b []byte) (Header, error) {
	if !Match(b) {
		return Header{}, malformed("not a Monkey's Audio file")
	}
	if len(b) < 8 {
		return Header{}, malformed("file header truncated")
	}
	version := int(binary.LittleEndian.Uint16(b[4:]))
	var (
		h   Header
		err error
	)
	if version >= 3980 {
		h, err = parseCurrent(b)
	} else {
		h, err = parseOld(b, version)
	}
	if err != nil {
		return Header{}, err
	}
	if err := h.validate(); err != nil {
		return Header{}, err
	}
	return h, nil
}

// parseCurrent reads the 3980-and-later form: a descriptor of section lengths
// followed by the format header, then the seek table, then the source file's
// stored header.
func parseCurrent(b []byte) (Header, error) {
	if len(b) < descriptorLen {
		return Header{}, malformed("descriptor truncated")
	}
	descBytes := int64(binary.LittleEndian.Uint32(b[8:]))
	seekTableBytes := int64(binary.LittleEndian.Uint32(b[16:]))
	headerDataBytes := int64(binary.LittleEndian.Uint32(b[20:]))
	frameDataBytes := int64(binary.LittleEndian.Uint32(b[24:])) |
		int64(binary.LittleEndian.Uint32(b[28:]))<<32
	h := Header{
		FileVersion:      int(binary.LittleEndian.Uint16(b[4:])),
		ProgramVersion:   int(binary.LittleEndian.Uint16(b[6:])),
		FrameDataBytes:   frameDataBytes,
		TerminatingBytes: int64(binary.LittleEndian.Uint32(b[32:])),
	}
	// The descriptor states its own length so it can grow; a file written by
	// a later version puts the format header past the 52 bytes we know.
	hdrBytes := int64(binary.LittleEndian.Uint32(b[12:]))
	if descBytes < descriptorLen || hdrBytes < headerLen {
		return Header{}, malformed("descriptor declares a %d-byte descriptor and %d-byte header, want at least %d and %d",
			descBytes, hdrBytes, descriptorLen, headerLen)
	}
	if descBytes+hdrBytes > MaxHeaderLen {
		return Header{}, malformed("file header of %d bytes exceeds the %d-byte bound", descBytes+hdrBytes, MaxHeaderLen)
	}
	if int64(len(b)) < descBytes+headerLen {
		return Header{}, malformed("format header truncated: %d bytes past a %d-byte descriptor, want %d",
			int64(len(b))-descBytes, descBytes, headerLen)
	}
	f := b[descBytes:]
	h.CompressionLevel = int(binary.LittleEndian.Uint16(f[0:]))
	h.FormatFlags = int(binary.LittleEndian.Uint16(f[2:]))
	h.BlocksPerFrame = int(binary.LittleEndian.Uint32(f[4:]))
	h.FinalFrameBlocks = int(binary.LittleEndian.Uint32(f[8:]))
	h.TotalFrames = int(binary.LittleEndian.Uint32(f[12:]))
	h.BitsPerSample = int(binary.LittleEndian.Uint16(f[16:]))
	h.Channels = int(binary.LittleEndian.Uint16(f[18:]))
	h.Rate = int(binary.LittleEndian.Uint32(f[20:]))

	if seekTableBytes%4 != 0 {
		return Header{}, malformed("seek table of %d bytes is not a whole number of entries", seekTableBytes)
	}
	// The field is only read when the file stored a header; with the
	// synthesize-on-decode flag set the reference neither reads nor bounds
	// it, so bounding it first would refuse a file it plays.
	if h.FormatFlags&flagCreateWAVHeader != 0 {
		headerDataBytes = 0
	}
	if headerDataBytes < 0 || headerDataBytes > maxWAVHeaderBytes {
		return Header{}, malformed("stored source header of %d bytes exceeds the %d-byte bound", headerDataBytes, maxWAVHeaderBytes)
	}
	h.SeekTableOffset = descBytes + hdrBytes
	h.SeekTableEntries = int(seekTableBytes / 4)
	h.FrameDataOffset = h.SeekTableOffset + seekTableBytes + headerDataBytes
	return h, nil
}

// parseOld reads the pre-3980 form, where the one header struct carries the
// format and the optional peak level, seek-element count, source header, and
// seek table follow it in that order.
func parseOld(b []byte, version int) (Header, error) {
	if len(b) < oldHeaderLen {
		return Header{}, malformed("file header truncated")
	}
	h := Header{
		FileVersion:      version,
		CompressionLevel: int(binary.LittleEndian.Uint16(b[6:])),
		FormatFlags:      int(binary.LittleEndian.Uint16(b[8:])),
		Channels:         int(binary.LittleEndian.Uint16(b[10:])),
		Rate:             int(binary.LittleEndian.Uint32(b[12:])),
		TotalFrames:      int(binary.LittleEndian.Uint32(b[24:])),
		FinalFrameBlocks: int(binary.LittleEndian.Uint32(b[28:])),
		TerminatingBytes: int64(binary.LittleEndian.Uint32(b[20:])),
		FrameDataBytes:   -1,
	}
	// The frame length is not stored: it follows from the version, and every
	// version in scope uses the same one regardless of level. (The 3980-and-
	// later form writes it down, and that is where the deep levels get their
	// longer frames.)
	h.BlocksPerFrame = 73728 * 4
	switch {
	case h.FormatFlags&flag8Bit != 0:
		h.BitsPerSample = 8
	case h.FormatFlags&flag24Bit != 0:
		h.BitsPerSample = 24
	default:
		h.BitsPerSample = 16
	}
	headerDataBytes := int64(binary.LittleEndian.Uint32(b[16:]))
	if h.FormatFlags&flagCreateWAVHeader != 0 {
		headerDataBytes = 0
	}
	if headerDataBytes < 0 || headerDataBytes > maxWAVHeaderBytes {
		return Header{}, malformed("stored source header of %d bytes exceeds the %d-byte bound", headerDataBytes, maxWAVHeaderBytes)
	}
	off := int64(oldHeaderLen)
	if h.FormatFlags&flagHasPeakLevel != 0 {
		off += 4
	}
	h.SeekTableEntries = h.TotalFrames
	if h.FormatFlags&flagHasSeekElements != 0 {
		if int64(len(b)) < off+4 {
			return Header{}, malformed("file header truncated")
		}
		h.SeekTableEntries = int(binary.LittleEndian.Uint32(b[off:]))
		off += 4
	}
	h.SeekTableOffset = off + headerDataBytes
	h.FrameDataOffset = h.SeekTableOffset + int64(h.SeekTableEntries)*4
	return h, nil
}

// validate refuses the headers this decoder cannot serve, each by name. Most
// are legal Monkey's Audio, so the message says what the file is rather than
// that it is broken.
func (h Header) validate() error {
	switch {
	case h.FileVersion < MinFileVersion:
		return unsupported("stream version %d predates %d: the 3.9x bitstreams are a different codec",
			h.FileVersion, MinFileVersion)
	case h.FileVersion > MaxFileVersion:
		return unsupported("stream version %d is past the supported %d", h.FileVersion, MaxFileVersion)
	case h.FormatFlags&flagFloatingPoint != 0:
		return unsupported("floating-point streams are not supported")
	case h.BitsPerSample != 8 && h.BitsPerSample != 16 && h.BitsPerSample != 24:
		return unsupported("%d-bit samples: only 8, 16, and 24-bit are supported", h.BitsPerSample)
	case h.Channels < 1 || h.Channels > maxChannels:
		return unsupported("%d channels: only mono and stereo are supported", h.Channels)
	case h.Rate <= 0:
		return malformed("sample rate %d", h.Rate)
	case h.CompressionLevel < LevelFast || h.CompressionLevel > LevelInsane ||
		h.CompressionLevel%1000 != 0:
		return unsupported("compression level %d is not one of 1000..5000", h.CompressionLevel)
	case h.TotalFrames < 0 || h.SeekTableEntries < 0:
		return malformed("negative frame count")
	case h.TotalFrames == 0:
		// A zero-frame file is one an encoder never finalized. The reference
		// refuses it too rather than decoding an empty stream.
		return malformed("no frames: the file was never finalized")
	case h.SeekTableEntries < h.TotalFrames:
		return malformed("seek table has %d entries for %d frames", h.SeekTableEntries, h.TotalFrames)
	case h.BlocksPerFrame <= 0:
		return malformed("frame length of %d blocks", h.BlocksPerFrame)
	case h.CompressionLevel >= LevelInsane && h.BlocksPerFrame > maxBlocksPerFrame:
		return malformed("frame length of %d blocks exceeds %d", h.BlocksPerFrame, maxBlocksPerFrame)
	case h.CompressionLevel < LevelInsane && h.BlocksPerFrame > maxShallowBlocksPerFrame:
		return malformed("frame length of %d blocks exceeds %d", h.BlocksPerFrame, maxShallowBlocksPerFrame)
	case h.FinalFrameBlocks <= 0 || h.FinalFrameBlocks > h.BlocksPerFrame:
		return malformed("final frame of %d blocks, want 1..%d", h.FinalFrameBlocks, h.BlocksPerFrame)
	case h.TerminatingBytes < 0 || h.TerminatingBytes > maxWAVHeaderBytes:
		return malformed("trailing source data of %d bytes exceeds the %d-byte bound", h.TerminatingBytes, maxWAVHeaderBytes)
	}
	return nil
}

// Samples is the stream length in frames of audio.
func (h Header) Samples() int64 {
	return int64(h.TotalFrames-1)*int64(h.BlocksPerFrame) + int64(h.FinalFrameBlocks)
}

// FrameBlocks is how many blocks frame i carries.
func (h Header) FrameBlocks(i int) int {
	if i == h.TotalFrames-1 {
		return h.FinalFrameBlocks
	}
	return h.BlocksPerFrame
}

// Config is the stream shape a decoder needs, which is everything about the
// coded form: the header's remaining fields describe the file's layout and
// belong to the container.
func (h Header) Config() Config {
	return Config{
		FileVersion:      h.FileVersion,
		CompressionLevel: h.CompressionLevel,
		BlocksPerFrame:   h.BlocksPerFrame,
		BitsPerSample:    h.BitsPerSample,
		Channels:         h.Channels,
		Rate:             h.Rate,
	}
}

// Config is what a Decoder is built from: the coded stream's shape.
type Config struct {
	FileVersion      int
	CompressionLevel int
	BlocksPerFrame   int
	BitsPerSample    int
	Channels         int
	Rate             int
}

// Format is the pipeline format the decoder emits: the int domain at the
// stream's own width.
func (c Config) Format() audio.Format {
	return audio.Format{
		Rate:     c.Rate,
		Channels: c.Channels,
		Layout:   audio.DefaultLayout(c.Channels),
		Type:     audio.Int,
		BitDepth: c.BitsPerSample,
	}
}

// Validate checks a Config against what this decoder covers. It restates the
// header's checks rather than borrowing them: a Config carries no format flags,
// so a Header built from one could never refuse a float stream and the shared
// path would only look like it was checking.
func (c Config) Validate() error {
	switch {
	case c.FileVersion < MinFileVersion:
		return unsupported("stream version %d predates %d: the 3.9x bitstreams are a different codec",
			c.FileVersion, MinFileVersion)
	case c.FileVersion > MaxFileVersion:
		return unsupported("stream version %d is past the supported %d", c.FileVersion, MaxFileVersion)
	case c.BitsPerSample != 8 && c.BitsPerSample != 16 && c.BitsPerSample != 24:
		return unsupported("%d-bit samples: only 8, 16, and 24-bit are supported", c.BitsPerSample)
	case c.Channels < 1 || c.Channels > maxChannels:
		return unsupported("%d channels: only mono and stereo are supported", c.Channels)
	case c.Rate <= 0:
		return malformed("sample rate %d", c.Rate)
	case c.CompressionLevel < LevelFast || c.CompressionLevel > LevelInsane ||
		c.CompressionLevel%1000 != 0:
		return unsupported("compression level %d is not one of 1000..5000", c.CompressionLevel)
	case c.BlocksPerFrame <= 0 || c.BlocksPerFrame > maxBlocksPerFrame:
		return malformed("frame length of %d blocks outside 1..%d", c.BlocksPerFrame, maxBlocksPerFrame)
	}
	return nil
}

// configVersion versions the marshaled Config layout.
const configVersion = 1

// MarshalBinary encodes the Config for Track.CodecConfig. APE rides in no
// container that carries an out-of-band config blob, so this is our own
// canonical form rather than a format artifact.
func (c Config) MarshalBinary() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	b := make([]byte, 16)
	b[0] = configVersion
	b[1] = byte(c.Channels)
	b[2] = byte(c.BitsPerSample)
	binary.LittleEndian.PutUint16(b[4:], uint16(c.FileVersion))
	binary.LittleEndian.PutUint16(b[6:], uint16(c.CompressionLevel))
	binary.LittleEndian.PutUint32(b[8:], uint32(c.Rate))
	binary.LittleEndian.PutUint32(b[12:], uint32(c.BlocksPerFrame))
	return b, nil
}

// ParseConfig decodes a Track.CodecConfig produced by MarshalBinary.
func ParseConfig(b []byte) (Config, error) {
	if len(b) != 16 || b[0] != configVersion {
		return Config{}, malformed("malformed codec config")
	}
	c := Config{
		Channels:         int(b[1]),
		BitsPerSample:    int(b[2]),
		FileVersion:      int(binary.LittleEndian.Uint16(b[4:])),
		CompressionLevel: int(binary.LittleEndian.Uint16(b[6:])),
		Rate:             int(binary.LittleEndian.Uint32(b[8:])),
		BlocksPerFrame:   int(binary.LittleEndian.Uint32(b[12:])),
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// FrameHeaderLen is the size of the header a packet carries ahead of a frame's
// coded bytes.
//
// An APE frame states neither its length nor its block count: the file header
// and the seek table hold both, and a codec.Packet is bytes and nothing else,
// so the container writes them here. The alignment skip is the second of them
// because the range coder reads 32-bit words anchored at the start of the
// frame data, and a frame begins wherever in a word the one before it ended.
const FrameHeaderLen = 8

// PutFrameHeader writes a frame's block count and alignment skip into the
// first FrameHeaderLen bytes of b.
func PutFrameHeader(b []byte, blocks, skip int) {
	binary.LittleEndian.PutUint32(b[0:], uint32(blocks))
	binary.LittleEndian.PutUint32(b[4:], uint32(skip))
}

// ParseFrameHeader reads back what PutFrameHeader wrote and returns the
// frame's coded bytes.
func ParseFrameHeader(pkt []byte) (blocks, skip int, data []byte, err error) {
	if len(pkt) < FrameHeaderLen {
		return 0, 0, nil, malformed("frame header of %d bytes, want %d", len(pkt), FrameHeaderLen)
	}
	blocks = int(binary.LittleEndian.Uint32(pkt[0:]))
	skip = int(binary.LittleEndian.Uint32(pkt[4:]))
	if blocks <= 0 || blocks > maxBlocksPerFrame {
		return 0, 0, nil, malformed("frame of %d blocks", blocks)
	}
	if skip < 0 || skip > 3 {
		return 0, 0, nil, malformed("frame alignment skip of %d bytes, want 0..3", skip)
	}
	return blocks, skip, pkt[FrameHeaderLen:], nil
}
