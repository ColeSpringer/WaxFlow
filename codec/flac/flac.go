// Package flac implements a FLAC decoder (RFC 9639), written from the
// specification (clean-room Tier A, ADR-0001). It covers the full frame
// syntax: constant, verbatim, fixed, and LPC subframes, Rice-coded
// residuals with escaped partitions, all stereo decorrelation modes,
// wasted bits, and sample depths from 4 to 32 bits (side channels carry
// up to 33 significant bits and are reconstructed in 64-bit arithmetic).
//
// The package also exports the pieces containers need to packetize the
// codec: STREAMINFO parsing, frame-header parsing, and the frame CRC-16,
// so container/flacn can confirm frame boundaries and container/ogg can
// stamp packet positions without duplicating the wire format.
package flac

import (
	"fmt"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

// StreamInfoLen is the byte length of a STREAMINFO metadata block body,
// which is also the codec configuration blob (codec.Encoder.CodecConfig
// shape) carried in container.Track.CodecConfig.
const StreamInfoLen = 34

// MaxBlockSize is the largest legal FLAC block size in samples.
const MaxBlockSize = 65535

// MaxRate is the largest sample rate STREAMINFO's 20-bit field can carry.
const MaxRate = 1<<20 - 1

// StreamInfo is a parsed STREAMINFO metadata block.
type StreamInfo struct {
	// MinBlock and MaxBlock bound the block size in samples. Equal values
	// promise a constant block size, which fixed-strategy frame numbering
	// relies on.
	MinBlock, MaxBlock int
	// MinFrame and MaxFrame bound the coded frame size in bytes; 0 means
	// unknown.
	MinFrame, MaxFrame int
	Rate               int
	Channels           int
	Bits               int
	// Samples is the total sample count, 0 when unknown.
	Samples int64
	// MD5 is the unencoded-audio signature; all zero when unset.
	MD5 [16]byte
}

func malformed(format string, args ...any) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, "flac: "+fmt.Sprintf(format, args...))
}

// ParseStreamInfo parses a 34-byte STREAMINFO block body.
func ParseStreamInfo(b []byte) (StreamInfo, error) {
	var si StreamInfo
	if len(b) != StreamInfoLen {
		return si, malformed("STREAMINFO of %d bytes, want %d", len(b), StreamInfoLen)
	}
	si.MinBlock = int(b[0])<<8 | int(b[1])
	si.MaxBlock = int(b[2])<<8 | int(b[3])
	si.MinFrame = int(b[4])<<16 | int(b[5])<<8 | int(b[6])
	si.MaxFrame = int(b[7])<<16 | int(b[8])<<8 | int(b[9])
	si.Rate = int(b[10])<<12 | int(b[11])<<4 | int(b[12])>>4
	si.Channels = (int(b[12])>>1)&0x7 + 1
	si.Bits = (int(b[12])&0x1)<<4 | int(b[13])>>4
	si.Bits++
	si.Samples = int64(b[13]&0xF)<<32 | int64(b[14])<<24 | int64(b[15])<<16 | int64(b[16])<<8 | int64(b[17])
	copy(si.MD5[:], b[18:])
	switch {
	case si.Rate == 0:
		return si, malformed("STREAMINFO sample rate 0")
	case si.Bits < 4:
		return si, malformed("STREAMINFO bit depth %d, want at least 4", si.Bits)
	case si.MaxBlock < 16:
		return si, malformed("STREAMINFO max block size %d, want at least 16", si.MaxBlock)
	}
	return si, nil
}

// MarshalBinary packs si into the 34-byte STREAMINFO wire form, the
// inverse of ParseStreamInfo. It validates the fields against their wire
// widths (rate is 20 bits, sample counts 36, frame sizes 24) so an
// impossible StreamInfo fails here instead of silently truncating.
func (si StreamInfo) MarshalBinary() ([]byte, error) {
	switch {
	case si.Rate < 1 || si.Rate > MaxRate:
		return nil, malformed("STREAMINFO rate %d outside 1..%d", si.Rate, MaxRate)
	case si.Channels < 1 || si.Channels > 8:
		return nil, malformed("STREAMINFO channels %d outside 1..8", si.Channels)
	case si.Bits < 4 || si.Bits > 32:
		return nil, malformed("STREAMINFO bit depth %d outside 4..32", si.Bits)
	case si.MinBlock < 16 || si.MaxBlock > MaxBlockSize || si.MinBlock > si.MaxBlock:
		return nil, malformed("STREAMINFO block bounds %d..%d invalid", si.MinBlock, si.MaxBlock)
	case si.MinFrame < 0 || si.MinFrame >= 1<<24 || si.MaxFrame < 0 || si.MaxFrame >= 1<<24:
		return nil, malformed("STREAMINFO frame bounds %d..%d overflow 24 bits", si.MinFrame, si.MaxFrame)
	case si.Samples < 0 || si.Samples >= 1<<36:
		return nil, malformed("STREAMINFO sample count %d overflows 36 bits", si.Samples)
	}
	b := make([]byte, StreamInfoLen)
	b[0], b[1] = byte(si.MinBlock>>8), byte(si.MinBlock)
	b[2], b[3] = byte(si.MaxBlock>>8), byte(si.MaxBlock)
	b[4], b[5], b[6] = byte(si.MinFrame>>16), byte(si.MinFrame>>8), byte(si.MinFrame)
	b[7], b[8], b[9] = byte(si.MaxFrame>>16), byte(si.MaxFrame>>8), byte(si.MaxFrame)
	b[10] = byte(si.Rate >> 12)
	b[11] = byte(si.Rate >> 4)
	b[12] = byte(si.Rate&0xF)<<4 | byte(si.Channels-1)<<1 | byte((si.Bits-1)>>4)
	b[13] = byte((si.Bits-1)&0xF)<<4 | byte(si.Samples>>32)
	b[14], b[15], b[16], b[17] = byte(si.Samples>>24), byte(si.Samples>>16), byte(si.Samples>>8), byte(si.Samples)
	copy(b[18:], si.MD5[:])
	return b, nil
}

// PCMFormat is the pipeline format the decoder emits for this stream.
// FLAC's channel orders for 1 to 8 channels coincide with the ascending
// WAVE_FORMAT_EXTENSIBLE bit order, so no reordering happens anywhere.
func (si StreamInfo) PCMFormat() audio.Format {
	return audio.Format{
		Rate:     si.Rate,
		Channels: si.Channels,
		Layout:   audio.DefaultLayout(si.Channels),
		Type:     audio.Int,
		BitDepth: si.Bits,
	}
}
