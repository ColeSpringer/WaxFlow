// Package riff reads and writes RIFF/WAVE, the WAV container, including
// the RF64/BW64 64-bit extension: RF64 is always read, and the muxer
// switches to it automatically when output projects past RIFF's 4 GiB
// size fields (24-bit/96 kHz audiobooks overflow plain WAV at about two
// hours; decided here, not discovered in production).
//
// Supported audio: integer and IEEE-float PCM, plain and
// WAVE_FORMAT_EXTENSIBLE. Compressed WAV payloads (ADPCM, a-law) are out
// of scope for the Wax family and rejected as unsupported.
package riff

import (
	"encoding/binary"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/waxerr"
)

const (
	idRIFF = "RIFF"
	idRF64 = "RF64"
	idBW64 = "BW64"
	idWAVE = "WAVE"
	idFmt  = "fmt "
	idData = "data"
	idDS64 = "ds64"
	idJUNK = "JUNK"
	idFact = "fact"
)

const (
	tagPCM        = 0x0001
	tagIEEEFloat  = 0x0003
	tagExtensible = 0xFFFE
)

// guidTail is the constant remainder of the EXTENSIBLE SubFormat GUID
// after the leading 16-bit format tag.
var guidTail = [14]byte{0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}

// size32Unknown is the streaming placeholder in 32-bit size fields, and
// also RIFF's hard size ceiling.
const size32Unknown = 0xFFFFFFFF

// ds64Payload is the fixed ds64 payload this package reads and writes:
// riffSize, dataSize, sampleCount (all u64) and an empty chunk table.
const ds64Payload = 28

// Match reports whether head (the first bytes of a source, at least 12)
// looks like a WAV file. It backs format's ordered sniff table.
func Match(head []byte) bool {
	if len(head) < 12 {
		return false
	}
	id := string(head[:4])
	return (id == idRIFF || id == idRF64 || id == idBW64) && string(head[8:12]) == idWAVE
}

// DefaultConfig returns the natural WAV wire encoding for a pipeline
// format: little-endian, unsigned 8-bit, signed wider integers packed in
// whole bytes with valid bits marked, float32 for the float domain.
func DefaultConfig(f audio.Format) (pcm.Config, error) {
	if err := f.Valid(); err != nil {
		return pcm.Config{}, err
	}
	if f.Type == audio.Float {
		return pcm.Config{Encoding: pcm.Float, Bits: 32}, nil
	}
	bits := pcm.ContainerBits(f.BitDepth)
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: bits}
	if bits == 8 {
		cfg.Encoding = pcm.UnsignedInt
	}
	if f.BitDepth != bits {
		cfg.ValidBits = f.BitDepth
	}
	if err := cfg.Validate(); err != nil {
		return pcm.Config{}, waxerr.Wrap(waxerr.CodeUnsupportedFormat, "riff: no wav encoding for format", err)
	}
	return cfg, nil
}

var le = binary.LittleEndian
