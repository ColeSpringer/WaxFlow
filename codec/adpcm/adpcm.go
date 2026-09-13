// Package adpcm decodes the two 4-bit ADPCM families that fill WAV, AIFF-C
// and QuickTime files: IMA/DVI ADPCM in its two block layouts, and Microsoft
// ADPCM.
//
// Both code a sample as a nibble scaling an adaptive step, and both reset
// that adaptation at a block boundary so a block is independently decodable.
// What differs is everything around it: IMA carries a step INDEX and one
// predictor, Microsoft carries a step size and a second-order predictor whose
// coefficients the file supplies, and the two IMA layouts disagree about the
// block header, the nibble order, the channel interleave, and even the
// arithmetic that turns a nibble into a difference (see ima.go).
//
// The layout rides in Config rather than in the codec ID because nothing
// outside the container names the IMA layouts apart: one WAVE format tag and
// one AIFF-C compression type spell the same codec two ways.
package adpcm

import (
	"encoding/binary"
	"fmt"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

// Layout names a block layout. The two IMA layouts share a predictor and
// nothing else; MS is a different codec under a different ID.
type Layout uint8

const (
	// IMAWav is the Microsoft WAV layout of IMA ADPCM (format tag 0x0011):
	// a 4-byte header per channel whose predictor is also the block's first
	// sample, then 4-byte words of nibbles round-robin across channels.
	IMAWav Layout = iota
	// IMAQuickTime is Apple's 'ima4' layout: 34 bytes per channel holding a
	// 2-byte header and 64 samples, with no header sample.
	IMAQuickTime
	// MS is Microsoft ADPCM (format tag 0x0002).
	MS
)

func (l Layout) String() string {
	switch l {
	case IMAWav:
		return "IMA ADPCM (WAV layout)"
	case IMAQuickTime:
		return "IMA ADPCM (QuickTime layout)"
	case MS:
		return "MS ADPCM"
	default:
		return fmt.Sprintf("Layout(%d)", uint8(l))
	}
}

// Versions are the decoders' algorithm revisions for cache keys (ADR-0004),
// one per codec ID. The two IMA layouts share a constant because they share
// an ID: a fix to either invalidates cached decodes of both, which is the
// conservative direction.
const (
	IMAVersion = "ima-adpcm-dec-1"
	MSVersion  = "ms-adpcm-dec-1"
)

// SourceBitDepth is what both families store a sample in, which is what
// probe reports beside the 16-bit decoded depth and what ffprobe puts in
// bits_per_sample.
const SourceBitDepth = 4

// QuickTime geometry. Apple's 'ima4' has no fields to state it: a packet is
// always 34 bytes per channel (a 2-byte header and 32 bytes of nibbles) and
// always 64 samples.
const (
	QuickTimeBlockBytes  = 34
	QuickTimeBlockFrames = 64
)

// The per-channel block header widths each layout carries.
const (
	imaWavHeaderBytes    = 4 // predictor(2) stepIndex(1) reserved(1)
	msHeaderBytes        = 7 // predictorIndex(1) delta(2) sample1(2) sample2(2)
	quickTimeHeaderBytes = 2 // predictor's top nine bits and a step index
)

// msMaxChannels is what Microsoft ADPCM defines: the nibbles alternate
// channels one at a time, which the layout only spells for mono and stereo.
const msMaxChannels = 2

// Config describes a track's block geometry, which is all a decoder needs
// beyond the layout. It marshals into Track.CodecConfig.
type Config struct {
	Layout   Layout
	Channels int
	// BlockAlign is one block's size in bytes across all channels, and
	// SamplesPerBlock the frames it decodes to. Both are stated by the
	// container (nBlockAlign and wSamplesPerBlock) and both are derivable
	// from the layout, which is what BlockBytes and BlockFrames compute; a
	// demuxer resolves the disagreement before it gets here.
	BlockAlign      int
	SamplesPerBlock int
	// Coefs is the MS predictor's coefficient table, read from the file
	// rather than assumed: the specification says a stream carries its own,
	// and DefaultCoefs is only what almost every encoder writes.
	Coefs [7][2]int16
}

// BlockBytes returns one block's size in bytes across all channels, which is
// the unit every container addresses these codecs by. It is BlockAlign, and
// exists as a name rather than as a computation: Validate is what pins the
// QuickTime layout's to the 34 bytes per channel its fourcc fixes, so after
// validation the field is the geometry.
func (c Config) BlockBytes() int { return c.BlockAlign }

// BlockFrames returns the frames one block decodes to, derived from the
// layout and the block size rather than from the declared count.
//
// The IMA WAV layout is 4-byte words taken round-robin across the channels,
// and a block holds whole rounds only. That is measured rather than read off
// the format's own samples-per-block formula, which says
// (blockAlign - 4*ch)*2/ch + 1 and disagrees whenever the body is not a whole
// number of rounds: ffmpeg 8.0.1 decodes 9 frames from a 10-byte mono block
// where the formula says 13, and 1009 from a 1020-byte stereo block where it
// says 1013. The two agree at every block align an encoder actually writes,
// which is what makes the disagreement easy to miss.
func (c Config) BlockFrames() int {
	if c.Channels < 1 {
		// No geometry to derive, and the divisions below would trap. Validate
		// reports it; this only has to reach there without crashing.
		return 0
	}
	switch c.Layout {
	case IMAQuickTime:
		return QuickTimeBlockFrames
	case MS:
		body := c.BlockAlign - msHeaderBytes*c.Channels
		if body < 0 {
			return 0
		}
		// Two nibbles a byte, dealt one at a time across the channels, and
		// the two samples the header states come first.
		return body*2/c.Channels + 2
	default:
		body := c.BlockAlign - imaWavHeaderBytes*c.Channels
		if body < 0 {
			return 0
		}
		word := 4 * c.Channels
		return body/word*8 + 1
	}
}

// HeaderBytes returns the per-channel block header the layout carries, which
// is the floor a block size has to clear. Exported because the containers
// check that against a file's own field and say so in their own words, rather
// than letting this package's Validate speak to a user about a WAV.
func (c Config) HeaderBytes() int {
	switch c.Layout {
	case IMAQuickTime:
		return quickTimeHeaderBytes
	case MS:
		return msHeaderBytes
	default:
		return imaWavHeaderBytes
	}
}

// Validate reports whether the geometry is one this package decodes.
func (c Config) Validate() error {
	bad := func(format string, args ...any) error {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "adpcm: "+fmt.Sprintf(format, args...))
	}
	switch c.Layout {
	case IMAWav, IMAQuickTime, MS:
	default:
		return bad("unknown block layout %d", uint8(c.Layout))
	}
	if c.Channels < 1 || c.Channels > audio.MaxChannels {
		return bad("%d channels (supported: 1..%d)", c.Channels, audio.MaxChannels)
	}
	if c.Layout == MS && c.Channels > msMaxChannels {
		return bad("%d channels of MS ADPCM, whose nibbles alternate between at most %d", c.Channels, msMaxChannels)
	}
	if c.Layout == IMAQuickTime {
		// The fourcc fixes the whole geometry: 34 bytes and 64 samples per
		// channel, with no field anywhere to state anything else.
		if want := QuickTimeBlockBytes * c.Channels; c.BlockAlign != want {
			return bad("%d-byte blocks, but the %v is %d bytes per channel", c.BlockAlign, c.Layout, QuickTimeBlockBytes)
		}
	} else {
		if c.BlockAlign < c.HeaderBytes()*c.Channels {
			return bad("%d-byte blocks hold no %v header for %d channels", c.BlockAlign, c.Layout, c.Channels)
		}
	}
	if want := c.BlockFrames(); c.SamplesPerBlock != want {
		return bad("%d samples per block, but %d bytes of %v hold %d", c.SamplesPerBlock, c.BlockAlign, c.Layout, want)
	}
	if c.Layout == MS {
		return nil
	}
	if c.Coefs != ([7][2]int16{}) {
		return bad("%v carries a coefficient table, which only MS ADPCM has", c.Layout)
	}
	return nil
}

// Format returns the pipeline format an ADPCM track decodes to. Both
// families reconstruct into signed 16-bit samples, which is the domain their
// predictors clamp to.
func (c Config) Format(rate int, layout audio.ChannelMask) audio.Format {
	return audio.Format{Rate: rate, Channels: c.Channels, Layout: layout, Type: audio.Int, BitDepth: 16}
}

// configVersion versions the marshaled Config layout.
const configVersion = 1

// The marshaled sizes: the common prefix, and the MS form with its table.
const (
	configLen   = 9
	configLenMS = configLen + 7*4
)

// MarshalBinary encodes the Config for Track.CodecConfig. The coefficient
// table appears only for MS, so an IMA track's cache key does not carry 28
// bytes of zeros.
func (c Config) MarshalBinary() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	b := make([]byte, configLen, configLenMS)
	b[0] = configVersion
	b[1] = byte(c.Layout)
	b[2] = byte(c.Channels)
	binary.LittleEndian.PutUint16(b[3:], uint16(c.BlockAlign))
	// Four bytes, not two: the block size is a uint16 field in every
	// container that states one, and a mono IMA block of 65535 bytes decodes
	// to 131063 frames. Through a uint16 that stored as 14457, and the
	// mismatch surfaced as a decoder refusal on a track probe had already
	// reported as healthy.
	binary.LittleEndian.PutUint32(b[5:], uint32(c.SamplesPerBlock))
	if c.Layout == MS {
		for _, pair := range c.Coefs {
			b = binary.LittleEndian.AppendUint16(b, uint16(pair[0]))
			b = binary.LittleEndian.AppendUint16(b, uint16(pair[1]))
		}
	}
	return b, nil
}

// ParseConfig decodes a Track.CodecConfig produced by MarshalBinary.
func ParseConfig(b []byte) (Config, error) {
	notOurs := func() (Config, error) {
		// Not malformed input: this blob is what MarshalBinary wrote for a
		// demuxer, never bytes off a file, so a version or length that does
		// not round-trip is a Track nobody in this tree built.
		return Config{}, waxerr.New(waxerr.CodeUnsupportedFormat, "adpcm: codec config is not one this package wrote")
	}
	if len(b) < configLen || b[0] != configVersion {
		return notOurs()
	}
	c := Config{
		Layout:          Layout(b[1]),
		Channels:        int(b[2]),
		BlockAlign:      int(binary.LittleEndian.Uint16(b[3:])),
		SamplesPerBlock: int(binary.LittleEndian.Uint32(b[5:])),
	}
	switch {
	case c.Layout == MS && len(b) == configLenMS:
		for i := range c.Coefs {
			c.Coefs[i][0] = int16(binary.LittleEndian.Uint16(b[configLen+4*i:]))
			c.Coefs[i][1] = int16(binary.LittleEndian.Uint16(b[configLen+4*i+2:]))
		}
	case c.Layout != MS && len(b) == configLen:
	default:
		return notOurs()
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}
