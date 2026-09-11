//go:build !wmaprotablesgen

package wmapro

import (
	"encoding/binary"
	"math/bits"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

// Version is the decoder's cache-key version constant (ADR-0004): bump on any
// change that alters decoded samples.
const Version = "wmapro-dec-1"

// tagPro is the wFormatTag this package decodes. The tags it refuses belong to
// container/asf's asfCodecID table, which names them before a track is wired.
const tagPro = 0x0162

// waveFormatLen is the fixed part of WAVEFORMATEX, up to and including cbSize.
// container/asf hands the whole structure over as Track.CodecConfig, so the
// extra bytes behind it arrive in the same blob.
const waveFormatLen = 18

// extraLen is how many extra bytes this format defines. Every real stream
// carries exactly this many.
const extraLen = 18

// Extra-byte offsets, relative to the start of the extra block.
const (
	extraBitsPerSample = 0  // 2 bytes: 16 or 24, and NOT wBitsPerSample
	extraChannelMask   = 2  // 4 bytes: WAVEFORMATEXTENSIBLE bit assignments
	extraDecodeFlags   = 14 // 2 bytes
	extraLowBitRate    = 16 // 2 bytes, and a non-zero value is a refusal
)

// Decode-flags bits. Bit 0 and bits 8 to 15 carry no meaning for a decoder;
// they are read as part of the word and ignored.
const (
	flagFrameLenAdjust = 0x0006 // bits 1-2, the frame-length adjustment
	flagSubframeDepth  = 0x0038 // bits 3-5, maxSubframes = 1 << value
	flagLengthPrefix   = 0x0040 // each frame opens with its own length in bits
	flagDRC            = 0x0080 // each frame carries an 8-bit gain, discarded

	shiftFrameLenAdjust = 1
	shiftSubframeDepth  = 3
)

// Format bounds. Each is a shape for which the notes define no layout, and
// each has to be checked here because container/asf validates none of them.
const (
	// maxSamplesPerFrame bounds the frame-length rule. The rule's rate ladder
	// tops out at 2^13 and the flags can add one more, which nothing can
	// produce or check.
	maxSamplesPerFrame = 8192
	// maxSubframeDepth is the largest subframe-depth field, so at most 32
	// subframes per channel per frame.
	maxSubframeDepth = 5
	// minMinSubframeLen is the shortest subframe the tiling may produce. Below
	// it the band walk has nothing to lay out.
	minMinSubframeLen = 64
	// maxFrameSizeBits bounds floorLog2(nBlockAlign)+4, the width of the
	// packet header's continuation count and of every frame's length prefix.
	maxFrameSizeBits = 25
	// maxSubframesPerChannel bounds the tiling walk. It is maxSubframes at the
	// deepest depth, and a channel that exceeds it is a broken frame.
	maxSubframesPerChannel = 32
	// maxFramesPerPacket bounds how many frames one packet may yield. Real
	// packets hold one to ten (measured across the corpus); a crafted one
	// whose length prefixes all read as the minimum could otherwise spin. A
	// reader's choice, not a format constant.
	maxFramesPerPacket = 256
	// maxCarryBytes bounds the cross-packet carry. A frame legitimately spans
	// more than two packets, so this is generous; past it the stream is
	// refused rather than allowed to grow a buffer without bound.
	maxCarryBytes = 1 << 20
	// maxWireField bounds the 32-bit WAVEFORMATEX fields this parser puts in
	// an int, so a crafted file cannot parse to different values on a 32-bit
	// build than on a 64-bit one. Nothing real is near it.
	maxWireField = 1 << 27
	// maxRate is the sample rate ceiling. The frame-length rule's top arm is
	// open-ended, and Windows offers nothing above 96 kHz; at four times that
	// every subframe size still lays out a band, so a config Validate accepts
	// is one NewDecoder can build, which is what keeps a probe from promising
	// a decode that then fails.
	maxRate = 384000
)

var le = binary.LittleEndian

// malformed reports bytes that deviate from this format: truncated,
// inconsistent, or out of range. See [waxerr.Malformed] for the rule that
// divides it from unsupported.
func malformed(format string, args ...any) error {
	return waxerr.Malformed("wmapro: ", format, args...)
}

// unsupported names a well-formed stream this build does not cover. It is a
// different answer from malformed and carries a different code: the file is
// fine and we are not.
func unsupported(format string, args ...any) error {
	return waxerr.Unsupported("wmapro: ", format, args...)
}

// Config is the stream shape a WMA Pro track decodes to. Like the rest of the
// WMA family the format carries no in-band configuration: no sync word and no
// per-frame codec header, so this is the whole of what a decoder knows before
// it reads a bit.
type Config struct {
	Rate     int
	Channels int
	// BitsPerSample is the valid depth from the extra bytes, 16 or 24. It is
	// NOT wBitsPerSample, which the format leaves as decoration. It feeds two
	// unrelated things that must both use it: the quantisation base and the
	// transform's output normalisation.
	BitsPerSample int
	// Layout is the channel mask from the extra bytes, or the default layout
	// for the channel count when the mask is zero or not positional.
	Layout audio.ChannelMask
	// BlockAlign is the container packet size in bytes. It is not a frame size
	// and has no arithmetic relation to one: it is the sole input to two
	// header field widths, so reading it wrong desynchronises every packet
	// rather than producing wrong audio.
	BlockAlign int
	// DecodeFlags is the feature word from the extra bytes.
	DecodeFlags uint16
}

// The derived geometry is methods rather than fields, in the shape wma.Config
// and wmalossless.Config take. A field can be left zero by a caller who builds
// a Config by hand instead of through ParseConfig, and a zero frame length is
// not a refusal but a panic three calls later; a method cannot go out of sync
// with the wire facts it is computed from.

// FrameLenBits is the log2 of the frame length: a ladder on the sample rate,
// then the adjustment the decode flags carry. The adjustment is 0 on every
// stream any encoder writes, so only the ladder is measured.
//
// The 32000 arm that appears in the WMA v1 rule does not exist here: at 32 kHz
// this codec uses 2048-sample frames.
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

// SamplesPerFrame is the frame length. Every frame in a stream is this long.
func (c Config) SamplesPerFrame() int { return 1 << c.FrameLenBits() }

// MaxSubframes is the deepest tiling a frame may use.
func (c Config) MaxSubframes() int { return 1 << c.subframeDepth() }

func (c Config) subframeDepth() int {
	return int(c.DecodeFlags&flagSubframeDepth) >> shiftSubframeDepth
}

// MinSubframeLen is the shortest subframe the tiling can produce, and the unit
// every subframe length is a multiple of.
func (c Config) MinSubframeLen() int { return c.SamplesPerFrame() / c.MaxSubframes() }

// FrameSizeBits is the width of the packet header's continuation count and of
// every frame's length prefix, both from nBlockAlign alone.
func (c Config) FrameSizeBits() int { return floorLog2(c.BlockAlign) + 4 }

// sizeCount is how many subframe sizes the tiling can produce, so how many
// rows every per-size table has.
func (c Config) sizeCount() int { return c.subframeDepth() + 1 }

// hasLengthPrefix reports whether every frame opens with its own length.
func (c Config) hasLengthPrefix() bool { return c.DecodeFlags&flagLengthPrefix != 0 }

// hasDRC reports whether every frame carries the 8-bit gain a decoder reads
// and discards.
func (c Config) hasDRC() bool { return c.DecodeFlags&flagDRC != 0 }

// Validate checks a Config against what this decoder covers. It runs on a
// Config built by hand as well as on one ParseConfig produced, which is the
// point: every bound below is the only thing standing between a crafted or
// careless header and a walk with no layout.
func (c Config) Validate() error {
	switch {
	case c.Rate <= 0 || c.Rate > maxWireField:
		return malformed("sample rate %d", c.Rate)
	case c.Rate > maxRate:
		return unsupported("sample rate %d (this build decodes up to %d)", c.Rate, maxRate)
	case !c.hasLengthPrefix():
		// The unprefixed frame layer, where a frame ends at the first set
		// bit, has no fixture anywhere; refused here so that the container
		// declines the track rather than every packet failing to decode.
		return unsupported("frames with no length prefix")
	case c.BlockAlign <= 0:
		return malformed("nBlockAlign %d", c.BlockAlign)
	case c.BitsPerSample != 16 && c.BitsPerSample != 24:
		return unsupported("%d bits per sample (this build decodes 16 and 24)", c.BitsPerSample)
	case c.Channels < 1:
		return malformed("%d channels", c.Channels)
	case c.Channels > audio.MaxChannels:
		return unsupported("%d channels (this build decodes up to %d)", c.Channels, audio.MaxChannels)
	}
	if n := c.FrameSizeBits(); n > maxFrameSizeBits {
		return malformed("nBlockAlign %d gives a %d-bit frame size field, want at most %d",
			c.BlockAlign, n, maxFrameSizeBits)
	}
	if d := c.subframeDepth(); d > maxSubframeDepth {
		return unsupported("subframe depth %d, so %d subframes a frame", d, 1<<d)
	}
	n := c.FrameLenBits()
	if n < 1 || 1<<n > maxSamplesPerFrame {
		return unsupported("the frame-length rule gives 2^%d samples, want at most %d",
			n, maxSamplesPerFrame)
	}
	if c.MinSubframeLen() < minMinSubframeLen {
		return malformed("%d subframes of a %d-sample frame leaves %d samples each, want at least %d",
			c.MaxSubframes(), c.SamplesPerFrame(), c.MinSubframeLen(), minMinSubframeLen)
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
	if tag := le.Uint16(b); tag != tagPro {
		return c, malformed("wFormatTag 0x%04x is not WMA Pro", tag)
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
	c.DecodeFlags = le.Uint16(extra[extraDecodeFlags:])

	// The low-bit-rate tool, refused before a bit of bitstream is read. The
	// only reference decoder in reach never reads this word, so it decodes
	// such a stream silently wrong: measured against Windows' own decoder on
	// the two cells that carry it, the reference is 61% and 94% wrong overall
	// and still 9% and 91% wrong below 6 kHz, with the coded spectrum stopping
	// at half the subframe. There is no oracle for these streams here, so a
	// decode of one would be plausible audio nothing could check.
	//
	// The predicate is "non-zero" and not a bit test. Only three values exist
	// in the encoder's whole enumeration (0x0000, 0xc042, 0x20c6); the two
	// non-zero ones share bits 1 and 6, but nothing establishes that those are
	// the enabling bits, so a narrower test would be a guess.
	if w := le.Uint16(extra[extraLowBitRate:]); w != 0 {
		return c, unsupported("the stream sets the low-bit-rate tool (extra word %#04x); "+
			"no decoder outside Windows implements it", w)
	}

	// The count comes from nChannels and the mask supplies only the layout,
	// and only when it is positional and covers exactly that count. That is
	// narrower than the notes describe, which have the mask's population
	// count override nChannels, and it is the lesson codec/wmalossless
	// learned: Windows also writes SPEAKER_ALL (0x80000000), which names no
	// position and counts one bit whatever the stream carries, and taking a
	// count from it turned a stereo file into a mono track that probed
	// playable and died mid-frame. On every stream any encoder writes the two
	// agree, so the narrowing is invisible there.
	c.Channels = int(le.Uint16(b[2:]))
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

// Format is the pipeline format the decoder emits. The transform normalises to
// a nominal full scale of 1.0 at both depths, so the float domain is the
// codec's own and the depth is carried for the caller rather than applied.
func (c Config) Format() audio.Format {
	return audio.Format{
		Rate:     c.Rate,
		Channels: c.Channels,
		Layout:   c.Layout,
		Type:     audio.Float,
		BitDepth: 32,
	}
}

// floorLog2 is the largest n with 2^n <= x, and 0 for x <= 0. The zero case is
// not a convenience: two field widths are computed from it, and a width of
// zero means read no bits and use zero rather than read one bit.
func floorLog2(x int) int {
	if x <= 0 {
		return 0
	}
	return bits.Len(uint(x)) - 1
}
