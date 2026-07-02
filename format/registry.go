package format

import (
	"fmt"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/aiff"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/container/ogg"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/waxerr"
)

// driver is one row of the ordered sniff table.
type driver struct {
	name  string
	match func(head []byte) bool
	// need is how many leading bytes match requires. The sniff read is
	// sized to the largest registered need (capped at sniffLen), so
	// probing only ever reads what the current table can use.
	need int
	exts []string
	open func(src container.Source, opts *Options) (container.Demuxer, error)
}

// drivers is the explicit ordered magic table (no blank-import
// registration). The full v1.0 order is: fLaC, RIFF, FORM, OggS, ftyp,
// EBML, ADTS syncword, MPEG syncword last (it false-positives); entries
// appear here as their milestones land. flac, wav, aiff, and ogg are in.
var drivers = []driver{
	{
		name:  "flac",
		match: flacn.Match,
		need:  4,
		exts:  []string{"flac"},
		open: func(src container.Source, opts *Options) (container.Demuxer, error) {
			return flacn.NewDemuxer(src, &flacn.DemuxerOptions{Strict: opts != nil && opts.Strict})
		},
	},
	{
		name:  "wav",
		match: riff.Match,
		need:  12,
		exts:  []string{"wav", "wave", "rf64", "bw64"},
		open: func(src container.Source, opts *Options) (container.Demuxer, error) {
			return riff.NewDemuxer(src, &riff.DemuxerOptions{Strict: opts != nil && opts.Strict})
		},
	},
	{
		name:  "aiff",
		match: aiff.Match,
		need:  12,
		exts:  []string{"aif", "aiff", "aifc", "afc"},
		open: func(src container.Source, opts *Options) (container.Demuxer, error) {
			return aiff.NewDemuxer(src, &aiff.DemuxerOptions{Strict: opts != nil && opts.Strict})
		},
	},
	{
		name:  "ogg",
		match: ogg.Match,
		need:  4,
		exts:  []string{"ogg", "oga"},
		open: func(src container.Source, opts *Options) (container.Demuxer, error) {
			return ogg.NewDemuxer(src, &ogg.DemuxerOptions{Strict: opts != nil && opts.Strict})
		},
	},
}

// newDecoder builds a decoder for a track, capability-gated the same way
// the driver table is.
func newDecoder(t container.Track) (codec.Decoder, error) {
	switch t.Codec {
	case codec.PCM:
		cfg, err := pcm.ParseConfig(t.CodecConfig)
		if err != nil {
			return nil, err
		}
		return pcm.NewDecoder(cfg, t.Fmt)
	case codec.FLAC:
		si, err := flac.ParseStreamInfo(t.CodecConfig)
		if err != nil {
			return nil, err
		}
		return flac.NewDecoder(si, t.Fmt)
	default:
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("format: no decoder registered for codec %q", t.Codec))
	}
}
