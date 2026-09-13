// Package g711 decodes ITU-T G.711 companded speech: A-law, the European
// telephony law, and mu-law, the North American and Japanese one. Both map
// one byte onto a piecewise-logarithmic approximation of a linear sample,
// which is why the whole codec is a 256-entry table per law and why there
// is no configuration beyond the law itself.
//
// The law is the codec identity here rather than a config field: every
// container that carries G.711 names the two apart (a WAVE format tag, an
// AIFF-C compression type, a QuickTime fourcc), so codec.ALaw and
// codec.MuLaw are separate IDs and a track carries no blob at all.
package g711

import (
	"fmt"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

// Law selects the companding law.
type Law uint8

const (
	// ALaw is G.711 A-law, 13-bit linear expanded to the 16-bit domain.
	ALaw Law = iota
	// MuLaw is G.711 mu-law, 14-bit linear expanded the same way.
	MuLaw
)

func (l Law) String() string {
	switch l {
	case ALaw:
		return "A-law"
	case MuLaw:
		return "mu-law"
	default:
		return fmt.Sprintf("Law(%d)", uint8(l))
	}
}

// Valid reports whether l names a law this package decodes.
func (l Law) Valid() error {
	if l != ALaw && l != MuLaw {
		return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf("g711: unknown companding law %d", uint8(l)))
	}
	return nil
}

// Versions are the decoders' algorithm revisions for cache keys (ADR-0004),
// one per codec ID: the two laws share this package but not their cached
// decodes, so a fix to one must not serve stale audio for the other.
const (
	ALawVersion  = "g711-alaw-dec-1"
	MuLawVersion = "g711-mulaw-dec-1"
)

// SourceBitDepth is what a G.711 source stores a sample in, which is what
// probe reports beside the 16-bit decoded depth. It is the number ffprobe
// puts in bits_per_sample for pcm_alaw and pcm_mulaw.
const SourceBitDepth = 8

// Format returns the pipeline format a G.711 track decodes to. The law does
// not appear: both expand into the same signed 16-bit domain, which is what
// makes one output format serve two codec IDs.
func Format(rate, channels int, layout audio.ChannelMask) audio.Format {
	return audio.Format{Rate: rate, Channels: channels, Layout: layout, Type: audio.Int, BitDepth: 16}
}
