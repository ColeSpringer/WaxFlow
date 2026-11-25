package waxflow

import (
	"context"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/dsp"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/m3u8"
	"github.com/colespringer/waxflow/waxerr"
)

// DefaultSegmentSeconds is the HLS target segment duration when a request
// names none, per Apple's authoring guidance.
const DefaultSegmentSeconds = 4.0

// maxSegmentSeconds bounds a requested segment duration. The arithmetic
// holds past it, but delivery degenerates toward one segment per track,
// which defeats HLS.
const maxSegmentSeconds = 60.0

// primeSeconds is the pre-target audio a mid-stream segmented run feeds the
// encoder, rounded up to whole frames and discarded on a frame boundary, and
// the floor on the chain's pre-roll (ADR-0014).
const primeSeconds = 0.1

// snapSegmentSamples resolves a requested segment duration to a whole number
// of grid frames, at least one, so segment boundaries land between packets.
// The grid is the encoder's frame for a transcode and the source's packet
// duration for a remux. mp4.Segmenter's tfdt = index * SegmentSamples holds
// only on packet boundaries, and both rungs snap here so they agree on where
// a segment ends.
func snapSegmentSamples(segSeconds float64, rate, grid int) (int, error) {
	segSeconds, err := segmentSeconds(segSeconds)
	if err != nil {
		return 0, err
	}
	if grid <= 0 {
		return 0, waxerr.New(waxerr.CodeInternal, "waxflow: segment grid must be positive")
	}
	return max(int(segSeconds*float64(rate)/float64(grid)+0.5), 1) * grid, nil
}

// segmentSeconds resolves a requested segment duration: 0 is the default, and
// a NaN, negative or over-long one is refused. A copy planner asks it before
// it measures the packet grid.
func segmentSeconds(s float64) (float64, error) {
	switch {
	case s == 0:
		return DefaultSegmentSeconds, nil
	// A NaN compares false against both bounds, so it is named.
	case math.IsNaN(s) || s < 0 || s > maxSegmentSeconds:
		return 0, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: segment duration %g outside 0..%g seconds", s, maxSegmentSeconds))
	}
	return s, nil
}

// SegmentPlan describes the segmented CMAF (HLS) form of a transcode,
// computed from headers alone like the TranscodePlan it embeds. Its Versions
// carry the segmenter revision, so an HLS cache key derives from it directly.
// The embedded Container, MediaType and Live name the row's default
// progressive form, which the plan's identity keys on; the segments are
// fMP4, and a trim or muxer question about them asks copyDest, never these
// fields.
type SegmentPlan struct {
	TranscodePlan
	// SegmentSamples is the decode duration of every segment but the last,
	// in output samples: the requested duration snapped to whole frames.
	SegmentSamples int
	// Delay is the encoder delay the init segment's edit list carries: the
	// row's for a transcode, the source's own for a remux, the cut's
	// synthesized one for a cut.
	Delay int64
	// Codecs is the RFC 6381 CODECS attribute for master playlists.
	Codecs string
	// Bandwidth is the master playlist's BANDWIDTH value: the encoder's
	// target rate where the plan reports one (TranscodePlan.BitRate), else
	// the PCM wire rate, plus segmentation overhead.
	Bandwidth int
	// TotalDecodeSamples is the whole stream's decode duration: the
	// trimmed output length plus the delay and padding frames the encoder
	// flushes, rounded as the codec rounds. -1 when the source length is
	// unknown (VOD playlists then need the length measured first).
	TotalDecodeSamples int64
	// Segments is the exact segment count a VOD playlist promises, -1
	// when the source length is unknown.
	Segments int64
}

// SegmentDuration returns segment n's decode duration in samples, or -1
// when n is out of range or the total is unknown.
func (p *SegmentPlan) SegmentDuration(n int64) int64 {
	if p.Segments < 0 || n < 0 || n >= p.Segments {
		return -1
	}
	if n < p.Segments-1 {
		return int64(p.SegmentSamples)
	}
	return p.TotalDecodeSamples - (p.Segments-1)*int64(p.SegmentSamples)
}

// PresentationDuration returns segment n's playable duration in samples, or
// -1 where SegmentDuration does: its decode span intersected with the edit
// list's presentation window [Delay, Delay+Samples), so the durations sum to
// Samples. Only the playlist reads it; the segment count, the workers and
// the tfdt stay on the decode timeline. With no delay (FLAC, ALAC) it equals
// SegmentDuration.
func (p *SegmentPlan) PresentationDuration(n int64) int64 {
	d := p.SegmentDuration(n)
	if d < 0 {
		return -1
	}
	start := n * int64(p.SegmentSamples)
	return max(0, min(start+d, p.Delay+p.Samples)-max(start, p.Delay))
}

// MediaSegments lists the plan's segments for a VOD media playlist, segment
// n at segmentURI(n) timed by PresentationDuration, for m3u8.Media to render
// as the daemon's /hls/media.m3u8 does, from any rung's planner. It refuses
// with CodeInvalidRequest a plan of unknown length (measure the source with
// MeasureLength and plan from that track), a nil segmentURI, a plan with no
// output rate, one whose segment fields disagree (only a hand-built plan
// can), and one past m3u8.MaxSegments.
func (p *SegmentPlan) MediaSegments(segmentURI func(n int64) string) ([]m3u8.MediaSegment, error) {
	switch {
	case p.Segments < 0:
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			"waxflow: the plan's length is unknown, so no playlist can list its segments; plan from a measured track")
	case segmentURI == nil:
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: MediaSegments needs a segment URI function")
	case p.Format.Rate <= 0:
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: the plan has no output rate")
	case p.SegmentSamples <= 0 || p.Delay < 0 || p.Samples < 0 || p.Delay+p.Samples > p.TotalDecodeSamples ||
		p.Segments != (p.TotalDecodeSamples+int64(p.SegmentSamples)-1)/int64(p.SegmentSamples):
		// What every planner's plan holds by construction.
		return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
			"waxflow: the plan's segment fields disagree: %d segments of %d over %d decode samples, %d presented after %d",
			p.Segments, p.SegmentSamples, p.TotalDecodeSamples, p.Samples, p.Delay))
	case p.Segments > m3u8.MaxSegments:
		return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
			"waxflow: %d segments are past the %d a playlist lists; raise the segment duration", p.Segments, m3u8.MaxSegments))
	}
	rate := float64(p.Format.Rate)
	segs := make([]m3u8.MediaSegment, p.Segments)
	for n := range segs {
		segs[n] = m3u8.MediaSegment{
			URI: segmentURI(int64(n)),
			// The presentation span: a player sums EXTINF into a total,
			// which the decode span overshoots by the delay and tail rounding.
			Seconds: float64(p.PresentationDuration(int64(n))) / rate,
		}
	}
	return segs, nil
}

// MasterVariant is the plan's rung of a master playlist, at uri: the
// bandwidth bound and CODECS the daemon's master advertises for it.
func (p *SegmentPlan) MasterVariant(uri string) m3u8.MasterVariant {
	return m3u8.MasterVariant{URI: uri, Bandwidth: p.Bandwidth, Codecs: p.Codecs}
}

// segmentBitDepth applies the segmented depth rule to opts: a flac request
// naming no BitDepth, off an integer source at a depth other than 8, 16, 24
// or 32, is widened to the next whole byte. Chromium's MSE parser takes only
// those four and requires a FLAC entry's depth to equal STREAMINFO's, so no
// header makes such a track playable. Widening is lossless (FLAC's
// wasted-bits coding takes the padding back out); it costs the remux rung,
// which declines a depth that is not the source's. Other codecs carry a
// clamped entry depth instead, and progressive output is untouched
// (docs/hls-validation.md).
func segmentBitDepth(opts TranscodeOptions, src audio.Format) TranscodeOptions {
	if opts.Format != "flac" || opts.BitDepth != 0 || src.Type != audio.Int {
		return opts
	}
	switch src.BitDepth {
	case 8, 16, 24, 32:
		return opts
	}
	// The next whole byte, which takes a sub-8-bit source to 8.
	opts.BitDepth = pcm.ContainerBits(src.BitDepth)
	return opts
}

// PlanSegments plans the segmented form of a transcode of track. opts is
// the per-variant output selection (FromSample must be zero: segments own
// the timeline); segSeconds is the target segment duration, 0 for the
// default. A plan that succeeds guarantees TranscodeSegments and
// InitSegment accept the same options. It applies segmentBitDepth itself, as
// the other segmented plans and the transcode, remux and cut runs do off the
// same source format, so no rung plans one depth and runs another.
func (e *Engine) PlanSegments(track container.Track, opts TranscodeOptions, segSeconds float64) (*SegmentPlan, error) {
	row, err := segmentOutputRow(opts)
	if err != nil {
		return nil, err
	}
	opts = segmentBitDepth(opts, track.Fmt)
	plan, err := e.PlanTranscode(track, opts)
	if err != nil {
		return nil, err
	}
	if plan.FrameSize <= 0 {
		return nil, waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("waxflow: %s registered an HLS form without a native frame size", opts.Format))
	}
	segSamples, err := snapSegmentSamples(segSeconds, plan.Format.Rate, plan.FrameSize)
	if err != nil {
		return nil, err
	}
	// The edit list discards all of a segment no longer than the encoder
	// delay, so segment 0 would present nothing; refused while segSeconds can
	// still be corrected.
	if row.hls.delay > 0 && int64(segSamples) <= row.hls.delay {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: segment duration %g s is %d samples, no longer than %s's %d-sample encoder priming; "+
				"the first segment would present nothing",
				segSeconds, segSamples, opts.Format, row.hls.delay))
	}
	codecs := row.hls.codecs
	if row.hls.codecsFor != nil {
		codecs = row.hls.codecsFor(opts)
	}
	sp := &SegmentPlan{
		TranscodePlan:      *plan,
		SegmentSamples:     segSamples,
		Delay:              row.hls.delay,
		Codecs:             codecs,
		TotalDecodeSamples: -1,
		Segments:           -1,
	}
	// The plan cache shares its Versions slice across callers; the
	// segmenter revision is appended to a copy.
	sp.Versions = append(append([]string{}, plan.Versions...), mp4.SegmenterVersion)

	if plan.Samples >= 0 {
		sp.TotalDecodeSamples = totalDecodeSamples(plan.Samples, row.hls.delay, int64(plan.FrameSize))
		sp.Segments = (sp.TotalDecodeSamples + int64(segSamples) - 1) / int64(segSamples)
	}

	// A plan with no bit rate (compressed lossless, unconstrained VBR) is
	// bounded by its PCM wire rate. The overhead is the 8-byte trun row per
	// packet plus a little for the fixed boxes.
	base := plan.BitRate
	if base == 0 {
		base = plan.Format.Rate * plan.Format.Channels * max(plan.Format.BitDepth, 16)
	}
	sp.Bandwidth = base + plan.Format.Rate/plan.FrameSize*64 + 2000
	return sp, nil
}

// PlanSegmentsTimeline plans the segmented form of a concatenated timeline
// from its members' tracks alone (no open, no decode): PlanSegments over the
// synthetic track ConcatTrack computes, with the versions that track cannot
// name prepended. opts and segSeconds are as for PlanSegments. copts must be
// the options the matching Concat is built with, every field, or the plan
// and the run describe different audio (see ConcatOptions).
func (e *Engine) PlanSegmentsTimeline(tracks []container.Track, copts ConcatOptions,
	opts TranscodeOptions, segSeconds float64) (*SegmentPlan, error) {
	env, err := ConcatTrack(tracks, copts)
	if err != nil {
		return nil, err
	}
	plan, err := e.PlanSegments(env, opts, segSeconds)
	if err != nil {
		return nil, err
	}
	if err := finishTimelinePlan(tracks, env.Fmt, copts, &plan.TranscodePlan); err != nil {
		return nil, err
	}
	return plan, nil
}

// finishTimelinePlan is the tail both timeline plans share, so they refuse and
// key alike: plan is PlanTranscode's or PlanSegments' over the synthetic track
// ConcatTrack built from tracks and copts, env its format. It refuses what a
// run would refuse under the width rule (ADR-0010), on the count the row
// delivers, naming the remedy; then it prepends timelineVersions. Both plans
// own their Versions slice, never the plan cache's, so prepending is safe.
func finishTimelinePlan(tracks []container.Track, env audio.Format, copts ConcatOptions, plan *TranscodePlan) error {
	if mixedWidthConversion(tracks, env, plan.Format.Channels) {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
			"waxflow: this timeline's members were conformed to %d channels from other widths, so delivering it "+
				"as %d channels is not any member's own conversion; build the timeline at the delivered width "+
				"instead: ConcatOptions.Channels, see Engine.TimelineChannels",
			env.Channels, plan.Format.Channels))
	}
	extra, err := timelineVersions(tracks, env, copts)
	if err != nil {
		return err
	}
	plan.Versions = append(extra, plan.Versions...)
	return nil
}

// timelineVersions lists the cache-key terms a timeline's synthetic PCM track
// cannot name (ADR-0009): the crossfade and width terms, every member's
// demuxer and decoder revision, and the revisions of the chain each member
// format normalizes to the envelope through. The caller prepends them to the
// plan's own, and they are deduplicated, so a long timeline keys on a handful
// of entries.
func timelineVersions(tracks []container.Track, env audio.Format, copts ConcatOptions) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(v string) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	// Both terms only when nonzero, so a timeline that sets neither keys
	// exactly as one without them.
	if copts.Crossfade != 0 {
		add(crossfadeVersion(copts.Crossfade))
	}
	if copts.Channels != 0 {
		add(timelineWidthVersion(copts.Channels))
	}
	normalized := map[audio.Format]bool{}
	for i, t := range tracks {
		if t.DemuxerVersion != "" {
			add(t.DemuxerVersion)
		}
		add(decodeVersion(t.Codec))
		if t.Fmt == env || normalized[t.Fmt] {
			continue
		}
		normalized[t.Fmt] = true
		// Build the chain the run will build and read its versions off, as
		// buildPlanCore does, so the two cannot describe different processing.
		chain, err := dsp.NewChain(dsp.NewSource(eofReader{}, t.Fmt), concatSpec(env, copts))
		if err != nil {
			// The first member of this format; its index is what a caller
			// can act on.
			return nil, waxerr.Annotate(fmt.Sprintf("member %d", i), err)
		}
		for _, v := range chain.Versions() {
			add(v)
		}
		chain.Release()
	}
	return out, nil
}

// crossfadeVersion is a blended timeline's cache-key term. It names the blend
// length too, since blends of different lengths are different audio out of
// the same code. No comma, so strings.Join(versions, ",") stays unambiguous.
func crossfadeVersion(x int64) string { return fmt.Sprintf("xfade-%d-1", x) }

// timelineWidthVersion is a width-pinned timeline's cache-key term, naming
// the width for crossfadeVersion's reason: each member is folded or placed to
// that width before the seam. No comma, so strings.Join stays unambiguous.
func timelineWidthVersion(ch int) string { return fmt.Sprintf("tlwidth-%d-1", ch) }

// primeStarts returns the priming starts on the output timeline of a run
// whose first kept sample is p0: pChain, the first sample fed to the chain,
// and pEnc, the first fed to the encoder. The encoder's window is
// primeSeconds and stops at 0; the chain's adds horizon in front of it,
// uncapped, and stops at floor (0 for a file, negative for a span, see
// format.Headroomer). Given a p0 of whole frames, both are whole frames, so
// the packet discard is exact; a run from the top of a file primes neither.
// See ADR-0014.
func primeStarts(p0 int64, rate, frame int, horizon time.Duration, floor int64) (pChain, pEnc int64) {
	roundUp := func(n int64) int64 { return (n + int64(frame) - 1) / int64(frame) * int64(frame) }
	encPrime := roundUp(int64(float64(rate) * primeSeconds))
	// The horizon assumes the encoder's window settled the finite-memory
	// nodes, so the two add; no horizon primes the encoder's window alone.
	chainPrime := encPrime + roundUp(int64(horizon.Seconds()*float64(rate)))
	// Toward zero to a whole frame, so every start stays a frame multiple.
	floor = floor / int64(frame) * int64(frame)
	return max(floor, p0-chainPrime), max(0, p0-encPrime)
}

// headroomFloor is the lowest output position the chain may be fed from: 0
// for an ordinary media, negative for one with real audio ahead of its
// sample 0 (format.Headroomer). It holds back headroomMargin source samples,
// since the caller maps its start back to the source with a floor and one
// more sample: overstating the headroom fails the seek, understating it
// costs a sample of priming.
func headroomFloor(med format.Media, chain *dsp.Chain) int64 {
	room := format.MediaHeadroom(med) - headroomMargin
	if room <= 0 {
		return 0
	}
	l, m := chain.Ratio()
	return -(room * int64(l) / int64(m))
}

// headroomMargin is the source samples headroomFloor holds back to cover
// the floor-and-back-off the output-to-source map applies.
const headroomMargin = 2

// floorDiv is floor(a/b) for a positive b and any a. Go's division
// truncates toward zero, which takes a negative position (a span priming
// ahead of its sample 0) past a target the map must land at or before.
func floorDiv(a, b int64) int64 {
	q, r := a/b, a%b
	if r < 0 {
		q--
	}
	return q
}

// totalDecodeSamples is the whole stream's decode duration for a trimmed
// output length: a delayed encoder (Opus) flushes whole frames until the
// output covers samples+delay; the others emit exactly the input, the
// last packet short.
func totalDecodeSamples(samples, delay, frame int64) int64 {
	if delay == 0 {
		return samples
	}
	return (samples + delay + frame - 1) / frame * frame
}

// SegmentedFormats lists the output formats with a segmented (HLS) form,
// in table order. Remux-only rows are omitted: their hls column serves only
// the segmented remux of their sources.
func SegmentedFormats() []string {
	var names []string
	for _, o := range outputs {
		if o.hls != nil && !o.remuxOnly {
			names = append(names, o.name)
		}
	}
	return names
}

// InitSegment builds the CMAF init header for a planned segmented
// transcode: the ftyp+moov all the plan's media segments share. opts must
// be the options the plan was computed from. Deterministic: regenerating
// after cache eviction yields identical bytes.
func (e *Engine) InitSegment(plan *SegmentPlan, opts TranscodeOptions) ([]byte, error) {
	row, err := segmentOutputRow(opts)
	if err != nil {
		return nil, err
	}
	enc, err := row.hls.encode(plan.Format, opts, 0)
	if err != nil {
		return nil, err
	}
	return mp4.InitSegment(container.Track{
		Codec:       row.codecID,
		CodecConfig: enc.CodecConfig(),
		Fmt:         plan.Format,
		Samples:     plan.Samples,
		Delay:       plan.Delay,
		Default:     true,
	})
}

// SegmentedOptions selects which slice of the segment sequence a
// TranscodeSegments run produces.
type SegmentedOptions struct {
	// SegmentSamples is the segment length in output samples, from the
	// plan (it must be a positive multiple of the encoder frame).
	SegmentSamples int
	// StartSegment is the first segment to emit; the run continues from
	// there to the end of the stream. Zero encodes the whole sequence.
	StartSegment int64
}

// SegmentedResult reports what a TranscodeSegments run produced.
type SegmentedResult struct {
	// Samples is the stream's decode-timeline length as this run measured
	// it: the priming start position plus everything the encoder consumed
	// and flushed.
	Samples int64
	// Segments is the number of segments this run emitted.
	Segments int64
	// ClippedSamples counts samples the integer output could not carry (see
	// TranscodeResult.ClippedSamples), over everything fed to the chain,
	// priming included: summed over a variant's workers, overlaps count twice.
	ClippedSamples int64
	// TruePeak is the output's true-peak level, linear, 1.0 = full scale
	// (see TranscodeResult.TruePeak). It spans the priming too, which is
	// harmless since workers' peaks combine by max.
	TruePeak float64
}

// TranscodeSegments decodes src and emits numbered CMAF media segments in
// order from StartSegment: the variant-worker back end of HLS delivery. A
// run from StartSegment > 0 seeks the source sample-exact and primes the
// chain and the encoder (ADR-0014), discarding the priming packets on a
// frame boundary, so the kept packets sit where a continuous run puts them.
// ctx is checked between chunks.
func (e *Engine) TranscodeSegments(ctx context.Context, src container.Source, hint string, opts TranscodeOptions,
	segOpts SegmentedOptions, emit func(mp4.Segment) error) (*SegmentedResult, error) {
	// Refused on the options before anything opens, with the same error
	// TranscodeSegmentsMedia would give.
	if _, err := segmentOutputRow(opts); err != nil {
		return nil, err
	}
	med, err := e.OpenStream(src, hint)
	if err != nil {
		return nil, err
	}
	defer med.Close()
	return e.TranscodeSegmentsMedia(ctx, med, opts, segOpts, emit)
}

// segmentOutputRow validates the segmented options that do not depend on the
// source and returns the output row they select. It is a pure function of opts,
// so the segmented plan, InitSegment and both runs call it and refuse alike.
func segmentOutputRow(opts TranscodeOptions) (*output, error) {
	if err := checkSegmented(opts); err != nil {
		return nil, err
	}
	if opts.FromSample != 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: segmented transcodes have no FromSample; segments address time")
	}
	row, err := outputRow(opts.Format)
	if err != nil {
		return nil, err
	}
	if row.hls == nil {
		return nil, noSegmentedForm(opts.Format)
	}
	return row, nil
}

// checkSegmented runs Check and refuses a Container: the segmenter writes
// fragmented MP4 whatever the options name.
func checkSegmented(opts TranscodeOptions) error {
	if err := opts.Check(); err != nil {
		return err
	}
	if opts.Container != "" {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
			"waxflow: segmented output is always fragmented MP4 and takes no Container (got %q)", opts.Container))
	}
	return nil
}

// noSegmentedForm refuses a format with no HLS form, naming those with one.
func noSegmentedForm(format string) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat,
		fmt.Sprintf("waxflow: %s has no segmented (HLS) form (available: %s)", format, strings.Join(SegmentedFormats(), ", ")))
}

// TranscodeSegmentsMedia emits numbered CMAF media segments from an
// already-opened Media: the seam TranscodeSegments is built on, for inputs
// that are not a single sniffable Source (a concatenated album timeline).
// The caller owns med and closes it.
func (e *Engine) TranscodeSegmentsMedia(ctx context.Context, med format.Media, opts TranscodeOptions,
	segOpts SegmentedOptions, emit func(mp4.Segment) error) (*SegmentedResult, error) {
	row, err := segmentOutputRow(opts)
	if err != nil {
		return nil, err
	}
	srcTrack := med.Info().Default()
	// A declared length that is only a bound is confirmed first, as
	// TranscodeMedia confirms it, so the decode stops where the plan's did.
	if unconfirmedBound(srcTrack) {
		if confirm := confirmableLength(srcTrack, med); confirm != nil {
			if err := confirm(ctx); err != nil {
				return nil, err
			}
			srcTrack = med.Info().Default()
		}
	}
	// PlanSegments' rule off the same format (a timeline reports the envelope
	// track), so the run encodes the depth the plan and init header promised.
	opts = segmentBitDepth(opts, srcTrack.Fmt)

	spec := specFor(opts)
	if row.adjust != nil {
		row.adjust(&spec, srcTrack.Fmt, opts)
	}
	if err := refuseMixedWidthConversion(med, srcTrack.Fmt, spec.Channels); err != nil {
		return nil, err
	}
	// A run meters its output, exactly as TranscodeMedia does.
	spec.MeterTruePeak = true
	frame := spec.FrameSize
	if frame <= 0 {
		return nil, waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("waxflow: %s registered an HLS form without a native frame size", opts.Format))
	}
	// The run frames the encoder's chunks itself (the priming discard
	// shifts chunk boundaries), so the chain framer would only add a copy.
	spec.FrameSize = 0
	switch {
	case segOpts.SegmentSamples <= 0 || segOpts.SegmentSamples%frame != 0:
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: segment length %d is not a positive multiple of the %d-sample frame", segOpts.SegmentSamples, frame))
	case segOpts.StartSegment < 0:
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: negative StartSegment")
	case segOpts.StartSegment > (1<<62)/int64(segOpts.SegmentSamples):
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: StartSegment overflows the sample timeline")
	}

	chain, err := dsp.NewChain(dsp.NewSource(med, srcTrack.Fmt), spec)
	if err != nil {
		return nil, err
	}
	defer chain.Release()
	f := chain.Format()
	// The same adjust hook as a progressive transcode, so the same log of a
	// fold nobody asked for.
	e.logImplicitDownmix(opts, srcTrack.Fmt, f)
	e.logTimelineDownmix(opts, med, srcTrack.Fmt)

	// Output-timeline positions: p0 is the first kept sample, pChain the
	// first fed to the chain, pEnc the first fed to the encoder. A span's
	// floor is negative, so it primes even its segment 0.
	p0 := segOpts.StartSegment * int64(segOpts.SegmentSamples)
	floor := headroomFloor(med, chain)
	pChain, pEnc := primeStarts(p0, f.Rate, frame, chain.Horizon(), floor)

	if pChain != 0 {
		// To the source timeline, at or before pChain: floor, then one more
		// sample back. The first chunk's Pos places the excess, dropped below.
		srcPos := pChain
		if l, m := chain.Ratio(); l != m {
			srcPos = floorDiv(pChain*int64(m), int64(l)) - 1
		}
		if _, err := med.SeekSample(ctx, srcPos); err != nil {
			return nil, err
		}
	}

	enc, err := row.hls.encode(f, opts, pEnc)
	if err != nil {
		return nil, err
	}
	seg, err := mp4.NewSegmenter(container.Track{
		Codec:       row.codecID,
		CodecConfig: enc.CodecConfig(),
		Fmt:         f,
		Samples:     -1,
		Delay:       row.hls.delay,
		Default:     true,
	}, &mp4.SegmenterOptions{SegmentSamples: segOpts.SegmentSamples, StartSegment: segOpts.StartSegment})
	if err != nil {
		return nil, err
	}

	res := &SegmentedResult{}
	emitSeg := func(s mp4.Segment) error {
		res.Segments++
		return emit(s)
	}
	// Packets count off from pEnc, one per frame; the chain's longer
	// pre-roll never reaches the encoder.
	discard := (p0 - pEnc) / int64(frame)
	pkts := int64(0)
	emitPkt := func(p codec.Packet) error {
		pkts++
		if pkts <= discard {
			return nil
		}
		return seg.WritePacket(p, emitSeg)
	}

	e.log.Debug("segmented transcode started",
		"container", med.Info().Container, "source", srcTrack.Fmt.String(), "format", f.String(),
		"out", opts.Format, "segment", segOpts.StartSegment, "segSamples", segOpts.SegmentSamples,
		"dsp", strings.Join(chain.Versions(), ","))

	// stage re-frames the chain's chunks to exact encoder frames: the
	// priming discard starts mid-chunk, and the frame-native encoders
	// accept a short chunk only as the stream's last.
	buf := audio.Get(f, audio.StandardChunk)
	defer audio.Put(buf)
	stage := audio.Get(f, frame)
	defer audio.Put(stage)

	// Output samples dropped ahead of pEnc, set by the first chunk's Pos:
	// the seek's slop before pChain and the chain's pre-roll up to pEnc.
	skip := int64(-1)
	for {
		if err := ctx.Err(); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeCanceled, "segmented transcode canceled", err)
		}
		err := chain.ReadChunk(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if skip < 0 {
			skip = pEnc - buf.Pos
			if skip < 0 {
				return nil, waxerr.New(waxerr.CodeInternal,
					fmt.Sprintf("waxflow: chain landed at %d, past the priming start %d", buf.Pos, pEnc))
			}
		}
		from := 0
		if skip > 0 {
			n := min(skip, int64(buf.N))
			skip -= n
			from = int(n)
		}
		if err := stageFrames(stage, buf, from, frame, enc, emitPkt); err != nil {
			return nil, err
		}
	}
	if stage.N > 0 {
		if err := enc.Encode(stage, emitPkt); err != nil {
			return nil, err
		}
		stage.N = 0
	}
	trailer, err := enc.Finish(emitPkt)
	if err != nil {
		return nil, err
	}
	if err := seg.End(emitSeg); err != nil {
		return nil, err
	}
	// The encoder is the authority on the length, so it counts from pEnc.
	res.Samples = pEnc + trailer.Samples
	res.ClippedSamples = chain.Clipped()
	res.TruePeak = chain.TruePeak()
	e.log.Debug("segmented transcode finished", "samples", res.Samples, "segments", res.Segments,
		"clipped", res.ClippedSamples, "truePeak", res.TruePeak)
	return res, nil
}

// stageFrames copies src[from:] into the frame-sized stage buffer,
// encoding each frame as it fills. A partial frame stays staged for the
// next chunk (or the final short Encode at end of stream).
func stageFrames(stage, src *audio.Buffer, from, frame int, enc codec.Encoder, emit func(codec.Packet) error) error {
	for from < src.N {
		n := min(frame-stage.N, src.N-from)
		for c := 0; c < stage.Fmt.Channels; c++ {
			if stage.I != nil {
				copy(stage.I[c*stage.Stride+stage.N:c*stage.Stride+stage.N+n], src.I[c*src.Stride+from:c*src.Stride+from+n])
			} else {
				copy(stage.F[c*stage.Stride+stage.N:c*stage.Stride+stage.N+n], src.F[c*src.Stride+from:c*src.Stride+from+n])
			}
		}
		if stage.N == 0 {
			stage.Pos = src.Pos + int64(from)
		}
		stage.N += n
		from += n
		if stage.N == frame {
			if err := enc.Encode(stage, emit); err != nil {
				return err
			}
			stage.N = 0
		}
	}
	return nil
}
