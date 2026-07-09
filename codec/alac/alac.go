// Package alac implements an Apple Lossless (ALAC) decoder. It is a
// clean-room port of Apple's ALAC reference decoder (see
// THIRD-PARTY-NOTICES.md). The algorithm is faithful to
// the reference: adaptive-Golomb residual coding, the cascaded adaptive FIR
// predictor, and the lossless middle-side matrix, so decodes are bit-exact.
//
// The decoder covers mono and stereo elements at 16/20/24/32-bit depths,
// the uncompressed escape path, and the wasted-byte shift. Cookies that
// declare more than two channels are rejected: the WAV channel remap for
// multichannel layouts is not implemented, so decoding them in bitstream
// order would emit a silently wrong layout. SBR-style extensions do not
// exist in ALAC.
package alac

import (
	"fmt"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

// CookieLen is the byte length of an ALACSpecificConfig (the magic cookie),
// the blob carried in container.Track.CodecConfig.
const CookieLen = 24

// maxFrameLength bounds a packet's declared sample count, which sizes the
// decoder's scratch. The reference default is 4096; 16384 leaves headroom
// without letting a crafted cookie force a large allocation.
const maxFrameLength = 16384

// Config is a parsed ALACSpecificConfig.
type Config struct {
	FrameLength   uint32
	BitDepth      int
	PB            uint32 // rice history multiplier tuning
	MB            uint32 // rice initial history
	KB            uint32 // rice k-modifier
	Channels      int
	MaxRun        uint32
	MaxFrameBytes uint32
	AvgBitRate    uint32
	SampleRate    int

	// Cookie is the canonical 24-byte ALACSpecificConfig, retained so the
	// format registry can round-trip the track config to the decoder.
	Cookie []byte
}

func malformed(format string, args ...any) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, "alac: "+fmt.Sprintf(format, args...))
}

// ParseMagicCookie parses the 24-byte ALACSpecificConfig at the head of b
// (trailing channel-layout bytes are ignored).
func ParseMagicCookie(b []byte) (Config, error) {
	if len(b) < CookieLen {
		return Config{}, malformed("magic cookie of %d bytes, want at least %d", len(b), CookieLen)
	}
	c := Config{
		FrameLength:   be32(b[0:]),
		BitDepth:      int(b[5]),
		PB:            uint32(b[6]),
		MB:            uint32(b[7]),
		KB:            uint32(b[8]),
		Channels:      int(b[9]),
		MaxRun:        uint32(be16(b[10:])),
		MaxFrameBytes: be32(b[12:]),
		AvgBitRate:    be32(b[16:]),
		SampleRate:    int(be32(b[20:])),
	}
	switch c.BitDepth {
	case 16, 20, 24, 32:
	default:
		return Config{}, malformed("bit depth %d, want 16/20/24/32", c.BitDepth)
	}
	switch {
	case c.FrameLength == 0 || c.FrameLength > maxFrameLength:
		return Config{}, malformed("frame length %d outside 1..%d", c.FrameLength, maxFrameLength)
	case c.Channels < 1 || c.Channels > 2:
		return Config{}, malformed("channel count %d: only mono and stereo are supported", c.Channels)
	case c.SampleRate <= 0:
		return Config{}, malformed("sample rate %d", c.SampleRate)
	}
	c.Cookie = append([]byte(nil), b[:CookieLen]...)
	return c, nil
}

// Format is the pipeline format the decoder emits for this stream: the
// int domain, right-justified at the stream's bit depth.
func (c Config) Format() audio.Format {
	return audio.Format{
		Rate:     c.SampleRate,
		Channels: c.Channels,
		Layout:   audio.DefaultLayout(c.Channels),
		Type:     audio.Int,
		BitDepth: c.BitDepth,
	}
}

func be16(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
