// Package aiff reads and writes AIFF and AIFF-C, the Apple/SGI audio
// container. Reading covers the PCM compression types found in real
// libraries (NONE and twos for big-endian integers, sowt for little-endian
// ones, in24 and in32 at their own widths, raw for offset-binary 8-bit, and
// fl32/fl64 floats) plus four compressed ones: G.711 alaw and ulaw, Apple's
// ima4 ADPCM, and MP3 in either spelling ('.mp3' or QuickTime's "ms" plus
// the WAVE format tag). Writing produces plain AIFF for big-endian integer
// PCM and AIFF-C for floats, and nothing compressed: those types are lossy
// codings of what the container already carries losslessly.
//
// Compression types are matched case-insensitively, as the reference readers
// match them: a file spelling one in capitals is the same file. The one
// exception is QuickTime's "ms" escape hatch, whose last two bytes are a WAVE
// format tag rather than text, so folding the case of the whole four would
// rewrite the tag (0x6161 is "aa", and upper-casing it names a different
// codec). It is matched as written, which is also how container/mp4 matches
// it, through the same shared helper.
//
// Unlike WAV, AIFF has no streaming convention: FORM and SSND sizes and
// the COMM frame count all live before the audio data, so the muxer
// declares NeedsSeek and back-patches at End.
//
// One field changes meaning with the compression type, and it is the length:
// COMM's numSampleFrames counts PACKETS for ima4, not frames, so a 125-packet
// file declares 125 and holds 8000 samples. For MP3 it is not read at all:
// nothing defines which of the two it counts there, and the frames are
// walked rather than divided (container/internal/mpegframes does the walk,
// wherever the frames are found).
package aiff

import (
	"encoding/binary"
	"strings"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container/internal/codecname"
	"github.com/colespringer/waxflow/waxerr"
)

const (
	idFORM = "FORM"
	idAIFF = "AIFF"
	idAIFC = "AIFC"
	idCOMM = "COMM"
	idSSND = "SSND"
	idFVER = "FVER"
)

// Compression type FourCCs, in the canonical spelling parseCOMM folds a
// file's onto.
const (
	compNONE = "NONE"
	compTwos = "twos"
	compSowt = "sowt"
	compRaw  = "raw "
	compIn24 = "in24"
	compIn32 = "in32"
	compFl32 = "fl32"
	compFl64 = "fl64"
	compALaw = "alaw"
	compULaw = "ulaw"
	compIMA4 = "ima4"
	compMP3  = ".mp3"
)

// waveTagMP3 is MP3's WAVE format tag, which QuickTime's "ms" spelling
// carries into an AIFF-C the same way it carries it into a .mov.
const waveTagMP3 = 0x0055

// foldComp maps a compression type onto its canonical spelling, or returns
// "" for one this package does not read. Case-insensitive, since AIFF-C
// writers disagree about the case of every one of these and the reference
// readers fold too; matching exactly would refuse a file every other reader
// opens, and refuse it while naming the codec this package decodes.
func foldComp(comp string) string {
	// QuickTime's escape hatch first: the two bytes behind "ms" are a WAVE
	// format tag rather than text, so they are not a spelling to fold.
	//
	// MP3 is the only tag mapped, and the others are not an oversight. The two
	// ADPCM families state their block geometry in a WAVEFORMATEX that an
	// AIFF-C has nowhere to put, so they cannot be read through this spelling
	// at all; PCM and the G.711 pair can, and already have compression types
	// of their own that every writer uses. What is left is a refusal that
	// names the codec, through the same table, which is the answer a file
	// spelling one of them this way deserves.
	if tag, ok := codecname.QuickTimeWaveTag(comp); ok {
		if tag == waveTagMP3 {
			return compMP3
		}
		return ""
	}
	switch strings.ToUpper(comp) {
	case "NONE":
		return compNONE
	case "TWOS":
		return compTwos
	case "SOWT":
		return compSowt
	case "RAW ":
		return compRaw
	case "IN24":
		return compIn24
	case "IN32":
		return compIn32
	case "FL32":
		return compFl32
	case "FL64":
		return compFl64
	case "ALAW":
		return compALaw
	case "ULAW":
		return compULaw
	case "IMA4":
		return compIMA4
	case ".MP3":
		return compMP3
	}
	return ""
}

// fverTimestamp is the one defined AIFF-C version (May 23, 1990).
const fverTimestamp = 0xA2805140

// size32Max is the ceiling of AIFF's 32-bit size fields. AIFF has no
// RF64-style extension; output that would cross it is refused.
const size32Max = 0xFFFFFFFF

// Match reports whether head (at least 12 bytes) looks like an AIFF or
// AIFF-C file. It backs format's ordered sniff table.
func Match(head []byte) bool {
	if len(head) < 12 {
		return false
	}
	form := string(head[8:12])
	return string(head[:4]) == idFORM && (form == idAIFF || form == idAIFC)
}

// DefaultConfig returns the natural AIFF wire encoding for a pipeline
// format: big-endian signed integers packed in whole bytes (plain AIFF),
// float32 for the float domain (AIFF-C fl32).
func DefaultConfig(f audio.Format) (pcm.Config, error) {
	if err := f.Valid(); err != nil {
		return pcm.Config{}, err
	}
	if f.Type == audio.Float {
		return pcm.Config{Encoding: pcm.Float, Bits: 32, BigEndian: true}, nil
	}
	bits := pcm.ContainerBits(f.BitDepth)
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: bits, BigEndian: bits > 8}
	if f.BitDepth != bits {
		cfg.ValidBits = f.BitDepth
	}
	if err := cfg.Validate(); err != nil {
		return pcm.Config{}, waxerr.Wrap(waxerr.CodeUnsupportedFormat, "aiff: no aiff encoding for format", err)
	}
	return cfg, nil
}

var be = binary.BigEndian
