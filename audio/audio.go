// Package audio defines WaxFlow's PCM model: sample formats, channel
// layouts, and the planar dual-domain Buffer that every decoder, DSP node,
// and encoder exchanges.
//
// Positions are int64 sample counts at a stated rate; float seconds never
// appear inside the pipeline (ADR-0006). The package is stdlib-only, like
// the whole public codec/DSP tree (enforced by `make depcheck`).
package audio

import (
	"fmt"

	"github.com/colespringer/waxflow/waxerr"
)

// SampleType selects a Buffer's populated domain.
type SampleType uint8

const (
	// Int samples live in Buffer.I as int32 values right-justified at
	// Format.BitDepth: a 16-bit sample spans [-32768, 32767]. Integers
	// survive the pipeline so lossless to lossless stays bit-exact.
	Int SampleType = iota
	// Float samples live in Buffer.F as float32 in [-1, 1] nominal range.
	// Lossy and DSP paths run in this domain; a float32 mantissa holds
	// up to 24-bit integer PCM exactly.
	Float
)

func (t SampleType) String() string {
	switch t {
	case Int:
		return "int"
	case Float:
		return "float"
	default:
		return fmt.Sprintf("SampleType(%d)", uint8(t))
	}
}

// MaxChannels is the widest layout the pipeline decodes (7.1).
const MaxChannels = 8

// Format describes PCM audio in the pipeline domain: what a Buffer holds,
// not how bytes are packed on the wire (wire packing is the pcm codec's
// concern).
type Format struct {
	// Rate is the sample rate in Hz.
	Rate int
	// Channels is the channel count, 1 to MaxChannels.
	Channels int
	// Layout assigns speaker positions. Zero means unknown; when set, its
	// bit count must equal Channels.
	Layout ChannelMask
	// Type selects the populated Buffer domain.
	Type SampleType
	// BitDepth is the meaningful sample precision: 1 to 32 for Int
	// (right-justified in int32), always 32 for Float.
	BitDepth int
}

// Valid reports whether the format is internally consistent. Errors carry
// waxerr.CodeInvalidRequest.
func (f Format) Valid() error {
	switch {
	case f.Rate <= 0:
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("audio: rate %d must be positive", f.Rate))
	case f.Channels < 1 || f.Channels > MaxChannels:
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("audio: %d channels outside 1..%d", f.Channels, MaxChannels))
	case f.Layout != 0 && f.Layout.Count() != f.Channels:
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("audio: layout %v has %d positions for %d channels", f.Layout, f.Layout.Count(), f.Channels))
	}
	switch f.Type {
	case Int:
		if f.BitDepth < 1 || f.BitDepth > 32 {
			return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("audio: int bit depth %d outside 1..32", f.BitDepth))
		}
	case Float:
		if f.BitDepth != 32 {
			return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("audio: float bit depth must be 32, got %d", f.BitDepth))
		}
	default:
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("audio: unknown sample type %d", f.Type))
	}
	return nil
}

func (f Format) String() string {
	return fmt.Sprintf("%dHz %dch %s%d", f.Rate, f.Channels, f.Type, f.BitDepth)
}
