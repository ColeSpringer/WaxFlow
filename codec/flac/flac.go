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
