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
	// source to this channel count (matching a later
	// TranscodeOptions.Channels), so a two-pass gain is computed on the
	// audio the encode will meter. 0 keeps the source layout. The mix is
	// the same one the encode applies (dsp/mix), but with no limiter, gain,
	// or dither: a measurement observes the raw mix, so TruePeakDB stays
	// honest where the encode's overshoot limiter would flatten it. This is
	// the substantive difference from TranscodeOptions.Channels.
	//
	// A widening that places changes nothing measurable: the source's
	// positions land on their own at unity, the rest are silent, and channel
	// powers sum under BS.1770, so the measurement is the source's own. One
	// that duplicates does change it, and mono is the case: a lone channel
	// copied across a target's front pair (stereo, quad) measures
	// +10*log10(2), where the same channel placed on a target's center (3.0,
	// 5.1, 7.1) does not.
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
	// InputWarnings is the input damage the read worked around, as the
	// source's Info reports it once the whole stream has been read: a
	// frame-walked payload finds its damage where the read reaches it, so
	// the list is complete only now, and Analyze closes the source before
	// returning, so this is where its verdict survives. Nil for a clean
	// source. A group's own result carries none; each member's list is on
	// that member's result.
	InputWarnings []string
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
	res, _, err := e.analyzeMedia(ctx, med, opts)
	return res, err
}

// analyzeMedia is AnalyzeMedia, also handing back the flushed meter so a
// group can fold the member's blocks in. Nothing else wants it: every
// number a caller reads is already on the result.
func (e *Engine) analyzeMedia(ctx context.Context, med format.Media, opts AnalyzeOptions) (*AnalyzeResult, *loudness.Meter, error) {
	// A negative channel count is a malformed request, not an unsupported
	// layout: reject it upfront with the same code the encode's NewChain
	// gives TranscodeOptions.Channels < 0 (dsp.go), so a two-pass job that
	// passes the same bad value to both passes reports it the same way.
	if opts.Channels < 0 {
		return nil, nil, waxerr.New(waxerr.CodeInvalidRequest,
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
		return nil, nil, err
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
		// Before mix.For, because the refusal is about which samples are
		// being folded rather than which layouts: a timeline whose members
		// were placed into a wider envelope has no fold that is any member's
		// own. See refuseMixedWidthConversion.
		if err := refuseMixedWidthConversion(med, f, opts.Channels); err != nil {
			return nil, nil, err
		}
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
			return nil, nil, waxerr.New(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("analyze: no layout convention for %d -> %d channels", f.Channels, opts.Channels))
		}
		// mix.For next, before any buffer: a pair it cannot serve (a source
		// position the target layout has no place for) rejects here with a
		// clean error, whereas audio.Get below panics on an invalid format.
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
	// Taken here rather than by the caller: Analyze and a group member
	// opened on demand close the media before the caller sees the result.
	if ws := med.Info().Warnings; len(ws) > 0 {
		res.InputWarnings = slices.Clone(ws)
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
	return res, meter, nil
}

// GroupMember is one stream of an album measurement: its source, handed
// open or opened on demand, and the width it is measured at.
type GroupMember struct {
	// Media is the opened source. The caller owns it and closes it. Set
	// exactly one of Media and Open.
	Media format.Media
	// Open opens the source when the measurement reaches this member. The
	// engine closes what it returns before opening the next member, so a
	// group of such members holds one descriptor at a time however long the
	// album, the way ConcatSource.Open does for a timeline. The damage the
	// member's read finds comes back on its AnalyzeResult.InputWarnings,
	// since the Media is closed by the time the call returns.
	//
	// Unlike ConcatSource.Open this fires inside the AnalyzeGroup call, so
	// a closure may bind that call's context.
	Open func() (format.Media, error)
	// Channels is the width this member will be DELIVERED at, which is
	// what it must be measured at (ADR-0010). Read it off the member's own
	// plan -- PlanTranscode(...).Format.Channels is the number the two-pass
	// job already uses -- rather than passing the group's widest: a mono
	// member measured inside a stereo envelope reads 3.01 dB hot, because
	// the envelope duplicates it, and the album gain then pushes that
	// member down by the same 3 dB.
	//
	// 0 measures the member at its own width, which is right when it is
	// delivered unchanged.
	Channels int
}

// AnalyzeGroupResult is AnalyzeGroup's answer: the group's own measurement
// and each member's, from one decode per member.
type AnalyzeGroupResult struct {
	// Group is the measurement over every member at once, which is the one
	// an album gain is computed from. Its Format is the zero value: a group
	// has no single basis, since each member is measured at its own
	// delivered width. Samples is the total measured across members.
	Group AnalyzeResult
	// Members holds each member's own measurement, in the order given.
	Members []AnalyzeResult
}

// AnalyzeGroup measures a set of members as one programme, which is what an
// album normalize needs: one gain, computed from the gates run over every
// member at once rather than from an average of per-track numbers.
//
// Each member is decoded once and metered at its own delivered width (see
// GroupMember.Channels), and the group's gates then run over the union of
// their blocks. That is the difference from concatenating the members and
// measuring the result: a timeline is built at one width (ADR-0010), so a
// concatenation would have to widen every member into one envelope and
// measure each through a fold that is not its own.
//
// AnalyzeOptions.Silence, AnalyzeOptions.Tap and AnalyzeOptions.Channels are
// refused here: the first two are per-stream properties on a source's own
// timeline and a group has no timeline, and the third is per member. Call
// Analyze per member for those. Progress, when set, reports the samples
// measured across the whole group against the sum of the members' projected
// totals, -1 when any member's is unknown; as in Analyze the two are
// different quantities, so a member whose declared length is advisory can
// carry the count past the total. A member opened on demand has no
// projected total until the run reaches it, so a group with such members
// reports -1 until its last member is open, and from then on the members'
// sum when every one of them declares a length.
func (e *Engine) AnalyzeGroup(ctx context.Context, members []GroupMember, opts AnalyzeOptions) (*AnalyzeGroupResult, error) {
	if len(members) == 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "analyze: a group needs at least one member")
	}
	if opts.Silence != nil || opts.Tap != nil {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			"analyze: a group measurement carries no silence map and no tap; both are per-source and Analyze reports them per member")
	}
	if opts.Channels != 0 {
		// Refused rather than overwritten by each GroupMember.Channels: a
		// group's whole point is that its members are delivered at
		// different widths, so one count for all of them is a request that
		// cannot be honoured rather than one to quietly reinterpret.
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			"analyze: a group takes each member's delivered width from GroupMember.Channels, not one count for all of them")
	}
	// projected is the progress total: the members' declared lengths
	// summed, -1 while any is unknown. A member handed open declares now;
	// one opened on demand declares when the run reaches it, so unopened
	// counts those still to come and the total stays -1 until it is zero.
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
			// analyzeMedia refuses this too, but only when the run reaches
			// the member, after every earlier one has been decoded.
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
	var base int64
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
	}
	out.Group = AnalyzeResult{
		Samples:        base,
		IntegratedLUFS: group.Integrated(),
		LoudnessRange:  group.Range(),
		TruePeakDB:     group.TruePeak(),
		SamplePeakDB:   group.SamplePeak(),
	}
	return out, nil
}

// analyzeGroupMember measures one member. A member handed an Open function
// is opened here and closed on the way out, so the next member opens only
// once this one is closed; opened runs between the two, with the media the
// Open returned. A member handed open is measured as it is and left open.
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
		// The read's warnings are on the result by the time this runs, and
		// its error is discarded as Analyze discards it: the measurement is
		// already taken.
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
