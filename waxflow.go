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
			// A complete index is a measurement already taken, and walking
			// it costs nothing, so the media reports the length at once; with
			// nothing left to stop, the context is the background one.
			if err := format.WalkMedia(context.Background(), med); err != nil {
				e.log.Debug("settling a restored index failed", "hint", hint, "err", err)
			}
		}
	}
	return &indexSavingMedia{Forwarder: format.Forwarder{Media: med}, ix: ix, cache: e.idx, src: src}, nil
}

// The assertion keeps a method added to container.Indexer from silently
// dropping the capability, and with it every sidecar, here.
var _ container.Indexer = (*indexSavingMedia)(nil)

// indexSavingMedia saves a grown source index when the media closes.
// Close is idempotent like the media it wraps (ix nils after the first
// call, so a double Close cannot double-save), and the wrapper stays
// transparent to container.Indexer asserts: format.Forwarder does not
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
	// ClippedSamples counts samples that reached the quantizer beyond full
	// scale: channel samples, not frames, against Samples*Format.Channels.
	// Gain and resampler overshoot land here too (see dsp.Chain.Clipped).
	// Zero for float outputs and copy rungs; the concat timeline's own
	// clamps sit upstream and go uncounted.
	ClippedSamples int64
	// TruePeak is the output's true-peak level, linear, 1.0 = full scale;
	// AnalyzeResult.TruePeakDB is 20*log10 of it. Lossy outputs measure the
	// encoder's input. Zero for copy rungs, pure integer passes, silence.
	TruePeak float64
	// Quantized reports whether a quantizer ran (float cut to integer).
	// It gates the true-peak note; see LevelNote.
	Quantized bool
	// InputWarnings is the input damage the read worked around, as the
	// source's Info reports it once the whole stream has been read: a
	// frame-walked payload finds its damage where the read reaches it, so
	// the list is complete only now. Nil for a clean source. A concatenated
	// timeline has no damage of its own and reports its members' under
	// their index ("member 1: ..."), as its Info does.
	InputWarnings []string
	// LostSamples counts the output's samples that are silence standing in
	// for audio damage took from the source (format.Info.Lost). A hole keeps
	// the stream's length, so Samples cannot say how much of the output is
	// the file's own. It is this run's own read, on the output's timeline as
	// Samples is: a run that resamples scales the source's count to
	// Format.Rate. A copy rung reports the silent frames it wrote, and a copy
	// of a codec whose packets state their own positions (WavPack) the gap its
	// packets carry, writing no silence. Zero for a source with no hole, and
	// for one whose holes shorten the read instead, which is every format but
	// FLAC (native or in Ogg) and WavPack.
	LostSamples int64
	// TruncatedSamples counts the samples the source's header declares past
	// the end of what the file holds (format.Info.Truncated), as this run's
	// read left it, scaled to Format.Rate as LostSamples is. None of them is
	// in the output, which ends where the source's audio does: Samples is
	// what was there and this is how much more the file claimed. Zero for a
	// whole source, for a header that states no length to fall short of,
	// and for a window that stops short of the source's end (Slice).
	TruncatedSamples int64
}

// LossNote returns the one warning line LostSamples and TruncatedSamples
// warrant, or "" for a source that lost nothing: how much of the output is
// silence standing in for a hole, and how far it ends short of the length its
// source declared. Both are counted in this result's own samples, at
// Format.Rate, so the line speaks of the audio and never of a number of the
// source's samples, which a run that resamples does not report. Every
// surface that reports a result words it through here.
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

// Transcode decodes src and writes it to dst in the requested output
// format: decode -> DSP -> encode -> mux, checking ctx between chunks.
// The DSP chain (convert, resample, mix, gain, dither, in that fixed
// order) is assembled only from the options that differ from the source, so
// zero options add no stage at all and the decoder's samples reach the
// encoder unaltered. Against a lossless source and a lossless output that
// makes the transcode a bit-exact container rewrite. A lossy source is still
// decoded and re-encoded, which is a new generation rather than a rewrite of
// its packets; moving packets through untouched is a separate rung this
// engine does not yet have. A positive FromSample seeks sample-exact before
// the first chunk (the HTTP t= parameter, converted at the boundary).
// Output formats whose muxer needs to back-patch headers (AIFF, exact
// WAV sizes) want a dst that can really seek, which is probed rather than
// read off the method set: an *os.File on a pipe carries io.WriteSeeker and
// cannot seek. WAV falls back to a compliant streaming form on one that
// cannot; AIFF refuses. A dst that cannot be patched also makes the output's
// declared length a commitment, so a source length a header declared and
// nothing checked is confirmed by walking the source before it is projected.
func (e *Engine) Transcode(ctx context.Context, src container.Source, hint string, dst io.Writer, opts TranscodeOptions, run ...RunOption) (*TranscodeResult, error) {
	med, err := e.OpenStream(src, hint)
	if err != nil {
		return nil, err
	}
	defer med.Close()
	return e.TranscodeMedia(ctx, med, dst, opts, run...)
}

// TranscodeMedia transcodes an already-opened Media to dst, the same
// decode -> DSP -> encode -> mux pipeline as Transcode without the source-open
// step. It is the entry point for inputs that are not a single sniffable Source:
// a caller that assembles a presentation from many fetched resources, as the
// tests' HLS client does, hands it over as a format.Media, which flows through
// here exactly like a local file. The caller owns med and closes it.
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
	// The run muxes through the form resolved here, first, as the plan does, so
	// a pair no form takes gets the same refusal from both. Both checks come
	// before the seek and the confirm below, so nothing reads the source first.
	fm, err := row.form(opts.Container)
	if err != nil {
		return nil, err
	}
	containerName := row.containerName(fm)
	// The chain spec is built here rather than below the seek so the width
	// guard can read the count the row actually delivers and still refuse
	// before anything reads the source, which is the rule the two checks above
	// keep. A confirm walk moves only the length fields (format.media.Walk), so
	// the spec this produces is the spec the refreshed track would.
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
	// A header a muxer cannot go back and fix is a commitment, so the length it
	// commits to is confirmed first. Walk leaves the packet position, so this
	// sits after the seek and changes nothing about what is read out. A
	// declared length that is a bound the decode is not yet held to is
	// confirmed for any destination (unconfirmedBound).
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
	// Where the run's own read begins: a caller's Media may have been read
	// already, and neither the seek nor the walk above delivered anything.
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

	// What the output's headers are allowed to promise. On a destination the
	// muxer can patch, the projection is provisional: a miss is corrected at
	// End and the bytes are the same either way. On one it cannot, the headers
	// are a commitment, so only a confirmed length is projected.
	//
	// An advisory total projects nothing. It is a number the decode is not
	// expected to match (ASF's ticks, a Matroska Info Duration, a WAV fact
	// chunk over MP3 frames), so a muxer that sizes its headers from it writes
	// a byte count the encoder then misses: the WAV muxer catches that at End
	// on a writer it cannot seek and fails a whole transcode. Walking would not
	// help either, since the estimate is not a claim about content. Unknown is
	// the honest projection, and every muxer already handles it, since an
	// untagged MP3 opens with -1 today.
	//
	// A declared count whose source could confirm it has been walked above; one
	// whose walk did not finish stays unprojected, since the source itself says
	// the payload is unaccounted for. Every other count projects as it always
	// did, so a streamed WAV of an mp4 or a FLAC keeps its exact sizes.
	//
	// Which destinations those are is worth being exact about, because the
	// walk is not free. A job's output file is seekable and pays nothing here.
	// A live pipeline's cache entry is not: it is an append-only ring with no
	// Seek, so every cold /stream and /transcode of a source with a declared
	// count and a deferred walk (a Xing-tagged MP3, an ADTS stream, a
	// fragmented MP4 whose segment index states a length) builds the
	// frame index before the first encoded byte. That is a full pass of header
	// hops, and the index sidecar makes repeats of the same source free.
	//
	// For the destinations that are only ever hints, the cost is a hint lost
	// rather than a size: Matroska reserves a Duration slot only for a
	// projected length or a writer it can seek, so a live mka of an advisory
	// source carries no Duration where it used to carry a rounded one.
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
	// Progress keeps the estimate the headers gave, because a progress total
	// is the one place a rounded number is fit for purpose: it is a
	// denominator, not a promise, and -1 there would leave every advisory
	// source with no progress bar at all.
	progressTotal := chain.OutputSamples(srcSamples)
	// Encoders with priming declare it; muxers that can signal it (the
	// fMP4 edit list) read it from the track, and the trailer restates
	// it exactly at End.
	if d, ok := enc.(interface{ Delay() int }); ok {
		track.Delay = int64(d.Delay())
	}
	// The muxer is built from the finished track, which is why this follows
	// the encoder rather than arriving beside it: a muxer reads the stream's
	// shape off the track, and the remux rung has a track with no encoder
	// behind it. See form.mux.
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
// methods: format.Walker on a Media, container.Walker on a demuxer. The rungs
// below ask the same question of each, so they ask it once.
type lengthWalker interface {
	Walk(ctx context.Context) error
	Walked() bool
}

// confirmableLength returns the work that would confirm t's length when it is
// a count nothing has checked yet, and nil otherwise: declared by a header, not
// an estimate, on a source that has not yet checked it against the payload.
//
// It answers both halves at once (is this the shape, and what confirms it) so
// no caller asserts the confirmation separately from the test that decided
// there is one. A nil answer therefore means two things a caller treats alike:
// the length needs no confirming, or nothing here can confirm it.
//
// It is the one shape a header on an unpatchable destination must not commit
// to. A Xing frame count is a claim the frames can contradict, and a run that
// projects it writes a byte count the encoder then misses, failing at End with
// the output's headers already on the wire. Confirming first is the remedy,
// which is why the work is part of the shape rather than a separate test: a
// source with none has nothing better to offer than the number it declared, and
// dropping that would cost every streamed WAV of an mp4 its exact sizes for a
// mismatch nothing could have caught anyway.
//
// Two things can do the work, and they are one answer here because the caller
// does one thing with either. A file answers with its own deferred walk
// (lengthWalker, the two-method shape both domains expose). A Media that is not
// a file answers with format.LengthConfirmer, which runs whatever is underneath
// it and re-derives its own length from the result: an open-ended span of a
// Xing-tagged MP3 is the shape that reaches it, since a bounded span's length
// is its own arithmetic and is already exact. A concatenated timeline answers
// neither, by the contract in ConcatSource.Track.
//
// The other lengths are fine as they are: an exact one is authoritative, an
// advisory one projects nothing at all, and an absent one commits to nothing.
func confirmableLength(t container.Track, src any) func(context.Context) error {
	if t.Samples < 0 || t.SamplesExact || t.SamplesAdvisory {
		return nil
	}
	// A finished walk is still the work: until Walk runs, the measurement is
	// not on the track, and once the walk has finished Walk costs nothing.
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
// source track: the track's own count, or -1 for no claim at all.
//
// One function because the rule is one rule, and the two rungs that apply it
// would otherwise state it twice and drift. patchable is whether the muxer can
// go back and fix what it wrote; src is the source the length came from, asked
// again in case its own walk could still confirm it.
func projectedLength(t container.Track, src any, patchable bool) int64 {
	if t.SamplesAdvisory || (!patchable && confirmableLength(t, src) != nil) {
		return -1
	}
	return t.Samples
}

// missedProjection classifies a muxer's End failure when the output's length
// came up short of what its headers promised.
//
// With a confirmed count, the only non-bug cause left is damage the read
// tolerated: a page missing from an exact Ogg, a frame gap inside a verified
// FLAC. That is the input's fault and a caller can act on it, where the
// internal error the muxer raises says the library miscounted. On a patchable
// destination no projection was a promise, so a failure there is the muxer's
// own and passes through unchanged.
//
// It asks what the error was, not only what the counts were. A muxer's End
// fails for reasons that have nothing to do with the length (a write that did
// not land, a trim the container cannot signal), and those carry their own
// codes; relabelling an unwritable output as malformed input would point an
// operator at the file when the disk is what went wrong. Only the internal
// error the muxer raises for a projection it could not keep is reclassified.
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

// checkSeekable enforces the one capability bit Muxer exposes: a
// back-patching muxer needs a seekable destination (a file). It is checked
// before any work starts on a doomed output, in one place so no muxer, and
// no rung of the ladder, re-invents the guard.
//
// It probes rather than asserting the type, for the reason the muxers do:
// *os.File carries io.WriteSeeker for a pipe too, and a guard that took the
// method set for the capability would pass the doomed output straight through
// to the muxer it exists to spare.
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
	// Versions are the version constants of every node whose revision can
	// change the output bytes, in pipeline order: source decoder, DSP
	// chain, encoder, muxer. A decoder revision must invalidate cached
	// transcodes of that codec's sources just as an encoder revision
	// invalidates its outputs, and the muxer term covers the one change the
	// others cannot see: the same encoded packets framed differently.
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

// decodeVersion is the read-side member of the plan's version tuple: the
// source codec's decoder revision (each codec package's Version constant).
// It rides in TranscodePlan.Versions so a decoder change invalidates
// cached transcodes OF that codec's sources, closing the loop ADR-0004
// requires of every sample-affecting node; the encoder version alone only
// covers outputs. The plan core stays codec-independent (it is keyed and
// cached on the decoded PCM format), so this composes per call.
//
// The mapping itself lives in format's decoder registry, beside the
// constructor for each codec, rather than in a switch here: a second table
// naming the same codecs is a table that can be short one.
func decodeVersion(id codec.ID) string {
	if v := format.DecoderVersion(id); v != "" {
		return v
	}
	// Unregistered codecs cannot decode, so no cached bytes exist to go
	// stale; keep their plans keyed distinctly all the same.
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
		Versions:       withDemuxer(track, append([]string{decodeVersion(track.Codec)}, core.versions...)...),
		Samples:        samples,
		BytesPerFrame:  core.bytesPerFrame,
		FrameSize:      core.frameSize,
		BitRate:        bitRate,
		EstimatedBytes: estimated,
	}, nil
}

// PlanTranscodeTimeline plans a transcode of a concatenated timeline from its
// members' tracks alone (no decode, no open), the progressive sibling of
// PlanSegmentsTimeline: PlanTranscode over the synthetic track ConcatTrack
// computes, with the versions that track cannot name prepended and a channel
// conversion across a mixed-width seam refused.
//
// copts must be the options the Concat this plan describes is built with (see
// ConcatOptions), and opts.FromSample seeks on the timeline's own clock.
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
		// Encoder channel refusals surface here in the encoder's own words,
		// which name no remedy to a caller who never asked for a channel
		// count. Phrased in option vocabulary, not any boundary's spelling:
		// this is the public engine. %w keeps the encoder's code.
		//
		// planAcceptsStereo is load-bearing, not belt and braces: rows that
		// hold multichannel natively validate their own options first, so a
		// bad --flac-level on a 5.1 source lands here with nothing to do
		// with channels.
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

// output is one row of the writer-side capability table, the analog of
// format's read-side driver table: the single source of truth for what
// the engine can produce. Rows appear here as encoders and muxers land;
// the CLI's extension inference and the /caps endpoint both read this
// table instead of maintaining their own lists.
type output struct {
	name string
	// exts are the extensions that resolve TO this format (the read
	// direction, OutputFormatForExt). A row claims one only where it is what
	// that extension should mean, so at most one row claims any extension.
	exts []string
	// writeExt is the extension a file of this format is written with (the
	// write direction, OutputExt) when it is not the first claimed one. The
	// two directions are not inverses, and alac is why: it writes an entirely
	// ordinary .m4a, it just is not what .m4a resolves back to, since aac
	// claimed that. Adding m4a to its exts to say so would make the read
	// direction answer two formats for one extension, so the row carries the
	// name it writes separately from the names it answers to.
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
	// encode constructs the row's encoder for one transcode.
	encode func(f audio.Format, opts TranscodeOptions) (codec.Encoder, error)
	// hls describes the format's segmented CMAF form; nil means the format
	// has none and cannot serve HLS.
	hls *hlsOutput
	// remuxOnly marks a row that exists so remux can carry its codec while
	// no encoder produces it: outputRow still resolves it by name, but the
	// Outputs/OutputFormats enumerations (and so /caps) omit it, keeping
	// them honest about what the engine can encode. The flag drops when
	// the encoder lands.
	remuxOnly bool
}

// form is one wrapper a row can be written in.
type form struct {
	name      string // the Container value that selects it; "" is the row's own
	mediaType string // what an HTTP response carries
	ext       string // "" keeps the row's extension
	version   string // ADR-0004's muxer term
	live      bool   // writes to a plain io.Writer (NeedsSeek false), so /stream can serve it
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
	// mux constructs the writer. It is apart from the row's encode because
	// remux has no encoder and must write through the same muxer. t is the
	// output track, where a muxer reads what it needs (the mpa Xing delay),
	// and x the run's payloads (tags, chapters, art).
	// enc is nil on a remux: FLAC's MD5 over the unencoded audio is knowable
	// only by having encoded it, and a remux keeps the source's own signature.
	mux func(t container.Track, opts TranscodeOptions, x runConfig, enc codec.Encoder, dst io.Writer) (container.Muxer, error)
}

// hlsOutput is the writer-side table's segmented-delivery column: what an
// output format needs beyond its progressive muxer to become numbered
// fMP4 segments.
type hlsOutput struct {
	// codecs is the RFC 6381 CODECS attribute value for master playlists.
	codecs string
	// codecsFor resolves an option-dependent CODECS value; nil rows use
	// the static field. The he-aac row's encode splits on HEAACv2
	// (mp4a.40.5 against mp4a.40.29); remuxed sources resolve from their
	// own ASC in remuxCodecs instead.
	codecsFor func(opts TranscodeOptions) string
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
		// headerBytes approximates the Ogg-Opus overhead the sample-based
		// estimate omits: the two header pages plus per-page framing. It is a
		// hint; the live stream is chunked with no Content-Length.
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
			// The encoder clamps to what a 20 ms frame can carry; report
			// the actual rate. Unconstrained VBR reports 0, leaving rate
			// and size hints honestly unknown (the PlanTranscode VBR
			// contract).
			return opus.EncoderVersion, 0, enc.Bitrate(), nil
		},
		encode: func(f audio.Format, opts TranscodeOptions) (codec.Encoder, error) {
			eopts, err := opusEncoderOptions(opts)
			if err != nil {
				return nil, err
			}
			return opus.NewEncoder(f, eopts)
		},
		// Opus rides in Matroska and WebM (its native container besides Ogg).
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
		// The opus row claims only "opus", and flac's Ogg form is a container
		// override (its extension stays "flac"), so "ogg"/"oga" are free for the
		// native Ogg-Vorbis output the extension conventionally implies.
		exts:  []string{"ogg", "oga"},
		lossy: true,
		// headerBytes approximates the Ogg-Vorbis overhead the sample estimate
		// omits: the identification/comment/setup header pages plus per-page
		// framing. The setup header is the bulk; it is a hint, and the live
		// stream is chunked with no Content-Length.
		headerBytes: 4096,
		codecID:     codec.Vorbis,
		adjust: func(spec *dsp.ChainSpec, _ audio.Format, _ TranscodeOptions) {
			// Vorbis encodes float at the source rate and supports multichannel
			// natively (up to eight channels), so unlike Opus it neither resamples
			// to a fixed rate nor downmixes: keep the source rate and channel
			// count. Block sizes vary per packet, so leave FrameSize 0 and let the
			// encoder do its own overlap buffering.
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
		// Vorbis rides in Matroska and WebM (it is, with Opus, one of webm's two
		// audio codecs) besides its native Ogg.
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
		// FLAC rides in Matroska (A_FLAC) and Ogg (the Xiph FLAC-in-Ogg
		// mapping), but not WebM (Opus/Vorbis only). Both FLAC muxers write a
		// STREAMINFO, so both take the encoder's MD5.
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
		// headerBytes approximates the non-audio overhead the sample-based
		// estimate omits: the leading Xing/Info frame plus the two flush
		// frames. The exact size is rate-dependent, so this is a hint (the
		// stream is chunked with no Content-Length anyway).
		headerBytes: 1024,
		lossy:       true,
		codecID:     codec.MP3,
		adjust: func(spec *dsp.ChainSpec, src audio.Format, _ TranscodeOptions) {
			// MP3 encodes float32 in native frames: two granules (MPEG-1) or
			// one (MPEG-2/2.5), so the frame follows the output rate. The
			// lossy path always runs in the float domain, so any integer
			// depth request is dropped.
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
			// The encoder clamps to a layer-legal rate; report the actual
			// one. A VBR encoder reports 0, leaving rate and size hints
			// honestly unknown (the PlanTranscode VBR contract).
			return mp3.EncoderVersion, 0, enc.Bitrate(), nil
		},
		encode: func(f audio.Format, opts TranscodeOptions) (codec.Encoder, error) {
			return mp3.NewEncoder(f, mp3EncoderOptions(opts))
		},
		// The Xing header's delay reads off the track rather than the encoder,
		// which for a transcode is the same number by construction (Transcode
		// stamps track.Delay from the encoder) and for a remux is the source's,
		// carried across. What differs between the two is which convention that
		// number is in, and nil enc is the discriminator, as it is for FLAC's
		// MD5: an encoder's delay is the LAME field's own encoder-side share,
		// while a demuxer's has the fixed 529-sample decoder latency already
		// added. See mpa.MuxerOptions.DecodedTrims.
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
		// m4a moved here from the alac row when the AAC encoder landed
		// (the anticipated disambiguation): the extension overwhelmingly
		// means AAC in the wild. The bare .aac extension implies the
		// ADTS container at the CLI boundary.
		//
		// m4b is the audiobook spelling of the same file and already reads
		// (format/registry.go); it answers here so `transcode in.flac
		// out.m4b` infers a format rather than demanding --format. It stays
		// after m4a, which is what an m4a-or-m4b output is written as
		// (OutputExt takes the first).
		exts:  []string{"m4a", "aac", "m4b"},
		lossy: true,
		// headerBytes approximates the fMP4 init header (ftyp+moov).
		headerBytes: 700,
		codecID:     codec.AACLC,
		adjust: func(spec *dsp.ChainSpec, src audio.Format, _ TranscodeOptions) {
			// AAC-LC encodes float32 in 1024-sample frames. The lossy path
			// always runs in the float domain, so any integer depth request
			// is dropped. The rate is not snapped: the encoder accepts the
			// thirteen AAC rates and rejects anything else at plan time,
			// like the mp3 row (an explicit rate= converts first).
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
			// The encoder clamps to its legal range; report the actual
			// target (an ABR mean, held by the bit reservoir).
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
		// he-aac encodes HE-AAC v1 (SBR over a half-rate AAC-LC core) and
		// keeps copyable HE-AAC sources their identity through remux
		// (PlanRemux redirects a format=aac request here when the track is
		// HE-AAC). exts stays empty (.m4a and format=auto resolve to the aac
		// row; this format is reached by name) and the file it writes is an
		// ordinary .m4a, the alac precedent.
		name:        "he-aac",
		knobs:       KnobBitrate | KnobHEAACv2,
		exts:        []string{},
		writeExt:    "m4a",
		lossy:       true,
		headerBytes: 700,
		codecID:     codec.HEAAC,
		adjust: func(spec *dsp.ChainSpec, src audio.Format, opts TranscodeOptions) {
			// One AU is 2048 output samples. The encoder accepts three
			// dual-rate pairs, so a rateless request snaps the source onto
			// them (the opus row's precedent for a narrow rate set: a whole
			// hi-res or low-rate library must degrade to a resample, not
			// 415); an explicit rate= is honored and validated at plan time.
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
		// exts is empty since the aac row claimed m4a: ALAC output is
		// reachable by naming the format explicitly. It still writes an m4a,
		// which is what writeExt says; see the field.
		exts:     []string{},
		writeExt: "m4a",
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
		encode: func(f audio.Format, _ TranscodeOptions) (codec.Encoder, error) {
			return alac.NewEncoder(f, nil)
		},
		// ALAC rides only in MP4, so its overrides are the two box shapes:
		// progressive (flat) and fragmented (CMAF, the row's default).
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
		// headerBytes stays 0: size estimates are gated on a fixed
		// bytesPerFrame, which VBR lossless lacks. A .wv file has no
		// pre-audio header to account for either.
		codecID: codec.WavPack,
		adjust: func(spec *dsp.ChainSpec, src audio.Format, opts TranscodeOptions) {
			spec.FrameSize = wavpack.BlockSamples
			if opts.BitDepth != 0 {
				return // explicit depth; plan validates it against WavPack's set
			}
			// WavPack holds integer PCM at 8/16/24/32 bits. A float source
			// with no depth requested quantizes to 24, which carries the whole
			// float32 mantissa; an integer source at another depth snaps up to
			// the nearest container width, losslessly widened.
			if src.Type == audio.Float {
				spec.BitDepth = 24
			} else if d := wavpackSnapDepth(src.BitDepth); d != src.BitDepth {
				spec.BitDepth = d
			}
			// No fold to stereo: WavPack here is a lossless output, and one
			// that silently dropped four channels would be lying about what it
			// holds (the alac rule, not the lossy rows').
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
		// One form: the only other wrapper WavPack rides in is Matroska's
		// A_WAVPACK4, which our mka muxer does not write. The muxer writes the
		// APEv2 tag block after the audio itself.
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
		// headerBytes stays 0: size estimates are gated on a fixed
		// bytesPerFrame, which VBR lossless lacks.
		codecID: codec.APE,
		adjust: func(spec *dsp.ChainSpec, src audio.Format, opts TranscodeOptions) {
			spec.FrameSize = ape.BlocksPerFrame
			if opts.BitDepth != 0 {
				return // explicit depth; plan validates it against APE's set
			}
			// Monkey's Audio holds integer PCM at 8, 16, and 24 bits. A float
			// source with no depth requested quantizes to 24, which carries
			// the whole float32 mantissa; an integer source at another depth
			// snaps up to the nearest width, losslessly widened.
			//
			// A 32-bit integer source is left alone and refused by the
			// encoder, which is the same rule the channel policy below
			// follows: this codec covers no 32-bit mode, and every other
			// lossless row here carries 32-bit through, so quietly dropping
			// eight bits would make this the one output that loses data
			// without saying so. A caller who wants it asks for --bits 24.
			if src.Type == audio.Float {
				spec.BitDepth = 24
			} else if d := apeSnapDepth(src.BitDepth); d != src.BitDepth {
				spec.BitDepth = d
			}
			// No fold to stereo: this is a lossless output, and one that
			// silently dropped channels would be lying about what it holds
			// (the alac rule, not the lossy rows'). APE holds one or two
			// channels here, so anything wider is refused by the encoder.
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
		// One form, not live: a .ape opens with a seek table that is its only
		// index, and with totals nothing knows until the audio has gone out.
		// The muxer writes the APEv2 tag block after the audio itself.
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

// logImplicitDownmix says out loud that the chain folded channels the caller
// never asked to lose, at Warn rather than Debug: the default level is info,
// and a scripted conversion that quietly halves a 5.1 master should not need a
// flag to find out.
//
// Here rather than in buildPlanCore, which is package-level with no logger
// and, worse, memoized: PlanTranscode caches cores per (format, options), so a
// warning there would fire on the first of 500 identically shaped files and
// stay silent for the other 499. Every run-side entry point calls this, which
// is why it is a method and not four copies: the segmented path folds through
// the same adjust hook and used to say nothing at all.
func (e *Engine) logImplicitDownmix(opts TranscodeOptions, src, out audio.Format) {
	if opts.Channels == 0 && out.Channels < src.Channels {
		e.log.Warn("downmixed to fit the output format",
			"format", opts.Format, "source", src.Channels, "out", out.Channels)
	}
}

// logTimelineDownmix is the same announcement for a fold that happened upstream
// of the chain. A timeline built at the width it delivers (ConcatOptions.
// Channels) folds each member before the seam, so by the time the run's chain
// sees the envelope there is nothing left to fold and logImplicitDownmix has
// nothing to say. A 5.1 master was still quietly halved, and the operator
// should not need a flag to find out.
//
// It reads the members rather than the option, so it fires exactly when audio
// was discarded. It lives on the run side and not in Engine.TimelineChannels,
// which answers the width: that is a planning call on the HLS request path, and
// a Warn there is one line per master fetch rather than one per encode.
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

// foldWideToStereo is the lossy rows' channel policy, in one place because
// it is a policy and not an encoder detail: a source wider than the encoder
// can hold folds to stereo when the caller asked for no particular width.
//
// The split it draws is lossy against lossless, not one encoder against
// another. opus, mp3 and aac are all 1-2 channel encoders, and a listener
// asking for a lossy delivery of a 5.1 film has already accepted that audio
// is being discarded; refusing the request outright serves nobody, and it
// used to be that only opus folded while mp3 and aac 415'd the same request.
// alac is the counterexample and stays a refusal: a lossless output that
// silently drops four channels is a lie about what it holds. flac and vorbis
// hold multichannel natively and never reach here.
//
// An explicit channel request is never overridden: 1 and 2 are honored, and
// anything else fails loudly in the chain or the encoder rather than being
// silently rewritten. The fold itself is dsp/mix's BS.775 matrix with the
// chain's true-peak limiter behind it, unchanged; this decides only when it
// engages. Engine.TranscodeMedia logs the folds nobody asked for.
func foldWideToStereo(spec *dsp.ChainSpec, src audio.Format) {
	if spec.Channels == 0 && src.Channels > 2 {
		spec.Channels = 2
	}
}

// aacForms are the AAC rows' forms: fragmented MP4 by default (gapless
// edit-list signaling), "adts" the raw elementary stream (no gapless
// signaling, which is why it is the opt-out), the two MP4 box shapes by
// name, and Matroska (A_AAC), though not WebM (Opus/Vorbis only).
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
// moov+mdat form, "the .m4a most players expect"; it back-patches its header,
// so it needs a seekable destination and is not live. Fragmented is the CMAF
// form /stream and HLS deliver, and it is the aac and alac rows' default (the
// empty override).
//
// Fragmented is spellable even though it is the default because the empty
// override no longer reaches it everywhere: a file output takes the flat form
// (FileOutputContainer), which would otherwise leave the delivery form with no
// name a caller could ask for.
//
// Exported because four packages spell them: the CLI's --container flag, the
// job request's container field, the /stream query parameter, and this table.
// They were restated per boundary until the empty-to-progressive rule was
// reimplemented three times and one of the three was missed, which is what
// FileOutputContainer below now prevents by construction.
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

// FileOutputContainer resolves the container a file output should be written
// with: the caller's explicit choice, or the flat MP4 form when the plan says
// this is an mp4-family output and the caller expressed no preference.
//
// A file can satisfy the back-patch the flat header needs, streaming buys it
// nothing, and it is the only form that carries a QuickTime chapter track or
// that a tag rewriter can edit afterwards. Delivery keeps the fragmented
// default, which is where it belongs.
//
// It lives here rather than at each boundary because every writer of a file
// needs it: `waxflow transcode`, `waxflow split`, and all of the transcode,
// split and merge job types. Pass the plan taken from the caller's own options
// and re-plan when this changes the answer, so the plan and the run agree
// about what was asked for.
func FileOutputContainer(requested string, plan *TranscodePlan) string {
	if requested == "" && plan != nil && plan.MediaType == mp4MediaType {
		return ContainerProgressive
	}
	return requested
}

// isMatroska reports whether a Container override selects the MKA muxer.
func isMatroska(name string) bool { return name == "mka" || name == "webm" }

// StatesTrimsPerPacket reports whether a container states its gapless trims per
// packet (container.Packet.Padding) rather than once in a header or a trailer.
//
// Matroska's DiscardPadding is the only one, and the question is worth a name
// because three unrelated decisions turn on it and would otherwise each spell
// "is this Matroska" in their own words: whether a packet copy may carry a trim
// that is not at the end (PlanRemux), whether a muxer writes Packet.Padding at
// all (container.Packet), and whether a source is worth measuring before it is
// planned from, since only a walk finds such a trim and only such a container
// can have one (the daemon's prepareSource).
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
		// The copy starts from a non-nil empty slice so a format with no
		// extensions (alac since aac claimed m4a) marshals as [] not null.
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
			// A remux-only row claims no extensions today; the skip keeps
			// that true if one ever grows them.
			continue
		}
		if slices.Contains(o.exts, ext) {
			return o.name
		}
	}
	return ""
}

// OutputContainerForExt maps a container-selecting output extension (one that
// names a container form rather than a top-level format) to the format and
// container override it implies. MKA and WebM are reached through a Container
// override on a codec row, not their own output rows, so the extension alone
// does not resolve through OutputFormatForExt; this fills that gap for the CLI.
// A ".mka"/".mkv" defaults to lossless FLAC-in-Matroska (matching a lossless
// source), ".webm" to Opus-in-WebM. ok is false for any other extension.
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
// container (empty for the format's default form), without the leading dot.
//
// It is the write direction of OutputFormatForExt and a separate function
// rather than that one inverted, because the two are not inverses: an
// extension resolves to at most one format, so of the two formats that write
// .m4a only aac claims it, and yet both write it. A caller naming a download
// needs the name the file should carry, which is the question the read
// direction cannot answer. See output.writeExt.
//
// A container override names a wrapper, not a box shape, and only a
// different wrapper renames the file: adts, mka, webm, and ogg each write a
// different kind of file than the row's default, while progressive is the
// aac/alac row's own MP4 with its boxes flattened and stays an m4a. That
// distinction is what this exists for. A container name used as an extension
// yields foo.progressive, which is not a file Apple Books will open.
//
// An unregistered format falls back to bin. The caller is naming a download,
// so "" is not an answer; the format's own name is the confident lie this
// removes (foo.alac), and a format with no row has no file form to name at
// all, which is exactly what bin says.
//
// This names the result and does not validate the request: a pair no form
// takes answers for the row's default form.
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

// outputExtFallback is the extension for an output there is no file form for:
// opaque bytes, promising nothing. See OutputExt.
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
