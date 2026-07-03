package format

import (
	"fmt"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/aiff"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/container/mpa"
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
	// mediaType is the container's HTTP media type; direct play serves it
	// from here so no handler maintains its own container-to-type switch.
	mediaType string
	open      func(src container.Source, opts *Options) (container.Demuxer, error)
}

// drivers is the explicit ordered magic table (no blank-import
// registration). The full v1.0 order is: fLaC, RIFF, FORM, OggS, ftyp,
// EBML, ADTS syncword, MPEG syncword last (it false-positives); entries
// appear here as their containers land. flac, wav, aiff, and ogg are in.
var drivers = []driver{
	{
		name:      "flac",
		match:     flacn.Match,
		need:      4,
		exts:      []string{"flac"},
		mediaType: "audio/flac",
		open: func(src container.Source, opts *Options) (container.Demuxer, error) {
			return flacn.NewDemuxer(src, &flacn.DemuxerOptions{Strict: opts != nil && opts.Strict})
		},
	},
	{
		name:      "wav",
		match:     riff.Match,
		need:      12,
		exts:      []string{"wav", "wave", "rf64", "bw64"},
		mediaType: "audio/wav",
		open: func(src container.Source, opts *Options) (container.Demuxer, error) {
			return riff.NewDemuxer(src, &riff.DemuxerOptions{Strict: opts != nil && opts.Strict})
		},
	},
	{
		name:      "aiff",
		match:     aiff.Match,
		need:      12,
		exts:      []string{"aif", "aiff", "aifc", "afc"},
		mediaType: "audio/aiff",
		open: func(src container.Source, opts *Options) (container.Demuxer, error) {
			return aiff.NewDemuxer(src, &aiff.DemuxerOptions{Strict: opts != nil && opts.Strict})
		},
	},
	{
		name:      "ogg",
		match:     ogg.Match,
		need:      4,
		exts:      []string{"ogg", "oga"},
		mediaType: "audio/ogg",
		open: func(src container.Source, opts *Options) (container.Demuxer, error) {
			return ogg.NewDemuxer(src, &ogg.DemuxerOptions{Strict: opts != nil && opts.Strict})
		},
	},
	// The MPEG sync word stays last: it is twelve set bits anywhere in a
	// window, which false-positives on other formats' payloads.
	{
		name:      "mp3",
		match:     mpa.Match,
		need:      mpa.MatchNeed,
		exts:      []string{"mp3", "mpga"},
		mediaType: "audio/mpeg",
		open: func(src container.Source, opts *Options) (container.Demuxer, error) {
			return mpa.NewDemuxer(src, &mpa.DemuxerOptions{Strict: opts != nil && opts.Strict})
		},
	},
}

// Inputs lists the registered container drivers in sniff order: the
// read-side capability surface /caps advertises. Probe and /caps never
// claim what does not work because this is the same table Probe resolves
// against.
func Inputs() []string {
	names := make([]string, len(drivers))
	for i := range drivers {
		names[i] = drivers[i].name
	}
	return names
}

// MediaTypeFor returns the HTTP media type for a registered container
// name, or application/octet-stream for anything unregistered.
func MediaTypeFor(name string) string {
	for i := range drivers {
		if drivers[i].name == name {
			return drivers[i].mediaType
		}
	}
	return "application/octet-stream"
}

// decoders is the codec registry: one table drives both wiring and the
// Decoders capability list, so the two cannot drift.
var decoders = []struct {
	id    codec.ID
	build func(t container.Track) (codec.Decoder, error)
}{
	{codec.PCM, func(t container.Track) (codec.Decoder, error) {
		cfg, err := pcm.ParseConfig(t.CodecConfig)
		if err != nil {
			return nil, err
		}
		return pcm.NewDecoder(cfg, t.Fmt)
	}},
	{codec.FLAC, func(t container.Track) (codec.Decoder, error) {
		si, err := flac.ParseStreamInfo(t.CodecConfig)
		if err != nil {
			return nil, err
		}
		return flac.NewDecoder(si, t.Fmt)
	}},
	{codec.MP3, func(t container.Track) (codec.Decoder, error) {
		return mp3.NewDecoder(t.Fmt)
	}},
}

// Decoders lists the codecs with registered decoders, in registry order.
func Decoders() []codec.ID {
	ids := make([]codec.ID, len(decoders))
	for i := range decoders {
		ids[i] = decoders[i].id
	}
	return ids
}

// newDecoder builds a decoder for a track, capability-gated the same way
// the driver table is.
func newDecoder(t container.Track) (codec.Decoder, error) {
	for i := range decoders {
		if decoders[i].id == t.Codec {
			return decoders[i].build(t)
		}
	}
	return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
		fmt.Sprintf("format: no decoder registered for codec %q", t.Codec))
}
