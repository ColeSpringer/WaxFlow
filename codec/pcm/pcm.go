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
	"slices"

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
	// Order maps output channels onto wire channels: output channel c reads
	// wire channel Order[c]. Nil is the identity, which is every container's
	// order except a QuickTime or ISO layout box that lists the file's
	// channels in an order other than the WAVE one. It is a decode-side
	// mapping (the encoder refuses it) and a permutation of 0..len-1, and it
	// is marshaled only when it is not the identity, so a blob with no order
	// is the same five bytes it always was.
	Order []uint8
}

// Equal reports whether two configurations describe the same wire format,
// orders included. Config holds a slice, so it is not comparable with ==.
func (c Config) Equal(o Config) bool {
	return c.Encoding == o.Encoding && c.Bits == o.Bits && c.ValidBits == o.ValidBits &&
		c.BigEndian == o.BigEndian && slices.Equal(c.order(), o.order())
}

// order is Order with a spelled-out identity read as nil, the form
// MarshalBinary writes and ParseConfig returns.
func (c Config) order() []uint8 {
	if c.identityOrder() {
		return nil
	}
	return c.Order
}

// identityOrder reports whether Order is nil or spells out the identity.
func (c Config) identityOrder() bool {
	for i, w := range c.Order {
		if int(w) != i {
			return false
		}
	}
	return true
}

// Validate reports whether the wire configuration is one this package
// packs and unpacks.
func (c Config) Validate() error {
	bad := func(msg string) error {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "pcm: "+msg)
	}
	if c.Order != nil {
		if len(c.Order) < 1 || len(c.Order) > audio.MaxChannels {
			return bad(fmt.Sprintf("channel order lists %d channels (supported: 1..%d)", len(c.Order), audio.MaxChannels))
		}
		var seen [audio.MaxChannels]bool
		for _, w := range c.Order {
			if int(w) >= len(c.Order) || seen[w] {
				return bad(fmt.Sprintf("channel order %v is not a permutation of the wire channels", c.Order))
			}
			seen[w] = true
		}
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

// MarshalBinary encodes the Config for Track.CodecConfig: five bytes, then
// the channel order only when it is not the identity, so the blob (and the
// cache key it is part of, ADR-0004) changes only for a permuted file.
func (c Config) MarshalBinary() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	var flags byte
	if c.BigEndian {
		flags |= 1
	}
	b := []byte{configVersion, byte(c.Encoding), byte(c.Bits), byte(c.ValidBits), flags}
	if !c.identityOrder() {
		b = append(b, c.Order...)
	}
	return b, nil
}

// ParseConfig decodes a Track.CodecConfig produced by MarshalBinary.
func ParseConfig(b []byte) (Config, error) {
	// Five bytes, or five plus a permutation of at least two channels (a
	// one-channel order is the identity and is never written).
	if len(b) < 5 || len(b) == 6 || len(b) > 5+audio.MaxChannels || b[0] != configVersion {
		// Not malformed input: this blob is what MarshalBinary wrote for a
		// demuxer, never bytes off a file, so a version or length that does
		// not round-trip is a Track nobody in this tree built.
		return Config{}, waxerr.New(waxerr.CodeUnsupportedFormat, "pcm: codec config is not one this package wrote")
	}
	c := Config{
		Encoding:  Encoding(b[1]),
		Bits:      int(b[2]),
		ValidBits: int(b[3]),
		BigEndian: b[4]&1 != 0,
	}
	if len(b) > 5 {
		c.Order = slices.Clone(b[5:])
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}
