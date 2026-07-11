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
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/codec/alac"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/codec/opus"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/adts"
	"github.com/colespringer/waxflow/container/aiff"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/container/mpa"
	"github.com/colespringer/waxflow/container/ogg"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/dsp"
	"github.com/colespringer/waxflow/dsp/dither"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/waxerr"
)

// Engine is the library-first entry point to the transcoding pipeline.
// The CLI and the HTTP server are both thin layers over it.
type Engine struct {
	log *slog.Logger
	idx IndexCache // nil: no index sidecar

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

// OpenStream opens src for decoded, sample-exact PCM access. With an
// IndexCache configured, a saved source index (the MP3 frame table) is
// restored into the demuxer before the first read, and a grown one is
// saved back when the media closes.
func (e *Engine) OpenStream(src container.Source, hint string) (format.Media, error) {
	med, err := format.Open(src, hint, nil)
	if err != nil || e.idx == nil {
		return med, err
	}
	ix, ok := med.(container.Indexer)
	if !ok {
		return med, nil
	}
	if blob := e.idx.Load(src); blob != nil {
		if !ix.RestoreIndex(blob) {
			// The demuxer found the blob inconsistent with the source;
			// drop it so it is not served (and kept warm) again.
			e.idx.Drop(src)
			e.log.Debug("index sidecar rejected and dropped", "hint", hint)
		}
	}
	return &indexSavingMedia{Media: med, ix: ix, cache: e.idx, src: src}, nil
}

// indexSavingMedia saves a grown source index when the media closes.
// Close is idempotent like the media it wraps (ix nils after the first
// call, so a double Close cannot double-save), and the wrapper stays
// transparent to container.Indexer asserts: embedding the Media
// interface does not promote methods outside it.
type indexSavingMedia struct {
	format.Media
	ix    container.Indexer
	cache IndexCache
	src   container.Source
}

func (m *indexSavingMedia) Close() error {
	if m.ix != nil {
		if blob := m.ix.IndexSnapshot(); blob != nil {
			m.cache.Save(m.src, blob)
		}
		m.ix = nil
	}
	return m.Media.Close()
}

func (m *indexSavingMedia) IndexSnapshot() []byte {
	if m.ix == nil {
		return nil
	}
	return m.ix.IndexSnapshot()
}

func (m *indexSavingMedia) RestoreIndex(blob []byte) bool {
	return m.ix != nil && m.ix.RestoreIndex(blob)
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
	row, err := outputRow(opts.Format)
	if err != nil {
		return nil, err
	}
	if opts.Container != "" && row.container == nil {
		// Mirror of the plan-side check: a container override the format
		// cannot honor must fail here too, so a caller skipping the plan
		// cannot have it silently ignored.
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: format %s has no alternate container (container=%s)", row.name, opts.Container))
	}
	spec := specFor(opts)
	if row.adjust != nil {
		row.adjust(&spec, srcTrack.Fmt, opts)
	}
	chain, err := dsp.NewChain(dsp.NewSource(med, srcTrack.Fmt), spec)
	if err != nil {
		return nil, err
	}
	defer chain.Release()

	f := chain.Format()
	enc, mux, err := row.build(f, opts, dst)
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

	track := container.Track{
		Codec:       row.codecID,
		CodecConfig: enc.CodecConfig(),
		Fmt:         f,
		Samples:     chain.OutputSamples(srcSamples),
		Default:     true,
	}
	// Encoders with priming declare it; muxers that can signal it (the
	// fMP4 edit list) read it from the track, and the trailer restates
	// it exactly at End.
	if d, ok := enc.(interface{ Delay() int }); ok {
		track.Delay = int64(d.Delay())
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
	buf := audio.Get(f, max(audio.StandardChunk, spec.FrameSize))
	defer audio.Put(buf)
	var done int64
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
		if opts.Progress != nil {
			done += int64(buf.N)
			opts.Progress(done, track.Samples)
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
	// The container is the format's default unless the request overrode
	// it, the same resolution PlanTranscode reports.
	containerName := row.name
	if opts.Container != "" {
		containerName = opts.Container
	}
	return &TranscodeResult{Samples: trailer.Samples, Format: f, Container: containerName}, nil
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
		// FrameSize stays 0 here; frame-native encoders set it through
		// their output row's adjust hook (the PCM rows accept any chunk
		// length).
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
	// FrameSize is the encoder-native frame length in output samples (the
	// chain framer's chunk), 0 for formats that accept any chunk length.
	// Segmented (HLS) outputs snap their boundaries to it.
	FrameSize int
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
// the seek position and the per-call payloads (tags, chapters, art, the
// progress callback), none of which shape the chain.
type planKey struct {
	fmt  audio.Format
	opts planOpts
}

// planOpts is the comparable projection of TranscodeOptions the plan
// cache keys on. Every TranscodeOptions field that shapes the chain or
// the plan MUST appear here with the same name and type;
// TestPlanOptsCoverage pins the two field lists together, because a new
// option missing from this key would let two different requests share a
// stale plan (wrong Format, wrong Versions, wrong cache entries).
type planOpts struct {
	Format          string
	Container       string
	Rate            int
	Channels        int
	BitDepth        int
	GainDB          float64
	FLACLevel       int
	MP3Bitrate      int
	MP3VBR          bool
	OpusBitrate     int
	AACBitrate      int
	OpusComplexity  int
	OpusVBR         bool
	OpusSignal      string
	Shaping         dither.Shaping
	ResampleProfile resample.Profile
}

// planOptsOf projects the plan-shaping option fields.
func planOptsOf(opts TranscodeOptions) planOpts {
	return planOpts{
		Format:          opts.Format,
		Container:       opts.Container,
		Rate:            opts.Rate,
		Channels:        opts.Channels,
		BitDepth:        opts.BitDepth,
		GainDB:          opts.GainDB,
		FLACLevel:       opts.FLACLevel,
		MP3Bitrate:      opts.MP3Bitrate,
		MP3VBR:          opts.MP3VBR,
		OpusBitrate:     opts.OpusBitrate,
		AACBitrate:      opts.AACBitrate,
		OpusComplexity:  opts.OpusComplexity,
		OpusVBR:         opts.OpusVBR,
		OpusSignal:      opts.OpusSignal,
		Shaping:         opts.Shaping,
		ResampleProfile: opts.ResampleProfile,
	}
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
	// bitRate is a fixed output bit rate in bits per second, for compressed
	// CBR encoders whose per-sample byte cost is fractional. 0 means derive
	// it from bytesPerFrame (uncompressed PCM) or leave it unknown (VBR).
	bitRate     int
	headerBytes int
	frameSize   int
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
	key := planKey{fmt: track.Fmt, opts: planOptsOf(opts)}

	e.mu.RLock()
	core, ok := e.plans[key]
	e.mu.RUnlock()
	if !ok {
		// The core builds from the normalized options: no seek position,
		// no per-call payloads, exactly what the key says.
		norm := opts
		norm.FromSample = 0
		norm.Tags, norm.Chapters, norm.Art, norm.Progress = nil, nil, nil, nil
		var err error
		if core, err = buildPlanCore(track.Fmt, norm); err != nil {
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
	// A fixed CBR bit rate (compressed) overrides the per-sample derivation;
	// PCM leaves bitRate 0 and derives it from bytesPerFrame. VBR encoders
	// report both 0, leaving size and rate hints honestly unknown.
	bitRate := core.bitRate
	if bitRate == 0 {
		bitRate = core.bytesPerFrame * core.format.Rate * 8
	}
	estimated := int64(-1)
	switch {
	case samples >= 0 && core.bytesPerFrame > 0:
		estimated = int64(core.headerBytes) + samples*int64(core.bytesPerFrame)
	case samples >= 0 && bitRate > 0 && core.format.Rate > 0:
		// CBR compressed: bytes = bit rate * duration / 8.
		estimated = int64(core.headerBytes) + samples*int64(bitRate)/(int64(core.format.Rate)*8)
	}
	return &TranscodePlan{
		Format:         core.format,
		Container:      core.container,
		MediaType:      core.mediaType,
		Live:           core.live,
		Versions:       core.versions,
		Samples:        samples,
		BytesPerFrame:  core.bytesPerFrame,
		FrameSize:      core.frameSize,
		BitRate:        bitRate,
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
	spec := specFor(opts)
	if row.adjust != nil {
		row.adjust(&spec, in, opts)
	}
	chain, err := dsp.NewChain(dsp.NewSource(eofReader{}, in), spec)
	if err != nil {
		return nil, err
	}
	defer chain.Release()
	f := chain.Format()
	version, bytesPerFrame, bitRate, err := row.plan(f, opts)
	if err != nil {
		return nil, err
	}
	l, m := chain.Ratio()
	containerName, mediaType := row.name, row.mediaType
	if opts.Container != "" {
		if row.container == nil {
			return nil, waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("waxflow: format %s has no alternate container (container=%s)", row.name, opts.Container))
		}
		mt, err := row.container(opts.Container)
		if err != nil {
			return nil, err
		}
		containerName, mediaType = opts.Container, mt
	}
	return &planCore{
		format:        f,
		container:     containerName,
		mediaType:     mediaType,
		live:          row.live,
		versions:      append(chain.Versions(), version),
		l:             l,
		m:             m,
		bytesPerFrame: bytesPerFrame,
		bitRate:       bitRate,
		headerBytes:   row.headerBytes,
		frameSize:     spec.FrameSize,
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
	// lossy reports the encoder discards audio for a target bit rate, so it
	// accepts the bitrate/q quality parameters. This is the single source of
	// truth for lossiness (a hardcoded name in the server would drift).
	lossy bool
	// mediaType is the HTTP media type transcode responses carry.
	mediaType string
	// headerBytes is the nominal container overhead, for size estimates.
	headerBytes int
	// codecID is what the encoder produces, for the muxed Track.
	codecID codec.ID
	// adjust folds the encoder's input constraints into the chain spec:
	// its native frame size, and a depth default when the encoder cannot
	// take the float domain the chain would otherwise emit. Plan and
	// Transcode both apply it, so they cannot disagree about the output
	// format. Nil when the encoder takes anything.
	adjust func(spec *dsp.ChainSpec, src audio.Format, opts TranscodeOptions)
	// plan validates the encoder configuration against the chain output
	// format and reports the encoder's cache-key version (ADR-0004), the
	// wire bytes per frame (0 when per-sample size is signal-dependent), and
	// a fixed bit rate in bits per second (0 unless the encoder is CBR with a
	// fractional per-sample byte cost). A plan that succeeds must guarantee
	// build succeeds.
	plan func(f audio.Format, opts TranscodeOptions) (version string, bytesPerFrame, bitRate int, err error)
	// build constructs the wired encoder and muxer for one transcode.
	build func(f audio.Format, opts TranscodeOptions, dst io.Writer) (codec.Encoder, container.Muxer, error)
	// container resolves a TranscodeOptions.Container override to its
	// HTTP media type; nil means the format has no alternate container
	// and any override is rejected up front (the unknown-parameter
	// principle: a request naming a container the format cannot honor
	// must fail, never fall back silently).
	container func(name string) (mediaType string, err error)
	// hls describes the format's segmented CMAF form; nil means the format
	// has none and cannot serve HLS.
	hls *hlsOutput
}

// hlsOutput is the writer-side table's segmented-delivery column: what an
// output format needs beyond its progressive muxer to become numbered
// fMP4 segments.
type hlsOutput struct {
	// codecs is the RFC 6381 CODECS attribute value for master playlists.
	codecs string
	// delay is the encoder delay in output samples. It rides in the init
	// segment's edit list and shifts the decode timeline: packet j holds
	// input samples [j*F-delay, (j+1)*F-delay).
	delay int64
	// encode builds the encoder for one segmented run. startSample is the
	// decode-timeline position of the first PCM sample the run feeds
	// (zero for a whole stream): FLAC numbers its frame headers by
	// absolute position, so a worker restarted mid-stream must say where
	// it stands; the other codecs ignore it.
	encode func(f audio.Format, opts TranscodeOptions, startSample int64) (codec.Encoder, error)
}

var outputs = []output{
	{
		name:        "wav",
		exts:        []string{"wav", "wave", "rf64", "bw64"},
		live:        true,
		mediaType:   "audio/wav",
		headerBytes: 44,
		codecID:     codec.PCM,
		plan: func(f audio.Format, _ TranscodeOptions) (string, int, int, error) {
			cfg, err := riff.DefaultConfig(f)
			if err != nil {
				return "", 0, 0, err
			}
			return pcm.Version, cfg.BytesPerFrame(f.Channels), 0, nil
		},
		build: func(f audio.Format, _ TranscodeOptions, dst io.Writer) (codec.Encoder, container.Muxer, error) {
			cfg, err := riff.DefaultConfig(f)
			if err != nil {
				return nil, nil, err
			}
			enc, err := pcm.NewEncoder(cfg, f)
			if err != nil {
				return nil, nil, err
			}
			return enc, riff.NewMuxer(dst, nil), nil
		},
	},
	{
		name:      "opus",
		exts:      []string{"opus"},
		live:      true,
		lossy:     true,
		mediaType: "audio/ogg",
		// headerBytes approximates the Ogg-Opus overhead the sample-based
		// estimate omits: the two header pages plus per-page framing. It is a
		// hint; the live stream is chunked with no Content-Length.
		headerBytes: 512,
		codecID:     codec.Opus,
		adjust: func(spec *dsp.ChainSpec, src audio.Format, _ TranscodeOptions) {
			// Opus decodes at 48 kHz and encodes float; more than two channels
			// downmix to stereo by default (the lossy-output convention). An
			// explicit channel request is never overridden: 1 and 2 are
			// honored, and anything else fails loudly in the chain or the
			// encoder rather than being silently rewritten.
			spec.Rate = opus.SampleRate
			spec.Float = true
			spec.BitDepth = 0
			spec.FrameSize = 960 // 20 ms at 48 kHz, the encoder-native frame
			if spec.Channels == 0 && src.Channels > 2 {
				spec.Channels = 2
			}
		},
		plan: func(f audio.Format, opts TranscodeOptions) (string, int, int, error) {
			eopts, err := opusEncoderOptions(opts)
			if err != nil {
				return "", 0, 0, err
			}
			enc, err := opus.NewEncoder(f, eopts)
			if err != nil {
				return "", 0, 0, err
			}
			return opus.EncoderVersion, 0, enc.Bitrate(), nil
		},
		build: func(f audio.Format, opts TranscodeOptions, dst io.Writer) (codec.Encoder, container.Muxer, error) {
			eopts, err := opusEncoderOptions(opts)
			if err != nil {
				return nil, nil, err
			}
			enc, err := opus.NewEncoder(f, eopts)
			if err != nil {
				return nil, nil, err
			}
			return enc, ogg.NewMuxer(dst, &ogg.MuxerOptions{Tags: opts.Tags}), nil
		},
		hls: &hlsOutput{
			codecs: "Opus",
			delay:  opus.EncoderDelay,
			encode: func(f audio.Format, opts TranscodeOptions, _ int64) (codec.Encoder, error) {
				eopts, err := opusEncoderOptions(opts)
				if err != nil {
					return nil, err
				}
				return opus.NewEncoder(f, eopts)
			},
		},
	},
	{
		name:        "aiff",
		exts:        []string{"aif", "aiff", "aifc", "afc"},
		live:        false,
		mediaType:   "audio/aiff",
		headerBytes: 54,
		codecID:     codec.PCM,
		plan: func(f audio.Format, _ TranscodeOptions) (string, int, int, error) {
			cfg, err := aiff.DefaultConfig(f)
			if err != nil {
				return "", 0, 0, err
			}
			return pcm.Version, cfg.BytesPerFrame(f.Channels), 0, nil
		},
		build: func(f audio.Format, _ TranscodeOptions, dst io.Writer) (codec.Encoder, container.Muxer, error) {
			cfg, err := aiff.DefaultConfig(f)
			if err != nil {
				return nil, nil, err
			}
			enc, err := pcm.NewEncoder(cfg, f)
			if err != nil {
				return nil, nil, err
			}
			return enc, aiff.NewMuxer(dst), nil
		},
	},
	{
		name:      "flac",
		exts:      []string{"flac"},
		live:      true,
		mediaType: "audio/flac",
		// headerBytes stays 0: size estimates are gated on a fixed
		// bytesPerFrame, which VBR lossless lacks.
		codecID: codec.FLAC,
		adjust: func(spec *dsp.ChainSpec, src audio.Format, opts TranscodeOptions) {
			// FLAC holds integer PCM only; a float source with no depth
			// requested quantizes to 24 bits, which carries the whole
			// float32 mantissa. An invalid level leaves FrameSize 0 and
			// plan reports the error.
			if level, err := flacLevel(opts); err == nil {
				spec.FrameSize = flac.EncoderBlockSize(level)
			}
			if src.Type == audio.Float && opts.BitDepth == 0 {
				spec.BitDepth = 24
			}
		},
		plan: func(f audio.Format, opts TranscodeOptions) (string, int, int, error) {
			level, err := flacLevel(opts)
			if err != nil {
				return "", 0, 0, err
			}
			if _, err := flac.NewEncoder(f, &flac.EncoderOptions{Level: level}); err != nil {
				return "", 0, 0, err
			}
			return flac.EncoderVersion(level), 0, 0, nil
		},
		build: func(f audio.Format, opts TranscodeOptions, dst io.Writer) (codec.Encoder, container.Muxer, error) {
			level, err := flacLevel(opts)
			if err != nil {
				return nil, nil, err
			}
			enc, err := flac.NewEncoder(f, &flac.EncoderOptions{Level: level})
			if err != nil {
				return nil, nil, err
			}
			return enc, flacn.NewMuxer(dst, &flacn.MuxerOptions{MD5: enc.MD5, Tags: opts.Tags}), nil
		},
		hls: &hlsOutput{
			codecs: "fLaC",
			encode: func(f audio.Format, opts TranscodeOptions, startSample int64) (codec.Encoder, error) {
				level, err := flacLevel(opts)
				if err != nil {
					return nil, err
				}
				// Segment boundaries are frame multiples, so a mid-stream
				// start is a whole frame number.
				return flac.NewEncoder(f, &flac.EncoderOptions{
					Level:      level,
					FirstFrame: startSample / int64(flac.EncoderBlockSize(level)),
				})
			},
		},
	},
	{
		name: "mp3",
		exts: []string{"mp3", "mpga"},
		live: true,
		// headerBytes approximates the non-audio overhead the sample-based
		// estimate omits: the leading Xing/Info frame plus the two flush
		// frames. The exact size is rate-dependent, so this is a hint (the
		// stream is chunked with no Content-Length anyway).
		headerBytes: 1024,
		lossy:       true,
		mediaType:   "audio/mpeg",
		codecID:     codec.MP3,
		adjust: func(spec *dsp.ChainSpec, _ audio.Format, _ TranscodeOptions) {
			// MP3 encodes float32 in native frames: two granules (MPEG-1) or
			// one (MPEG-2/2.5), which the framer resolves from the rate. The
			// lossy path always runs in the float domain, so any integer
			// depth request is dropped.
			spec.FrameSize = 1152
			spec.BitDepth = 0
			spec.Float = true
		},
		plan: func(f audio.Format, opts TranscodeOptions) (string, int, int, error) {
			eo, err := mp3EncoderOptions(opts)
			if err != nil {
				return "", 0, 0, err
			}
			enc, err := mp3.NewEncoder(f, eo)
			if err != nil {
				return "", 0, 0, err
			}
			// The encoder clamps to a layer-legal rate; report the actual
			// one. A VBR encoder reports 0, leaving rate and size hints
			// honestly unknown (the PlanTranscode VBR contract).
			return mp3.EncoderVersion, 0, enc.Bitrate(), nil
		},
		build: func(f audio.Format, opts TranscodeOptions, dst io.Writer) (codec.Encoder, container.Muxer, error) {
			eo, err := mp3EncoderOptions(opts)
			if err != nil {
				return nil, nil, err
			}
			enc, err := mp3.NewEncoder(f, eo)
			if err != nil {
				return nil, nil, err
			}
			return enc, mpa.NewMuxer(dst, &mpa.MuxerOptions{Delay: enc.Delay(), VBR: opts.MP3VBR, Tags: opts.Tags}), nil
		},
	},
	{
		name: "aac",
		// m4a moved here from the alac row when the AAC encoder landed
		// (the anticipated disambiguation): the extension overwhelmingly
		// means AAC in the wild. The bare .aac extension implies the
		// ADTS container at the CLI boundary.
		exts:      []string{"m4a", "aac"},
		live:      true,
		lossy:     true,
		mediaType: "audio/mp4",
		// headerBytes approximates the fMP4 init header (ftyp+moov).
		headerBytes: 700,
		codecID:     codec.AACLC,
		adjust: func(spec *dsp.ChainSpec, _ audio.Format, _ TranscodeOptions) {
			// AAC-LC encodes float32 in 1024-sample frames. The lossy path
			// always runs in the float domain, so any integer depth request
			// is dropped. The rate is not snapped: the encoder accepts the
			// thirteen AAC rates and rejects anything else at plan time,
			// like the mp3 row (an explicit rate= converts first).
			spec.FrameSize = 1024
			spec.BitDepth = 0
			spec.Float = true
		},
		plan: func(f audio.Format, opts TranscodeOptions) (string, int, int, error) {
			if _, err := aacContainerMediaType(opts.Container); err != nil {
				return "", 0, 0, err
			}
			enc, err := aac.NewEncoder(f, &aac.EncoderOptions{Bitrate: opts.AACBitrate})
			if err != nil {
				return "", 0, 0, err
			}
			// The encoder clamps to its legal range; report the actual
			// target (an ABR mean, held by the bit reservoir).
			return aac.EncoderVersion, 0, enc.Bitrate(), nil
		},
		build: func(f audio.Format, opts TranscodeOptions, dst io.Writer) (codec.Encoder, container.Muxer, error) {
			if _, err := aacContainerMediaType(opts.Container); err != nil {
				return nil, nil, err
			}
			enc, err := aac.NewEncoder(f, &aac.EncoderOptions{Bitrate: opts.AACBitrate})
			if err != nil {
				return nil, nil, err
			}
			if opts.Container == "adts" {
				return enc, adts.NewMuxer(dst), nil
			}
			return enc, mp4.NewMuxer(dst, mp4MuxerOptions(opts)), nil
		},
		container: aacContainerMediaType,
		hls: &hlsOutput{
			codecs: "mp4a.40.2",
			delay:  aac.EncoderDelay,
			encode: func(f audio.Format, opts TranscodeOptions, _ int64) (codec.Encoder, error) {
				return aac.NewEncoder(f, &aac.EncoderOptions{Bitrate: opts.AACBitrate})
			},
		},
	},
	{
		name: "alac",
		// exts is empty since the aac row claimed m4a: ALAC output is
		// reachable by naming the format explicitly.
		exts:      []string{},
		live:      true,
		mediaType: "audio/mp4",
		// headerBytes stays 0: size estimates are gated on a fixed
		// bytesPerFrame, which VBR lossless lacks.
		codecID: codec.ALAC,
		adjust: func(spec *dsp.ChainSpec, src audio.Format, opts TranscodeOptions) {
			spec.FrameSize = alac.FrameSize
			if opts.BitDepth != 0 {
				return // explicit depth; plan validates it against ALAC's set
			}
			// ALAC holds integer PCM at 16/20/24/32 bits. A float source with
			// no depth requested quantizes to 24 bits (the whole float32
			// mantissa); an integer source at another depth snaps up to the
			// nearest ALAC depth (8-bit becomes 16, losslessly widened).
			// alacSnapDepth is the identity on the ALAC depths, so a source
			// already at one needs no override.
			if src.Type == audio.Float {
				spec.BitDepth = 24
			} else if d := alacSnapDepth(src.BitDepth); d != src.BitDepth {
				spec.BitDepth = d
			}
		},
		plan: func(f audio.Format, _ TranscodeOptions) (string, int, int, error) {
			if _, err := alac.NewEncoder(f, nil); err != nil {
				return "", 0, 0, err
			}
			return alac.EncoderVersion, 0, 0, nil
		},
		build: func(f audio.Format, opts TranscodeOptions, dst io.Writer) (codec.Encoder, container.Muxer, error) {
			enc, err := alac.NewEncoder(f, nil)
			if err != nil {
				return nil, nil, err
			}
			return enc, mp4.NewMuxer(dst, mp4MuxerOptions(opts)), nil
		},
		hls: &hlsOutput{
			codecs: "alac",
			encode: func(f audio.Format, _ TranscodeOptions, _ int64) (codec.Encoder, error) {
				return alac.NewEncoder(f, nil)
			},
		},
	},
}

// aacContainerMediaType resolves the aac row's container override:
// empty selects progressive fragmented MP4 (the default, with gapless
// edit-list signaling), "adts" the raw elementary stream (no gapless
// signaling at all, which is why it is the opt-out and not the default).
func aacContainerMediaType(name string) (string, error) {
	switch name {
	case "":
		return "audio/mp4", nil
	case "adts":
		return "audio/aac", nil
	}
	return "", waxerr.New(waxerr.CodeInvalidRequest,
		fmt.Sprintf("waxflow: aac container %q: want adts (or empty for fMP4)", name))
}

// mp4MuxerOptions carries the per-call metadata payloads into the MP4
// muxer; nil when there are none, so the default construction stays the
// zero options.
func mp4MuxerOptions(opts TranscodeOptions) *mp4.MuxerOptions {
	if len(opts.Tags) == 0 && len(opts.Chapters) == 0 && opts.Art == nil {
		return nil
	}
	return &mp4.MuxerOptions{Tags: opts.Tags, Chapters: opts.Chapters, Art: opts.Art}
}

// alacSnapDepth rounds an integer bit depth up to the nearest ALAC depth.
func alacSnapDepth(d int) int {
	switch {
	case d <= 16:
		return 16
	case d <= 20:
		return 20
	case d <= 24:
		return 24
	default:
		return 32
	}
}

// opusEncoderOptions builds the codec-level Opus options from a transcode
// request, resolving the zero values to encoder defaults. An unknown signal
// hint fails here, so plans reject it before any work starts.
func opusEncoderOptions(opts TranscodeOptions) (*opus.EncoderOptions, error) {
	sig, err := opusSignal(opts)
	if err != nil {
		return nil, err
	}
	return &opus.EncoderOptions{
		Bitrate:    opusBitrate(opts),
		Complexity: opts.OpusComplexity,
		VBR:        opts.OpusVBR,
		Signal:     sig,
	}, nil
}

// opusSignal resolves TranscodeOptions.OpusSignal to the codec-level hint.
func opusSignal(opts TranscodeOptions) (opus.Signal, error) {
	switch opts.OpusSignal {
	case "", "auto":
		return opus.SignalAuto, nil
	case "voice":
		return opus.SignalVoice, nil
	case "music":
		return opus.SignalMusic, nil
	}
	return opus.SignalAuto, waxerr.New(waxerr.CodeInvalidRequest,
		fmt.Sprintf("opus signal hint %q is not auto, voice, or music", opts.OpusSignal))
}

// opusBitrate resolves TranscodeOptions.OpusBitrate: the zero value keeps the
// encoder default (96 kbit/s); any other value passes through to the encoder,
// which validates it.
func opusBitrate(opts TranscodeOptions) int {
	if opts.OpusBitrate == 0 {
		return opus.DefaultBitrate
	}
	return opts.OpusBitrate
}

// mp3EncoderOptions builds the codec-level MP3 options from a transcode
// request. The zero bit rate keeps the encoder default (128 kbit/s); any
// other value passes through to the encoder, which validates it against
// the layer's legal rates (a CBR rate, or the VBR quality anchor).
func mp3EncoderOptions(opts TranscodeOptions) (*mp3.EncoderOptions, error) {
	bitrate := opts.MP3Bitrate
	if bitrate == 0 {
		bitrate = mp3.DefaultBitrate
	}
	if bitrate < 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: MP3 bit rate %d is negative", bitrate))
	}
	return &mp3.EncoderOptions{Bitrate: bitrate, VBR: opts.MP3VBR}, nil
}

// flacLevel resolves TranscodeOptions.FLACLevel: the zero value keeps
// the encoder default, -1 selects level 0 (which the zero value cannot
// mean without stealing the default), and 1..8 pass through.
func flacLevel(opts TranscodeOptions) (int, error) {
	switch {
	case opts.FLACLevel == FLACLevelDefault:
		return flac.DefaultEncoderLevel, nil
	case opts.FLACLevel == FLACLevelFastest:
		return 0, nil
	case opts.FLACLevel >= 1 && opts.FLACLevel <= 8:
		return opts.FLACLevel, nil
	}
	return 0, waxerr.New(waxerr.CodeInvalidRequest,
		fmt.Sprintf("waxflow: FLAC level %d outside -1..8", opts.FLACLevel))
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
		// The copy starts from a non-nil empty slice so a format with no
		// extensions (alac since aac claimed m4a) marshals as [] not null.
		infos[i] = OutputInfo{Name: o.name, Exts: append([]string{}, o.exts...), Live: o.live}
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

// LossyFormat reports whether the named output format is lossy (accepts
// bitrate/q), and whether it is a registered format at all. An unregistered
// name returns (false, false) so callers defer to the format-existence error
// rather than mislabeling it as lossless.
func LossyFormat(name string) (lossy, known bool) {
	for _, o := range outputs {
		if o.name == name {
			return o.lossy, true
		}
	}
	return false, false
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
