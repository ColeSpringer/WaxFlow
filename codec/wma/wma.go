//go:build !wmatablesgen

package wma

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

// Version is the decoder's cache-key version constant (ADR-0004): bump on any
// change that alters decoded samples.
const Version = "wma-dec-2"

// The wFormatTag values this package decodes. The tags it refuses belong to
// container/asf's asfCodecID table, which names them before a track is ever
// wired, so nothing here has to repeat that vocabulary.
const (
	tagV1 = 0x0160
	tagV2 = 0x0161
)

// waveFormatLen is the fixed part of WAVEFORMATEX, up to and including cbSize.
// container/asf hands the whole structure over as Track.CodecConfig, so the
// extra bytes behind it arrive in the same blob.
const waveFormatLen = 18

// MaxRate is the highest sample rate this decoder accepts. Encoders stop at 48
// kHz; the extra headroom matches what derived decoders accept, and past it the
// frame-length rule and the band layout stop describing anything real.
const MaxRate = 50000

// flags2 bits, from the codec-specific extra bytes behind WAVEFORMATEX.
const (
	flagExpVLC     = 0x0001 // exponents are VLC-coded deltas, not LSP-coded
	flagReservoir  = 0x0002 // a frame may straddle two superframes
	flagVarBlkLen  = 0x0004 // blocks within a frame may be shorter than the frame
	flagBlkDepth   = 0x0018 // block-size depth, ((flags2>>3)&3)+1
	flagBlkDepthSh = 3
)

// badVarBlockFlags is the flags2 word one bad writer emits: it claims variable
// block lengths it never uses, and readers clear the bit for exactly this
// value.
const badVarBlockFlags = 0x000d

// Hostile-input caps.
const (
	// maxCarryBytes bounds the bit-reservoir carry. A frame cannot outlast a
	// handful of superframes in anything real; the cap is a reader's choice,
	// not a format constant, and past it the stream is refused rather than
	// allowed to grow a buffer without bound.
	maxCarryBytes = 32 << 10
	// maxTotalGain bounds the block gain's 127-escape ladder. The ladder is
	// self-terminating on any real stream and the escape-width table stops
	// caring above 45; this ceiling is what keeps 10^(gain/20) and everything
	// downstream of it inside float32 on a crafted one.
	maxTotalGain = 512
	// maxOffsetBits bounds the superframe header's bit-offset field. It is
	// derived from the bit rate, so an absurd rate for the frame length would
	// otherwise ask for a field wider than the bit reader's word.
	maxOffsetBits = 24
)

func malformed(format string, args ...any) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, "wma: "+fmt.Sprintf(format, args...))
}

// unsupported names a stream shape this decoder deliberately does not cover.
// It is the same error as malformed, since a caller can act on neither; the
// two names exist so the call site says whether the file is broken or merely
// outside our scope.
func unsupported(format string, args ...any) error {
	return malformed(format, args...)
}

// Config is the stream shape a WMA track decodes to: everything the codec
// needs, all of it from the WAVEFORMATEX. WMA carries no in-band configuration
// at all -- no sync word, no frame header, no per-frame length -- so this is
// the whole of what a decoder knows before it reads a bit.
type Config struct {
	// V2 distinguishes wFormatTag 0x0161 from 0x0160. Both ride under
	// codec.WMA; this is the discriminator the ID does not carry.
	V2       bool
	Rate     int
	Channels int
	// BitRate is nAvgBytesPerSec*8. It is declared, not recovered from
	// nBlockAlign: the block-align identity floors, so a rate read back from it
	// comes out slightly low, and the coefficient-book and noise thresholds
	// below are knife edges a 0.001 error flips.
	BitRate int
	// BlockAlign is the superframe size in bytes. Every packet is exactly this.
	BlockAlign int
	// Flags2 is the codec-specific feature word.
	Flags2 uint16
}

// Format is the pipeline format the decoder emits. WMA reconstructs in the
// float domain, so the WAVEFORMATEX bit depth (always 16, and a fiction the
// encoder writes rather than a property of the coded data) does not reach it.
func (c Config) Format() audio.Format {
	return audio.Format{
		Rate:     c.Rate,
		Channels: c.Channels,
		Layout:   audio.DefaultLayout(c.Channels),
		Type:     audio.Float,
		BitDepth: 32,
	}
}

func (c Config) expVLC() bool      { return c.Flags2&flagExpVLC != 0 }
func (c Config) reservoir() bool   { return c.Flags2&flagReservoir != 0 }
func (c Config) varBlockLen() bool { return c.Flags2&flagVarBlkLen != 0 }

// ParseConfig reads a WAVEFORMATEX plus its codec-specific extra bytes, which
// is what container/asf carries as Track.CodecConfig.
func ParseConfig(b []byte) (Config, error) {
	if len(b) < waveFormatLen {
		return Config{}, malformed("codec config of %d bytes, want at least the %d-byte WAVEFORMATEX",
			len(b), waveFormatLen)
	}
	tag := binary.LittleEndian.Uint16(b)
	c := Config{
		Rate:       int(binary.LittleEndian.Uint32(b[4:])),
		Channels:   int(binary.LittleEndian.Uint16(b[2:])),
		BitRate:    int(binary.LittleEndian.Uint32(b[8:])) * 8,
		BlockAlign: int(binary.LittleEndian.Uint16(b[12:])),
	}
	switch tag {
	case tagV1:
	case tagV2:
		c.V2 = true
	default:
		return Config{}, unsupported("audio format %#04x is not Windows Media Audio 1 or 2", tag)
	}
	// cbSize can overstate what is present; the fixed fields are what select
	// the codec, so clamp rather than refuse.
	extra := b[waveFormatLen:]
	if cb := int(binary.LittleEndian.Uint16(b[16:])); cb < len(extra) {
		extra = extra[:cb]
	}
	// flags2 sits at a version-dependent offset in the extra bytes. Fewer extra
	// bytes than the version needs means flags2 0, which is LSP exponents and
	// no reservoir, not a broken file.
	at := 2
	if c.V2 {
		at = 4
	}
	if len(extra) >= at+2 {
		c.Flags2 = binary.LittleEndian.Uint16(extra[at:])
	}
	if c.V2 && c.Flags2 == badVarBlockFlags {
		// One writer claims variable block lengths it does not use, always with
		// this exact word.
		c.Flags2 &^= flagVarBlkLen
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate refuses, by name and before any bitstream is read, every shape this
// decoder does not cover. nAvgBytesPerSec and nBlockAlign in particular are
// checked here and nowhere else: container/asf validates neither, and the
// derived quantities below divide by the first and step by the second.
func (c Config) Validate() error {
	switch {
	case c.Channels < 1 || c.Channels > 2:
		return unsupported("%d channels: Windows Media Audio 1 and 2 are mono or stereo", c.Channels)
	case c.Rate <= 0:
		return malformed("sample rate %d", c.Rate)
	case c.Rate > MaxRate:
		return unsupported("sample rate %d above the %d Hz this decoder covers", c.Rate, MaxRate)
	case c.BitRate <= 0:
		return malformed("nAvgBytesPerSec is %d; the frame layout is derived from it", c.BitRate/8)
	case c.BlockAlign <= 0:
		return malformed("nBlockAlign is %d; it is the superframe size", c.BlockAlign)
	case !c.V2 && c.varBlockLen():
		// The two halves of the reference disagree here and agree only because
		// no such file exists: v1's band layout is computed into slot 0 for
		// every block size while the exponent decoder indexes slot k. Refusing
		// beats inventing a layout for a file no encoder writes.
		return unsupported("Windows Media Audio 1 with variable block lengths: no encoder writes it and no layout is defined for it")
	case !c.V2 && c.Channels == 2 && c.reservoir():
		// The same shape of gap, found while building the re-framing test.
		// A stereo v1 block byte-aligns the reader after each channel slot,
		// and an align is relative to the buffer the frame is being read from.
		// Without a reservoir that is always the frame's own start and the
		// question never arises. With one, a frame begins at an arbitrary bit
		// offset in its packet and is completed out of a carry buffer, so the
		// same frame's bits mean two different things depending on where the
		// reservoir happened to put them -- and moving frames is the only
		// thing a reservoir is for. Nothing in docs/notes/wma-bitstream.md
		// settles which grid wins, no encoder here writes the combination, and
		// a named refusal beats decoding it one of the two ways in silence.
		return unsupported("stereo Windows Media Audio 1 with a bit reservoir: its per-channel byte alignment has no defined meaning once a frame can move")
	}
	if _, err := c.offsetBits(); err != nil {
		return err
	}
	return nil
}

// frameLenBits is the whole frame-length rule for v1 and v2. Note the v1
// clause at 32 kHz: v1 frames 1024 samples there where v2 frames 2048, which
// makes that rate the one place the version split is visible without decoding.
func (c Config) frameLenBits() int {
	switch {
	case c.Rate <= 16000:
		return 9
	case c.Rate <= 22050, c.Rate <= 32000 && !c.V2:
		return 10
	default:
		return 11
	}
}

// FrameLen is the number of samples one coded frame produces per channel.
func (c Config) FrameLen() int { return 1 << c.frameLenBits() }

// bps is bits per coded sample per channel, and bps1 the stereo-weighted form
// the coefficient-book and noise ladders threshold on.
func (c Config) bps() float64 {
	return float64(c.BitRate) / float64(c.Channels*c.Rate)
}

func (c Config) bps1() float64 {
	if c.Channels == 2 {
		return c.bps() * 1.6
	}
	return c.bps()
}

// offsetBits is the width of the superframe header's bit-offset field, less
// the fixed three. A bit rate absurd for the frame length overflows the bit
// reader's word here rather than deep inside a packet walk.
func (c Config) offsetBits() (int, error) {
	v := int(c.bps()*float64(c.FrameLen())/8 + 0.5)
	if v < 1 {
		v = 1
	}
	n := bitsOf(v) + 2
	if n+3 > maxOffsetBits {
		return 0, malformed("bit rate %d needs a %d-bit superframe offset field", c.BitRate, n+3)
	}
	return n, nil
}

// bitsOf is floor(log2(v)) for v >= 1.
func bitsOf(v int) int {
	n := 0
	for v > 1 {
		v >>= 1
		n++
	}
	return n
}

// nbBlockSizes is how many block lengths a frame may mix. Without the variable
// block length flag there is exactly one and it is the frame length.
func (c Config) nbBlockSizes() int {
	if !c.varBlockLen() {
		return 1
	}
	nb := int(c.Flags2&flagBlkDepth)>>flagBlkDepthSh + 1
	if c.BitRate/c.Channels >= 32000 {
		nb += 2
	}
	return min(nb, c.frameLenBits()-7) + 1
}

// coefBookPair picks the coefficient book pair from the RAW sample rate and
// bits per sample. Raw matters: at 32 kHz on v2 the raw rate clears the
// threshold and the normalised rate (22050) does not, and the two arms are
// different books.
func (c Config) coefBookPair() int {
	if c.Rate >= 32000 {
		switch b := c.bps1(); {
		case b < 0.72:
			return 0
		case b < 1.16:
			return 1
		}
	}
	return 2
}

// normRate is the rate the noise ladder is selected by: v2 snaps down to the
// nearest tabulated rate, v1 uses the rate as it stands, so 32 and 48 kHz take
// the default arm on v1 and the 22050 and 44100 arms on v2.
func (c Config) normRate() int {
	if !c.V2 {
		return c.Rate
	}
	for _, r := range [...]int{44100, 22050, 16000, 11025, 8000} {
		if c.Rate >= r {
			return r
		}
	}
	return c.Rate
}

// highFreqMult decides noise coding and, when it is on, how far up the
// spectrum ordinary coefficients reach. The multiplier is the only thing the
// ladder produces; a 1 means noise coding is off and the coded span runs to the
// end of the codeable region.
func (c Config) highFreqMult() (mult float64, noise bool) {
	bps, bps1 := c.bps(), c.bps1()
	switch c.normRate() {
	case 44100:
		if bps1 >= 0.61 {
			return 1, false
		}
		return 0.4, true
	case 22050:
		switch {
		case bps1 >= 1.16:
			return 1, false
		case bps1 >= 0.72:
			return 0.7, true
		}
		return 0.6, true
	case 16000:
		if bps > 0.5 {
			return 0.5, true
		}
		return 0.3, true
	case 11025:
		return 0.7, true
	case 8000:
		switch {
		case bps <= 0.625:
			return 0.5, true
		case bps > 0.75:
			return 1, false
		}
		return 0.65, true
	default:
		switch {
		case bps >= 0.8:
			return 0.75, true
		case bps >= 0.6:
			return 0.6, true
		}
		return 0.5, true
	}
}

// coefsStart is the first codeable coefficient: v1 never transmits the lowest
// three bins.
func (c Config) coefsStart() int {
	if c.V2 {
		return 0
	}
	return 3
}

// coefsEnd is the last codeable coefficient for block size k, roughly the low
// 91% of the block; the top is never transmitted.
func (c Config) coefsEnd(k int) int {
	n := c.FrameLen()
	return (n - n*9/100) >> k
}

// noiseMult scales the generated noise table. It is a property of the exponent
// strategy, so it is fixed for the whole stream.
func (c Config) noiseMult() float64 {
	if c.expVLC() {
		return 0.02
	}
	return 0.04
}

// mdctNorm is the transform normalisation folded into the coefficient scale:
// the standard 4/N for an unnormalised inverse of N = 2*blockLen, plus v1's
// extra sqrt(n4) on top of it.
func mdctNorm(blockLen int, v2 bool) float64 {
	n4 := float64(blockLen / 2)
	norm := 1 / n4
	if !v2 {
		norm *= math.Sqrt(n4)
	}
	return norm
}
