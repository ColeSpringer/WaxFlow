package waxflow

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/dsp"
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
// feeds before its first kept sample, rounded up to whole encoder frames:
// enough to settle the encoder's cross-frame state (psychoacoustic and
// prefilter history) and the resampler's window so segment n from a
// restarted worker joins segment n-1 from a continuous one without an
// audible seam. The priming packets are discarded on an exact frame
// boundary, so the kept stream's decode timeline is unshifted.
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

	// Output-timeline positions: p0 is the first kept sample, pStart the
	// first fed one (priming rounds up to whole frames and clamps at the
	// stream top, so both stay frame multiples).
	p0 := segOpts.StartSegment * int64(segOpts.SegmentSamples)
	var pStart int64
	if segOpts.StartSegment > 0 {
		prime := (int64(float64(f.Rate)*primeSeconds) + int64(frame) - 1) / int64(frame) * int64(frame)
		pStart = max(0, p0-prime)
	}

	if pStart > 0 {
		// Map the output position back to the source timeline. Rounding
		// must land at or before pStart, so the floor conversion backs off
		// one more source sample; the remainder is discarded from the
		// chain's output below, positioned by the first chunk's Pos.
		srcPos := pStart
		if l, m := chain.Ratio(); l != m {
			srcPos = pStart * int64(m) / int64(l)
			if srcPos > 0 {
				srcPos--
			}
		}
		if _, err := med.SeekSample(srcPos); err != nil {
			return nil, err
		}
	}

	enc, err := row.hls.encode(f, opts, pStart)
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
	// Packets count off from pStart, one per frame (the priming feed is
	// frame-aligned, so the discard boundary is exact).
	discard := (p0 - pStart) / int64(frame)
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

	skip := int64(-1) // output samples to drop before pStart; set by the first chunk's Pos
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
			skip = pStart - buf.Pos
			if skip < 0 {
				return nil, waxerr.New(waxerr.CodeInternal,
					fmt.Sprintf("waxflow: chain landed at %d, past the priming start %d", buf.Pos, pStart))
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
	res.Samples = pStart + trailer.Samples
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
