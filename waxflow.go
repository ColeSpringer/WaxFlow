package waxflow

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/aiff"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/dsp"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/waxerr"
)

// Engine is the library-first entry point to the transcoding pipeline.
// The CLI and the HTTP server are both thin layers over it.
type Engine struct {
	log *slog.Logger
}

// New returns an Engine. Without WithLogger, logs are discarded.
func New(opts ...Option) *Engine {
	e := &Engine{log: slog.New(slog.DiscardHandler)}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Probe identifies src and returns its parsed headers. The hint is an
// optional file extension used only when no magic bytes match.
func (e *Engine) Probe(src container.Source, hint string, opts *ProbeOptions) (*format.Info, error) {
	return format.Probe(src, hint, &format.Options{Strict: opts != nil && opts.Strict})
}

// OpenStream opens src for decoded, sample-exact PCM access.
func (e *Engine) OpenStream(src container.Source, hint string) (format.Media, error) {
	return format.Open(src, hint, nil)
}

// TranscodeResult reports what Transcode produced.
type TranscodeResult struct {
	// Samples is the number of frames written.
	Samples int64
	// Format is the PCM format of the output track.
	Format audio.Format
	// Container is the output container name.
	Container string
}

// Transcode decodes src and writes it to dst in the requested output
// format: decode -> DSP -> encode -> mux, checking ctx between chunks.
// The DSP chain (convert, resample, mix, gain, dither; plan section 8)
// is assembled only from the options that differ from the source, so
// zero options make the transcode a bit-exact container rewrite.
// Output formats whose muxer needs to back-patch headers (AIFF, exact
// WAV sizes) want dst to be an io.WriteSeeker; WAV falls back to a
// compliant streaming form on a plain writer.
func (e *Engine) Transcode(ctx context.Context, src container.Source, hint string, dst io.Writer, opts TranscodeOptions) (*TranscodeResult, error) {
	med, err := e.OpenStream(src, hint)
	if err != nil {
		return nil, err
	}
	defer med.Close()

	srcTrack := med.Info().Default()
	chain, err := dsp.NewChain(dsp.NewSource(med, srcTrack.Fmt), dsp.ChainSpec{
		Rate:     opts.Rate,
		Channels: opts.Channels,
		BitDepth: opts.BitDepth,
		GainDB:   opts.GainDB,
		Shaping:  opts.Shaping,
		Profile:  opts.ResampleProfile,
		// FrameSize stays 0 through M3: the PCM encoder accepts any
		// chunk length. Once frame-native encoders land, their
		// InputFormat drives the spec and FrameSize carries FrameSize().
	})
	if err != nil {
		return nil, err
	}
	defer chain.Release()

	f := chain.Format()
	cfg, mux, err := outputFor(opts.Format, f, dst)
	if err != nil {
		return nil, err
	}
	// The one capability bit Muxer exposes: back-patching muxers need a
	// seekable destination (a file), checked here once so no future muxer
	// re-invents the guard and no work starts on a doomed transcode.
	if mux.NeedsSeek() {
		if _, ok := dst.(io.WriteSeeker); !ok {
			return nil, waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("waxflow: %s output requires a seekable destination", opts.Format))
		}
	}
	enc, err := pcm.NewEncoder(cfg, f)
	if err != nil {
		return nil, err
	}

	track := container.Track{
		Codec:       codec.PCM,
		CodecConfig: enc.CodecConfig(),
		Fmt:         f,
		Samples:     chain.OutputSamples(srcTrack.Samples),
		Default:     true,
	}
	if err := mux.Begin([]container.Track{track}); err != nil {
		return nil, err
	}

	e.log.Debug("transcode started",
		"container", med.Info().Container, "source", srcTrack.Fmt.String(),
		"format", f.String(), "samples", track.Samples, "out", opts.Format,
		"dsp", strings.Join(chain.Versions(), ","))

	emit := func(p codec.Packet) error {
		return mux.WritePacket(container.Packet{Track: 0, Packet: p})
	}
	buf := audio.Get(f, audio.StandardChunk)
	defer audio.Put(buf)
	for {
		if err := ctx.Err(); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeCanceled, "transcode canceled", err)
		}
		err := chain.ReadChunk(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if err := enc.Encode(buf, emit); err != nil {
			return nil, err
		}
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		return nil, err
	}
	if err := mux.End(trailer); err != nil {
		return nil, err
	}
	e.log.Debug("transcode finished", "samples", trailer.Samples)
	return &TranscodeResult{Samples: trailer.Samples, Format: f, Container: opts.Format}, nil
}

// output is one row of the writer-side capability table, the analog of
// format's read-side driver table: the single source of truth for what
// the engine can produce. Rows appear as encoder and muxer milestones
// land; the CLI's extension inference and the future /caps endpoint both
// read this table instead of maintaining their own lists.
type output struct {
	name  string
	exts  []string
	build func(f audio.Format, dst io.Writer) (pcm.Config, container.Muxer, error)
}

var outputs = []output{
	{
		name: "wav",
		exts: []string{"wav", "wave", "rf64", "bw64"},
		build: func(f audio.Format, dst io.Writer) (pcm.Config, container.Muxer, error) {
			cfg, err := riff.DefaultConfig(f)
			return cfg, riff.NewMuxer(dst, nil), err
		},
	},
	{
		name: "aiff",
		exts: []string{"aif", "aiff", "aifc", "afc"},
		build: func(f audio.Format, dst io.Writer) (pcm.Config, container.Muxer, error) {
			cfg, err := aiff.DefaultConfig(f)
			return cfg, aiff.NewMuxer(dst), err
		},
	},
}

// OutputFormats lists the registered output format names, in table order.
func OutputFormats() []string {
	names := make([]string, len(outputs))
	for i, o := range outputs {
		names[i] = o.name
	}
	return names
}

// OutputFormatForExt maps a file extension (with or without the leading
// dot, any case) to the output format name that writes it, or "" when no
// registered output claims the extension.
func OutputFormatForExt(ext string) string {
	ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	for _, o := range outputs {
		for _, e := range o.exts {
			if e == ext {
				return o.name
			}
		}
	}
	return ""
}

// outputFor maps an output format name onto a wire config and muxer.
func outputFor(name string, f audio.Format, dst io.Writer) (pcm.Config, container.Muxer, error) {
	if name == "" {
		return pcm.Config{}, nil, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: no output format requested")
	}
	for _, o := range outputs {
		if o.name == name {
			cfg, mux, err := o.build(f, dst)
			if err != nil {
				return pcm.Config{}, nil, err
			}
			return cfg, mux, nil
		}
	}
	return pcm.Config{}, nil, waxerr.New(waxerr.CodeUnsupportedFormat,
		fmt.Sprintf("waxflow: unsupported output format %q (available: %s)", name, strings.Join(OutputFormats(), ", ")))
}
