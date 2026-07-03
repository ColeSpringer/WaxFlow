package waxflow

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

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

	// plans caches the chain-invariant part of PlanTranscode results, so
	// per-request planning does not materialize a filter bank.
	mu    sync.RWMutex
	plans map[planKey]*planCore
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
// The DSP chain (convert, resample, mix, gain, dither, in that fixed
// order) is assembled only from the options that differ from the source, so
// zero options make the transcode a bit-exact container rewrite. A
// positive FromSample seeks sample-exact before the first chunk (the
// HTTP t= parameter, converted at the boundary).
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
	srcSamples := srcTrack.Samples
	if opts.FromSample < 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: negative FromSample")
	}
	if opts.FromSample > 0 {
		landed, err := med.SeekSample(opts.FromSample)
		if err != nil {
			return nil, err
		}
		if srcSamples >= 0 {
			srcSamples = max(0, srcSamples-landed)
		}
	}
	chain, err := dsp.NewChain(dsp.NewSource(med, srcTrack.Fmt), specFor(opts))
	if err != nil {
		return nil, err
	}
	defer chain.Release()

	f := chain.Format()
	row, err := outputRow(opts.Format)
	if err != nil {
		return nil, err
	}
	cfg, err := row.config(f)
	if err != nil {
		return nil, err
	}
	mux := row.mux(dst)
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
		Samples:     chain.OutputSamples(srcSamples),
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

// specFor maps TranscodeOptions onto the DSP chain spec, in one place so
// Transcode and PlanTranscode cannot drift.
func specFor(opts TranscodeOptions) dsp.ChainSpec {
	return dsp.ChainSpec{
		Rate:     opts.Rate,
		Channels: opts.Channels,
		BitDepth: opts.BitDepth,
		GainDB:   opts.GainDB,
		Shaping:  opts.Shaping,
		Profile:  opts.ResampleProfile,
		// FrameSize stays 0 until frame-native encoders land; the PCM
		// encoder accepts any chunk length. Frame-native encoders'
		// InputFormat will drive the spec and FrameSize carry FrameSize().
	}
}

// TranscodePlan describes what a transcode would produce, computed from
// headers alone: no decoding, no output. The HTTP layer plans before it
// runs, because the ADR-0004 cache key (node versions) and the response
// headers (duration, size estimate) must exist before any pipeline does.
type TranscodePlan struct {
	// Format is the output PCM format.
	Format audio.Format
	// Container is the output container name.
	Container string
	// MediaType is the output's HTTP media type.
	MediaType string
	// Live reports whether the container has a streaming form (a muxer
	// that does not need a seekable destination).
	Live bool
	// Versions are the version constants of every sample-affecting node,
	// DSP chain then encoder, for the cache key (ADR-0004).
	Versions []string
	// Samples is the projected output length from FromSample to the end,
	// -1 when the source length is unknown.
	Samples int64
	// BytesPerFrame is the output wire size of one frame across channels.
	BytesPerFrame int
	// BitRate is the projected output bit rate in bits per second, 0 when
	// unknown. PCM outputs derive it from the wire format; lossy encoders
	// will report their target rate here.
	BitRate int
	// EstimatedBytes is the projected total output size including the
	// nominal container header, -1 when the source length is unknown. A
	// hint for players, not a promise.
	EstimatedBytes int64
}

// eofReader satisfies dsp.Reader for plan-only chains, which are built
// for their Format and Versions and never pulled.
type eofReader struct{}

func (eofReader) ReadChunk(*audio.Buffer) error { return io.EOF }

// planKey addresses the chain-invariant part of a plan: everything except
// the seek position, which never shapes the chain.
type planKey struct {
	fmt  audio.Format
	opts TranscodeOptions
}

// planCore is the cached invariant part of a plan. Building it constructs
// (and releases) a real DSP chain, so Format and Versions can never drift
// from what Transcode assembles; the cache just keeps that construction
// off the per-request path.
type planCore struct {
	format        audio.Format
	container     string
	mediaType     string
	live          bool
	versions      []string
	l, m          int
	bytesPerFrame int
	headerBytes   int
}

// maxPlanCache bounds the plan cache; the key space is as unbounded as
// the rate parameter, so an adversarial parameter sweep must not grow
// memory. Past the cap, plans build per-request (correct, just slower).
const maxPlanCache = 1024

// PlanTranscode plans a transcode of the given source track without
// opening a pipeline. The same validation as Transcode applies, so a plan
// that succeeds will not fail chain assembly later.
func (e *Engine) PlanTranscode(track container.Track, opts TranscodeOptions) (*TranscodePlan, error) {
	if opts.FromSample < 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: negative FromSample")
	}
	key := planKey{fmt: track.Fmt, opts: opts}
	key.opts.FromSample = 0

	e.mu.RLock()
	core, ok := e.plans[key]
	e.mu.RUnlock()
	if !ok {
		var err error
		if core, err = buildPlanCore(track.Fmt, key.opts); err != nil {
			return nil, err
		}
		e.mu.Lock()
		if len(e.plans) < maxPlanCache {
			if e.plans == nil {
				e.plans = make(map[planKey]*planCore)
			}
			e.plans[key] = core
		}
		e.mu.Unlock()
	}

	remaining := track.Samples
	if remaining >= 0 {
		remaining = max(0, remaining-opts.FromSample)
	}
	samples := remaining
	if samples >= 0 {
		samples = (samples*int64(core.l) + int64(core.m) - 1) / int64(core.m)
	}
	estimated := int64(-1)
	if samples >= 0 {
		estimated = int64(core.headerBytes) + samples*int64(core.bytesPerFrame)
	}
	return &TranscodePlan{
		Format:         core.format,
		Container:      core.container,
		MediaType:      core.mediaType,
		Live:           core.live,
		Versions:       core.versions,
		Samples:        samples,
		BytesPerFrame:  core.bytesPerFrame,
		BitRate:        core.bytesPerFrame * core.format.Rate * 8,
		EstimatedBytes: estimated,
	}, nil
}

// buildPlanCore assembles and releases a throwaway chain to capture the
// plan invariants.
func buildPlanCore(in audio.Format, opts TranscodeOptions) (*planCore, error) {
	row, err := outputRow(opts.Format)
	if err != nil {
		return nil, err
	}
	chain, err := dsp.NewChain(dsp.NewSource(eofReader{}, in), specFor(opts))
	if err != nil {
		return nil, err
	}
	defer chain.Release()
	f := chain.Format()
	cfg, err := row.config(f)
	if err != nil {
		return nil, err
	}
	l, m := chain.Ratio()
	return &planCore{
		format:        f,
		container:     row.name,
		mediaType:     row.mediaType,
		live:          row.live,
		versions:      append(chain.Versions(), pcm.Version),
		l:             l,
		m:             m,
		bytesPerFrame: cfg.BytesPerFrame(f.Channels),
		headerBytes:   row.headerBytes,
	}, nil
}

// output is one row of the writer-side capability table, the analog of
// format's read-side driver table: the single source of truth for what
// the engine can produce. Rows appear here as encoders and muxers land;
// the CLI's extension inference and the /caps endpoint both read this
// table instead of maintaining their own lists.
type output struct {
	name string
	exts []string
	// live: the muxer writes a compliant stream to a plain io.Writer
	// (NeedsSeek false), so /stream can serve it.
	live bool
	// mediaType is the HTTP media type transcode responses carry.
	mediaType string
	// headerBytes is the nominal container overhead, for size estimates.
	headerBytes int
	config      func(f audio.Format) (pcm.Config, error)
	mux         func(dst io.Writer) container.Muxer
}

var outputs = []output{
	{
		name:        "wav",
		exts:        []string{"wav", "wave", "rf64", "bw64"},
		live:        true,
		mediaType:   "audio/wav",
		headerBytes: 44,
		config:      riff.DefaultConfig,
		mux:         func(dst io.Writer) container.Muxer { return riff.NewMuxer(dst, nil) },
	},
	{
		name:        "aiff",
		exts:        []string{"aif", "aiff", "aifc", "afc"},
		live:        false,
		mediaType:   "audio/aiff",
		headerBytes: 54,
		config:      aiff.DefaultConfig,
		mux:         func(dst io.Writer) container.Muxer { return aiff.NewMuxer(dst) },
	},
}

// DefaultLiveFormat returns the output format that format=auto resolves
// to when a transcode is required: the first registered output with a
// streaming form.
func DefaultLiveFormat() string {
	for _, o := range outputs {
		if o.live {
			return o.name
		}
	}
	return ""
}

// OutputInfo describes one entry of the writer-side capability table.
type OutputInfo struct {
	Name string
	Exts []string
	// Live reports a streaming form exists (plain io.Writer suffices).
	Live bool
}

// Outputs lists the registered output formats, in table order.
func Outputs() []OutputInfo {
	infos := make([]OutputInfo, len(outputs))
	for i, o := range outputs {
		infos[i] = OutputInfo{Name: o.name, Exts: append([]string(nil), o.exts...), Live: o.live}
	}
	return infos
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

// outputRow resolves an output format name against the table.
func outputRow(name string) (*output, error) {
	if name == "" {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: no output format requested")
	}
	for i := range outputs {
		if outputs[i].name == name {
			return &outputs[i], nil
		}
	}
	return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
		fmt.Sprintf("waxflow: unsupported output format %q (available: %s)", name, strings.Join(OutputFormats(), ", ")))
}
