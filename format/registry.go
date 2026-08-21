package format

import (
	"fmt"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/codec/alac"
	"github.com/colespringer/waxflow/codec/ape"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/codec/opus"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/codec/vorbis"
	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/codec/wma"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/adts"
	"github.com/colespringer/waxflow/container/aiff"
	"github.com/colespringer/waxflow/container/apen"
	"github.com/colespringer/waxflow/container/asf"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/container/mka"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/container/mpa"
	"github.com/colespringer/waxflow/container/ogg"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/container/wv"
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
		exts:      []string{"ogg", "oga", "opus"},
		mediaType: "audio/ogg",
		open: func(src container.Source, opts *Options) (container.Demuxer, error) {
			return ogg.NewDemuxer(src, &ogg.DemuxerOptions{Strict: opts != nil && opts.Strict})
		},
	},
	{
		name:      "mp4",
		match:     mp4.Match,
		need:      mp4.MatchNeed,
		exts:      []string{"m4a", "m4b", "mp4", "m4r", "mov"},
		mediaType: "audio/mp4",
		open: func(src container.Source, opts *Options) (container.Demuxer, error) {
			return mp4.NewDemuxer(src, &mp4.DemuxerOptions{Strict: opts != nil && opts.Strict})
		},
	},
	{
		name:      "mka",
		match:     mka.Match,
		need:      mka.MatchNeed,
		exts:      []string{"mka", "mkv", "webm"},
		mediaType: "audio/x-matroska",
		open: func(src container.Source, opts *Options) (container.Demuxer, error) {
			return mka.NewDemuxer(src, &mka.DemuxerOptions{Strict: opts != nil && opts.Strict})
		},
	},
	{
		name:      "adts",
		match:     adts.Match,
		need:      adts.MatchNeed,
		exts:      []string{"aac", "adts"},
		mediaType: "audio/aac",
		open: func(src container.Source, opts *Options) (container.Demuxer, error) {
			return adts.NewDemuxer(src, &adts.DemuxerOptions{Strict: opts != nil && opts.Strict})
		},
	},
	{
		name:      "ape",
		match:     apen.Match,
		need:      apen.MatchNeed,
		exts:      []string{"ape"},
		mediaType: "audio/x-ape",
		open: func(src container.Source, opts *Options) (container.Demuxer, error) {
			return apen.NewDemuxer(src, &apen.DemuxerOptions{Strict: opts != nil && opts.Strict})
		},
	},
	{
		name:      "wavpack",
		match:     wv.Match,
		need:      4,
		exts:      []string{"wv"},
		mediaType: "audio/x-wavpack",
		open: func(src container.Source, opts *Options) (container.Demuxer, error) {
			return wv.NewDemuxer(src, &wv.DemuxerOptions{Strict: opts != nil && opts.Strict})
		},
	},
	{
		// ASF is the container and WMA the codec inside it, but the driver
		// name is the one users say. The magic is the 16-byte Header Object
		// GUID, so it needs no ordering care.
		name:      "wma",
		match:     asf.Match,
		need:      asf.MatchNeed,
		exts:      []string{"wma", "asf"},
		mediaType: "audio/x-ms-wma",
		open: func(src container.Source, opts *Options) (container.Demuxer, error) {
			return asf.NewDemuxer(src, &asf.DemuxerOptions{Strict: opts != nil && opts.Strict})
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
// decoders is the decoder registry. Each row carries its package's cache-key
// version constant (ADR-0004) beside its constructor, so the two cannot drift:
// the engine reads the version from here rather than from a second table of
// its own, and a codec registered without one fails to compile.
var decoders = []struct {
	id      codec.ID
	version string
	build   func(t container.Track) (codec.Decoder, error)
}{
	{codec.PCM, pcm.Version, func(t container.Track) (codec.Decoder, error) {
		cfg, err := pcm.ParseConfig(t.CodecConfig)
		if err != nil {
			return nil, err
		}
		return pcm.NewDecoder(cfg, t.Fmt)
	}},
	{codec.FLAC, flac.Version, func(t container.Track) (codec.Decoder, error) {
		si, err := flac.ParseStreamInfo(t.CodecConfig)
		if err != nil {
			return nil, err
		}
		return flac.NewDecoder(si, t.Fmt)
	}},
	{codec.MP3, mp3.Version, func(t container.Track) (codec.Decoder, error) {
		return mp3.NewDecoder(t.Fmt)
	}},
	{codec.ALAC, alac.Version, func(t container.Track) (codec.Decoder, error) {
		cfg, err := alac.ParseMagicCookie(t.CodecConfig)
		if err != nil {
			return nil, err
		}
		return alac.NewDecoder(cfg, t.Fmt)
	}},
	// Both AAC identities share one constructor: the ASC alone decides
	// whether the SBR stage runs.
	{codec.AACLC, aac.Version, newAACDecoder},
	{codec.HEAAC, aac.HEVersion, newAACDecoder},
	{codec.WavPack, wavpack.Version, func(t container.Track) (codec.Decoder, error) {
		cfg, err := wavpack.ParseConfig(t.CodecConfig)
		if err != nil {
			return nil, err
		}
		return wavpack.NewDecoder(cfg, t.Fmt)
	}},
	{codec.APE, ape.Version, func(t container.Track) (codec.Decoder, error) {
		cfg, err := ape.ParseConfig(t.CodecConfig)
		if err != nil {
			return nil, err
		}
		return ape.NewDecoder(cfg, t.Fmt)
	}},
	{codec.WMA, wma.Version, func(t container.Track) (codec.Decoder, error) {
		// One decoder row for both versions: the WAVEFORMATEX the track
		// carries is what discriminates 0x0160 from 0x0161, so a second codec
		// ID would double the registry, caps and cache-key bookkeeping for
		// what the config already says.
		cfg, err := wma.ParseConfig(t.CodecConfig)
		if err != nil {
			return nil, err
		}
		return wma.NewDecoder(cfg, t.Fmt)
	}},
	{codec.Vorbis, vorbis.Version, func(t container.Track) (codec.Decoder, error) {
		cfg, err := vorbis.ParseConfig(t.CodecConfig)
		if err != nil {
			return nil, err
		}
		return vorbis.NewDecoder(cfg, t.Fmt)
	}},
	{codec.Opus, opus.Version, func(t container.Track) (codec.Decoder, error) {
		cfg, err := opus.ParseOpusHead(t.CodecConfig)
		if err != nil {
			return nil, err
		}
		return opus.NewDecoder(cfg, t.Fmt)
	}},
}

// Decoders lists the codecs with registered decoders, in registry order.
// DecoderVersion is the registered decoder's cache-key version for id, or ""
// when nothing decodes it. It is the one place that mapping lives.
func DecoderVersion(id codec.ID) string {
	for i := range decoders {
		if decoders[i].id == id {
			return decoders[i].version
		}
	}
	return ""
}

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

// newAACDecoder builds the decoder both AAC identities share: the parsed
// ASC alone decides whether the SBR stage runs.
func newAACDecoder(t container.Track) (codec.Decoder, error) {
	cfg, err := aac.ParseASC(t.CodecConfig)
	if err != nil {
		return nil, err
	}
	return aac.NewDecoder(cfg, t.Fmt)
}
