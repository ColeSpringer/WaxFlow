//go:build !wmavoicetablesgen

// Package wmavoice decodes Windows Media Audio Voice (wFormatTag 0x000A), the
// CELP speech coder of the WMA family. It shares nothing with WMA 1/2, Pro or
// Lossless but the ASF container and the WAVEFORMATEX wrapper.
//
// Everything here is written from docs/notes/wma-voice-bitstream.md and
// docs/notes/wma-voice-oracle-corpus.md, the ADR-0001 analysis artifacts, plus
// the data-only tables beside this file. Section numbers in comments refer to
// the bitstream note.
package wmavoice

import (
	"encoding/binary"
	"math/bits"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

// Version is the decoder's cache-key version constant (ADR-0004): bump on any
// change that alters decoded samples.
const Version = "wmavoice-dec-1"

// The wFormatTags this package decodes. 0x000B is the same coded layer under a
// second registered tag; container/asf declines it, because no reference
// decoder reads it and nothing could check a decode of one.
const (
	tagVoice9  = 0x000A
	tagVoice10 = 0x000B
)

// waveFormatLen is the fixed part of WAVEFORMATEX, up to and including cbSize.
// container/asf hands the whole structure over as Track.CodecConfig, so the
// extra bytes behind it arrive in the same blob.
const waveFormatLen = 18

// extraLen is how many extra bytes this format defines, and it is exact rather
// than a minimum: any other size is a refusal (note 14.2).
const extraLen = 46

// Extra-byte offsets, relative to the start of the extra block. Bytes 0 to 17
// are a WMA Pro extension block, present because a superframe can in principle
// carry a WMA Pro payload, and never read.
const (
	extraFlags = 18 // 4 bytes, little-endian
	extraTree  = 22 // 24 bytes: 17 fields of 3 bits, MSB first, then padding
)

// Flags-word fields (note 1.1). Bits 1, 11, 15 and 16 up carry no meaning for
// a decoder and are not constant across the seven real formats, so they are
// ignored rather than checked.
const (
	flagPostfilter   = 1 << 0
	flagDenoise      = 0x3C // bits 2-5
	flagDenoiseTilt  = 1 << 6
	flagDCLevel      = 0x780 // bits 7-10
	flagLSPOrder16   = 1 << 12
	flagLSPQuantiser = 1 << 13
	flagLSPDefault   = 1 << 14

	shiftDenoise = 2
	shiftDCLevel = 7
)

// Geometry the format fixes.
const (
	// superframeSamples is the decoder's output unit: three frames.
	superframeSamples = 480
	// frameSamples is one frame, which carries one frame type, one LSP set
	// and one to eight blocks.
	frameSamples = 160
	// framesPerSuperframe follows from the two above.
	framesPerSuperframe = superframeSamples / frameSamples
	// halfFrameSamples is the postfilter's unit: a frame runs the chain twice
	// (note 10).
	halfFrameSamples = frameSamples / 2
	// lsp10, lsp16 are the two LSP orders bit 12 selects.
	lsp10 = 10
	lsp16 = 16
	// dcRemovalLevel is the DC noise level above which the DC removal filter
	// runs (note 10.7). No real format reaches it.
	dcRemovalLevel = 8
	// denoiseRows is how many rows the denoise power table has, so a strength
	// of this or more is a refusal (note 14.3).
	denoiseRows = 12
)

// Format bounds, each a shape for which the notes define no behaviour.
const (
	// minRate and maxRate bound the sample rate (note 14.4). Below the floor
	// the minimum pitch rounds to zero and the pitch fields have no range;
	// above the ceiling the excitation history the adaptive codebook needs
	// exceeds the 416 samples the format's own history window holds, which at
	// 22050 Hz it reaches on the nose.
	minRate = 322
	maxRate = 22097
	// maxHistory is that window, and the ceiling above is derived from it.
	maxHistory = 416
	// maxBlockAlign bounds nBlockAlign, which feeds a shift width. Nothing
	// real is within three orders of magnitude of it.
	maxBlockAlign = 1 << 22
	// maxTreePerClass is how many frame types one variable-bit-mode class may
	// hold: a class is a codeword length and each length has three codewords.
	// A fourth is a refusal (note 14.5).
	maxTreePerClass = 3
	// treeSlots covers 8 classes of 3, since the class field is 3 bits.
	treeSlots = 24
	// maxWireField bounds the 32-bit WAVEFORMATEX fields this parser puts in
	// an int, so a crafted file cannot parse to different values on a 32-bit
	// build than on a 64-bit one.
	maxWireField = 1 << 27
)

var le = binary.LittleEndian

// malformed reports bytes that deviate from this format: truncated,
// inconsistent, or out of range. See [waxerr.Malformed] for the rule that
// divides it from unsupported.
func malformed(format string, args ...any) error {
	return waxerr.Malformed("wmavoice: ", format, args...)
}

// unsupported names a well-formed stream this build does not cover. It is a
// different answer from malformed and carries a different code: the file is
// fine and we are not.
func unsupported(format string, args ...any) error {
	return waxerr.Unsupported("wmavoice: ", format, args...)
}

// Config is the stream shape a WMA Voice track decodes to. Like the rest of
// the WMA family the format carries no in-band configuration: no sync word and
// no per-frame codec header, so this is the whole of what a decoder knows
// before it reads a bit.
type Config struct {
	Rate int
	// BlockAlign is the container packet size in bytes. It is not a frame size
	// and has no arithmetic relation to one: it is the sole input to the
	// spillover field's width, so reading it wrong desynchronises every packet
	// rather than producing wrong audio.
	BlockAlign int
	// Flags is the feature word at extra offset 18. It selects the postfilter,
	// the denoise strength and tilt, the DC noise level, the LSP order and the
	// two LSP mode bits.
	Flags uint32
	// Tree maps a frame type VLC symbol onto a frame type, and it comes from
	// the extradata rather than from the format: that is what "variable bit
	// mode" means here. An entry of -1 is a symbol no real stream produces and
	// decoding one is a refusal.
	Tree [treeSlots]int8
}

// The derived geometry is methods rather than fields, in the shape
// wmapro.Config takes. A field can be left zero by a caller who builds a
// Config by hand instead of through ParseConfig, and a zero pitch bound is not
// a refusal but a division by zero three calls later.

// LSPs is the line spectral frequency count, 10 or 16.
func (c Config) LSPs() int {
	if c.Flags&flagLSPOrder16 != 0 {
		return lsp16
	}
	return lsp10
}

// Postfilter reports whether the postfilter of note 10 runs. It is set on
// every format Windows' encoder offers, so a decoder that skips it is wrong on
// every real file rather than on some.
func (c Config) Postfilter() bool { return c.Flags&flagPostfilter != 0 }

// DenoiseStrength is the row of the denoise power table, 0 to 11.
func (c Config) DenoiseStrength() int { return int(c.Flags&flagDenoise) >> shiftDenoise }

// DenoiseTilt reports whether the tilt filter is applied to the denoise
// impulse response.
func (c Config) DenoiseTilt() bool { return c.Flags&flagDenoiseTilt != 0 }

// DCLevel is the DC noise level, 0 to 15. The DC removal filter runs only
// above dcRemovalLevel, which no real format reaches.
func (c Config) DCLevel() int { return int(c.Flags&flagDCLevel) >> shiftDCLevel }

// lspInterp is the inter-frame interpolation table the quantiser mode selects.
func (c Config) lspQuantiserB() bool { return c.Flags&flagLSPQuantiser != 0 }

// meanIndex is the mean LSF vector the default mode selects.
func (c Config) meanIndex() int {
	if c.Flags&flagLSPDefault != 0 {
		return 1
	}
	return 0
}

// geometry is everything the frame layer needs, and note 2 derives all of it
// from the sample rate and nBlockAlign with no per-rate table and no per-rate
// special case.
type geometry struct {
	minPitch, maxPitch int
	pitchRange         int
	pitchBits          int
	history            int
	conv               [4]int
	deltaPitchHalf     int
	deltaPitchBits     int
	blockPitchRange    int
	blockPitchBits     int
	spilloverBits      int
}

// geom derives the pitch bounds and every width that hangs off them. The
// intermediates are int64 because the second multiply happens before the
// divide and reaches 2^37 at the top of the admitted rate range.
func (c Config) geom() geometry {
	var g geometry
	g.minPitch = int((int64(c.Rate)<<8/400 + 50) >> 8)
	g.maxPitch = int((int64(c.Rate)<<8*37/2000 + 50) >> 8)
	g.pitchRange = g.maxPitch - g.minPitch
	g.pitchBits = ceilLog2(g.pitchRange)
	g.history = g.maxPitch + 8
	g.conv = [4]int{
		g.minPitch,
		(g.pitchRange * 25) >> 6,
		(g.pitchRange * 44) >> 6,
		g.maxPitch - 1,
	}
	g.deltaPitchHalf = (g.pitchRange >> 3) &^ 0xF
	g.deltaPitchBits = 1 + ceilLog2(g.deltaPitchHalf)
	g.blockPitchRange = g.conv[2] + g.conv[3] + 1 + 2*(g.conv[1]-2*g.minPitch)
	g.blockPitchBits = ceilLog2(g.blockPitchRange)
	g.spilloverBits = 3 + ceilLog2(c.BlockAlign)
	return g
}

// Validate checks a Config against what this decoder covers. It runs on a
// Config built by hand as well as on one ParseConfig produced, which is the
// point: every bound below is the only thing standing between a crafted or
// careless header and a walk with no layout.
func (c Config) Validate() error {
	switch {
	case c.Rate <= 0 || c.Rate > maxWireField:
		return malformed("sample rate %d", c.Rate)
	case c.Rate < minRate || c.Rate > maxRate:
		return unsupported("sample rate %d (this format admits %d to %d)", c.Rate, minRate, maxRate)
	case c.BlockAlign <= 0:
		return malformed("nBlockAlign %d", c.BlockAlign)
	case c.BlockAlign > maxBlockAlign:
		return unsupported("nBlockAlign %d (this build decodes up to %d)", c.BlockAlign, maxBlockAlign)
	case c.DenoiseStrength() >= denoiseRows:
		return unsupported("denoise strength %d (the table has %d rows)", c.DenoiseStrength(), denoiseRows)
	}
	g := c.geom()
	// Both of these are stated as requirements by note 2 and both bite well
	// above the note's own rate floor: the delta-pitch half-range is zero
	// until the pitch range reaches 128, which happens just under 8 kHz. The
	// floor above is the one the format states; this is the one the derived
	// widths need, and a rate between them would decode a delta-pitch field
	// of no width.
	if g.pitchRange <= 0 {
		return unsupported("sample rate %d leaves a pitch range of %d", c.Rate, g.pitchRange)
	}
	if g.deltaPitchHalf <= 0 {
		return unsupported("sample rate %d leaves a delta-pitch half-range of %d, so the per-block "+
			"pitch field has no width", c.Rate, g.deltaPitchHalf)
	}
	if g.history > maxHistory {
		return unsupported("sample rate %d needs %d samples of excitation history, want at most %d",
			c.Rate, g.history, maxHistory)
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
	tag := le.Uint16(b)
	if tag == tagVoice10 {
		// mmreg.h's WAVE_FORMAT_WMAVOICE10, the same coded layer under a second
		// tag. No reference decoder reads it (ffmpeg 8.0.1 reports an unknown
		// codec), so nothing could check a decode of one, and container/asf
		// refuses it by the same name.
		return c, unsupported("wFormatTag 0x000B is Windows Media Audio Voice 10, which no reference decoder reads")
	}
	if tag != tagVoice9 {
		return c, malformed("wFormatTag 0x%04x is not WMA Voice", tag)
	}
	// Exact, not a minimum, on both the field and the bytes behind it. The
	// reference refuses any other size by name and this format defines no
	// shorter or longer extension. container/asf slices the blob by cbSize, so
	// on that path the two checks agree by construction; another producer of
	// a CodecConfig gets the field checked here.
	if cb := int(le.Uint16(b[16:])); cb != extraLen {
		return c, malformed("cbSize declares %d codec extra bytes, want exactly %d", cb, extraLen)
	}
	extra := b[waveFormatLen:]
	if len(extra) != extraLen {
		return c, malformed("%d codec extra bytes, want exactly %d", len(extra), extraLen)
	}
	if ch := le.Uint16(b[2:]); ch != 1 {
		return c, unsupported("%d channels; this format is mono", ch)
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
	c.Flags = le.Uint32(extra[extraFlags:])

	// nAvgBytesPerSec and wBitsPerSample are deliberately not read: this codec
	// emits float and the bit rate is nBlockAlign's to state. nChannels is
	// read only to refuse: the bitstream carries one channel whatever the
	// header claims, and a header claiming more describes a file that does
	// not exist.

	if err := c.buildTree(extra[extraTree:]); err != nil {
		return c, err
	}
	return c, c.Validate()
}

// buildTree reads the seventeen 3-bit class fields and turns them into the
// symbol-to-frame-type map of note 1.2. A class is the frame type's codeword
// LENGTH, so class c owns the three symbols 3c, 3c+1 and 3c+2, and a fourth
// entry in one class is a refusal: the reference lets it overwrite the next
// class's first slot, which no real extradata provokes.
func (c *Config) buildTree(b []byte) error {
	for i := range c.Tree {
		c.Tree[i] = -1
	}
	var count [8]int
	var r bitReader
	r.reset(b)
	for n := range frameTypes {
		cls := int(r.bits(3))
		if count[cls] >= maxTreePerClass {
			return unsupported("variable bit mode class %d holds more than %d frame types",
				cls, maxTreePerClass)
		}
		c.Tree[3*cls+count[cls]] = int8(n)
		count[cls]++
	}
	return r.err
}

// Format is the pipeline format the decoder emits: one channel of float32 at a
// nominal full scale of 1.0. The format is mono by construction rather than
// from nChannels, because nothing in this bitstream can carry a second channel.
func (c Config) Format() audio.Format {
	return audio.Format{
		Rate:     c.Rate,
		Channels: 1,
		Layout:   audio.DefaultLayout(1),
		Type:     audio.Float,
		BitDepth: 32,
	}
}

// SeekRunUp is how far before its target a seek lands, in samples: 64
// superframes. A resumed decode converges rather than lands exact, since the
// adaptive codebook is a copy of the decoder's own past output, and measured
// over every media object of every cell the worst convergence is 56
// superframes. The notes say that figure is not a bound, so this is the
// corpus's worst case with headroom rather than a guarantee: the engine's
// seek is sample-exact on every cell, and the codec-level test asserts
// convergence rather than a pre-roll.
const SeekRunUp = 64 * superframeSamples

// SamplesPerSuperframe is the grid a decode resumes on, which container/asf snaps a
// landing to. It is the superframe and not the 160-sample frame: a resumed
// decode begins at the first superframe that starts in the packet.
func (c Config) SamplesPerSuperframe() int { return superframeSamples }

// ceilLog2 is the smallest n with 2^n >= x, and 0 for x <= 1. The zero case is
// not a convenience: two field widths are computed from it, and a width of
// zero means read no bits and use zero rather than read one bit.
func ceilLog2(x int) int {
	if x <= 1 {
		return 0
	}
	return bits.Len(uint(x - 1))
}

// floorLog2 is the largest n with 2^n <= x, for x > 0. The postfilter's pulse
// mask walk uses it to find the lowest free position in a 16-bit word.
func floorLog2(x uint32) int { return bits.Len32(x) - 1 }
