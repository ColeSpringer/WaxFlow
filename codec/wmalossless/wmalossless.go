// Package wmalossless decodes Windows Media Audio 9.2 Lossless
// (wFormatTag 0x0163) from the WAVEFORMATEX and packets container/asf
// delivers.
//
// The decode path is integer arithmetic end to end: no transform, no window,
// no gain, no normalisation. So a conforming decode returns the encoder's
// input sample for sample, and the package's gate asserts exact equality
// against source PCM with no tolerance anywhere.
//
// This is a different codec from codec/wma, sharing only the container and the
// WAVEFORMATEX. It is built from docs/notes/wma-lossless-bitstream.md and
// docs/notes/wma-lossless-oracle-corpus.md, the ADR-0001 analysis artifacts,
// and from nothing else.
package wmalossless

import (
	"encoding/binary"
	"math/bits"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

// Version is the decoder's cache-key version constant (ADR-0004): bump on any
// change that alters decoded samples.
const Version = "wmalossless-dec-1"

// tagLossless is the wFormatTag this package decodes. The tags it refuses
// belong to container/asf's asfCodecID table, which names them before a track
// is wired.
const tagLossless = 0x0163

// waveFormatLen is the fixed part of WAVEFORMATEX, up to and including cbSize.
// container/asf hands the whole structure over as Track.CodecConfig, so the
// extra bytes behind it arrive in the same blob.
const waveFormatLen = 18

// extraLen is how many extra bytes this format defines. Every real stream
// carries exactly this many.
const extraLen = 18

// Extra-byte offsets, relative to the start of the extra block.
const (
	extraBitsPerSample = 0  // 2 bytes: 16 or 24, and nothing else
	extraChannelMask   = 2  // 4 bytes: WAVEFORMATEXTENSIBLE bit assignments
	extraDecodeFlags   = 14 // 2 bytes
)

// Decode-flags bits. Bits 0 and 9 to 15 are unassigned and carry no meaning
// for a decoder; they are read as part of the word and ignored.
const (
	flagFrameLenAdjust = 0x0006 // bits 1-2, the frame-length adjustment
	flagSubframeDepth  = 0x0038 // bits 3-5, maxSubframes = 1 << value
	flagLengthPrefix   = 0x0040 // each frame opens with its own length in bits
	flagDRC            = 0x0080 // each frame carries an 8-bit gain, discarded
	flagLPC            = 0x0100 // each coded subframe carries an LPC flag

	shiftFrameLenAdjust = 1
	shiftSubframeDepth  = 3
)

// Format bounds. Each is a shape for which the notes define no layout, and
// each has to be checked here because container/asf validates none of them.
const (
	// maxBlockAlign is the largest packet this decoder accepts. The format
	// states no bound; this one matches what derived decoders accept and keeps
	// every derived width inside an int on a 32-bit build.
	maxBlockAlign = 1 << 21
	// maxSamplesPerFrame bounds the frame-length rule. Past it the rule stops
	// describing anything real.
	maxSamplesPerFrame = 16384
	// maxSubframeDepth is the largest subframe-depth field, so at most 32
	// subframes per channel per frame.
	maxSubframeDepth = 5
	// maxCDLMSOrder is the largest filter order the cascade accepts. The
	// 7-bit order field can express up to 1024; above 256 is malformed.
	maxCDLMSOrder = 256
	// maxCDLMSFilters is the deepest cascade, from the 3-bit filter count.
	maxCDLMSFilters = 8
	// maxSubframesPerChannel bounds the tiling walk. It is maxSubframes at
	// the deepest depth, and a channel that exceeds it is malformed.
	maxSubframesPerChannel = 32
	// maxGolombRun bounds the unary quotient. No encoder writes a run past the
	// 32 that triggers the escape; without the bound a stream of 1 bits spins
	// until the packet is exhausted. A reader's choice, not a format constant.
	maxGolombRun = 32
	// maxFramesPerPacket bounds how many frames one packet's carry may
	// produce. A frame whose channels are all uncoded is a handful of bits and
	// still covers a whole frame of samples on every channel, so without a
	// ceiling a crafted packet is a decompression bomb: at the largest
	// nBlockAlign this parser accepts it would emit hundreds of gigabytes.
	//
	// The margin is measured rather than guessed. Twenty seconds of digital
	// silence, which is the most compressible input there is, comes back from
	// Windows' encoder as 431 frames in 13 packets, so a real file reaches
	// about 33 frames in a packet and this is a thirtyfold margin over that.
	// A reader's choice, not a format constant.
	maxFramesPerPacket = 1024
	// maxCarryBytes bounds the cross-packet carry. A frame legitimately spans
	// three or more packets, so this is generous; past it the stream is
	// refused rather than allowed to grow a buffer without bound.
	maxCarryBytes = 1 << 20
	// maxWireField bounds the two 32-bit WAVEFORMATEX fields this parser puts
	// in an int, so a crafted file cannot parse to different values on a
	// 32-bit build than on a 64-bit one. Nothing real is near it.
	maxWireField = 1 << 27
)

var le = binary.LittleEndian

// malformed reports bytes that deviate from this format: truncated,
// inconsistent, or out of range. See [waxerr.Malformed] for the rule that
// divides it from unsupported.
func malformed(format string, args ...any) error {
	return waxerr.Malformed("wmalossless: ", format, args...)
}

// unsupported names a well-formed stream this build does not cover. It is a
// different answer from malformed and carries a different code: the file is
// fine and we are not.
func unsupported(format string, args ...any) error {
	return waxerr.Unsupported("wmalossless: ", format, args...)
}

// Config is the stream shape a WMA Lossless track decodes to. Like WMA v1/v2
// the format carries no in-band configuration: no sync word and no per-frame
// header, so this is the whole of what a decoder knows before it reads a bit.
type Config struct {
	Rate     int
	Channels int
	// BitsPerSample is the valid depth from the extra bytes, 16 or 24. It is
	// NOT wBitsPerSample, which the format leaves as decoration.
	BitsPerSample int
	// Layout is the channel mask from the extra bytes, or the default layout
	// for the channel count when the mask is zero.
	Layout audio.ChannelMask
	// BlockAlign is the container packet size in bytes. It is not a frame
	// size: it is the sole input to two header field widths, so reading it
	// wrong desynchronises every packet rather than producing wrong audio.
	BlockAlign int
	// DecodeFlags is the feature word from the extra bytes.
	DecodeFlags uint16
}

// The derived geometry is methods rather than fields, in the shape wma.Config
// and ape.Config take. A field can be left zero by a caller who builds a
// Config by hand instead of through ParseConfig, and a zero frame length is
// not a refusal but a panic three calls later; a method cannot go out of sync
// with the wire facts it is computed from.

// FrameLenBits is the log2 of the frame length: a ladder on the sample rate,
// then the adjustment the decode flags carry. The adjustment is 0 on every
// stream any encoder writes, so only the ladder is measured.
func (c Config) FrameLenBits() int {
	n := 13
	switch {
	case c.Rate <= 16000:
		n = 9
	case c.Rate <= 22050:
		n = 10
	case c.Rate <= 48000:
		n = 11
	case c.Rate <= 96000:
		n = 12
	}
	switch (c.DecodeFlags & flagFrameLenAdjust) >> shiftFrameLenAdjust {
	case 1:
		n++
	case 2:
		n--
	case 3:
		n -= 2
	}
	return n
}

// SamplesPerFrame is the frame length. Every frame in a stream is this long;
// there is no variable frame length in this format.
func (c Config) SamplesPerFrame() int { return 1 << c.FrameLenBits() }

// MaxSubframes is the deepest tiling a frame may use.
func (c Config) MaxSubframes() int {
	return 1 << (int(c.DecodeFlags&flagSubframeDepth) >> shiftSubframeDepth)
}

// MinSubframeLen is the shortest subframe the tiling can produce, and the unit
// every subframe length is a multiple of.
func (c Config) MinSubframeLen() int { return c.SamplesPerFrame() / c.MaxSubframes() }

// FrameSizeBits is the width of the packet header's continuation count and of
// the optional per-frame length prefix. It is one bit wider than the packet
// needs, which is what lets the count overshoot the payload to mean "all of
// it".
func (c Config) FrameSizeBits() int { return floorLog2(c.BlockAlign) + 4 }

// Validate checks a Config against what this decoder covers. It runs on a
// Config built by hand as well as on one ParseConfig produced, which is the
// point: every bound below is the only thing standing between a crafted or
// careless header and a walk with no layout.
func (c Config) Validate() error {
	switch {
	case c.Rate <= 0 || c.Rate > maxWireField:
		return malformed("sample rate %d", c.Rate)
	case c.BlockAlign <= 0 || c.BlockAlign > maxBlockAlign:
		return malformed("nBlockAlign %d outside 1..%d", c.BlockAlign, maxBlockAlign)
	case c.BitsPerSample != 16 && c.BitsPerSample != 24:
		return unsupported("%d bits per sample (this build decodes 16 and 24)", c.BitsPerSample)
	case c.Channels < 1:
		return malformed("%d channels", c.Channels)
	case c.Channels > audio.MaxChannels:
		return unsupported("%d channels (this build decodes up to %d)", c.Channels, audio.MaxChannels)
	}
	if depth := int(c.DecodeFlags&flagSubframeDepth) >> shiftSubframeDepth; depth > maxSubframeDepth {
		return unsupported("subframe depth %d, so %d subframes a frame", depth, 1<<depth)
	}
	if n := c.FrameLenBits(); n < 1 || 1<<n > maxSamplesPerFrame {
		return malformed("frame length rule gives 2^%d samples, want at most %d", n, maxSamplesPerFrame)
	}
	if c.MinSubframeLen() < 1 {
		return malformed("%d subframes of a %d-sample frame", c.MaxSubframes(), c.SamplesPerFrame())
	}
	return nil
}

// ParseConfig reads a Config from the WAVEFORMATEX plus extra bytes that
// container/asf carries as Track.CodecConfig.
func ParseConfig(b []byte) (Config, error) {
	var c Config
	if len(b) < waveFormatLen {
		return c, malformed("WAVEFORMATEX of %d bytes, want at least %d", len(b), waveFormatLen)
	}
	if tag := le.Uint16(b); tag != tagLossless {
		return c, malformed("wFormatTag 0x%04x is not WMA Lossless", tag)
	}
	extra := b[waveFormatLen:]
	if len(extra) < extraLen {
		return c, malformed("%d codec extra bytes, want at least %d", len(extra), extraLen)
	}

	// Bounded in int64 so acceptance does not depend on the platform's int
	// width: a value past 2^31 would come back negative on a 32-bit build and
	// be refused as damage rather than as the out-of-range value it is.
	rate64 := int64(le.Uint32(b[4:]))
	if rate64 <= 0 || rate64 > maxWireField {
		return c, malformed("sample rate %d", rate64)
	}
	c.Rate = int(rate64)
	c.BlockAlign = int(le.Uint16(b[12:]))
	c.BitsPerSample = int(le.Uint16(extra[extraBitsPerSample:]))
	c.Channels = int(le.Uint16(b[2:]))
	c.DecodeFlags = le.Uint16(extra[extraDecodeFlags:])

	// The count comes from nChannels and only the ORDER from the mask, which is
	// narrower than the notes describe: Windows also writes SPEAKER_ALL
	// (0x80000000), which names no position and counts one bit whatever the
	// stream carries, and overriding from it made a stereo file a mono track
	// that probed playable. A mask that is not positional or does not cover
	// nChannels keeps the guessed layout, as riff does with one it cannot use.
	mask := audio.ChannelMask(le.Uint32(extra[extraChannelMask:]))
	if mask != 0 && mask&^allPositions == 0 && mask.Count() == c.Channels {
		c.Layout = mask
	} else {
		c.Layout = audio.DefaultLayout(c.Channels)
	}
	return c, c.Validate()
}

// allPositions is every speaker position audio.ChannelMask names. A mask with
// a bit outside it is not a positional mask and cannot be a layout.
const allPositions = audio.ChannelMask(1<<18 - 1)

// Format is the pipeline format the decoder emits: the int domain at the
// stream's own width, with the channel order the mask gives.
func (c Config) Format() audio.Format {
	return audio.Format{
		Rate:     c.Rate,
		Channels: c.Channels,
		Layout:   c.Layout,
		Type:     audio.Int,
		BitDepth: c.BitsPerSample,
	}
}

// floorLog2 is the largest n with 2^n <= x, and 0 for x <= 0. The zero case is
// not a convenience: several field widths are computed from it, and a width of
// zero means read no bits and use zero rather than read one bit.
func floorLog2(x int) int {
	if x <= 0 {
		return 0
	}
	return bits.Len(uint(x)) - 1
}

// ceilLog2 is the smallest n with 2^n >= x, and 0 for x <= 1, with the same
// read-no-bits meaning at zero.
func ceilLog2(x int) int {
	if x <= 1 {
		return 0
	}
	return bits.Len(uint(x - 1))
}
