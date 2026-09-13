// Package riff reads and writes RIFF/WAVE, the WAV container, including
// the RF64/BW64 64-bit extension: RF64 is always read, and the muxer
// switches to it automatically when output projects past RIFF's 4 GiB
// size fields (24-bit/96 kHz audiobooks overflow plain WAV at about two
// hours; decided here, not discovered in production).
//
// Reading covers integer and IEEE-float PCM (plain and
// WAVE_FORMAT_EXTENSIBLE), G.711 A-law and mu-law, IMA ADPCM, Microsoft
// ADPCM and MP3. Writing produces PCM only: the compressed tags are lossy
// codings of exactly what a WAV already carries losslessly, so they are read
// because files hold them and not written because nothing needs another one.
// MP2 and WMA-in-WAV are refused by name.
//
// The payload takes one of two shapes. All but one of those codings lay it
// out as a flat array of fixed-size units, which is what lets one walk serve
// them: a unit is a frame for PCM and G.711 and a block for the two ADPCM
// families, and the sample timeline is the unit index times what a unit
// decodes to. MP3 is the exception, since a Layer III frame states its own
// length rather than taking one from the header; those frames are walked by
// container/internal/mpegframes, which walks them wherever they are found.
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

// The WAVE format tags this package reads. Every other tag is refused by
// name where a name is known; see container/internal/codecname.
const (
	tagPCM        = 0x0001
	tagMSADPCM    = 0x0002
	tagIEEEFloat  = 0x0003
	tagALaw       = 0x0006
	tagMuLaw      = 0x0007
	tagIMAADPCM   = 0x0011
	tagMP3        = 0x0055
	tagExtensible = 0xFFFE
)

// mpegLayer3Extra is the cbSize region MPEGLAYER3WAVEFORMAT adds to a
// WAVEFORMATEX: wID(2) fdwFlags(4) nBlockSize(2) nFramesPerBlock(2)
// nCodecDelay(2).
const mpegLayer3Extra = 12

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
		return pcm.Config{}, waxerr.Wrap(waxerr.CodeUnsupportedFormat, "wav: no wav encoding for format", err)
	}
	return cfg, nil
}

var le = binary.LittleEndian
