// Package aiff reads and writes AIFF and AIFF-C, the Apple/SGI audio
// container. Reading covers the PCM compression types found in real
// libraries: NONE and twos (big-endian integers), sowt (little-endian
// integers), raw (offset-binary 8-bit), and fl32/fl64 floats. Writing
// produces plain AIFF for big-endian integer PCM and AIFF-C for floats.
//
// Unlike WAV, AIFF has no streaming convention: FORM and SSND sizes and
// the COMM frame count all live before the audio data, so the muxer
// declares NeedsSeek and back-patches at End.
package aiff

import (
	"encoding/binary"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
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

// Compression type FourCCs.
const (
	compNONE = "NONE"
	compTwos = "twos"
	compSowt = "sowt"
	compRaw  = "raw "
	compFl32 = "fl32"
	compFL32 = "FL32"
	compFl64 = "fl64"
	compFL64 = "FL64"
)

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
