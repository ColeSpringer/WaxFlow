// Package waveformat reads the WAVEFORMATEX structure, and the block
// geometry the compressed WAVE format tags state inside it.
//
// Two containers in this tree describe audio with this one structure: a RIFF
// "fmt " chunk is a WAVEFORMATEX outright, and a QuickTime "ms" sample entry
// stores one inside its 'wave' wrapper. The rules for reading a block codec's
// geometry out of it are the same in both, and they are the fiddly part (a
// coefficient table, a count that must be seven, a samples-per-block field
// that is derivable and therefore checkable), so they live here once rather
// than in each demuxer.
//
// It is internal because it is a parsing detail, not an API: a caller
// routing on a codec has codec.ID and Track for that.
package waveformat

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/colespringer/waxflow/audio"

	"github.com/colespringer/waxflow/codec/adpcm"
	"github.com/colespringer/waxflow/container/internal/codecname"
	"github.com/colespringer/waxflow/waxerr"
)

// The WAVE format tags the compressed arms here cover.
const (
	TagMSADPCM  = 0x0002
	TagIMAADPCM = 0x0011
)

// adpcmBits is what both ADPCM families store a sample in, and the only
// value their headers' wBitsPerSample field can mean.
const adpcmBits = 4

// coefPairs is the predictor coefficient count MS ADPCM is defined with and
// that every encoder writes.
const coefPairs = 7

// msMaxChannels is what Microsoft ADPCM defines, mirrored here so the refusal
// carries the container's name rather than the codec package's.
const msMaxChannels = 2

// headerLen is the fixed part of a WAVEFORMATEX: wFormatTag(2) nChannels(2)
// nSamplesPerSec(4) nAvgBytesPerSec(4) nBlockAlign(2) wBitsPerSample(2).
const headerLen = 16

// Ex is the part of a WAVEFORMATEX that describes the stream, plus the extra
// bytes a format tag may state its own geometry in.
type Ex struct {
	Tag        uint16
	Channels   int
	Rate       int
	BlockAlign int
	Bits       int
	// Extra is everything past the fixed header, which is the cbSize region
	// where a tag states what the fixed fields cannot. It is what the caller
	// found rather than what cbSize declares, so a truncated one is short
	// rather than a read past the end.
	Extra []byte
}

// Parse reads a WAVEFORMATEX out of a little-endian buffer. It reports false
// for a buffer too short to hold the fixed header.
func Parse(b []byte) (Ex, bool) {
	if len(b) < headerLen {
		return Ex{}, false
	}
	le := binary.LittleEndian
	ex := Ex{
		Tag:        le.Uint16(b),
		Channels:   int(le.Uint16(b[2:])),
		Rate:       int(le.Uint32(b[4:])),
		BlockAlign: int(le.Uint16(b[12:])),
		Bits:       int(le.Uint16(b[14:])),
	}
	if len(b) >= headerLen+2 {
		// cbSize is present. The extra is bounded by what the buffer holds
		// rather than by the field, which is the file's own number.
		n := int(le.Uint16(b[headerLen:]))
		ex.Extra = b[headerLen+2 : headerLen+2+min(n, len(b)-headerLen-2)]
	}
	return ex, true
}

// Finding is something a parse noticed and could not act on alone: the
// caller reports it in its own vocabulary, at its own offset, and decides
// whether Strict escalates it.
type Finding struct {
	Msg string
	// Note marks a finding about a well-formed file rather than damage in it,
	// which is the difference between container.Note and container.Damage.
	Note bool
}

// ADPCM reads a block codec's geometry out of a WAVEFORMATEX. prefix is the
// caller's error prefix ("wav: ", "mp4: ").
//
// wSamplesPerBlock is read and then checked rather than trusted: it is
// derivable from the block size and the channel count, the decoder's loops
// are written to the derivation, and a file that states another number is
// describing a block boundary its own payload does not have. The derived
// value wins, and the disagreement comes back as a finding.
func ADPCM(ex Ex, prefix string) (adpcm.Config, []Finding, error) {
	var found []Finding
	name := codecname.WaveFormat(ex.Tag)
	malformed := func(format string, args ...any) error {
		return waxerr.Malformed(prefix, format, args...)
	}
	cfg := adpcm.Config{Layout: adpcm.IMAWav, Channels: ex.Channels, BlockAlign: ex.BlockAlign}
	if ex.Tag == TagMSADPCM {
		cfg.Layout = adpcm.MS
	}
	if ex.Bits != adpcmBits {
		found = append(found, Finding{Msg: fmt.Sprintf(
			"%s declares %d bits per sample, which is %d by definition", name, ex.Bits, adpcmBits)})
	}
	if len(ex.Extra) < 2 {
		return cfg, found, malformed("%s header states no samples per block", name)
	}
	le := binary.LittleEndian
	declared := int(le.Uint16(ex.Extra))
	if ex.Tag == TagMSADPCM {
		if len(ex.Extra) < 4 {
			return cfg, found, malformed("MS ADPCM header states no coefficient count")
		}
		n := int(le.Uint16(ex.Extra[2:]))
		if n != coefPairs {
			// Well-formed and unread: the field is a count, and a stream with
			// another one needs a coefficient table this build has no shape
			// for. Every encoder writes the seven.
			return cfg, found, waxerr.Unsupported(prefix,
				"MS ADPCM with %d predictor coefficient pairs (this build reads %d)", n, coefPairs)
		}
		if len(ex.Extra) < 4+4*n {
			return cfg, found, malformed("MS ADPCM header holds %d of its %d coefficient pairs",
				max(len(ex.Extra)-4, 0)/4, n)
		}
		for i := range cfg.Coefs {
			cfg.Coefs[i][0] = int16(le.Uint16(ex.Extra[4+4*i:]))
			cfg.Coefs[i][1] = int16(le.Uint16(ex.Extra[6+4*i:]))
		}
		if cfg.Coefs != adpcm.DefaultCoefs {
			// A Note rather than damage: the file is well formed and the
			// specification says its own table is the one to use. It is worth
			// saying because readers that ignore the field (ffmpeg among
			// them) decode such a file differently, so a comparison against
			// one of them will not match.
			found = append(found, Finding{Note: true, Msg: "MS ADPCM coefficient table is not the conventional one; " +
				"readers that ignore it decode this file differently"})
		}
	}
	// The geometry defects a file can actually express are named here, in the
	// caller's vocabulary and with the caller's prefix: these errors travel to
	// users verbatim, and a WAV's defect reported as "adpcm: ..." names a
	// package the user never asked about. Validate stays behind them as a
	// backstop for a config no file can produce.
	if ex.Channels < 1 {
		return cfg, found, malformed("%s header declares %d channels", name, ex.Channels)
	}
	if ex.Channels > audio.MaxChannels {
		return cfg, found, waxerr.Unsupported(prefix, "%d channels (supported: 1..%d)", ex.Channels, audio.MaxChannels)
	}
	if cfg.Layout == adpcm.MS && ex.Channels > msMaxChannels {
		// Well formed and unread: the nibbles alternate one at a time, which
		// the layout only spells for mono and stereo.
		return cfg, found, waxerr.Unsupported(prefix,
			"%d channels of MS ADPCM, whose nibbles alternate between at most %d", ex.Channels, msMaxChannels)
	}
	if floor := cfg.HeaderBytes() * ex.Channels; ex.BlockAlign < floor {
		// Two fields of the same header contradicting each other: the tag
		// defines a per-channel block header the declared block size cannot
		// hold. Malformed rather than unsupported, which is the difference
		// between a damaged file and one this build declines.
		return cfg, found, malformed("%s declares %d-byte blocks, too small for the %d bytes its %d channels of header need",
			name, ex.BlockAlign, floor, ex.Channels)
	}
	cfg.SamplesPerBlock = cfg.BlockFrames()
	if err := cfg.Validate(); err != nil {
		return cfg, found, waxerr.Annotate(strings.TrimSuffix(prefix, ": ")+": block geometry", err)
	}
	if declared != cfg.SamplesPerBlock {
		found = append(found, Finding{Msg: fmt.Sprintf(
			"%s declares %d samples per block, its %d-byte blocks hold %d; using %d",
			name, declared, ex.BlockAlign, cfg.SamplesPerBlock, cfg.SamplesPerBlock)})
	}
	return cfg, found, nil
}
