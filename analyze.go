package waxflow

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/dsp"
	"github.com/colespringer/waxflow/dsp/loudness"
	"github.com/colespringer/waxflow/dsp/mix"
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
	// Channels, when non-zero, measures the loudness after mixing the
	// source down to this channel count (1 or 2, matching a later
	// TranscodeOptions.Channels), so a two-pass gain is computed on the
	// audio the encode will meter. 0 keeps the source layout. The fold is
	// the same one the encode applies (dsp/mix), but with no limiter, gain,
	// or dither: a measurement observes the raw fold, so TruePeakDB stays
	// honest where the encode's overshoot limiter would flatten it. This is
	// the substantive difference from TranscodeOptions.Channels.
	Channels int
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
	// Format is the PCM format the measurement ran on: the source rate in
	// the float domain, and the source channel layout unless
	// AnalyzeOptions.Channels asked for a downmix, in which case it is the
	// folded layout (the rate stays the source rate either way). When a
	// downmix was asked for, every measured field below (IntegratedLUFS,
	// LoudnessRange, TruePeakDB, SamplePeakDB) is on that downmix basis,
	// since all come off one meter fed the folded channels: a 5.1 source
	// measured at Channels 2 reports a stereo loudness, range, and true
	// peak, which is what makes the two-pass gain correct.
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
	// A negative channel count is a malformed request, not an unsupported
	// layout: reject it upfront with the same code the encode's NewChain
	// gives TranscodeOptions.Channels < 0 (dsp.go), so a two-pass job that
	// passes the same bad value to both passes reports it the same way.
	if opts.Channels < 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("analyze: negative channel count %d", opts.Channels))
	}
	track := med.Info().Default()
	// The chain only converts to float here (no resample, no mix): the
	// meter is rate-aware, and absent a downmix it weighs the source
	// channels itself, so measurement runs on the source's own timeline. An
	// AnalyzeOptions.Channels downmix folds below with the same dsp/mix
	// primitive mixStage uses, but deliberately outside this chain, so it
	// skips the overshoot limiter the chain inserts for a downmix (dsp.go):
	// that limiter holds true peak at the ceiling and acts non-linearly on
	// pre-gain overshoots, which would corrupt the very loudness and
	// true-peak numbers the measurement exists to report.
	chain, err := dsp.NewChain(dsp.NewSource(med, track.Fmt), dsp.ChainSpec{Float: true})
	if err != nil {
		return nil, err
	}
	defer chain.Release()

	f := chain.Format()

	// meterFmt is the format the meter runs on: the source format, unless a
	// downmix was asked for, in which case the channel count and layout
	// become the fold's target. The rate stays the source rate on purpose:
	// only channels are folded here (loudness is essentially
	// resample-invariant and the meter is rate-aware), so a job that both
	// resamples and downmixes keeps a deliberate sub-0.01 LU rate residual.
	// Do not "fix" it by resampling the measurement; the multi-dB error is
	// the channel count, which this handles.
	meterFmt := f
	var matrix *mix.Matrix
	var scratch *audio.Buffer
	var dstV [][]float32
	if opts.Channels != 0 && opts.Channels != f.Channels {
		// srcLayout mirrors the encode's mixStage fallback (dsp.go): an
		// unmasked source takes its count's default layout, so the fold's
		// inputs are byte-identical to the encode's. A decoded source is
		// always 1..MaxChannels, all of which have a default, so in practice
		// only dstLayout can come back zero.
		srcLayout := f.Layout
		if srcLayout == 0 {
			srcLayout = audio.DefaultLayout(f.Channels)
		}
		dstLayout := audio.DefaultLayout(opts.Channels)
		// A target count with no layout convention (above MaxChannels) has a
		// zero mask; reject it before mix.For with the dsp.go phrasing. The
		// srcLayout == 0 disjunct only mirrors dsp.go:292 one for one; after
		// the fallback above it cannot fire for a real 1..MaxChannels source.
		if srcLayout == 0 || dstLayout == 0 {
			return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("analyze: no layout convention for %d -> %d channels", f.Channels, opts.Channels))
		}
		// mix.For next, before any buffer: a target with a valid mask but no
		// downmix (3 or 6 channels, since only mono and stereo targets exist)
		// rejects here with a clean error, whereas audio.Get below panics on
		// an invalid format.
		matrix, err = mix.For(srcLayout, dstLayout)
		if err != nil {
			return nil, err
		}
		meterFmt.Channels = opts.Channels
		meterFmt.Layout = dstLayout
		scratch = audio.Get(meterFmt, audio.StandardChunk)
		defer audio.Put(scratch)
		dstV = make([][]float32, opts.Channels)
	}

	meter, err := loudness.NewMeter(meterFmt.Rate, meterFmt.Channels, meterFmt.Layout)
	if err != nil {
		return nil, err
	}
	// The silence detector and Tap keep consuming the source channels, never
	// the downmix: Tap's contract is the source's own rate and layout, and
	// the fold drops LFE, so an LFE-only span reads silent in a stereo fold
	// yet is not silent in the source. A silence span is a source-timeline
	// property, so it must be measured on the source.
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
		if matrix != nil {
			// scratch.N sizes the ChanF views below, and the meter infers
			// its frame count from len(dstV[c]) (loudness.Process takes no
			// explicit count). A zero-N scratch would not panic (Apply folds
			// correctly into the backing array whatever N is); it would make
			// the meter silently measure zero frames. Set N to this chunk's
			// count so the views are the right length. buf and scratch are
			// distinct pool allocations, so dst and src never alias.
			scratch.N = buf.N
			for c := range dstV {
				dstV[c] = scratch.ChanF(c)
			}
			matrix.Apply(dstV, chans, buf.N)
			if err := meter.Process(dstV); err != nil {
				return nil, err
			}
		} else if err := meter.Process(chans); err != nil {
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
		Format:         meterFmt,
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
