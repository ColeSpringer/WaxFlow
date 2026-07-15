package waxflow

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/dsp"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/waxerr"
)

// DefaultSegmentSeconds is the HLS target segment duration when a request
// names none, per the Apple authoring guidance for low-latency-enough
// audio streaming without playlist bloat.
const DefaultSegmentSeconds = 4.0

// maxSegmentSeconds bounds requested segment durations: beyond it the
// segment-sample arithmetic still holds but the delivery would be
// degenerate (one segment per track defeats HLS).
const maxSegmentSeconds = 60.0

// primeSeconds is how much pre-target audio a mid-stream segmented run
// feeds the *encoder* before its first kept sample, rounded up to whole
// encoder frames: enough to settle the encoder's cross-frame state
// (psychoacoustic and prefilter history) and the resampler's window so
// segment n from a restarted worker joins segment n-1 from a continuous one
// without an audible seam. The priming packets are discarded on an exact
// frame boundary, so the kept stream's decode timeline is unshifted.
//
// It is also the floor on the *chain's* pre-roll, which is a longer window
// whenever the chain holds a node whose state decays rather than ends; see
// dsp.Settler and primeStarts. The two were one constant only because
// nothing yet needed them to differ.
const primeSeconds = 0.1

// SegmentPlan describes the segmented CMAF (HLS) form of a transcode,
// computed from headers alone like TranscodePlan (which it embeds: the
// embedded Versions already carry the segmenter revision, so an HLS cache
// key derives from it directly).
type SegmentPlan struct {
	TranscodePlan
	// SegmentSamples is the decode duration of every segment but the last,
	// in output samples: the requested duration snapped to a whole number
	// of encoder frames so boundaries land exactly between packets.
	SegmentSamples int
	// Delay is the encoder delay the init segment's edit list carries.
	Delay int64
	// Codecs is the RFC 6381 CODECS attribute for master playlists.
	Codecs string
	// Bandwidth is a peak-bit-rate bound for master playlists: the exact
	// rate for CBR encoders, the PCM wire rate for VBR lossless (whose
	// compressed peak can only be below it), plus segmentation overhead.
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

// PlanSegments plans the segmented form of a transcode of track. opts is
// the per-variant output selection (FromSample must be zero: segments own
// the timeline); segSeconds is the target segment duration, 0 for the
// default. A plan that succeeds guarantees TranscodeSegments and
// InitSegment accept the same options.
func (e *Engine) PlanSegments(track container.Track, opts TranscodeOptions, segSeconds float64) (*SegmentPlan, error) {
	if opts.FromSample != 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: segmented transcodes have no FromSample; segments address time")
	}
	row, err := outputRow(opts.Format)
	if err != nil {
		return nil, err
	}
	if row.hls == nil {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("waxflow: %s has no segmented (HLS) form (available: %s)", opts.Format, strings.Join(SegmentedFormats(), ", ")))
	}
	switch {
	case segSeconds == 0:
		segSeconds = DefaultSegmentSeconds
	case segSeconds < 0 || segSeconds > maxSegmentSeconds:
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: segment duration %g outside 0..%g seconds", segSeconds, maxSegmentSeconds))
	}
	plan, err := e.PlanTranscode(track, opts)
	if err != nil {
		return nil, err
	}
	f := plan.FrameSize
	if f <= 0 {
		return nil, waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("waxflow: %s registered an HLS form without a native frame size", opts.Format))
	}
	// Snap the target duration to whole encoder frames, at least one.
	segSamples := int(segSeconds*float64(plan.Format.Rate)/float64(f)+0.5) * f
	if segSamples < f {
		segSamples = f
	}

	sp := &SegmentPlan{
		TranscodePlan:      *plan,
		SegmentSamples:     segSamples,
		Delay:              row.hls.delay,
		Codecs:             row.hls.codecs,
		TotalDecodeSamples: -1,
		Segments:           -1,
	}
	// The plan cache shares its Versions slice across callers; the
	// segmenter revision is appended to a copy.
	sp.Versions = append(append([]string{}, plan.Versions...), mp4.SegmenterVersion)

	if plan.Samples >= 0 {
		sp.TotalDecodeSamples = totalDecodeSamples(plan.Samples, row.hls.delay, int64(f))
		sp.Segments = (sp.TotalDecodeSamples + int64(segSamples) - 1) / int64(segSamples)
	}

	// Peak bandwidth: CBR encoders report exactly; VBR lossless is bounded
	// by its PCM wire rate. Segmentation overhead is dominated by the
	// 8-byte trun row per packet, padded a little for the fixed boxes.
	base := plan.BitRate
	if base == 0 {
		base = plan.Format.Rate * plan.Format.Channels * max(plan.Format.BitDepth, 16)
	}
	sp.Bandwidth = base + plan.Format.Rate/f*64 + 2000
	return sp, nil
}

// PlanSegmentsTimeline plans the segmented form of a concatenated timeline
// from its members' tracks alone (no decode, no open), exactly as
// PlanSegments does for one track: it is the same plan, over the synthetic
// track ConcatTrack computes, with the versions the synthetic track cannot
// name prepended.
//
// opts and segSeconds mean what they mean for PlanSegments, and
// opts.ResampleProfile must be the profile the matching Concat is built with
// (see ConcatOptions.Profile).
func (e *Engine) PlanSegmentsTimeline(tracks []container.Track, opts TranscodeOptions, segSeconds float64) (*SegmentPlan, error) {
	env, err := ConcatTrack(tracks)
	if err != nil {
		return nil, err
	}
	plan, err := e.PlanSegments(env, opts, segSeconds)
	if err != nil {
		return nil, err
	}
	extra, err := timelineVersions(tracks, env.Fmt, opts.ResampleProfile)
	if err != nil {
		return nil, err
	}
	// PlanSegments already returned a Versions slice of its own, so
	// prepending cannot reach the plan cache's shared one.
	plan.Versions = append(extra, plan.Versions...)
	return plan, nil
}

// timelineVersions lists what a timeline's synthetic track cannot: every
// member's decoder revision, and the revisions of the nodes each member's
// own normalization to the envelope runs through.
//
// Both are silent under-keying if left out, which ADR-0004 exists to
// prevent. The synthetic track's codec is PCM, so the plan folds in pcm's
// version and names no member's decoder, and a FLAC decoder fix would leave
// every album's cached segments stale. The plan's chain runs envelope to
// output, so it names none of the resampling a member does to reach the
// envelope: a 44.1 kHz member of a 48 kHz timeline delivered to a 48 kHz
// output resamples through a profile the key never mentions.
//
// The entries are prepended rather than replacing the synthetic pcm entry.
// Replacing would mean knowing which entry the plan's decode version is,
// which is fragile in exactly the way this is not; over-keying is safe, and
// under-keying is the sin. Both lists are deduplicated (there are about
// seven decoders, and a queue holds few distinct formats), so a
// thousand-member timeline still keys on a handful of entries.
func timelineVersions(tracks []container.Track, env audio.Format, profile resample.Profile) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(v string) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	normalized := map[audio.Format]bool{}
	for _, t := range tracks {
		add(decodeVersion(t.Codec))
		if t.Fmt == env || normalized[t.Fmt] {
			continue
		}
		normalized[t.Fmt] = true
		// The same throwaway-chain trick buildPlanCore uses: build what the
		// run will build, read its versions off, and release it, so the two
		// cannot describe different processing.
		chain, err := dsp.NewChain(dsp.NewSource(eofReader{}, t.Fmt), concatSpec(env, ConcatOptions{Profile: profile}))
		if err != nil {
			return nil, err
		}
		for _, v := range chain.Versions() {
			add(v)
		}
		chain.Release()
	}
	return out, nil
}

// primeStarts computes a run's two priming starts on the output timeline:
// pChain is the first sample fed to the DSP chain, pEnc the first fed to the
// encoder, both for a run whose first kept sample is p0.
//
// A run from the top of a media that has nothing ahead of its sample 0 primes
// neither, and that falls out of the clamps rather than being tested for:
// both windows reach back from p0, and at the top of such a stream there is
// nothing behind p0 for them to reach into.
//
// The two windows are split because they settle different things at very
// different costs. The encoder needs primeSeconds to settle its own
// cross-frame state, and priming is discarded *after* the encoder, so every
// primed sample costs a decode plus DSP plus an encode. A chain holding a
// node whose state decays exponentially needs far longer (10 s for a voice
// compressor against the encoder's 0.1 s), and feeding that through the
// encoder would cost roughly 170 ms of CPU per worker restart instead of
// ~2 ms. Feeding the long window to the chain alone and the short one to
// the encoder keeps the expensive half at its old cost, which is what makes
// the horizon affordable rather than an optimization held in reserve.
//
// Capping the long window is never an option: a short pre-roll is precisely
// the bug the horizon exists to fix, and a restarted worker would resume
// emitting segments that are not the ones a continuous run produces.
//
// Both starts round up to whole encoder frames and clamp at the stream top,
// so the packet discard below stays exact.
//
// floor is the lowest position the media can deliver, which is 0 for a file
// and negative for a span: see Headroomer. It bounds the chain's window
// only, and the asymmetry is the point rather than an oversight. Feeding
// the chain from before a span's sample 0 is what makes that sample hold
// the audio a continuous run of the whole source delivers there, since the
// resampler's window is then full of real samples instead of zeros. The
// encoder needs no such thing: a span is served as its own stream, with its
// own init segment, so its sample 0 genuinely is where its encoder starts,
// exactly as a whole track's is. Priming the encoder from a negative
// position would also number its first frame below zero, which is a thing
// no encoder here can express.
func primeStarts(p0 int64, rate, frame int, horizon time.Duration, floor int64) (pChain, pEnc int64) {
	roundUp := func(n int64) int64 { return (n + int64(frame) - 1) / int64(frame) * int64(frame) }
	encPrime := roundUp(int64(float64(rate) * primeSeconds))
	// The chain's window sits in front of the encoder's, it does not replace
	// it: the encoder's covers the finite-memory nodes (the resampler's FIR
	// window) and the encoder's own cross-frame state, and the chain's
	// horizon assumes those already converged. So they add. A chain that
	// declares no horizon (no gain, no dynamics: every plain FLAC or
	// untouched Opus stream) adds zero and primes exactly as it always did.
	chainPrime := encPrime + roundUp(int64(horizon.Seconds()*float64(rate)))
	// The floor rounds toward zero to a whole frame, keeping every start a
	// frame multiple so the packet discard stays exact.
	floor = floor / int64(frame) * int64(frame)
	return max(floor, p0-chainPrime), max(0, p0-encPrime)
}

// headroomFloor is the lowest output position the chain may be fed from: 0
// for an ordinary media, negative for one that has real audio ahead of its
// sample 0. See Headroomer.
//
// It converts the media's headroom from source samples to output samples,
// keeping a small margin at the far end, because the caller maps the
// position it picks back to the source with a floor and one further sample
// of slop. Without the margin, a run priming to the very edge of the
// headroom would ask the media for a sample just before its source's start
// and fail the seek. Understating the headroom costs a sample of priming;
// overstating it costs the request.
func headroomFloor(med format.Media, chain *dsp.Chain) int64 {
	h, ok := med.(Headroomer)
	if !ok {
		return 0
	}
	room := h.Headroom() - headroomMargin
	if room <= 0 {
		return 0
	}
	l, m := chain.Ratio()
	return -(room * int64(l) / int64(m))
}

// headroomMargin is the source samples headroomFloor holds back to cover
// the floor-and-back-off the output-to-source map applies.
const headroomMargin = 2

// floorDiv is floor(a/b) for a positive b, over the whole range of a.
//
// Go's integer division truncates toward zero, which rounds a negative
// position up, and up is the wrong way for a map that must land at or
// before its target. A span priming ahead of its own sample 0 is the case
// where a is negative.
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
// in table order.
func SegmentedFormats() []string {
	var names []string
	for _, o := range outputs {
		if o.hls != nil {
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
	row, err := outputRow(opts.Format)
	if err != nil {
		return nil, err
	}
	if row.hls == nil {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("waxflow: %s has no segmented (HLS) form", opts.Format))
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
}

// TranscodeSegments decodes src and emits numbered CMAF media segments:
// the variant-worker back end of HLS delivery. A run starting mid-stream
// (StartSegment > 0) seeks the source sample-exact and primes both sides:
// the decode chain warms its resampler history and the encoder settles
// its cross-frame state on ~100 ms of pre-target audio, whose packets are
// discarded on an exact frame boundary, so the kept packets sit at the
// same decode positions a continuous run would put them. Segments arrive
// in order starting at StartSegment; ctx is checked between chunks.
func (e *Engine) TranscodeSegments(ctx context.Context, src container.Source, hint string, opts TranscodeOptions,
	segOpts SegmentedOptions, emit func(mp4.Segment) error) (*SegmentedResult, error) {
	// Reject on the options alone before opening anything. TranscodeSegmentsMedia
	// checks the same things, so the two entry points still fail identically;
	// this only keeps a request that was never going to run from paying for a
	// container open first, and keeps the error about the option that is wrong
	// rather than about a source we had no reason to touch.
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
// so both segmented entry points can call it and agree.
func segmentOutputRow(opts TranscodeOptions) (*output, error) {
	if opts.FromSample != 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: segmented transcodes have no FromSample")
	}
	row, err := outputRow(opts.Format)
	if err != nil {
		return nil, err
	}
	if row.hls == nil {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("waxflow: %s has no segmented (HLS) form", opts.Format))
	}
	return row, nil
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

	spec := specFor(opts)
	if row.adjust != nil {
		row.adjust(&spec, srcTrack.Fmt, opts)
	}
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

	// Output-timeline positions: p0 is the first kept sample, pChain the
	// first fed to the chain, pEnc the first fed to the encoder.
	//
	// A span primes its own segment 0, which a file cannot: headroomFloor is
	// negative exactly when the media has real audio ahead of its sample 0,
	// so the run has something to prime with even at the top of the stream,
	// because for a span there genuinely is.
	p0 := segOpts.StartSegment * int64(segOpts.SegmentSamples)
	floor := headroomFloor(med, chain)
	pChain, pEnc := primeStarts(p0, f.Rate, frame, chain.Horizon(), floor)

	if pChain != 0 {
		// Map the output position back to the source timeline. Rounding
		// must land at or before pChain, so the conversion floors and then
		// backs off one more source sample; the remainder is discarded from
		// the chain's output below, positioned by the first chunk's Pos.
		srcPos := pChain
		if l, m := chain.Ratio(); l != m {
			srcPos = floorDiv(pChain*int64(m), int64(l)) - 1
		}
		if _, err := med.SeekSample(srcPos); err != nil {
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
	// Packets count off from pEnc, one per frame (the priming feed is
	// frame-aligned, so the discard boundary is exact). The chain's own
	// longer pre-roll never reaches the encoder, so it is not counted here.
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

	// Output samples to drop before the encoder's first fed sample, set by
	// the first chunk's Pos. It spans two things at once: the slop between
	// where the seek landed and pChain, and the chain's own pre-roll from
	// pChain to pEnc, which has done its work by passing through the chain
	// and must not reach the encoder.
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
	// The encoder is the authority on what was produced, so the run's
	// length counts from where the encoder started, not from the chain's
	// earlier pre-roll start.
	res.Samples = pEnc + trailer.Samples
	e.log.Debug("segmented transcode finished", "samples", res.Samples, "segments", res.Segments)
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
