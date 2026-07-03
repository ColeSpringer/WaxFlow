// Package pcm implements the PCM "codec": the bridge between raw
// interleaved wire bytes inside containers (WAV, AIFF, and later MP4 and
// Matroska PCM tracks) and the pipeline's planar audio.Buffer domain.
//
// The wire encoding lives in Config, marshaled into Track.CodecConfig by
// demuxers so decoding needs no container knowledge. Integer samples cross
// into the pipeline right-justified at their valid bit depth, which is
// what makes lossless round-trips bit-exact by construction.
package pcm

import (
	"fmt"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

// Encoding is the wire sample encoding.
type Encoding uint8

const (
	// SignedInt is two's-complement PCM, the common case.
	SignedInt Encoding = iota
	// UnsignedInt is offset-binary PCM; only 8-bit, the WAV convention.
	UnsignedInt
	// Float is IEEE 754 PCM, 32- or 64-bit. 64-bit wires convert through
	// the pipeline's float32 domain (documented precision loss).
	Float
)

func (e Encoding) String() string {
	switch e {
	case SignedInt:
		return "signed"
	case UnsignedInt:
		return "unsigned"
	case Float:
		return "float"
	default:
		return fmt.Sprintf("Encoding(%d)", uint8(e))
	}
}

// Config describes how PCM samples are packed on the wire. It marshals
// into Track.CodecConfig.
type Config struct {
	Encoding Encoding
	// Bits is the container word size per sample: 8, 16, 24, or 32 for
	// integers; 32 or 64 for floats.
	Bits int
	// ValidBits is the meaningful precision for integers, left-justified
	// in the container word per WAVE_FORMAT_EXTENSIBLE (for example 24
	// valid bits in 32-bit words). Zero means all Bits are valid. Floats
	// require zero.
	ValidBits int
	// BigEndian selects byte order for multi-byte samples (AIFF is
	// big-endian, WAV little-endian).
	BigEndian bool
}

// Validate reports whether the wire configuration is one this package
// packs and unpacks.
func (c Config) Validate() error {
	bad := func(msg string) error {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "pcm: "+msg)
	}
	switch c.Encoding {
	case SignedInt:
		switch c.Bits {
		case 8, 16, 24, 32:
		default:
			return bad(fmt.Sprintf("signed int with %d-bit words", c.Bits))
		}
		if c.ValidBits < 0 || c.ValidBits > c.Bits {
			return bad(fmt.Sprintf("%d valid bits in %d-bit words", c.ValidBits, c.Bits))
		}
	case UnsignedInt:
		if c.Bits != 8 {
			return bad(fmt.Sprintf("unsigned int with %d-bit words (only 8-bit exists in the wild)", c.Bits))
		}
		if c.ValidBits < 0 || c.ValidBits > 8 {
			return bad(fmt.Sprintf("%d valid bits in 8-bit words", c.ValidBits))
		}
	case Float:
		if c.Bits != 32 && c.Bits != 64 {
			return bad(fmt.Sprintf("float with %d-bit words", c.Bits))
		}
		if c.ValidBits != 0 && c.ValidBits != c.Bits {
			return bad("float with partial valid bits")
		}
	default:
		return bad(fmt.Sprintf("unknown encoding %d", c.Encoding))
	}
	return nil
}

// depth is the pipeline bit depth: valid bits for integers, 32 for floats.
func (c Config) depth() int {
	if c.Encoding == Float {
		return 32
	}
	if c.ValidBits != 0 {
		return c.ValidBits
	}
	return c.Bits
}

// shift is how far integer samples are left-justified within the container
// word.
func (c Config) shift() int {
	if c.Encoding == Float || c.ValidBits == 0 {
		return 0
	}
	return c.Bits - c.ValidBits
}

// PCMFormat returns the pipeline format this wire configuration decodes
// to, for a track with the given rate and channel layout.
func (c Config) PCMFormat(rate, channels int, layout audio.ChannelMask) audio.Format {
	t := audio.Int
	if c.Encoding == Float {
		t = audio.Float
	}
	return audio.Format{Rate: rate, Channels: channels, Layout: layout, Type: t, BitDepth: c.depth()}
}

// BytesPerFrame returns the wire size of one frame across channels.
func (c Config) BytesPerFrame(channels int) int {
	return c.Bits / 8 * channels
}

// ContainerBits returns the smallest whole-byte container width holding
// the given number of valid bits (20 valid bits pack into 24-bit words).
// Containers with no separate valid-bits field derive storage this way.
func ContainerBits(validBits int) int {
	return (validBits + 7) / 8 * 8
}

// Version is the PCM encoder's algorithm revision for cache keys
// (ADR-0004). PCM packing has no tunable algorithm, but the constant
// exists from birth like every encoder's: a packing fix must invalidate
// cached outputs.
const Version = "pcm-1"

// configVersion versions the marshaled Config layout.
const configVersion = 1

// MarshalBinary encodes the Config for Track.CodecConfig.
func (c Config) MarshalBinary() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	var flags byte
	if c.BigEndian {
		flags |= 1
	}
	return []byte{configVersion, byte(c.Encoding), byte(c.Bits), byte(c.ValidBits), flags}, nil
}

// ParseConfig decodes a Track.CodecConfig produced by MarshalBinary.
func ParseConfig(b []byte) (Config, error) {
	if len(b) != 5 || b[0] != configVersion {
		return Config{}, waxerr.New(waxerr.CodeUnsupportedFormat, "pcm: malformed codec config")
	}
	c := Config{
		Encoding:  Encoding(b[1]),
		Bits:      int(b[2]),
		ValidBits: int(b[3]),
		BigEndian: b[4]&1 != 0,
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}
