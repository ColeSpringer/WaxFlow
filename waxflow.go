package waxflow

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/colespringer/waxflow/audio"
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
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/adts"
	"github.com/colespringer/waxflow/container/aiff"
	"github.com/colespringer/waxflow/container/apen"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/container/mka"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/container/mpa"
	"github.com/colespringer/waxflow/container/ogg"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/container/wv"
	"github.com/colespringer/waxflow/dsp"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/muxseek"
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
// optional file extension used only when no magic bytes match. ctx bounds the
// walk a strict probe runs for its verdict (see format.Probe).
func (e *Engine) Probe(ctx context.Context, src container.Source, hint string, opts *ProbeOptions) (*format.Info, error) {
	return format.Probe(ctx, src, hint, &format.Options{Strict: opts != nil && opts.Strict})
}

// OpenStream opens src for decoded, sample-exact PCM access. With an
// IndexCache configured, a saved source index (the MP3 frame table, a WMA
// v1/v2 frame count) is restored into the demuxer before the first read,
// and a grown one is saved back when the media closes.
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
		} else if format.MediaWalked(med) {
			// A complete index is a measurement already taken and walks for
			// free, so the media reports its length at once.
			if err := format.WalkMedia(context.Background(), med); err != nil {
				e.log.Debug("settling a restored index failed", "hint", hint, "err", err)
			}
		}
	}
	return &indexSavingMedia{Forwarder: format.Forwarder{Media: med}, ix: ix, cache: e.idx, src: src}, nil
}

// A method added to container.Indexer must not silently drop every sidecar.
var _ container.Indexer = (*indexSavingMedia)(nil)

// indexSavingMedia saves a grown source index when the media closes. Close is
// idempotent (ix nils after the first call, so nothing saves twice). It
// implements container.Indexer itself, since format.Forwarder does not
// forward it.
type indexSavingMedia struct {
	format.Forwarder
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

func (m *indexSavingMedia) KeepsIndex() bool { return m.ix != nil && m.ix.KeepsIndex() }

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
	// ClippedSamples counts channel samples (not frames, so against
	// Samples*Format.Channels) that reached the quantizer beyond full scale,
	// gain and resampler overshoot included (dsp.Chain.Clipped). Zero for
	// float outputs and copy rungs; a concat timeline's own clamps sit
	// upstream and go uncounted.
	ClippedSamples int64
	// TruePeak is the output's true-peak level, linear, 1.0 = full scale;
	// AnalyzeResult.TruePeakDB is 20*log10 of it. Lossy outputs measure the
	// encoder's input. Zero for copy rungs, pure integer passes, silence.
	TruePeak float64
	// Quantized reports whether a quantizer ran (float cut to integer).
	// It gates the true-peak note; see LevelNote.
	Quantized bool
	// InputWarnings is the input damage the read worked around, from the
	// source's Info after the whole stream was read, since a frame-walked
	// payload finds its damage only as the read reaches it. Nil for a clean
	// source. A concatenated timeline reports its members' under their index
	// ("member 1: ...").
	InputWarnings []string
	// LostSamples counts output samples that are silence standing in for
	// audio damage took (format.Info.Lost), since a hole keeps the length.
	// It counts this run's own read, scaled to Format.Rate. A copy rung
	// reports the silent frames it wrote, and a copy of a codec whose packets
	// state their positions (WavPack) the gap they carry, writing no silence.
	// Zero for a source with no hole, and for one whose holes shorten the
	// read instead: every format but FLAC (native or in Ogg) and WavPack.
	LostSamples int64
	// TruncatedSamples counts the samples the source's header declares past
	// the end of what the file holds (format.Info.Truncated), as this run's
	// read left it, scaled to Format.Rate. None is in the output. Zero for a
	// whole source, for a header that states no length, and for a window
	// that stops short of the source's end (Slice).
	TruncatedSamples int64
}

// LossNote returns the one warning line LostSamples and TruncatedSamples
// warrant, or "" for a source that lost nothing. It speaks of the output's
// own samples at Format.Rate, never the source's, which a resampling run does
// not report. Every surface that reports a result words it through here.
func (r *TranscodeResult) LossNote() string {
	if r == nil {
		return ""
	}
	return lossNote(r.LostSamples, r.TruncatedSamples, r.Format.Rate)
}

// lossNote is LossNote's wording for a result at rate, which says how long
// the counts are; a result with no single rate (a group's) states them bare.
func lossNote(lost, truncated int64, rate int) string {
	count := func(n int64) string {
		s := fmt.Sprintf("%d samples", n)
		if n == 1 {
			s = "1 sample"
		}
		if rate > 0 {
			s += fmt.Sprintf(" (%.2f s)", float64(n)/float64(rate))
		}
		return s
	}
	var parts []string
	if lost > 0 {
		verb := "are"
		if lost == 1 {
			verb = "is"
		}
		parts = append(parts, count(lost)+" "+verb+" silence where damage took the audio")
	}
	if truncated > 0 {
		parts = append(parts, "the audio ends "+count(truncated)+" short of the length its source declares")
	}
	return strings.Join(parts, "; ")
}

// LevelNote returns the one warning line the level fields warrant, or "":
// the clip count, else the true peak when a quantizer ran and only the
// waveform between samples is over. Copy and float paths stay silent, so
// the note cannot nag on every pass. Callers may append remedies.
func (r *TranscodeResult) LevelNote() string {
	switch {
	case r == nil:
		return ""
	case r.ClippedSamples > 0:
		return fmt.Sprintf("clipping: %d of %d output samples exceeded full scale",
			r.ClippedSamples, r.Samples*int64(r.Format.Channels))
	case r.Quantized && r.TruePeak > 1:
		return fmt.Sprintf("true peak: %+.2f dBTP; no stored sample is over, "+
			"but the waveform between samples crosses full scale and playback can clip it",
			20*math.Log10(r.TruePeak))
	}
	return ""
}

// Transcode decodes src and writes it to dst in the requested output format
// (decode, DSP, encode, mux), checking ctx between chunks. The DSP chain holds
// only the stages the options and the output row need, in dsp.NewChain's
// order, so with no options and a row that takes the source's format as it
// is, the decoder's samples reach the encoder unaltered: lossless to lossless
// is then a bit-exact rewrite. A lossy source is re-encoded (Remux copies
// packets). A positive FromSample seeks sample-exact before the first chunk.
// A muxer that back-patches its headers (AIFF, exact WAV sizes) needs a dst
// that can really seek, probed rather than read off the method set: WAV falls
// back to a streaming form on one that cannot, AIFF refuses. On a dst that
// cannot seek, a length a header declared and nothing checked is confirmed
// before it is projected.
func (e *Engine) Transcode(ctx context.Context, src container.Source, hint string, dst io.Writer, opts TranscodeOptions, run ...RunOption) (*TranscodeResult, error) {
	med, err := e.OpenStream(src, hint)
	if err != nil {
		return nil, err
	}
	defer med.Close()
	return e.TranscodeMedia(ctx, med, dst, opts, run...)
}

// TranscodeMedia is Transcode over an already-opened Media, for inputs that
// are not one sniffable Source (a timeline, a span, a presentation assembled
// from fetched resources). The caller owns med and closes it.
func (e *Engine) TranscodeMedia(ctx context.Context, med format.Media, dst io.Writer, opts TranscodeOptions, run ...RunOption) (*TranscodeResult, error) {
	if err := opts.Check(); err != nil {
		return nil, err
	}
	x := newRunConfig(run)
	srcTrack := med.Info().Default()
	row, err := outputRow(opts.Format)
	if err != nil {
		return nil, err
	}
	// The form resolves first, as PlanTranscode does, so both refuse a pair
	// no form takes alike, and before anything reads the source.
	fm, err := row.form(opts.Container)
	if err != nil {
		return nil, err
	}
	containerName := row.containerName(fm)
	// The spec is built before the seek so the width guard refuses before any
	// read. A confirm walk moves only the length fields, so the refreshed track
	// would give the same spec.
	spec := specFor(opts)
	if row.adjust != nil {
		row.adjust(&spec, srcTrack.Fmt, opts)
	}
	if err := refuseMixedWidthConversion(med, srcTrack.Fmt, spec.Channels); err != nil {
		return nil, err
	}
	var landed int64
	if opts.FromSample > 0 {
		if landed, err = med.SeekSample(ctx, opts.FromSample); err != nil {
			return nil, err
		}
	}
	// A header the muxer cannot patch commits, so its length is confirmed
	// first; a bound the decode is not yet held to is confirmed for any dst
	// (ADR-0002). Walk leaves the packet position, so the seek stands.
	patchable := muxseek.CanSeek(dst)
	if !patchable || unconfirmedBound(srcTrack) {
		if confirm := confirmableLength(srcTrack, med); confirm != nil {
			if err := confirm(ctx); err != nil {
				return nil, err
			}
			srcTrack = med.Info().Default()
		}
	}
	srcSamples := srcTrack.Samples
	if srcSamples >= 0 {
		srcSamples = max(0, srcSamples-landed)
	}
	// Where this run's read begins: a caller's Media may have been read
	// already, and the seek and walk above deliver nothing.
	lostBefore := med.Info().Lost
	// A run meters its output; specFor leaves this off so the plan's
	// throwaway chains and Analyze (which runs its own meter) do not.
	spec.MeterTruePeak = true
	chain, err := dsp.NewChain(dsp.NewSource(med, srcTrack.Fmt), spec)
	if err != nil {
		return nil, err
	}
	defer chain.Release()

	f := chain.Format()
	e.logImplicitDownmix(opts, srcTrack.Fmt, f)
	e.logTimelineDownmix(opts, med, srcTrack.Fmt)
	enc, err := row.encode(f, opts)
	if err != nil {
		return nil, err
	}

	// What the output's headers may promise (ADR-0002): provisional on a dst
	// the muxer can patch, since End corrects a miss. On one it cannot, a
	// count the source could confirm projects only once confirmed, and one it
	// cannot confirm projects as declared. An advisory total projects nothing
	// (-1).
	projected := srcSamples
	if projectedLength(srcTrack, med, patchable) < 0 {
		projected = -1
	}
	track := container.Track{
		Codec:       row.codecID,
		CodecConfig: enc.CodecConfig(),
		Fmt:         f,
		Samples:     chain.OutputSamples(projected),
		Default:     true,
	}
	// Progress keeps the headers' estimate, which promises nothing.
	progressTotal := chain.OutputSamples(srcSamples)
	// Encoders with priming declare it; muxers that can signal it (the fMP4
	// edit list) read it from the track, and the trailer restates it at End.
	if d, ok := enc.(interface{ Delay() int }); ok {
		track.Delay = int64(d.Delay())
	}
	// The muxer reads the stream's shape off the finished track, so it is
	// built after the encoder (form.mux).
	mux, err := fm.mux(track, opts, x, enc, dst)
	if err != nil {
		return nil, err
	}
	if err := checkSeekable(mux, dst, opts.Format); err != nil {
		return nil, err
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
		if x.progress != nil {
			done += int64(buf.N)
			x.progress(done, progressTotal)
		}
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		return nil, err
	}
	if err := mux.End(trailer); err != nil {
		return nil, missedProjection(patchable, track.Samples, trailer.Samples, err)
	}
	clipped, truePeak := chain.Clipped(), chain.TruePeak()
	e.log.Debug("transcode finished", "samples", trailer.Samples, "clipped", clipped, "truePeak", truePeak)
	found := med.Info()
	l, m := chain.Ratio()
	return &TranscodeResult{Samples: trailer.Samples, Format: f, Container: containerName,
		ClippedSamples: clipped, TruePeak: truePeak, Quantized: chain.Quantized(),
		InputWarnings:    slices.Clone(found.Warnings),
		LostSamples:      rescale(found.Lost-lostBefore, l, m),
		TruncatedSamples: rescale(found.Truncated, l, m)}, nil
}

// lengthWalker is the deferred walk both domains expose under the same two
// methods: format.Walker on a Media, container.Walker on a demuxer.
type lengthWalker interface {
	Walk(ctx context.Context) error
	Walked() bool
}

// confirmableLength returns the work that confirms t's length when a header
// declared it and nothing has checked it (not exact, not advisory, not
// unknown), and nil otherwise. src's own deferred walk (lengthWalker) does the
// work, or format.LengthConfirmer on a Media that is not a file (an open
// span); a Concat has neither. Nil means no confirming is needed or none is
// possible, and a caller treats the two alike: a source that cannot confirm
// projects what it declared (ADR-0002).
func confirmableLength(t container.Track, src any) func(context.Context) error {
	if t.Samples < 0 || t.SamplesExact || t.SamplesAdvisory {
		return nil
	}
	// A finished walk is still the work: only Walk puts the measurement on
	// the track, and once finished it costs nothing.
	if w, ok := src.(lengthWalker); ok {
		return w.Walk
	}
	if c, ok := src.(format.LengthConfirmer); ok {
		return c.ConfirmLength
	}
	return nil
}

// unconfirmedBound reports whether t's declared length is a bound nothing
// holds the run to yet: its end trim is unread, and no delay caps the decode.
func unconfirmedBound(t container.Track) bool { return t.TrimsUnread && t.Delay == 0 }

// projectedLength is what a run may write into its output's headers for a
// source track: the track's own count, or -1 for no claim. The transcode and
// remux rungs both ask here, so the rule cannot drift. patchable is whether
// the muxer can fix what it wrote; src is asked again in case its walk could
// still confirm the length.
func projectedLength(t container.Track, src any, patchable bool) int64 {
	if t.SamplesAdvisory || (!patchable && confirmableLength(t, src) != nil) {
		return -1
	}
	return t.Samples
}

// missedProjection reclassifies a muxer's End failure on an unpatchable dst
// whose output missed its projected length: the internal error the muxer
// raises for that becomes CodeMalformedInput, since with a confirmed count
// only damage the read tolerated is left to cause it. Every other error,
// and any error on a patchable dst, passes through unchanged (ADR-0002).
func missedProjection(patchable bool, projected, produced int64, err error) error {
	if err == nil || patchable || projected < 0 || produced == projected ||
		waxerr.CodeOf(err) != waxerr.CodeInternal {
		return err
	}
	return waxerr.Wrap(waxerr.CodeMalformedInput, fmt.Sprintf(
		"waxflow: the encoder produced %d output frames against a projection of %d; "+
			"the output's headers were already written from the projection", produced, projected), err)
}

// form resolves a Container value against the row: "" selects the row's
// default form, any other name one of its overrides. A name the row has no
// form for is refused, never served in the default form, and every rung
// resolves through here so a plan and its run agree on the wrapper.
func (r *output) form(name string) (*form, error) {
	if name == "" {
		return &r.forms[0], nil
	}
	for i := 1; i < len(r.forms); i++ {
		if r.forms[i].name == name {
			return &r.forms[i], nil
		}
	}
	alts := make([]string, 0, len(r.forms)-1)
	for _, f := range r.forms[1:] {
		alts = append(alts, f.name)
	}
	var want string
	switch n := len(alts); n {
	case 0:
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: format %s has no alternate container (container=%s)", r.name, name))
	case 1:
		want = alts[0]
	case 2:
		want = alts[0] + " or " + alts[1]
	default:
		want = strings.Join(alts[:n-1], ", ") + ", or " + alts[n-1]
	}
	return nil, waxerr.New(waxerr.CodeInvalidRequest,
		fmt.Sprintf("waxflow: %s container %q: want %s", r.name, name, want))
}

// containerName is the name a plan reports for f: its override name, or the
// row's own name for the default form.
func (r *output) containerName(f *form) string {
	if f.name == "" {
		return r.name
	}
	return f.name
}

// formOrDefault is the form a (format, container) pair names, or the row's
// default form for a pair no form takes. ok is false for an unknown format.
func formOrDefault(format, container string) (*output, *form, bool) {
	row, err := outputRow(format)
	if err != nil {
		return nil, nil, false
	}
	f, err := row.form(container)
	if err != nil {
		f = &row.forms[0]
	}
	return row, f, true
}

// checkSeekable refuses, with CodeInvalidRequest and before any work starts,
// a back-patching muxer (NeedsSeek) on a dst that cannot seek, so no muxer
// or rung re-invents the guard. It probes rather than asserting the type: an
// *os.File on a pipe carries io.WriteSeeker and cannot seek.
func checkSeekable(mux container.Muxer, dst io.Writer, format string) error {
	if !mux.NeedsSeek() {
		return nil
	}
	if !muxseek.CanSeek(dst) {
		return waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: %s output requires a seekable destination", format))
	}
	return nil
}

// specFor maps TranscodeOptions onto the DSP chain spec, in one place so
// Transcode and PlanTranscode cannot drift.
func specFor(opts TranscodeOptions) dsp.ChainSpec {
	return dsp.ChainSpec{
		Rate:     opts.Rate,
		Channels: opts.Channels,
		BitDepth: opts.BitDepth,
		GainDB:   opts.GainDB,
		Dynamics: opts.Dynamics,
		Shaping:  opts.Shaping,
		Profile:  opts.ResampleProfile,
		// FrameSize stays 0 here; frame-native encoders set it through
		// their output row's adjust hook (the PCM rows accept any chunk
		// length).
	}
}

// TranscodePlan describes what a transcode would produce, from headers
// alone, so the ADR-0004 cache key and the response headers (duration, size
// estimate) exist before any pipeline does.
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
	// Versions are the version constants of every node whose revision can
	// change the output bytes, in pipeline order (ADR-0004): the demuxer when
	// it names one, the source decoder, the float build, the DSP chain, the
	// encoder, the muxer.
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
	// unknown. PCM outputs derive it from the wire format, lossy encoders
	// report their target rate, and unconstrained VBR reports 0.
	BitRate int
	// EstimatedBytes is the projected total output size including the
	// nominal container header, -1 when the source length is unknown. A
	// hint for players, not a promise.
	EstimatedBytes int64
}

// withDemuxer puts the track's demuxer revision, when it names one, ahead of
// a plan's versions. Every plan built from one source track assembles its
// Versions through here, so a rung that copies packets keys on the demuxer
// as a rung that decodes does. A timeline lists its members' revisions
// itself (timelineVersions).
func withDemuxer(track container.Track, versions ...string) []string {
	if track.DemuxerVersion == "" {
		return versions
	}
	return append([]string{track.DemuxerVersion}, versions...)
}

// decodeVersion is the plan's decoder term (ADR-0004), from format's decoder
// registry so no second table of codecs can fall short. The plan core is
// keyed on the decoded PCM format, so this composes per call.
func decodeVersion(id codec.ID) string {
	if v := format.DecoderVersion(id); v != "" {
		return v
	}
	// An unregistered codec cannot decode, but its plans still key apart.
	return "dec:" + string(id)
}

// eofReader satisfies dsp.Reader for plan-only chains, which are built
// for their Format and Versions and never pulled.
type eofReader struct{}

func (eofReader) ReadChunk(*audio.Buffer) error { return io.EOF }

// planKey addresses the chain-invariant part of a plan: the source format and
// the options with FromSample zeroed, since a seek does not shape the chain.
type planKey struct {
	fmt  audio.Format
	opts TranscodeOptions
}

// planCore is the cached invariant part of a plan. Building it constructs
// and releases a real DSP chain, so Format and Versions cannot drift from
// what Transcode assembles.
type planCore struct {
	format        audio.Format
	container     string
	mediaType     string
	live          bool
	versions      []string
	l, m          int
	bytesPerFrame int
	// bitRate is a lossy encoder's target bit rate in bits per second. 0
	// derives it from bytesPerFrame (PCM) or leaves it unknown
	// (unconstrained VBR, lossless).
	bitRate     int
	headerBytes int
	frameSize   int
}

// maxPlanCache bounds the plan cache against a parameter sweep (the key
// space is as unbounded as the rate). Past the cap, plans build per request.
const maxPlanCache = 1024

// PlanTranscode plans a transcode of the given source track without
// opening a pipeline. The same validation as Transcode applies, so a plan
// that succeeds will not fail chain assembly later.
func (e *Engine) PlanTranscode(track container.Track, opts TranscodeOptions) (*TranscodePlan, error) {
	if err := opts.Check(); err != nil {
		return nil, err
	}
	// The core builds from the options the key names: no seek position.
	norm := opts.normalized()
	norm.FromSample = 0
	key := planKey{fmt: track.Fmt, opts: norm}

	e.mu.RLock()
	core, ok := e.plans[key]
	e.mu.RUnlock()
	if !ok {
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
	// A lossy target rate overrides the per-sample derivation PCM uses.
	// Unconstrained VBR and lossless report both 0, leaving size and rate
	// unknown.
	bitRate := core.bitRate
	if bitRate == 0 {
		bitRate = core.bytesPerFrame * core.format.Rate * 8
	}
	estimated := int64(-1)
	switch {
	case samples >= 0 && core.bytesPerFrame > 0:
		estimated = int64(core.headerBytes) + samples*int64(core.bytesPerFrame)
	case samples >= 0 && bitRate > 0 && core.format.Rate > 0:
		// A lossy target rate: bytes = bit rate * duration / 8.
		estimated = int64(core.headerBytes) + samples*int64(bitRate)/(int64(core.format.Rate)*8)
	}
	return &TranscodePlan{
		Format:         core.format,
		Container:      core.container,
		MediaType:      core.mediaType,
		Live:           core.live,
		Versions:       withDemuxer(track, append([]string{decodeVersion(track.Codec)}, core.versions...)...),
		Samples:        samples,
		BytesPerFrame:  core.bytesPerFrame,
		FrameSize:      core.frameSize,
		BitRate:        bitRate,
		EstimatedBytes: estimated,
	}, nil
}

// PlanTranscodeTimeline plans a transcode of a concatenated timeline from its
// members' tracks alone (no open), as PlanSegmentsTimeline does for HLS: it
// is PlanTranscode over ConcatTrack's track, with the versions that track
// cannot name prepended (timelineVersions) and a channel conversion across
// a mixed-width seam refused. copts must be the options the planned Concat
// is built with, and opts.FromSample seeks on the timeline's own clock.
func (e *Engine) PlanTranscodeTimeline(tracks []container.Track, copts ConcatOptions,
	opts TranscodeOptions) (*TranscodePlan, error) {
	env, err := ConcatTrack(tracks, copts)
	if err != nil {
		return nil, err
	}
	plan, err := e.PlanTranscode(env, opts)
	if err != nil {
		return nil, err
	}
	if err := finishTimelinePlan(tracks, env.Fmt, copts, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// buildPlanCore assembles and releases a throwaway chain to capture the
// plan invariants.
func buildPlanCore(in audio.Format, opts TranscodeOptions) (*planCore, error) {
	row, err := outputRow(opts.Format)
	if err != nil {
		return nil, err
	}
	// The container is resolved before any other option is read, as the run
	// and PlanRemux do, so a pair no form takes gets the same refusal from all.
	fm, err := row.form(opts.Container)
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
		// The encoder's own words name no remedy to a caller who never asked
		// for a channel count, so one is added, in option vocabulary; %w keeps
		// the code. planAcceptsStereo keeps it off refusals that are not about
		// channels (a bad FLAC level on a 5.1 source).
		if opts.Channels == 0 && in.Channels > 2 && planAcceptsStereo(row, f, opts) {
			return nil, fmt.Errorf("%w; set the output channel count to 2", err)
		}
		return nil, err
	}
	l, m := chain.Ratio()
	// A chain computes in floats, so its plan names how this build rounds
	// them, where that can differ (FloatBuildVersion). The copy rungs run no
	// chain and name no build.
	versions := append(chain.Versions(), version, fm.version)
	if FloatBuildVersion != "" {
		versions = append([]string{FloatBuildVersion}, versions...)
	}
	return &planCore{
		format:        f,
		container:     row.containerName(fm),
		mediaType:     fm.mediaType,
		live:          fm.live,
		versions:      versions,
		l:             l,
		m:             m,
		bytesPerFrame: bytesPerFrame,
		bitRate:       bitRate,
		headerBytes:   row.headerBytes,
		frameSize:     spec.FrameSize,
	}, nil
}

// planAcceptsStereo reports whether row.plan would have succeeded at stereo:
// "would asking for a channel count have helped?", put to the encoder rather
// than guessed from the error text. Failure path only.
func planAcceptsStereo(row *output, f audio.Format, opts TranscodeOptions) bool {
	f.Channels = 2
	f.Layout = audio.DefaultLayout(2)
	_, _, _, err := row.plan(f, opts)
	return err == nil
}

// output is one row of the writer-side capability table, the single source
// of truth for what the engine can produce; the CLI's extension inference
// and /caps read it.
type output struct {
	name string
	// exts are the extensions that resolve to this format (OutputFormatForExt).
	// At most one row claims any extension.
	exts []string
	// writeExt is the extension a file of this format is written with
	// (OutputExt) when it is not the first claimed one: alac writes .m4a,
	// which resolves to aac.
	writeExt string
	// forms are the wrappers the row is written in; the first is the default,
	// selected by the empty Container.
	forms []form
	// lossy reports the encoder discards audio. A lossless row holds no bit
	// rate, which is what KnobError.Lossless says.
	lossy bool
	// knobs are the encoder knobs the row takes; Check refuses any other set.
	knobs Knob
	// headerBytes is the nominal container overhead, for size estimates.
	headerBytes int
	// codecID is what the encoder produces, for the muxed Track.
	codecID codec.ID
	// adjust folds the encoder's input constraints into the chain spec (its
	// native frame size, a rate, a width, a depth default). Plan and run both
	// apply it, so they agree on the output format. Nil when the encoder
	// takes anything.
	adjust func(spec *dsp.ChainSpec, src audio.Format, opts TranscodeOptions)
	// plan validates the encoder configuration against the chain output
	// format and reports the encoder's cache-key version (ADR-0004), the wire
	// bytes per frame (0 when signal-dependent), and the target bit rate in
	// bits per second of a lossy encoder that holds one (0 for unconstrained
	// VBR, lossless and PCM). A plan that succeeds must guarantee encode
	// succeeds.
	plan func(f audio.Format, opts TranscodeOptions) (version string, bytesPerFrame, bitRate int, err error)
	// encode constructs the row's encoder for one transcode.
	encode func(f audio.Format, opts TranscodeOptions) (codec.Encoder, error)
	// hls describes the format's segmented CMAF form; nil means the format
	// has none and cannot serve HLS.
	hls *hlsOutput
	// remuxOnly marks a row that exists so remux can carry a codec no
	// encoder produces. outputRow resolves it by name, and every list of what
	// the engine can encode skips it (Outputs, OutputFormats,
	// OutputFormatForExt, DefaultLiveFormat, SegmentedFormats, CutFormats,
	// knobTakers).
	remuxOnly bool
}

// form is one wrapper a row can be written in.
type form struct {
	name      string // the Container value that selects it; "" is the row's own
	mediaType string // what an HTTP response carries
	ext       string // "" keeps the row's extension
	version   string // ADR-0004's muxer term
	live      bool   // NeedsSeek false: /stream can serve it
	// embedsTags: the tagging post-pass skips this output (OutputEmbedsTags).
	// A form with it false may still write the run's tags; the post-pass then
	// writes the full set over them.
	embedsTags bool
	// tagsFit is the muxer's answer to whether a tag set fits its block, from
	// the builder it writes with (TagsFit). Every form has one: noTags for a
	// muxer that takes none.
	tagsFit func([]container.Tag) error
	// trackFits is the muxer's own refusal of a track at Begin (TrackFits),
	// which PlanRemux asks so a copy declines what the muxer would refuse.
	// Nil for a muxer whose Begin refuses nothing its row's codec can bring.
	trackFits func(container.Track) error
	// trimByLength: with no delay, the muxer states an end trim only in the
	// length its Begin writes, so a copy needs both before Begin.
	trimByLength bool
	// mux constructs the writer, apart from encode because remux writes
	// through the same muxer with no encoder. t is the output track, where a
	// muxer reads what it needs (the mpa Xing delay), and x the run's
	// payloads (tags, chapters, art). enc is nil on a remux, which keeps the
	// source's own FLAC MD5.
	mux func(t container.Track, opts TranscodeOptions, x runConfig, enc codec.Encoder, dst io.Writer) (container.Muxer, error)
}

// hlsOutput is the writer-side table's segmented-delivery column: what an
// output format needs beyond its progressive muxer to become numbered
// fMP4 segments.
type hlsOutput struct {
	// codecs is the RFC 6381 CODECS attribute value for master playlists.
	codecs string
	// codecsFor resolves an option-dependent CODECS value (he-aac's on
	// HEAACv2); nil uses codecs. A remux resolves from the source's ASC
	// instead (remuxCodecs).
	codecsFor func(opts TranscodeOptions) string
	// delay is the encoder delay in output samples. It rides in the init
	// segment's edit list and shifts the decode timeline: packet j holds
	// input samples [j*F-delay, (j+1)*F-delay).
	delay int64
	// encode builds the encoder for one segmented run. startSample is the
	// decode-timeline position of the first sample the run feeds (zero for
	// a whole stream), which FLAC's absolute frame numbers need; the other
	// codecs ignore it.
	encode func(f audio.Format, opts TranscodeOptions, startSample int64) (codec.Encoder, error)
}

// The AAC family's RFC 6381 CODECS strings, shared by the rows below and
// the remux path's own-signalling override (remuxCodecs).
const (
	codecsAACLC = "mp4a.40.2"
	codecsHEv1  = "mp4a.40.5"
	codecsHEv2  = "mp4a.40.29"
)

// heEncOpts maps the engine options onto the HE encoder's, the one
// literal the he-aac row's plan, encode, and segmented columns share.
func heEncOpts(opts TranscodeOptions) *aac.EncoderOptions {
	return &aac.EncoderOptions{Bitrate: opts.Bitrate, ParametricStereo: opts.HEAACv2}
}

var outputs = []output{
	{
		name:        "wav",
		exts:        []string{"wav", "wave", "rf64", "bw64"},
		headerBytes: 44,
		codecID:     codec.PCM,
		plan: func(f audio.Format, _ TranscodeOptions) (string, int, int, error) {
			cfg, err := riff.DefaultConfig(f)
			if err != nil {
				return "", 0, 0, err
			}
			return pcm.Version, cfg.BytesPerFrame(f.Channels), 0, nil
		},
		encode: func(f audio.Format, opts TranscodeOptions) (codec.Encoder, error) {
			if isMatroska(opts.Container) {
				return pcm.NewEncoder(mkaPCMConfig(f), f)
			}
			cfg, err := riff.DefaultConfig(f)
			if err != nil {
				return nil, err
			}
			return pcm.NewEncoder(cfg, f)
		},
		// PCM rides in Matroska (A_PCM/INT/LIT or A_PCM/FLOAT/IEEE) but not
		// WebM, whose audio subset is Opus and Vorbis only.
		forms: []form{
			{
				mediaType: "audio/wav", version: riff.MuxerVersion, live: true, tagsFit: noTags,
				mux: func(_ container.Track, _ TranscodeOptions, _ runConfig, _ codec.Encoder, dst io.Writer) (container.Muxer, error) {
					return riff.NewMuxer(dst, nil), nil
				},
			},
			mkaForm(),
		},
	},
	{
		name:  "opus",
		knobs: KnobBitrate | KnobVBR | KnobOpusComplexity | KnobOpusSignal,
		exts:  []string{"opus"},
		lossy: true,
		// headerBytes approximates the Ogg-Opus header pages and page framing.
		headerBytes: 512,
		codecID:     codec.Opus,
		adjust: func(spec *dsp.ChainSpec, src audio.Format, _ TranscodeOptions) {
			// Opus decodes at 48 kHz and encodes float.
			spec.Rate = opus.SampleRate
			spec.Float = true
			spec.BitDepth = 0
			spec.FrameSize = opus.FrameSize
			foldWideToStereo(spec, src)
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
			// The encoder clamps to what a 20 ms frame can carry, so this
			// reports the rate it uses; unconstrained VBR reports 0.
			return opus.EncoderVersion, 0, enc.Bitrate(), nil
		},
		encode: func(f audio.Format, opts TranscodeOptions) (codec.Encoder, error) {
			eopts, err := opusEncoderOptions(opts)
			if err != nil {
				return nil, err
			}
			return opus.NewEncoder(f, eopts)
		},
		forms: []form{oggForm(ogg.OpusTagsFit), mkaForm(), webmForm()},
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
		name:  "vorbis",
		knobs: KnobVorbisQuality,
		// "ogg" and "oga" conventionally mean Ogg-Vorbis; no other row claims
		// them.
		exts:  []string{"ogg", "oga"},
		lossy: true,
		// headerBytes approximates the three Ogg-Vorbis header pages (the
		// setup header is the bulk) and page framing.
		headerBytes: 4096,
		codecID:     codec.Vorbis,
		adjust: func(spec *dsp.ChainSpec, _ audio.Format, _ TranscodeOptions) {
			// Vorbis encodes float at the source rate and width (up to eight
			// channels). Its block sizes vary, so FrameSize stays 0.
			spec.Float = true
			spec.BitDepth = 0
		},
		plan: func(f audio.Format, opts TranscodeOptions) (string, int, int, error) {
			eopts, err := vorbisEncoderOptions(opts)
			if err != nil {
				return "", 0, 0, err
			}
			enc, err := vorbis.NewEncoder(f, eopts)
			if err != nil {
				return "", 0, 0, err
			}
			return vorbis.EncoderVersion(opts.VorbisQuality), 0, enc.Bitrate(), nil
		},
		encode: func(f audio.Format, opts TranscodeOptions) (codec.Encoder, error) {
			eopts, err := vorbisEncoderOptions(opts)
			if err != nil {
				return nil, err
			}
			return vorbis.NewEncoder(f, eopts)
		},
		forms: []form{oggForm(ogg.VorbisTagsFit), mkaForm(), webmForm()},
		// hls stays nil: Vorbis has no CMAF/HLS segmented form.
	},
	{
		name:        "aiff",
		exts:        []string{"aif", "aiff", "aifc", "afc"},
		headerBytes: 54,
		codecID:     codec.PCM,
		plan: func(f audio.Format, _ TranscodeOptions) (string, int, int, error) {
			cfg, err := aiff.DefaultConfig(f)
			if err != nil {
				return "", 0, 0, err
			}
			return pcm.Version, cfg.BytesPerFrame(f.Channels), 0, nil
		},
		encode: func(f audio.Format, _ TranscodeOptions) (codec.Encoder, error) {
			cfg, err := aiff.DefaultConfig(f)
			if err != nil {
				return nil, err
			}
			return pcm.NewEncoder(cfg, f)
		},
		// The AIFF muxer back-patches its chunk sizes, so it is not live.
		forms: []form{{
			mediaType: "audio/aiff", version: aiff.MuxerVersion, tagsFit: noTags,
			mux: func(_ container.Track, _ TranscodeOptions, _ runConfig, _ codec.Encoder, dst io.Writer) (container.Muxer, error) {
				return aiff.NewMuxer(dst), nil
			},
		}},
	},
	{
		name:  "flac",
		knobs: KnobFLACLevel,
		exts:  []string{"flac"},
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
		encode: func(f audio.Format, opts TranscodeOptions) (codec.Encoder, error) {
			level, err := flacLevel(opts)
			if err != nil {
				return nil, err
			}
			return flac.NewEncoder(f, &flac.EncoderOptions{Level: level})
		},
		// Not WebM. Both FLAC muxers write a STREAMINFO, so both take the
		// encoder's MD5.
		forms: []form{
			{
				mediaType: "audio/flac", version: flacn.MuxerVersion, live: true, tagsFit: flacn.TagsFit,
				trackFits: flacn.TrackFits,
				mux: func(_ container.Track, _ TranscodeOptions, x runConfig, enc codec.Encoder, dst io.Writer) (container.Muxer, error) {
					return flacn.NewMuxer(dst, &flacn.MuxerOptions{Tags: x.tags, MD5: flacMD5(enc)}), nil
				},
			},
			mkaForm(),
			{
				// .oga, the Xiph extension for Ogg audio that is not Vorbis:
				// .flac would name a native-FLAC file these bytes are not.
				name: ContainerOgg, mediaType: "audio/ogg", ext: "oga", version: ogg.MuxerVersion,
				live: true, embedsTags: true, tagsFit: ogg.FLACTagsFit, trackFits: ogg.TrackFits,
				mux: func(_ container.Track, _ TranscodeOptions, x runConfig, enc codec.Encoder, dst io.Writer) (container.Muxer, error) {
					return ogg.NewMuxer(dst, &ogg.MuxerOptions{Tags: x.tags, MD5: flacMD5(enc)}), nil
				},
			},
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
		name:  "mp3",
		knobs: KnobBitrate | KnobVBR,
		exts:  []string{"mp3", "mpga"},
		// headerBytes approximates the leading Xing/Info frame and the two
		// flush frames, whose exact size is rate-dependent.
		headerBytes: 1024,
		lossy:       true,
		codecID:     codec.MP3,
		adjust: func(spec *dsp.ChainSpec, src audio.Format, _ TranscodeOptions) {
			// MP3 encodes float32 in frames of two granules (MPEG-1) or one
			// (MPEG-2/2.5), so the frame size follows the output rate. A depth
			// request is dropped.
			rate := spec.Rate
			if rate == 0 {
				rate = src.Rate
			}
			spec.FrameSize = mp3.FrameSize(rate)
			spec.BitDepth = 0
			spec.Float = true
			foldWideToStereo(spec, src)
		},
		plan: func(f audio.Format, opts TranscodeOptions) (string, int, int, error) {
			enc, err := mp3.NewEncoder(f, mp3EncoderOptions(opts))
			if err != nil {
				return "", 0, 0, err
			}
			// The encoder clamps to a layer-legal rate, so this reports the
			// rate it uses; VBR reports 0.
			return mp3.EncoderVersion, 0, enc.Bitrate(), nil
		},
		encode: func(f audio.Format, opts TranscodeOptions) (codec.Encoder, error) {
			return mp3.NewEncoder(f, mp3EncoderOptions(opts))
		},
		// The Xing delay reads off the track: the encoder's on a transcode, the
		// source's on a remux. A nil enc (remux) says the delay already holds
		// the 529-sample decoder latency (mpa.MuxerOptions.DecodedTrims).
		forms: []form{{
			mediaType: "audio/mpeg", version: mpa.MuxerVersion, live: true, tagsFit: mpa.TagsFit,
			mux: func(t container.Track, opts TranscodeOptions, x runConfig, enc codec.Encoder, dst io.Writer) (container.Muxer, error) {
				return mpa.NewMuxer(dst, &mpa.MuxerOptions{
					Delay:        int(t.Delay),
					DecodedTrims: enc == nil,
					VBR:          opts.VBR,
					Tags:         x.tags,
				}), nil
			},
		}},
	},
	{
		name:  "aac",
		knobs: KnobBitrate,
		// m4a means AAC in the wild. The bare .aac implies the ADTS container
		// at the CLI boundary. m4b, the audiobook spelling, stays after m4a,
		// which OutputExt writes.
		exts:  []string{"m4a", "aac", "m4b"},
		lossy: true,
		// headerBytes approximates the fMP4 init header (ftyp+moov).
		headerBytes: 700,
		codecID:     codec.AACLC,
		adjust: func(spec *dsp.ChainSpec, src audio.Format, _ TranscodeOptions) {
			// AAC-LC encodes float32 in 1024-sample frames; a depth request is
			// dropped. The rate is not snapped: the encoder refuses a rate
			// outside the thirteen AAC rates at plan time.
			spec.FrameSize = aac.FrameSize
			spec.BitDepth = 0
			spec.Float = true
			foldWideToStereo(spec, src)
		},
		plan: func(f audio.Format, opts TranscodeOptions) (string, int, int, error) {
			enc, err := aac.NewEncoder(f, &aac.EncoderOptions{Bitrate: opts.Bitrate})
			if err != nil {
				return "", 0, 0, err
			}
			// The encoder clamps to its legal range; this is the target it
			// uses (an ABR mean).
			return aac.EncoderVersion, 0, enc.Bitrate(), nil
		},
		encode: func(f audio.Format, opts TranscodeOptions) (codec.Encoder, error) {
			return aac.NewEncoder(f, &aac.EncoderOptions{Bitrate: opts.Bitrate})
		},
		forms: aacForms(),
		hls: &hlsOutput{
			codecs: codecsAACLC,
			delay:  aac.EncoderDelay,
			encode: func(f audio.Format, opts TranscodeOptions, _ int64) (codec.Encoder, error) {
				return aac.NewEncoder(f, &aac.EncoderOptions{Bitrate: opts.Bitrate})
			},
		},
	},
	{
		// he-aac encodes HE-AAC (v2 with HEAACv2) and keeps an HE-AAC
		// source's identity through remux (PlanRemux redirects format=aac
		// here for an HE-AAC track). It claims no extension and is reached by
		// name; it writes .m4a.
		name:        "he-aac",
		knobs:       KnobBitrate | KnobHEAACv2,
		exts:        []string{},
		writeExt:    "m4a",
		lossy:       true,
		headerBytes: 700,
		codecID:     codec.HEAAC,
		adjust: func(spec *dsp.ChainSpec, src audio.Format, opts TranscodeOptions) {
			// One AU is 2048 output samples. The encoder takes three dual-rate
			// pairs, so a rateless request resamples onto them rather than be
			// refused; an explicit rate is validated at plan time.
			spec.FrameSize = aac.HEFrameSize
			spec.BitDepth = 0
			spec.Float = true
			if opts.Rate == 0 {
				if snapped := aac.HESnapRate(src.Rate); snapped != src.Rate {
					spec.Rate = snapped
				}
			}
			foldWideToStereo(spec, src)
		},
		plan: func(f audio.Format, opts TranscodeOptions) (string, int, int, error) {
			bitrate, err := aac.HEPlanBitrate(f, heEncOpts(opts))
			if err != nil {
				return "", 0, 0, err
			}
			if opts.HEAACv2 {
				return aac.HEV2EncoderVersion, 0, bitrate, nil
			}
			return aac.HEEncoderVersion, 0, bitrate, nil
		},
		encode: func(f audio.Format, opts TranscodeOptions) (codec.Encoder, error) {
			return aac.NewHEEncoder(f, heEncOpts(opts))
		},
		forms: aacForms(),
		hls: &hlsOutput{
			codecs: codecsHEv1,
			codecsFor: func(opts TranscodeOptions) string {
				if opts.HEAACv2 {
					return codecsHEv2
				}
				return codecsHEv1
			},
			delay: aac.HEEncoderDelay,
			encode: func(f audio.Format, opts TranscodeOptions, _ int64) (codec.Encoder, error) {
				return aac.NewHEEncoder(f, heEncOpts(opts))
			},
		},
	},
	{
		name: "alac",
		// Reached by name only (aac claims m4a); it writes .m4a.
		exts:     []string{},
		writeExt: "m4a",
		// headerBytes stays 0: a size estimate needs a fixed bytesPerFrame.
		codecID: codec.ALAC,
		adjust: func(spec *dsp.ChainSpec, src audio.Format, opts TranscodeOptions) {
			spec.FrameSize = alac.FrameSize
			if opts.BitDepth != 0 {
				return // explicit depth; plan validates it against ALAC's set
			}
			// ALAC holds integer PCM at 16/20/24/32 bits. A float source
			// quantizes to 24 (the whole float32 mantissa); an integer source
			// snaps up to the nearest ALAC depth, losslessly widened.
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
		encode: func(f audio.Format, _ TranscodeOptions) (codec.Encoder, error) {
			return alac.NewEncoder(f, nil)
		},
		// ALAC rides only in MP4: the default (fragmented) and both box shapes.
		forms: []form{mp4Form(""), mp4Form(ContainerProgressive), mp4Form(ContainerFragmented)},
		hls: &hlsOutput{
			codecs: "alac",
			encode: func(f audio.Format, _ TranscodeOptions, _ int64) (codec.Encoder, error) {
				return alac.NewEncoder(f, nil)
			},
		},
	},
	{
		name:  "wavpack",
		knobs: KnobWavPackLevel,
		exts:  []string{"wv"},
		// headerBytes stays 0: a size estimate needs a fixed bytesPerFrame.
		codecID: codec.WavPack,
		adjust: func(spec *dsp.ChainSpec, src audio.Format, opts TranscodeOptions) {
			spec.FrameSize = wavpack.BlockSamples
			if opts.BitDepth != 0 {
				// Explicit depth; plan validates it against WavPack's set.
				return
			}
			// WavPack holds integer PCM at 8/16/24/32 bits. A float source
			// quantizes to 24; an integer source snaps up, losslessly widened.
			if src.Type == audio.Float {
				spec.BitDepth = 24
			} else if d := wavpackSnapDepth(src.BitDepth); d != src.BitDepth {
				spec.BitDepth = d
			}
			// No fold to stereo: a lossless output never drops channels.
		},
		plan: func(f audio.Format, opts TranscodeOptions) (string, int, int, error) {
			level, err := wavpackLevel(opts)
			if err != nil {
				return "", 0, 0, err
			}
			if _, err := wavpack.NewEncoder(f, &wavpack.EncoderOptions{Level: level}); err != nil {
				return "", 0, 0, err
			}
			return wavpack.EncoderVersion(level), 0, 0, nil
		},
		encode: func(f audio.Format, opts TranscodeOptions) (codec.Encoder, error) {
			level, err := wavpackLevel(opts)
			if err != nil {
				return nil, err
			}
			return wavpack.NewEncoder(f, &wavpack.EncoderOptions{Level: level})
		},
		// One form: the mka muxer does not write A_WAVPACK4. The muxer writes
		// the APEv2 tag block after the audio.
		forms: []form{{
			mediaType: "audio/x-wavpack", version: wv.MuxerVersion, live: true, embedsTags: true, tagsFit: wv.TagsFit,
			trackFits: wv.TrackFits,
			mux: func(_ container.Track, _ TranscodeOptions, x runConfig, _ codec.Encoder, dst io.Writer) (container.Muxer, error) {
				return wv.NewMuxer(dst, &wv.MuxerOptions{Tags: x.tags}), nil
			},
		}},
		// hls stays nil: WavPack has no CMAF/HLS segmented form.
	},
	{
		name:  "ape",
		knobs: KnobAPELevel,
		exts:  []string{"ape"},
		// headerBytes stays 0: a size estimate needs a fixed bytesPerFrame.
		codecID: codec.APE,
		adjust: func(spec *dsp.ChainSpec, src audio.Format, opts TranscodeOptions) {
			spec.FrameSize = ape.BlocksPerFrame
			if opts.BitDepth != 0 {
				return // explicit depth; plan validates it against APE's set
			}
			// Monkey's Audio holds integer PCM at 8, 16 and 24 bits. A float
			// source quantizes to 24; a narrower integer source snaps up,
			// losslessly widened. A wider one is left for the encoder to
			// refuse: a lossless output never drops bits unasked.
			if src.Type == audio.Float {
				spec.BitDepth = 24
			} else if d := apeSnapDepth(src.BitDepth); d != src.BitDepth {
				spec.BitDepth = d
			}
			// No fold to stereo: a lossless output never drops channels, so
			// the encoder refuses more than two.
		},
		plan: func(f audio.Format, opts TranscodeOptions) (string, int, int, error) {
			level, err := apeLevel(opts)
			if err != nil {
				return "", 0, 0, err
			}
			if _, err := ape.NewEncoder(f, &ape.EncoderOptions{Level: level}); err != nil {
				return "", 0, 0, err
			}
			return ape.EncoderVersion(level), 0, 0, nil
		},
		encode: func(f audio.Format, opts TranscodeOptions) (codec.Encoder, error) {
			level, err := apeLevel(opts)
			if err != nil {
				return nil, err
			}
			return ape.NewEncoder(f, &ape.EncoderOptions{Level: level})
		},
		// Not live: a .ape opens with its seek table and totals, known only
		// after the audio. The muxer writes the APEv2 tag block after it.
		forms: []form{{
			mediaType: "audio/x-ape", version: apen.MuxerVersion, embedsTags: true, tagsFit: apen.TagsFit,
			trackFits: apen.TrackFits,
			mux: func(_ container.Track, _ TranscodeOptions, x runConfig, _ codec.Encoder, dst io.Writer) (container.Muxer, error) {
				return apen.NewMuxer(dst, &apen.MuxerOptions{Tags: x.tags}), nil
			},
		}},
		// hls stays nil: it has no CMAF/HLS segmented form.
	},
}

// logImplicitDownmix warns that the chain folded channels the caller never
// asked to lose, at Warn so the default level shows it. The progressive and
// segmented runs call it; the plan cannot, since its core is cached and would
// warn once per shape rather than once per run.
func (e *Engine) logImplicitDownmix(opts TranscodeOptions, src, out audio.Format) {
	if opts.Channels == 0 && out.Channels < src.Channels {
		e.log.Warn("downmixed to fit the output format",
			"format", opts.Format, "source", src.Channels, "out", out.Channels)
	}
}

// logTimelineDownmix is the same warning for a timeline that folded its
// members before the seam (ConcatOptions.Channels), where the run's chain
// sees nothing left to fold. With no width requested, it fires exactly when
// a member is wider than the envelope. It runs per encode, never in
// Engine.TimelineChannels (ADR-0010).
func (e *Engine) logTimelineDownmix(opts TranscodeOptions, med format.Media, in audio.Format) {
	if opts.Channels != 0 {
		return
	}
	c, ok := med.(format.Composite)
	if !ok {
		return
	}
	widest := in.Channels
	for _, t := range c.Members() {
		widest = max(widest, t.Fmt.Channels)
	}
	if widest > in.Channels {
		e.log.Warn("downmixed to fit the output format",
			"format", opts.Format, "source", widest, "out", in.Channels)
	}
}

// foldWideToStereo is the lossy stereo rows' channel policy (opus, mp3, aac,
// he-aac): a source wider than stereo folds to stereo when the caller asked
// for no width, since a lossy delivery already discards audio. Lossless rows
// never fold; a width they cannot hold is refused. An explicit channel count
// is never overridden, and one the encoder cannot hold is refused. The fold
// is dsp/mix's BS.775 matrix with the chain's limiter behind it, and
// logImplicitDownmix announces it.
func foldWideToStereo(spec *dsp.ChainSpec, src audio.Format) {
	if spec.Channels == 0 && src.Channels > 2 {
		spec.Channels = 2
	}
}

// aacForms are the AAC rows' forms: fragmented MP4 by default (gapless
// edit-list signaling), "adts" the raw stream (no gapless signaling), the
// two MP4 box shapes by name, and Matroska, not WebM.
func aacForms() []form {
	return []form{
		mp4Form(""),
		{
			name: "adts", mediaType: "audio/aac", ext: "aac", version: adts.MuxerVersion, live: true, tagsFit: noTags,
			trackFits: adts.TrackFits,
			mux: func(_ container.Track, _ TranscodeOptions, _ runConfig, _ codec.Encoder, dst io.Writer) (container.Muxer, error) {
				return adts.NewMuxer(dst), nil
			},
		},
		mp4Form(ContainerProgressive),
		mp4Form(ContainerFragmented),
		mkaForm(),
	}
}

// mp4Form is one MP4 box shape: CMAF for the default and "fragmented", the
// flat moov+mdat for "progressive", which back-patches and so is not live.
// The muxer embeds an ilst, the only way the fragmented form gets tags.
func mp4Form(name string) form {
	progressive := name == ContainerProgressive
	fits := mp4.TrackFits
	if progressive {
		fits = mp4.ProgressiveTrackFits
	}
	return form{
		name: name, mediaType: mp4MediaType, version: mp4.MuxerVersion, live: !progressive, embedsTags: true,
		tagsFit: mp4.TagsFit, trackFits: fits, trimByLength: !progressive,
		mux: func(_ container.Track, _ TranscodeOptions, x runConfig, _ codec.Encoder, dst io.Writer) (container.Muxer, error) {
			if progressive {
				return mp4.NewProgressiveMuxer(dst, mp4MuxerOptions(x)), nil
			}
			return mp4.NewMuxer(dst, mp4MuxerOptions(x)), nil
		},
	}
}

// oggForm is Ogg-Opus and Ogg-Vorbis, each row's default form, whose comment
// header tagsFit answers for.
func oggForm(tagsFit func([]container.Tag) error) form {
	return form{
		mediaType: "audio/ogg", version: ogg.MuxerVersion, live: true, tagsFit: tagsFit, trackFits: ogg.TrackFits,
		mux: func(_ container.Track, _ TranscodeOptions, x runConfig, _ codec.Encoder, dst io.Writer) (container.Muxer, error) {
			return ogg.NewMuxer(dst, &ogg.MuxerOptions{Tags: x.tags}), nil
		},
	}
}

// mkaForm is Matroska, which every codec the mka muxer carries takes.
func mkaForm() form { return matroskaForm("mka", "audio/x-matroska") }

// webmForm is WebM, whose audio subset is Opus and Vorbis.
func webmForm() form { return matroskaForm("webm", "audio/webm") }

// matroskaForm is one DocType of the mka muxer, webm for the "webm" form. It
// defers tags to the post-pass: the muxer accepts Tags but does not write them.
func matroskaForm(name, mediaType string) form {
	webm := name == "webm"
	return form{
		name: name, mediaType: mediaType, ext: name, version: mka.MuxerVersion, live: true, tagsFit: mka.TagsFit,
		trackFits: func(t container.Track) error { return mka.TrackFits(t, webm) },
		mux: func(_ container.Track, _ TranscodeOptions, x runConfig, _ codec.Encoder, dst io.Writer) (container.Muxer, error) {
			return mka.NewMuxer(dst, &mka.MuxerOptions{WebM: webm, Tags: x.tags}), nil
		},
	}
}

// flacMD5 is a FLAC encoder's STREAMINFO signature, or nil without one: a
// remux keeps the source's own, which describes the packets it copies.
func flacMD5(enc codec.Encoder) func() [16]byte {
	if fe, ok := enc.(*flac.Encoder); ok {
		return fe.MD5
	}
	return nil
}

// ContainerProgressive and ContainerFragmented are the TranscodeOptions.
// Container overrides naming the two MP4 box shapes. Progressive is the flat
// moov+mdat form; it back-patches its header, so it needs a seekable
// destination and is not live. Fragmented is the CMAF form /stream and HLS
// deliver and the mp4 rows' default, named so a file output, which
// FileOutputContainer otherwise makes flat, can still ask for it.
const (
	ContainerProgressive = "progressive"
	ContainerFragmented  = "fragmented"
	// ContainerOgg is the flac row's Ogg wrapper override.
	ContainerOgg = "ogg"
)

// OutputEmbedsTags reports whether a tagging post-pass must skip the output of
// a (format, container) pair, since a second set written over its muxer's tags
// would conflict with them. The WavPack and Monkey's Audio muxers take no
// cover art, so the skip keeps it off them too.
//
// A pair no form takes answers for the row's default form; an unknown format
// answers false.
func OutputEmbedsTags(format, container string) bool {
	_, f, ok := formOrDefault(format, container)
	return ok && f.embedsTags
}

// TagsFit reports whether the muxer of the output format and container
// names would take tags as the run's tags: nil when it would, and the muxer's
// own refusal when its block cannot hold them. It asks the builder the muxer
// writes with, so the answer and the write cannot disagree. A muxer that
// writes no tags or sets no cap takes any set; so does a pair no row takes.
func TagsFit(format, container string, tags []container.Tag) error {
	_, f, ok := formOrDefault(format, container)
	if !ok || f.tagsFit == nil {
		return nil
	}
	return f.tagsFit(tags)
}

// noTags is the tag answer of a form whose muxer takes no tags: any set fits,
// since the muxer never sees it.
func noTags([]container.Tag) error { return nil }

// mp4MediaType is what an mp4-family output's plan reports. The file-output
// rule keys on it rather than on a format name, so a new mp4-family row is
// covered without the rule knowing it exists.
const mp4MediaType = "audio/mp4"

// FileOutputContainer resolves the container a file output is written with:
// the caller's explicit choice, or ContainerProgressive when the plan is an
// mp4-family output and the caller named none. The flat form is the only one
// that carries a QuickTime chapter track or that a tag rewriter can edit,
// and a file can take its back-patch. Every file writer (CLI and jobs) calls
// it; pass the plan from the caller's own options and re-plan when the
// answer changes.
func FileOutputContainer(requested string, plan *TranscodePlan) string {
	if requested == "" && plan != nil && plan.MediaType == mp4MediaType {
		return ContainerProgressive
	}
	return requested
}

// isMatroska reports whether a Container override selects the MKA muxer.
func isMatroska(name string) bool { return name == "mka" || name == "webm" }

// StatesTrimsPerPacket reports whether a container states its gapless trims
// per packet (container.Packet.Padding) rather than once in a header or a
// trailer. Only Matroska does (DiscardPadding). A packet copy carries a trim
// that is not at the end only into such a container, and only in one can a
// walk find a trim the headers did not state.
func StatesTrimsPerPacket(containerName string) bool { return isMatroska(containerName) }

// mkaPCMConfig is the PCM wire configuration for a track carried in Matroska:
// like riff.DefaultConfig, but signed rather than unsigned for 8-bit, since
// Matroska PCM is signed (A_PCM/INT/LIT) with no unsigned form. Float stays
// 32-bit IEEE (A_PCM/FLOAT/IEEE). This is what the mka muxer's CodecID and
// BitDepth declare, so the demuxer reads the bytes back unchanged.
func mkaPCMConfig(f audio.Format) pcm.Config {
	if f.Type == audio.Float {
		return pcm.Config{Encoding: pcm.Float, Bits: 32}
	}
	bits := pcm.ContainerBits(f.BitDepth)
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: bits}
	if f.BitDepth != bits {
		cfg.ValidBits = f.BitDepth
	}
	return cfg
}

// mp4MuxerOptions carries the per-call metadata payloads into the MP4
// muxer; nil when there are none, so the default construction stays the
// zero options.
func mp4MuxerOptions(x runConfig) *mp4.MuxerOptions {
	if len(x.tags) == 0 && len(x.chapters) == 0 && x.art == nil {
		return nil
	}
	return &mp4.MuxerOptions{Tags: x.tags, Chapters: x.chapters, Art: x.art}
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
		VBR:        opts.VBR,
		Signal:     sig,
	}, nil
}

// vorbisEncoderOptions builds the codec-level Vorbis options from a transcode
// request. A zero VorbisQuality keeps the encoder default; a Bitrate never
// arrives, since Check refuses one on vorbis.
func vorbisEncoderOptions(opts TranscodeOptions) (*vorbis.EncoderOptions, error) {
	return &vorbis.EncoderOptions{Quality: opts.VorbisQuality}, nil
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
	return opus.SignalAuto, rangeError(&RangeError{Knob: KnobOpusSignal, Value: opts.OpusSignal,
		Values: []string{"auto", "voice", "music"}})
}

// rangeError is Check's refusal of a knob out of range.
func rangeError(e *RangeError) error {
	return waxerr.Wrap(waxerr.CodeInvalidRequest, "", e)
}

// checkOpusComplexity refuses an OpusComplexity outside OpusComplexityLowest
// and 0 through 10.
func checkOpusComplexity(opts TranscodeOptions) error {
	if c := opts.OpusComplexity; c < OpusComplexityLowest || c > 10 {
		return rangeError(&RangeError{Knob: KnobOpusComplexity, Value: c, Min: OpusComplexityLowest, Max: 10})
	}
	return nil
}

// checkVorbisQuality refuses a VorbisQuality off libvorbis's -q scale. Zero is
// the default and in range.
func checkVorbisQuality(opts TranscodeOptions) error {
	if q := opts.VorbisQuality; q < -1 || q > 10 {
		return rangeError(&RangeError{Knob: KnobVorbisQuality, Value: q, Min: -1, Max: 10})
	}
	return nil
}

// opusBitrate resolves TranscodeOptions.Bitrate: the zero value keeps the
// encoder default (96 kbit/s); any other value passes through to the encoder,
// which validates it.
func opusBitrate(opts TranscodeOptions) int {
	if opts.Bitrate == 0 {
		return opus.DefaultBitrate
	}
	return opts.Bitrate
}

// mp3EncoderOptions builds the codec-level MP3 options from a transcode
// request. The zero bit rate keeps the encoder default (128 kbit/s); any
// other value passes through to the encoder, which validates it against
// the layer's legal rates (a CBR rate, or the VBR quality anchor).
func mp3EncoderOptions(opts TranscodeOptions) *mp3.EncoderOptions {
	bitrate := opts.Bitrate
	if bitrate == 0 {
		bitrate = mp3.DefaultBitrate
	}
	return &mp3.EncoderOptions{Bitrate: bitrate, VBR: opts.VBR}
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
	return 0, rangeError(&RangeError{Knob: KnobFLACLevel, Value: opts.FLACLevel, Min: FLACLevelFastest, Max: 8})
}

// wavpackLevel resolves TranscodeOptions.WavPackLevel: the zero value keeps
// the encoder default (normal), and the four named levels pass through. Unlike
// the FLAC scale this needs no sentinel, since WavPack's levels are named
// modes numbered from one rather than a range starting at zero.
func wavpackLevel(opts TranscodeOptions) (int, error) {
	switch {
	case opts.WavPackLevel == WavPackLevelDefault:
		return wavpack.DefaultEncoderLevel, nil
	case opts.WavPackLevel >= WavPackLevelFast && opts.WavPackLevel <= WavPackLevelVeryHigh:
		return opts.WavPackLevel, nil
	}
	return 0, rangeError(&RangeError{Knob: KnobWavPackLevel, Value: opts.WavPackLevel,
		Min: WavPackLevelFast, Max: WavPackLevelVeryHigh})
}

// apeLevel resolves TranscodeOptions.APELevel: the zero value keeps the
// encoder default (normal), and the three written levels pass through. The
// format's own vocabulary counts in thousands, so zero is outside it and the
// default needs no sentinel. The two deeper levels decode but are not
// written, and ape.NewEncoder says so by name.
func apeLevel(opts TranscodeOptions) (int, error) {
	switch {
	case opts.APELevel == APELevelDefault:
		return ape.DefaultEncoderLevel, nil
	case opts.APELevel >= APELevelFast && opts.APELevel <= APELevelHigh && opts.APELevel%1000 == 0:
		return opts.APELevel, nil
	}
	return 0, rangeError(&RangeError{Knob: KnobAPELevel, Value: opts.APELevel,
		Values: []string{fmt.Sprint(APELevelFast), fmt.Sprint(APELevelNormal), fmt.Sprint(APELevelHigh)},
		// The likeliest wrong answer, since the format documents five levels
		// and this writes three. Say which half of that it is.
		Unwritten: opts.APELevel > APELevelHigh && opts.APELevel <= ape.LevelInsane && opts.APELevel%1000 == 0})
}

// apeSnapDepth rounds an integer bit depth up to the nearest Monkey's Audio
// width. It is the identity on those widths, so a source already at one needs
// no override, and it is the identity above them too: nothing here narrows a
// source, so a 32-bit one reaches the encoder and is refused by name.
func apeSnapDepth(d int) int {
	switch {
	case d <= 8:
		return 8
	case d <= 16:
		return 16
	case d <= 24:
		return 24
	}
	return d
}

// wavpackSnapDepth rounds an integer bit depth up to the nearest WavPack
// container width. It is the identity on those widths, so a source already at
// one needs no override.
func wavpackSnapDepth(d int) int {
	switch {
	case d <= 8:
		return 8
	case d <= 16:
		return 16
	case d <= 24:
		return 24
	}
	return 32
}

// DefaultLiveFormat returns the output format that format=auto resolves
// to when a transcode is required: the first registered output with a
// streaming form.
func DefaultLiveFormat() string {
	for _, o := range outputs {
		if o.forms[0].live && !o.remuxOnly {
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
	// Lossy reports the encoder discards audio.
	Lossy bool
	// Knobs are the encoder knobs the format takes; Check refuses any other.
	Knobs Knob
}

// Outputs lists the registered output formats, in table order. Remux-only
// rows are omitted: they advertise nothing the engine can encode.
func Outputs() []OutputInfo {
	infos := make([]OutputInfo, 0, len(outputs))
	for _, o := range outputs {
		if o.remuxOnly {
			continue
		}
		// Non-nil, so a format with no extensions marshals as [], not null.
		infos = append(infos, OutputInfo{
			Name: o.name, Exts: append([]string{}, o.exts...), Live: o.forms[0].live,
			Lossy: o.lossy, Knobs: o.knobs,
		})
	}
	return infos
}

// OutputFormats lists the registered output format names, in table order,
// omitting remux-only rows like Outputs does.
func OutputFormats() []string {
	names := make([]string, 0, len(outputs))
	for _, o := range outputs {
		if o.remuxOnly {
			continue
		}
		names = append(names, o.name)
	}
	return names
}

// OutputFormatForExt maps a file extension (with or without the leading
// dot, any case) to the output format name that writes it, or "" when no
// registered output claims the extension.
func OutputFormatForExt(ext string) string {
	ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	for _, o := range outputs {
		if o.remuxOnly {
			// A remux-only row resolves no extension.
			continue
		}
		if slices.Contains(o.exts, ext) {
			return o.name
		}
	}
	return ""
}

// OutputContainerForExt maps an extension that names a container form
// rather than an output row to the format and container override it implies,
// which OutputFormatForExt cannot: ".mka" and ".mkv" are FLAC in Matroska,
// ".webm" Opus in WebM. ok is false for any other extension.
func OutputContainerForExt(ext string) (format, container string, ok bool) {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "mka", "mkv":
		return "flac", "mka", true
	case "webm":
		return "opus", "webm", true
	}
	return "", "", false
}

// OutputExt is the file extension for an output written as format in
// container (empty for the default form), without the leading dot. It is
// not OutputFormatForExt inverted: alac and he-aac write .m4a, which resolves
// to aac. Only a different wrapper renames the file (adts, mka, webm, ogg);
// progressive and fragmented MP4 stay .m4a. An unregistered format, or a row
// with no extension, gets "bin". It does not validate the request: a pair
// no form takes answers for the row's default form.
func OutputExt(format, container string) string {
	row, f, ok := formOrDefault(format, container)
	if !ok {
		return outputExtFallback
	}
	if f.ext != "" {
		return f.ext
	}
	if row.writeExt != "" {
		return row.writeExt
	}
	if len(row.exts) > 0 {
		return row.exts[0]
	}
	return outputExtFallback
}

// outputExtFallback is the extension for an output with no file form.
const outputExtFallback = "bin"

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
