package waxflow

import (
	"context"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/dsp"
	"github.com/colespringer/waxflow/dsp/loudness"
	"github.com/colespringer/waxflow/dsp/mix"
	"github.com/colespringer/waxflow/dsp/silence"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/satmath"
	"github.com/colespringer/waxflow/waxerr"
)

// Silence detection defaults, applied to the zero fields of SilenceOptions.
// The threshold suits studio-quiet content; the right one is a property of
// the source (see SilenceOptions.ThresholdDB).
const (
	DefaultSilenceThresholdDB = -50.0
	DefaultSilenceMinDuration = 500 * time.Millisecond
)

// AnalyzeOptions configures Engine.Analyze.
type AnalyzeOptions struct {
	// Channels, when non-zero, measures the loudness after the mix
	// TranscodeOptions.Channels applies (dsp/mix), so a two-pass gain is
	// computed on the audio the encode meters; 0 keeps the source layout and
	// a negative count is CodeInvalidRequest. No limiter, gain or dither
	// runs, so TruePeakDB is the raw mix's. A widening that places measures
	// as the source does; mono duplicated on a front pair (stereo, quad)
	// measures +10*log10(2) higher, and placed on a center (3.0, 5.1, 7.1)
	// does not.
	Channels int
	// Progress, when non-nil, is called after each decoded chunk with the
	// samples measured so far and the projected total (-1 unknown). It runs
	// on the analyzing goroutine, so blocking it pauses the analysis.
	Progress func(done, total int64)
	// Silence, when non-nil, maps the source's silent spans from the same
	// decode as the loudness. Nil omits the map.
	Silence *SilenceOptions
	// Tap, when non-nil, is called on the analyzing goroutine with each
	// decoded chunk at the source's own rate and layout: chans[c][i] is
	// sample i of channel c, all slices one length, nominal full scale
	// +-1.0. Its error fails the analysis. The slices are the pooled
	// chunk's, valid only during the call: a tap copies what it keeps and
	// never writes them (it runs after the engine's analyzers, so a write
	// spoils only its own result).
	Tap func(chans [][]float32) error
}

// SilenceOptions configures the silence map. Its fields are raw parameters,
// unlike the closed vocabularies of gain= and dynamics=, since they shape a
// report, key nothing, and the caller knows its content best.
type SilenceOptions struct {
	// ThresholdDB is the silence threshold in dBFS; 0 means
	// DefaultSilenceThresholdDB. The detector refuses one that is not
	// negative and finite; tighter clamps belong at the API boundary. The
	// right value depends on the content (dsp/silence.New): a wrong one
	// reports no silence, which SilenceResult.DroppedSamples shows.
	ThresholdDB float64
	// MinDuration is the shortest span worth reporting; 0 means
	// DefaultSilenceMinDuration. The detector refuses one that is not
	// positive.
	MinDuration time.Duration
}

// resolve applies the defaults to the zero fields.
func (o SilenceOptions) resolve() (thresholdDB float64, minDur time.Duration) {
	thresholdDB, minDur = o.ThresholdDB, o.MinDuration
	if thresholdDB == 0 {
		thresholdDB = DefaultSilenceThresholdDB
	}
	if minDur == 0 {
		minDur = DefaultSilenceMinDuration
	}
	return thresholdDB, minDur
}

// SilenceSpan is one silent span of the analyzed source, in frames on its
// own timeline (ADR-0006). To is exclusive.
type SilenceSpan struct {
	From int64
	To   int64
}

// SilenceResult is the silence map: the spans plus the parameters they were
// found with, so a caller that stores the map can tell what it means.
type SilenceResult struct {
	// Version is the detector revision (ADR-0004). WaxFlow keys nothing by
	// it; a caller caching the map reads it to tell when the map went stale.
	Version string
	// ThresholdDB and MinDuration are the resolved parameters, defaults
	// applied.
	ThresholdDB float64
	MinDuration time.Duration
	// Spans are the detected silences, in stream order.
	Spans []SilenceSpan
	// Dropped counts runs discarded for falling short of MinDuration. Read it
	// with DroppedSamples: audio dips under any threshold at every zero
	// crossing, so it is large even for a source with clean silences.
	Dropped int
	// DroppedSamples is the summed length of those runs, and the diagnostic:
	// near zero for a healthy source however large Dropped grows, a sizeable
	// share of AnalyzeResult.Samples when the threshold is wrong for it.
	DroppedSamples int64
	// TotalSamples is the summed length of Spans, the time trimming saves.
	TotalSamples int64
}

// AnalyzeResult is a full-stream loudness measurement of the decoded
// audio per ITU-R BS.1770-4 and EBU R128.
type AnalyzeResult struct {
	// Format is the PCM format measured: the source rate in the float
	// domain, at the source layout or the AnalyzeOptions.Channels mix's.
	// The loudness and peak fields below are on that basis, so a 5.1 source
	// measured at Channels 2 reports a stereo loudness, range and true peak.
	Format audio.Format
	// Samples is the number of frames measured.
	Samples int64
	// IntegratedLUFS is the gated integrated loudness. Silence that
	// never passes the absolute gate reports math.Inf(-1).
	IntegratedLUFS float64
	// LoudnessRange is the EBU Tech 3342 loudness range in LU.
	LoudnessRange float64
	// TruePeakDB is the maximum oversampled true peak in dBTP,
	// math.Inf(-1) for silence.
	TruePeakDB float64
	// SamplePeakDB is the maximum sample magnitude in dBFS, math.Inf(-1)
	// for silence.
	SamplePeakDB float64
	// Silence is the silence map, non-nil exactly when AnalyzeOptions
	// asked for one.
	Silence *SilenceResult
	// InputWarnings is the input damage the read worked around, taken once
	// the whole stream is read (a frame-walked payload finds damage only as
	// the read reaches it). Nil for a clean source, and for a group's own
	// result: each member's is on that member's result.
	InputWarnings []string
	// LostSamples counts the measured frames that are silence standing in
	// for damaged audio (format.Info.Lost) in this read, at Samples' rate; a
	// hole keeps the stream's length, so Samples cannot show it. A group's is
	// its members' total. Zero with no hole, and for a format whose holes
	// shorten the read instead: every one but FLAC (native or in Ogg) and
	// WavPack.
	LostSamples int64
	// TruncatedSamples counts the unmeasured samples the header declares
	// past the end of the file (format.Info.Truncated), as this read left it,
	// at Samples' rate. A group's is its members' total. Zero for a whole
	// source, a header that states no length, and a window (Slice) that
	// stops short of the source's end.
	TruncatedSamples int64
}

// LossNote returns the one warning line LostSamples and TruncatedSamples
// warrant, or "" for a source that lost nothing, worded as
// TranscodeResult.LossNote words it.
func (r *AnalyzeResult) LossNote() string {
	if r == nil {
		return ""
	}
	return lossNote(r.LostSamples, r.TruncatedSamples, r.Format.Rate)
}

// Analyze decodes src end to end and measures its loudness: integrated
// LUFS, loudness range, true peak and sample peak, for the analyze job and
// the loudness:analyze two-pass transcode (live streams stay tag-based).
// AnalyzeOptions.Silence and Tap ride the same decode.
func (e *Engine) Analyze(ctx context.Context, src container.Source, hint string, opts AnalyzeOptions) (*AnalyzeResult, error) {
	med, err := e.OpenStream(src, hint)
	if err != nil {
		return nil, err
	}
	defer med.Close()
	return e.AnalyzeMedia(ctx, med, opts)
}

// AnalyzeMedia is Analyze over an already-opened Media, for an input that is
// not one sniffable Source (a presentation assembled from fetched
// resources). The caller owns med and closes it.
func (e *Engine) AnalyzeMedia(ctx context.Context, med format.Media, opts AnalyzeOptions) (*AnalyzeResult, error) {
	res, _, err := e.analyzeMedia(ctx, med, opts)
	return res, err
}

// analyzeMedia is AnalyzeMedia, also returning the flushed meter so a group
// can fold the member's blocks in.
func (e *Engine) analyzeMedia(ctx context.Context, med format.Media, opts AnalyzeOptions) (*AnalyzeResult, *loudness.Meter, error) {
	// NewChain's code for TranscodeOptions.Channels < 0, so both passes agree.
	if opts.Channels < 0 {
		return nil, nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("analyze: negative channel count %d", opts.Channels))
	}
	opened := med.Info()
	track := opened.Default()
	lostBefore := opened.Lost // a caller's Media may have been read already
	// The chain only converts to float: the meter is rate-aware and, absent
	// a mix, weighs the source channels itself. A Channels mix folds below
	// as mixStage does, outside the chain, with no limiter (ADR-0010).
	chain, err := dsp.NewChain(dsp.NewSource(med, track.Fmt), dsp.ChainSpec{Float: true})
	if err != nil {
		return nil, nil, err
	}
	defer chain.Release()

	f := chain.Format()

	// meterFmt is the source format, or the fold's count and layout. The rate
	// stays the source's (ADR-0010).
	meterFmt := f
	var matrix *mix.Matrix
	var scratch *audio.Buffer
	var dstV [][]float32
	if opts.Channels != 0 && opts.Channels != f.Channels {
		// Before mix.For, since the refusal is about the samples rather than
		// the layouts: a member placed into a wider envelope has no fold of
		// its own (ADR-0010).
		if err := refuseMixedWidthConversion(med, f, opts.Channels); err != nil {
			return nil, nil, err
		}
		// An unmasked source takes its count's default layout, as mixStage
		// does (dsp.go), so the fold's inputs are the encode's.
		srcLayout := f.Layout
		if srcLayout == 0 {
			srcLayout = audio.DefaultLayout(f.Channels)
		}
		dstLayout := audio.DefaultLayout(opts.Channels)
		// A count above MaxChannels has no layout convention; refuse it with
		// dsp.go's phrasing. A decoded source has 1..MaxChannels, so only
		// dstLayout can be zero; the srcLayout test mirrors dsp.go's.
		if srcLayout == 0 || dstLayout == 0 {
			return nil, nil, waxerr.New(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("analyze: no layout convention for %d -> %d channels", f.Channels, opts.Channels))
		}
		// mix.For before any buffer: it refuses cleanly a source position the
		// target has no place for, and audio.Get panics on an invalid format.
		matrix, err = mix.For(srcLayout, dstLayout)
		if err != nil {
			return nil, nil, err
		}
		meterFmt.Channels = opts.Channels
		meterFmt.Layout = dstLayout
		scratch = audio.Get(meterFmt, audio.StandardChunk)
		defer audio.Put(scratch)
		dstV = make([][]float32, opts.Channels)
	}

	meter, err := loudness.NewMeter(meterFmt.Rate, meterFmt.Channels, meterFmt.Layout)
	if err != nil {
		return nil, nil, err
	}
	// The silence detector and Tap read the source channels, never the mix:
	// a silence span is on the source's timeline, and the fold drops LFE, so
	// an LFE-only span would read silent in a stereo fold.
	var det *silence.Detector
	var silThreshold float64
	var silMinDur time.Duration
	if opts.Silence != nil {
		silThreshold, silMinDur = opts.Silence.resolve()
		if det, err = silence.New(f.Rate, f.Channels, silThreshold, silMinDur); err != nil {
			return nil, nil, err
		}
	}
	buf := audio.Get(f, audio.StandardChunk)
	defer audio.Put(buf)
	chans := make([][]float32, f.Channels)
	var done int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, waxerr.Wrap(waxerr.CodeCanceled, "analyze canceled", err)
		}
		err := chain.ReadChunk(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		for c := range chans {
			chans[c] = buf.ChanF(c)
		}
		if matrix != nil {
			// The meter takes its frame count from len(dstV[c]), so a
			// zero-N scratch would measure nothing without panicking. buf
			// and scratch are distinct pool buffers, so they never alias.
			scratch.N = buf.N
			for c := range dstV {
				dstV[c] = scratch.ChanF(c)
			}
			matrix.Apply(dstV, chans, buf.N)
			if err := meter.Process(dstV); err != nil {
				return nil, nil, err
			}
		} else if err := meter.Process(chans); err != nil {
			return nil, nil, err
		}
		if det != nil {
			if err := det.Process(chans); err != nil {
				return nil, nil, err
			}
		}
		if opts.Tap != nil {
			if err := opts.Tap(chans); err != nil {
				return nil, nil, err
			}
		}
		done += int64(buf.N)
		if opts.Progress != nil {
			opts.Progress(done, track.Samples)
		}
	}
	meter.Flush()
	res := &AnalyzeResult{
		Format:         meterFmt,
		Samples:        done,
		IntegratedLUFS: meter.Integrated(),
		LoudnessRange:  meter.Range(),
		TruePeakDB:     meter.TruePeak(),
		SamplePeakDB:   meter.SamplePeak(),
	}
	// Taken here: Analyze and a group member opened on demand close the
	// media before the caller sees the result.
	found := med.Info()
	if ws := found.Warnings; len(ws) > 0 {
		res.InputWarnings = slices.Clone(ws)
	}
	res.LostSamples = found.Lost - lostBefore
	res.TruncatedSamples = found.Truncated
	if det != nil {
		det.Flush()
		spans := make([]SilenceSpan, len(det.Spans()))
		for i, s := range det.Spans() {
			spans[i] = SilenceSpan{From: s.From, To: s.To}
		}
		res.Silence = &SilenceResult{
			Version:        silence.Version,
			ThresholdDB:    silThreshold,
			MinDuration:    silMinDur,
			Spans:          spans,
			Dropped:        det.Dropped(),
			DroppedSamples: det.DroppedSamples(),
			TotalSamples:   det.TotalSamples(),
		}
	}
	return res, meter, nil
}

// GroupMember is one stream of an album measurement: its source, handed
// open or opened on demand, and the width it is measured at.
type GroupMember struct {
	// Media is the opened source. The caller owns it and closes it. Set
	// exactly one of Media and Open.
	Media format.Media
	// Open opens the source when the measurement reaches this member, inside
	// the AnalyzeGroup call (a closure may bind its context). The engine
	// closes what it returns before opening the next, so a group holds one
	// descriptor at a time; the read's damage is on the member's
	// AnalyzeResult.InputWarnings. A nil Media from it is CodeInvalidRequest.
	Open func() (format.Media, error)
	// Channels is the width this member is delivered and so measured at
	// (ADR-0010): its own plan's PlanTranscode(...).Format.Channels, never
	// the group's widest (a mono member measured at a stereo group's width
	// reads 3.01 dB hot). 0 measures it at its own width, for a member
	// delivered unchanged.
	Channels int
}

// AnalyzeGroupResult is AnalyzeGroup's answer: the group's own measurement
// and each member's, from one decode per member.
type AnalyzeGroupResult struct {
	// Group is the measurement over every member at once, the one an album
	// gain is computed from. Its Format is the zero value, since each member
	// is measured at its own width. Samples is the total across members.
	Group AnalyzeResult
	// Members holds each member's own measurement, in the order given.
	Members []AnalyzeResult
}

// AnalyzeGroup measures members as one programme, for an album gain: each
// member is decoded once and metered at its delivered width, and the gates
// run over the union of their blocks (ADR-0010). It refuses with
// CodeInvalidRequest an empty group, a member with neither or both of Media
// and Open or a negative width, and AnalyzeOptions.Silence, Tap or Channels,
// which are per member (call Analyze per member). Progress counts across the
// group against the members' summed projected totals, -1 while any is
// unknown or unopened; an advisory length can carry the count past it.
func (e *Engine) AnalyzeGroup(ctx context.Context, members []GroupMember, opts AnalyzeOptions) (*AnalyzeGroupResult, error) {
	if len(members) == 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "analyze: a group needs at least one member")
	}
	if opts.Silence != nil || opts.Tap != nil {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			"analyze: a group measurement carries no silence map and no tap; both are per-source and Analyze reports them per member")
	}
	if opts.Channels != 0 {
		// Refused rather than overwritten: one count cannot honour members
		// delivered at different widths.
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			"analyze: a group takes each member's delivered width from GroupMember.Channels, not one count for all of them")
	}
	// unopened counts members still to declare: one opened on demand
	// declares when the run reaches it.
	projected, unopened := int64(0), 0
	for i, m := range members {
		switch {
		case m.Media == nil && m.Open == nil:
			return nil, waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("analyze: group member %d has no media and no Open function", i))
		case m.Media != nil && m.Open != nil:
			return nil, waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("analyze: group member %d has both a Media and an Open function; set one", i))
		case m.Channels < 0:
			// analyzeMedia would refuse it only on reaching the member.
			return nil, waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("analyze: group member %d has a negative channel count %d", i, m.Channels))
		case m.Media == nil:
			unopened++
		default:
			projected = sumProjected(projected, m.Media.Info().Default().Samples)
		}
	}
	total := func() int64 {
		if unopened > 0 || projected < 0 {
			return -1
		}
		return projected
	}
	out := &AnalyzeGroupResult{Members: make([]AnalyzeResult, 0, len(members))}
	var group loudness.Group
	var base, lost, truncated int64
	for i, m := range members {
		mo := opts
		mo.Channels = m.Channels
		if opts.Progress != nil {
			mo.Progress = func(done, _ int64) { opts.Progress(base+done, total()) }
		}
		res, meter, err := e.analyzeGroupMember(ctx, m, mo, func(med format.Media) {
			unopened--
			projected = sumProjected(projected, med.Info().Default().Samples)
		})
		if err != nil {
			return nil, waxerr.Annotate(fmt.Sprintf("group member %d", i), err)
		}
		if err := group.Add(meter); err != nil {
			return nil, err
		}
		out.Members = append(out.Members, *res)
		base += res.Samples
		lost += res.LostSamples
		truncated = satmath.Add(truncated, res.TruncatedSamples)
	}
	out.Group = AnalyzeResult{
		Samples:          base,
		LostSamples:      lost,
		TruncatedSamples: truncated,
		IntegratedLUFS:   group.Integrated(),
		LoudnessRange:    group.Range(),
		TruePeakDB:       group.TruePeak(),
		SamplePeakDB:     group.SamplePeak(),
	}
	return out, nil
}

// analyzeGroupMember measures one member. One with an Open function is
// opened here, passed to opened, and closed on return, before the next
// opens; one handed open is left open.
func (e *Engine) analyzeGroupMember(ctx context.Context, m GroupMember, opts AnalyzeOptions, opened func(format.Media)) (*AnalyzeResult, *loudness.Meter, error) {
	med := m.Media
	if med == nil {
		var err error
		if med, err = m.Open(); err != nil {
			return nil, nil, err
		}
		if med == nil {
			return nil, nil, waxerr.New(waxerr.CodeInvalidRequest, "analyze: Open returned no media")
		}
		// Its Close error is dropped, as Analyze drops it: the result is
		// taken.
		defer med.Close()
		opened(med)
	}
	return e.analyzeMedia(ctx, med, opts)
}

// sumProjected adds a member's projected length to a running total, -1 once
// either side is unknown.
func sumProjected(sum, n int64) int64 {
	if sum < 0 || n < 0 {
		return -1
	}
	return sum + n
}
