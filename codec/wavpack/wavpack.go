// Package wavpack implements a WavPack decoder. It is a clean-room port of
// the reference libwavpack decoder (see THIRD-PARTY-NOTICES.md): the
// median-adaptive entropy coder, the cascaded decorrelation passes, and the
// joint-stereo matrix, so decodes are bit-exact.
//
// Scope is version-4 streams in pure lossless mode: 8/16/24/32-bit integers,
// mono and stereo, any sample rate. The false-stereo and mono-in-stereo block
// forms are covered because real encoders emit them for near-mono material,
// and they are orthogonal to the two-channel cap. Hybrid streams (with or
// without their wvc correction file), float data, DSD, and more than two
// channels are refused by name rather than decoded approximately.
//
// Every block is self-contained: it carries its own decorrelation terms,
// weights, sample history, and entropy medians, so each is a sync point and
// the decoder holds no cross-packet state. The block header's CRC covers the
// decoded samples, which makes it the integrity check a damaged block fails.
package wavpack

import (
	"encoding/binary"
	"fmt"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

// Version is the decoder's cache-key version constant (ADR-0004): bump on
// any change that alters decoded samples.
const Version = "wavpack-dec-1"

// BlockHeaderLen is the fixed size of a WavPack block header.
const BlockHeaderLen = 32

// Stream version bounds. Anything below 0x402 is the WavPack 3 bitstream, a
// different codec that shares only the four-byte magic.
const (
	MinStreamVersion = 0x402
	MaxStreamVersion = 0x410
)

// Block header flag bits.
const (
	flagBytesStored  = 0x3        // bytes per sample, minus one
	flagMono         = 0x4        // one channel of data in this block
	flagHybrid       = 0x8        // hybrid (lossy or wvc-corrected) mode
	flagJointStereo  = 0x10       // mid/side matrix applied
	flagCrossDecorr  = 0x20       // the cascade includes cross-channel passes
	flagFloatData    = 0x80       // IEEE 32-bit float samples
	flagInt32Data    = 0x100      // extended integer handling
	flagInitialBlock = 0x800      // first block of a multichannel segment
	flagFinalBlock   = 0x1000     // last block of a multichannel segment
	flagShiftLSB     = 13         // shift amount, 5 bits
	flagShiftMask    = 0x1f << 13 // zero LSBs stripped before coding
	flagMagLSB       = 18         // magnitude of the largest sample, 5 bits
	flagSrateLSB     = 23         // sample-rate index, 4 bits
	flagSrateMask    = 0xf << 23
	flagFalseStereo  = 0x40000000 // stereo block whose two channels are equal
	flagDSD          = 0x80000000 // 1-bit DSD audio

	// flagMonoData covers both ways a block carries one channel of samples:
	// a real mono block, and a false-stereo block whose second channel the
	// decoder reconstructs by copying the first.
	flagMonoData = flagMono | flagFalseStereo
)

// Metadata sub-block ids, with the size and class bits stripped.
const (
	idDecorrTerms   = 0x2
	idDecorrWeights = 0x3
	idDecorrSamples = 0x4
	idEntropyVars   = 0x5
	idInt32Info     = 0x9
	idWVBitstream   = 0xa
	idWVXBitstream  = 0xc
	idChannelInfo   = 0xd
	idSampleRate    = 0x27
	idWVXNew        = 0x2c

	idOddSize = 0x40 // the payload is one byte shorter than its word count
	idLarge   = 0x80 // the word count is three bytes, not one
)

// srateTable maps the header's four-bit rate index to a sample rate. Index 15
// means the rate is not in this table and rides in an ID_SAMPLE_RATE
// sub-block instead.
var srateTable = [15]int{
	6000, 8000, 9600, 11025, 12000, 16000, 22050, 24000,
	32000, 44100, 48000, 64000, 88200, 96000, 192000,
}

// Hostile-input caps (ADR-0005 invariants).
const (
	// maxBlockSamples bounds a block's declared sample count and so the
	// decoder's scratch. SyncOK already rejects anything larger: the
	// reference sync predicate requires the field's top byte to be zero and
	// the next below three.
	maxBlockSamples = 3 << 16
	// maxTerms is the reference's MAX_NTERMS: the decorrelation cascade
	// cannot be deeper.
	maxTerms = 16
	// maxTerm is the reference's MAX_TERM: the largest positive term, and so
	// the sample history each pass keeps.
	maxTerm = 8
)

func malformed(format string, args ...any) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, "wavpack: "+fmt.Sprintf(format, args...))
}

// unsupported names a stream shape this decoder deliberately does not cover.
// It is deliberately the same error as malformed, since callers can act on
// neither: the two names exist so a reader can tell at the call site whether
// the file is broken or merely outside our scope, which is what the message
// then has to say.
func unsupported(format string, args ...any) error {
	return malformed(format, args...)
}

// BlockHeader is a parsed 32-byte WavPack block header.
type BlockHeader struct {
	// Size is the block's total byte length: the ckSize field plus the eight
	// bytes ckSize does not count.
	Size int64
	// StreamVersion is the bitstream revision, 0x402 to 0x410.
	StreamVersion int
	// TotalSamples is the stream length in frames, -1 when the encoder never
	// learned it (a pipe, an interrupted encode).
	TotalSamples int64
	// BlockIndex is the position of this block's first frame.
	BlockIndex int64
	// BlockSamples is the frame count, 0 for a metadata-only block.
	BlockSamples int
	Flags        uint32
	CRC          uint32
}

// Match reports whether b begins with the WavPack block magic. It is the
// format sniff-table entry.
func Match(b []byte) bool { return len(b) >= 4 && string(b[:4]) == "wvpk" }

// SyncOK reports whether b is a plausible block header, using the reference
// decoder's own sync predicate: the magic, an even block size of at least 24
// and under a megabyte, a stream version this decoder knows, and a sample
// count under 3<<16. It is the cheap filter a boundary scan applies before
// parsing.
func SyncOK(b []byte) bool {
	if !Match(b) || len(b) < BlockHeaderLen {
		return false
	}
	size := binary.LittleEndian.Uint32(b[4:])
	// The reference reads the size byte by byte and requires the top byte
	// zero and the next below 16, which is exactly "even and under 1 MiB".
	if size&1 != 0 || size < BlockHeaderLen-8 || size >= 1<<20 {
		return false
	}
	if ver := binary.LittleEndian.Uint16(b[8:]); ver < MinStreamVersion || ver > MaxStreamVersion {
		return false
	}
	return b[22] < 3 && b[23] == 0
}

// ParseBlockHeader parses the 32-byte header at the head of b.
func ParseBlockHeader(b []byte) (BlockHeader, error) {
	if len(b) < BlockHeaderLen {
		return BlockHeader{}, malformed("block header of %d bytes, want %d", len(b), BlockHeaderLen)
	}
	if !Match(b) {
		return BlockHeader{}, malformed("not a WavPack block")
	}
	h := BlockHeader{
		Size:          int64(binary.LittleEndian.Uint32(b[4:])) + 8,
		StreamVersion: int(binary.LittleEndian.Uint16(b[8:])),
		BlockIndex:    int64(binary.LittleEndian.Uint32(b[16:])) | int64(b[10])<<32,
		BlockSamples:  int(binary.LittleEndian.Uint32(b[20:])),
		Flags:         binary.LittleEndian.Uint32(b[24:]),
		CRC:           binary.LittleEndian.Uint32(b[28:]),
	}
	// The 40-bit total splits across two fields, and an all-ones low word is
	// the escape: the encoder never learned the length.
	if lo := binary.LittleEndian.Uint32(b[12:]); lo == 0xffffffff {
		h.TotalSamples = -1
	} else {
		h.TotalSamples = int64(lo) + int64(b[11])<<32 - int64(b[11])
	}
	switch {
	case h.Size < BlockHeaderLen:
		return BlockHeader{}, malformed("block size %d below the %d-byte header", h.Size, BlockHeaderLen)
	case h.StreamVersion < MinStreamVersion || h.StreamVersion > MaxStreamVersion:
		return BlockHeader{}, unsupported("stream version %#x outside the supported %#x..%#x",
			h.StreamVersion, MinStreamVersion, MaxStreamVersion)
	case h.BlockSamples > maxBlockSamples:
		return BlockHeader{}, malformed("block of %d samples exceeds %d", h.BlockSamples, maxBlockSamples)
	case h.BlockSamples < 0 || h.TotalSamples < -1:
		return BlockHeader{}, malformed("negative sample count")
	case h.Flags&flagMonoData == flagMonoData:
		// Mono and false-stereo are two ways of saying one channel is coded,
		// and they disagree about how many come back out. The reference
		// rejects the combination; so must we, before anything sizes a buffer
		// from one flag and fills it from the other.
		return BlockHeader{}, malformed("block is flagged both mono and false-stereo")
	}
	return h, nil
}

// MaxSamples is the longest stream a block header can state, and so the
// longest one this package writes. The field is forty bits, but the reference
// skips every value whose low word would collide with the unknown-length
// escape and a reader subtracts that skip back off, so the largest count that
// survives the round trip is 257 short of the field's range rather than one.
// libwavpack's own cap is the same number for the same reason.
const MaxSamples = 1<<40 - 257

// MaxRate is the largest sample rate a block can state. Rates outside the
// fifteen-entry table ride in a sample-rate sub-block, three bytes wide up to
// this point and four beyond it, with the top bit of the fourth reserved.
const MaxRate = 1<<31 - 1

// The block CRC's recurrences, one definition each rather than one per
// direction. Both the encoder and the decoder fold every sample of a block
// into these, over the samples as they stand after the joint-stereo matrix is
// undone and before the shift and extension are restored; the extension
// stream carries its own over the samples it completes. A round trip already
// catches a divergence (the decoder checks the field it recomputes), but only
// because the two are the same arithmetic, and that is easier to keep true
// with nothing to keep in step.
func crcMono(crc uint32, v int32) uint32 { return crc*3 + uint32(v) }

func crcStereo(crc uint32, l, r int32) uint32 {
	return crc + (crc << 3) + (uint32(l) << 1) + uint32(l) + uint32(r)
}

func crcExtension(crc uint32, v int32) uint32 {
	return crc*9 + uint32(v&0xffff)*3 + uint32((v>>16)&0xffff)
}

// Audio reports whether the block carries samples; blocks with none hold only
// metadata (a RIFF trailer, an MD5 signature) and are not packets.
func (h BlockHeader) Audio() bool { return h.BlockSamples > 0 }

// Mono reports whether the block codes one channel of samples, either as a
// real mono block or as a false-stereo one. It is not the track's channel
// count: a false-stereo block codes one channel and decodes to two, so
// Channels is what a format check compares.
func (h BlockHeader) Mono() bool { return h.Flags&flagMonoData != 0 }

// Channels is how many channels the block decodes to, 1 or 2.
func (h BlockHeader) Channels() int {
	if h.Flags&flagMono != 0 {
		return 1
	}
	return 2
}

// BytesPerSample is the sample width the block decodes to, 1 to 4.
func (h BlockHeader) BytesPerSample() int { return int(h.Flags&flagBytesStored) + 1 }

// shift is how far decoded samples move back up: an encoder strips constant
// zero LSBs (a 20-bit source in a 24-bit container) and the decoder restores
// them.
func (h BlockHeader) shift() uint { return uint(h.Flags&flagShiftMask) >> flagShiftLSB }

// walkMeta calls fn for each metadata sub-block in the block body. Sub-blocks
// of every class are handed over; fn decides what it wants. The walk stops at
// the first sub-block that does not fit, matching the reference, which treats
// a short tail as the end of the metadata rather than as damage: the block's
// own CRC is what catches a stream that really is broken.
func walkMeta(block []byte, fn func(id byte, data []byte) error) error {
	if len(block) < BlockHeaderLen {
		return nil
	}
	b := block[BlockHeaderLen:]
	for len(b) >= 2 {
		id, n := b[0], int(b[1])<<1
		b = b[2:]
		if id&idLarge != 0 {
			if len(b) < 2 {
				return nil
			}
			n += int(b[0])<<9 + int(b[1])<<17
			b = b[2:]
			id &^= idLarge
		}
		if id&idOddSize != 0 {
			if n == 0 {
				return nil
			}
			id &^= idOddSize
			n--
		}
		if n == 0 {
			if err := fn(id, nil); err != nil {
				return err
			}
			continue
		}
		if len(b) < n+n&1 {
			return nil
		}
		if err := fn(id, b[:n]); err != nil {
			return err
		}
		b = b[n+n&1:]
	}
	return nil
}

// Config is the stream shape a WavPack track decodes to, derived from its
// first audio block.
type Config struct {
	Rate     int
	Channels int
	// BitDepth is the container width decoded samples occupy: 8, 16, 24, 32.
	BitDepth int
	// ValidBits is how many of those bits the source actually used, which is
	// fewer when the encoder stripped constant zero LSBs. It is reporting
	// only: samples come back in the BitDepth domain either way.
	ValidBits int
}

// Format is the pipeline format the decoder emits: the int domain, at the
// stream's container width.
func (c Config) Format() audio.Format {
	return audio.Format{
		Rate:     c.Rate,
		Channels: c.Channels,
		Layout:   audio.DefaultLayout(c.Channels),
		Type:     audio.Int,
		BitDepth: c.BitDepth,
	}
}

// Validate checks a Config against what this decoder covers.
func (c Config) Validate() error {
	switch {
	case c.Rate <= 0 || c.Rate > MaxRate:
		return malformed("sample rate %d outside 1..%d", c.Rate, MaxRate)
	case c.Channels < 1 || c.Channels > 2:
		return unsupported("%d channels: only mono and stereo are supported", c.Channels)
	case c.BitDepth != 8 && c.BitDepth != 16 && c.BitDepth != 24 && c.BitDepth != 32:
		return malformed("bit depth %d, want 8/16/24/32", c.BitDepth)
	case c.ValidBits < 1 || c.ValidBits > c.BitDepth:
		return malformed("valid bits %d outside 1..%d", c.ValidBits, c.BitDepth)
	}
	return nil
}

// configVersion versions the marshaled Config layout.
const configVersion = 1

// MarshalBinary encodes the Config for Track.CodecConfig. WavPack rides in no
// container that carries an out-of-band config blob, so this is our own
// canonical form rather than a format artifact.
func (c Config) MarshalBinary() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	b := make([]byte, 8)
	b[0] = configVersion
	b[1] = byte(c.Channels)
	b[2] = byte(c.BitDepth)
	b[3] = byte(c.ValidBits)
	binary.LittleEndian.PutUint32(b[4:], uint32(c.Rate))
	return b, nil
}

// ParseConfig decodes a Track.CodecConfig produced by MarshalBinary.
func ParseConfig(b []byte) (Config, error) {
	if len(b) != 8 || b[0] != configVersion {
		return Config{}, malformed("malformed codec config")
	}
	c := Config{
		Rate:      int(binary.LittleEndian.Uint32(b[4:])),
		Channels:  int(b[1]),
		BitDepth:  int(b[2]),
		ValidBits: int(b[3]),
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// ProbeBlock derives the stream Config from one whole audio block (header
// included), refusing by name every shape this decoder does not cover. The
// demuxer calls it on the first audio block it finds.
func ProbeBlock(block []byte) (Config, error) {
	h, err := ParseBlockHeader(block)
	if err != nil {
		return Config{}, err
	}
	if !h.Audio() {
		return Config{}, malformed("block carries no samples")
	}
	if err := h.supported(); err != nil {
		return Config{}, err
	}
	if h.Size > int64(len(block)) {
		return Config{}, malformed("block declares %d bytes but only %d are present", h.Size, len(block))
	}
	// Walk this block only: a caller holding a whole file would otherwise
	// have the metadata scan run on into the next block's sub-blocks.
	block = block[:h.Size]
	cfg := Config{Channels: h.Channels(), BitDepth: h.BytesPerSample() * 8}
	cfg.ValidBits = cfg.BitDepth - int(h.shift())
	if idx := (h.Flags & flagSrateMask) >> flagSrateLSB; idx < uint32(len(srateTable)) {
		cfg.Rate = srateTable[idx]
	}
	err = walkMeta(block, func(id byte, data []byte) error {
		switch id {
		case idSampleRate:
			if len(data) == 3 || len(data) == 4 {
				cfg.Rate = int(data[0]) | int(data[1])<<8 | int(data[2])<<16
				if len(data) == 4 {
					cfg.Rate |= int(data[3]&0x7f) << 24
				}
			}
		case idChannelInfo:
			// The first byte is the channel count; past two it is a
			// multichannel file whose later streams we never reach.
			if len(data) > 0 && data[0] > 2 {
				return unsupported("%d channels: only mono and stereo are supported", data[0])
			}
		}
		return nil
	})
	if err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// supported refuses the block shapes outside this decoder's scope, each by
// name. They are all legal WavPack, so the message says what the file is, not
// that it is broken.
func (h BlockHeader) supported() error {
	switch {
	case h.Flags&flagDSD != 0:
		return unsupported("DSD streams are not supported")
	case h.Flags&flagHybrid != 0:
		return unsupported("hybrid streams (and their wvc correction files) are not supported")
	case h.Flags&flagFloatData != 0:
		return unsupported("32-bit float streams are not supported")
	case h.Flags&(flagInitialBlock|flagFinalBlock) != flagInitialBlock|flagFinalBlock:
		// A block that is not both the first and the last of its segment is
		// one stream of a multichannel file; the rest follow in siblings.
		return unsupported("more than 2 channels: only mono and stereo are supported")
	}
	return nil
}
