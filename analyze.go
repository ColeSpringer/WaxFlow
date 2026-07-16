package waxflow

import (
	"context"
	"io"
	"time"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/dsp"
	"github.com/colespringer/waxflow/dsp/loudness"
	"github.com/colespringer/waxflow/dsp/silence"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/waxerr"
)

// Silence detection defaults, applied to the zero fields of
// SilenceOptions. The threshold suits studio-quiet content; see
// SilenceOptions.ThresholdDB for why the right value is a property of the
// source rather than of the detector.
const (
	DefaultSilenceThresholdDB = -50.0
	DefaultSilenceMinDuration = 500 * time.Millisecond
)

// AnalyzeOptions configures Engine.Analyze.
type AnalyzeOptions struct {
	// Progress, when non-nil, is called after each decoded chunk with the
	// samples measured so far and the projected total (-1 unknown). It
	// runs on the analyzing goroutine, so blocking it pauses the
	// analysis; the job runner's yield-to-live-streams check rides on
	// exactly that.
	Progress func(done, total int64)
	// Silence, when non-nil, maps the source's silent spans alongside the
	// loudness measurement, from the same decode. Nil omits the map
	// entirely, so an analysis that does not ask for it is unchanged.
	Silence *SilenceOptions
	// Tap, when non-nil, is called with each decoded chunk's planar channel
	// slices at the source's own rate and layout: chans[c][i] is sample i of
	// channel c, all channel slices the same length, values nominal full
	// scale +-1.0. It is the seam for an analyzer WaxFlow does not own,
	// riding the same decode as the meter rather than paying for a second
	// one, which is what AnalyzeOptions.Silence already does for one WaxFlow
	// does own.
	//
	// It runs on the analyzing goroutine, so blocking it pauses the
	// analysis; the same contract Progress carries. An error from it fails
	// the analysis, as an error from the meter does.
	//
	// The slices are borrowed: they alias the pooled chunk buffer and are
	// valid only for the duration of the call, and the next chunk reuses
	// them. A tap that keeps the samples must copy them. It must also not
	// write to them, which is why it runs after the analyzers this engine
	// owns rather than before: their measurements are already taken, so a
	// tap that breaks the rule breaks only its own result.
	Tap func(chans [][]float32) error
}

// SilenceOptions configures the silence map. Both fields are raw
// parameters rather than a closed vocabulary, which is the opposite of the
// choice gain= and dynamics= make, and deliberately: a closed vocabulary
// belongs where a value enters a cache key or a validated signal path,
// where it must mean the same thing forever. These values do neither. They
// shape a report, nothing is keyed by them, and the caller genuinely knows
// better than the daemon does.
type SilenceOptions struct {
	// ThresholdDB is the silence threshold in dBFS; 0 means
	// DefaultSilenceThresholdDB. It must be negative and finite, which the
	// detector enforces; tighter policy clamps live at the API boundary,
	// not here, exactly as they do for TranscodeOptions.GainDB.
	//
	// The right value is a property of the content, so there is no default
	// that suits everything. See dsp/silence.New for the guidance, and
	// SilenceResult.DroppedSamples for what a wrong one looks like: it does
	// not fail cleanly, it reports no silence at all.
	ThresholdDB float64
	// MinDuration is the shortest span worth reporting; 0 means
	// DefaultSilenceMinDuration. It must be positive, which the detector
	// enforces.
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
	// Version is the detector revision (ADR-0004 style). WaxFlow keys
	// nothing by it, but a caller caching the map needs it to know when
	// the map went stale.
	Version string
	// ThresholdDB and MinDuration are the resolved parameters, defaults
	// applied.
	ThresholdDB float64
	MinDuration time.Duration
	// Spans are the detected silences, in stream order.
	Spans []SilenceSpan
	// Dropped counts runs discarded for falling short of MinDuration.
	// Read it with DroppedSamples, never alone: ordinary audio dips under
	// any threshold at every zero crossing, so this is large even for a
	// source with clean silences.
	Dropped int
	// DroppedSamples is the summed length of those runs, and it is the
	// diagnostic. Against Samples it says how much of the source sat
	// below the threshold without ever staying there long enough to
	// report: near zero for a healthy source however large Dropped grows,
	// and a sizeable share of the stream when the threshold is wrong for
	// this source (see SilenceOptions.ThresholdDB).
	DroppedSamples int64
	// TotalSamples is the summed length of Spans, which is what a
	// "time saved by trimming" figure reads.
	TotalSamples int64
}

// AnalyzeResult is a full-stream loudness measurement of the decoded
// audio per ITU-R BS.1770-4 and EBU R128.
type AnalyzeResult struct {
	// Format is the PCM format the measurement ran on: the source's rate
	// and layout in the float domain.
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
}

// Analyze decodes src end to end and measures its loudness: integrated
// LUFS, loudness range, true peak, and sample peak. It powers the
// type:analyze job and the loudness:analyze two-pass transcode (the R128
// half of the loudness design: live streams stay tag-based, exact
// measurement belongs to jobs, where a second pass is affordable).
//
// AnalyzeOptions.Silence adds the silence map to the same pass. Both
// analyzers want the identical chain for the identical reason (the source's
// own rate and layout, in the float domain), so they share one decode
// rather than paying for two: the decode is the expensive half, and a
// library-wide sweep runs this over everything.
func (e *Engine) Analyze(ctx context.Context, src container.Source, hint string, opts AnalyzeOptions) (*AnalyzeResult, error) {
	med, err := e.OpenStream(src, hint)
	if err != nil {
		return nil, err
	}
	defer med.Close()
	return e.AnalyzeMedia(ctx, med, opts)
}

// AnalyzeMedia analyzes an already-opened Media, the same measurement as
// Analyze without the source-open step. It is the entry point for inputs
// that are not a single sniffable Source: the HLS client assembles a
// presentation from many fetched resources and exposes it as a
// format.Media, which flows through here exactly like a local file. The
// caller owns med and closes it.
func (e *Engine) AnalyzeMedia(ctx context.Context, med format.Media, opts AnalyzeOptions) (*AnalyzeResult, error) {
	track := med.Info().Default()
	// The chain only converts to float here (no resample, no mix): the
	// meter is rate-aware and weighs channels itself, so measurement runs
	// on the source's own timeline.
	chain, err := dsp.NewChain(dsp.NewSource(med, track.Fmt), dsp.ChainSpec{Float: true})
	if err != nil {
		return nil, err
	}
	defer chain.Release()

	f := chain.Format()
	meter, err := loudness.NewMeter(f.Rate, f.Channels, f.Layout)
	if err != nil {
		return nil, err
	}
	var det *silence.Detector
	var silThreshold float64
	var silMinDur time.Duration
	if opts.Silence != nil {
		silThreshold, silMinDur = opts.Silence.resolve()
		if det, err = silence.New(f.Rate, f.Channels, silThreshold, silMinDur); err != nil {
			return nil, err
		}
	}
	buf := audio.Get(f, audio.StandardChunk)
	defer audio.Put(buf)
	chans := make([][]float32, f.Channels)
	var done int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeCanceled, "analyze canceled", err)
		}
		err := chain.ReadChunk(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		for c := range chans {
			chans[c] = buf.ChanF(c)
		}
		if err := meter.Process(chans); err != nil {
			return nil, err
		}
		if det != nil {
			if err := det.Process(chans); err != nil {
				return nil, err
			}
		}
		if opts.Tap != nil {
			if err := opts.Tap(chans); err != nil {
				return nil, err
			}
		}
		done += int64(buf.N)
		if opts.Progress != nil {
			opts.Progress(done, track.Samples)
		}
	}
	meter.Flush()
	res := &AnalyzeResult{
		Format:         f,
		Samples:        done,
		IntegratedLUFS: meter.Integrated(),
		LoudnessRange:  meter.Range(),
		TruePeakDB:     meter.TruePeak(),
		SamplePeakDB:   meter.SamplePeak(),
	}
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
	return res, nil
}
