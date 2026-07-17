package server

import (
	"context"
	"fmt"
	"net/url"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// streamRequest is the shared front half of /stream and /transcode: one
// parsed, resolved, identity-checked, probed request. The two handlers
// diverge only in their tails (cache/direct-play delivery vs ring-only
// one-shot), so the checks that must agree live here once.
type streamRequest struct {
	p     *streamParams
	src   *source.File
	info  *format.Info
	track container.Track
	// from is the seek start in source samples (t= converted at the
	// boundary).
	from int64
	// gainDB is the resolved gain, computed once: it feeds the DSP spec,
	// the canonical cache-key params, and the entry meta, which must all
	// agree byte-for-byte.
	gainDB float64
	// meta is the source's mapped metadata, nil without a mapper. It
	// resolves tag-based gain and supplies the live minimal tag set.
	meta *meta.Info

	// Set by planTranscode for transcode-shaped requests.
	opts waxflow.TranscodeOptions
	plan *waxflow.TranscodePlan
	// remux is the ladder's rung 2 when this request can be served by
	// rewriting the container around the source's own packets, nil when it
	// cannot and the request takes rung 3. plan points at its embedded
	// TranscodePlan when it is set, so everything downstream reads one shape.
	remux *waxflow.RemuxPlan
	// cut is the ladder's rung between transmux and transcode: a span served by
	// moving the source's own packets rather than re-encoding them. nil when the
	// request is not a span, its codec is off the cut allowlist, or the cut
	// otherwise declines. plan points at its embedded TranscodePlan when set,
	// exactly as remux does, so everything downstream reads one shape.
	cut *waxflow.CutPlan
	// cutGrid is the source's packet grid and cutSamples its exact measured
	// length, both captured when the cut is planned and threaded to the run so
	// the bytes delivered match the bytes keyed. cutGrid is 0 and cutSamples -1
	// when the cut rung was not taken. cutSamples matters for an undeclared-length
	// source (ADTS): the plan measured it, and the run's init segment must carry
	// the same length rather than the header's -1.
	cutGrid    int
	cutSamples int64
	canonical  string
}

// Close releases the request's source handle.
func (req *streamRequest) Close() error { return req.src.Close() }

// prepareSource runs the shared request front half. On success the caller
// owns req.Close.
func (s *Server) prepareSource(ctx context.Context, q url.Values, sigAuthed bool) (*streamRequest, error) {
	p, err := parseStreamParams(q, s.defaultGain)
	if err != nil {
		return nil, err
	}
	src, err := s.resolver.Resolve(ctx, p.src)
	if err != nil {
		return nil, err
	}
	if err := checkIdentity(p, sigAuthed, src); err != nil {
		src.Close()
		return nil, err
	}
	info, err := s.eng.Probe(src, src.Ext, nil)
	if err != nil {
		src.Close()
		return nil, err
	}
	track := info.Default()
	if p.track >= 0 && p.track != track.ID {
		src.Close()
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("track %d: only the default track (%d) is selectable until multi-track containers land", p.track, track.ID))
	}
	// Everything downstream plans from the spanned track, not the file's,
	// which is the whole of what makes a virtual track real: plan.Samples
	// must promise the span's length, or a bounded request advertises the
	// whole rip's duration and its segment count runs off the end. The
	// window is resolved through the same funnel Slice uses at open, so the
	// plan and the delivery cannot disagree about it.
	if p.span.narrowed() {
		// The window is checked against the total, so the total has to be one
		// that was measured: an under-declared source would refuse a span the
		// file can serve, and an over-declared one would admit a span that
		// dies part way through delivery. The measure is only on this branch,
		// since an ordinary stream depends on no number and must not pay for a
		// walk to be told it.
		if track, err = s.trackFor(src, true); err != nil {
			src.Close()
			return nil, err
		}
		if track, err = waxflow.SpanTrack(track, p.span.from, p.span.end()); err != nil {
			src.Close()
			return nil, err
		}
	}
	m := s.readMeta(ctx, src, false)
	return &streamRequest{
		p:     p,
		src:   src,
		info:  info,
		track: track,
		// The seek is inside the span: t= addresses the virtual track's own
		// timeline, because that is the stream the caller is playing.
		from:   int64(p.t * float64(track.Fmt.Rate)),
		gainDB: p.gain.resolveDB(m, p.dynamics),
		meta:   m,
		// -1, not the zero value: cutSamples threads to CutStream as "the source's
		// own header length" when no cut set it, and 0 would zero the run's track.
		cutSamples: -1,
	}, nil
}

// planTranscode runs the shared transcode-shaped back half: resolve
// format=auto, plan the pipeline, and enforce the streaming-form and
// bitrate-cap policies identically for /stream and /transcode.
func (s *Server) planTranscode(req *streamRequest) error {
	outFormat := req.p.format
	if outFormat == "auto" {
		outFormat = waxflow.DefaultLiveFormat()
	}
	// bitrate/q select a lossy output bit rate; a registered lossless format
	// has none to set, so the request is refused rather than silently
	// ignored. An unregistered format falls through to PlanTranscode's
	// unsupported-format error instead of being mislabeled "lossless".
	if req.p.bitrate != 0 {
		if lossy, known := waxflow.LossyFormat(outFormat); known && !lossy {
			return waxerr.New(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("bitrate/q apply to lossy output; %s is lossless", outFormat))
		}
	}
	req.opts = waxflow.TranscodeOptions{
		Format:          outFormat,
		Container:       req.p.container,
		Rate:            req.p.rate,
		Channels:        req.p.ch,
		BitDepth:        req.p.bits,
		GainDB:          req.gainDB,
		Dynamics:        req.p.dynamics,
		FromSample:      req.from,
		ResampleProfile: s.profile,
		MP3Bitrate:      req.p.bitrate * 1000,
		OpusBitrate:     req.p.bitrate * 1000,
		AACBitrate:      req.p.bitrate * 1000,
		// The live passthrough: the minimal descriptive set, embedded by
		// muxers with a stream-form tag representation.
		Tags: meta.MinimalTags(req.meta),
	}
	// Ladder rung 2: rewrite the container around the source's own packets.
	// Rung 1 (directPlayable) already declined, or we would not be here, so
	// this is the cheapest remaining answer whenever the codec survives. A span
	// declines rung 2 (it cuts mid-packet), so the cut rung sits below it: the
	// same packet-move answer, filtered to the span, for a source whose codec
	// survives being repositioned. Anything neither serves takes rung 3.
	if req.remux = s.remuxPlanFor(req); req.remux != nil {
		req.plan = &req.remux.TranscodePlan
	} else if req.cut = s.cutPlanFor(req); req.cut != nil {
		req.plan = &req.cut.TranscodePlan
	} else {
		plan, err := s.eng.PlanTranscode(req.track, req.opts)
		if err != nil {
			return err
		}
		req.plan = plan
	}
	plan := req.plan
	if !plan.Live {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("%s has no streaming form; request it as a job output once jobs land", plan.Container))
	}
	if kbit := req.p.maxBitRate; kbit > 0 {
		// A plan without a bit rate (VBR lossless: the output size is
		// signal-dependent) cannot promise any cap, so a cap on it is
		// refused rather than silently unenforced.
		if plan.BitRate == 0 {
			return waxerr.New(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("%s output has no fixed bit rate to hold under maxBitRate %d kbit/s", plan.Container, kbit))
		}
		if plan.BitRate > kbit*1000 {
			return waxerr.New(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("no available encoding satisfies maxBitRate %d kbit/s (projected %d kbit/s; /caps lists the encoders)",
					kbit, plan.BitRate/1000))
		}
	}
	req.canonical = canonicalParams(plan, req.gainDB, req.p.dynamics, req.p.span, req.from)
	if len(req.opts.Tags) > 0 {
		req.canonical += "&tags=" + tagsFingerprint(req.opts.Tags)
	}
	return nil
}

// remuxPlanFor returns the ladder's rung-2 plan for a prepared request, or nil
// when the request must take rung 3.
//
// The two declines here are the server's own, and neither is visible to
// PlanRemux: the engine is handed options, and these are facts about the
// request around them.
//
// A span cuts the stream at an arbitrary sample, which means cutting
// mid-packet, so a virtual track always decodes. Note this cannot be left to
// the FromSample check inside PlanRemux: the span is applied by wrapping the
// Media, never through the options, so PlanRemux would see a plain request and
// happily remux the whole file for someone who asked for one track of it. It is
// the same asymmetry that makes the span easy to miss in directPlayable, and it
// bites the same way.
//
// A maxBitRate cap needs a rate this rung cannot promise: the source's real bit
// rate is in its packets, not its headers, and a plan reads headers. Declining
// hands the request to the rung that can hold a cap honestly, rather than
// refusing a request a transcode would serve.
func (s *Server) remuxPlanFor(req *streamRequest) *waxflow.RemuxPlan {
	switch {
	case req.p.span.narrowed(), req.p.maxBitRate > 0:
		return nil
	}
	// An error here is not this rung's to report: it means the request is wrong
	// for every rung (an unsupported format, a container the format cannot
	// produce), and PlanTranscode is about to say so with the same words.
	rp, err := s.eng.PlanRemux(req.track, req.opts)
	if err != nil {
		return nil
	}
	return rp
}

// cutPlanFor returns the ladder's cut plan for a prepared request, or nil when
// the request must fall through to a transcode. It is reached only after
// remuxPlanFor declined, which for a span it always does, since the cut IS the
// spanned answer this rung adds below rung 2.
//
// The gates run header-cheap first: a non-span, a bit-rate cap, a codec off the
// allowlist, all answer before the packet-grid walk, so no request pays for a
// walk it cannot use. gridFor is the same per-identity memo the segmented remux
// uses; this is its first progressive caller.
//
// Two facts about the track it plans from. It is the full source track from
// trackFor, not req.track: a spanned req.track is the SpanTrack'd virtual track,
// whose Delay and Padding are zeroed and whose codec config is the source's
// un-rewritten one, none of which a packet-move cut can work from. The source
// track was already measured exactly by prepareSource on the span branch, so
// trackFor is a memo hit rather than a second probe.
//
// PlanCut's errors are swallowed, not propagated, exactly as remuxPlanFor
// swallows PlanRemux's, and the reason is sharper here: validateCutSpans is
// stricter than SpanTrack (a zero-length span that rung 3 serves as an empty 200
// PlanCut refuses), so surfacing the error would turn a request the ladder
// serves today into a new 400. A nil return preserves the fall-through contract.
//
// Every decline is logged at Debug naming its reason, because a caller asking
// "why did my Opus span re-encode" has no other signal: the swallow is correct
// but silent, and PlanCut's own six Debug reasons do not cover this function's
// early gates. See the reachability note in the API docs: the cut is reached
// only with a source-matching format (format=opus, or format=aac for AAC-LC),
// since format=auto may resolve to a codec the source is not.
func (s *Server) cutPlanFor(req *streamRequest) *waxflow.CutPlan {
	if !req.p.span.narrowed() {
		return nil // not a span: there is nothing for this rung to cut.
	}
	decline := func(reason string, args ...any) *waxflow.CutPlan {
		kv := make([]any, 0, 4+len(args))
		kv = append(kv, "reason", reason, "src", req.p.src)
		kv = append(kv, args...)
		s.log.Debug("cut declined", kv...)
		return nil
	}
	if req.p.maxBitRate > 0 {
		// The same decline remuxPlanFor makes: this rung reads headers, and a
		// source's real bit rate is in its packets, so it cannot promise a cap.
		return decline("a bit rate cap cannot be promised by a packet-move rung")
	}
	track, err := s.trackFor(req.src, true)
	if err != nil {
		// prepareSource already measured this on the span branch, so an error
		// here is not expected; decline rather than fail, and let the transcode
		// rung report it if it recurs.
		return decline("the source track could not be measured", "err", err)
	}
	if !waxflow.Cuttable(track) {
		return decline("the codec is not on the cut allowlist", "codec", track.Codec)
	}
	grid, err := s.gridFor(req.src)
	if err != nil {
		return decline("the packet grid could not be measured", "err", err)
	}
	if grid <= 0 {
		return decline("the source's packet durations vary, so there is no grid to cut on")
	}
	plan, err := s.eng.PlanCut(track, req.opts,
		[]waxflow.Span{{From: req.p.span.from, To: req.p.span.end()}}, grid)
	if err != nil {
		return decline("PlanCut refused the span", "err", err)
	}
	if plan == nil {
		return decline("no cut serves these options (gap, tail, or destination)")
	}
	req.cutGrid = grid
	// The measured length the plan cut from, so the run's init segment carries it
	// rather than the header's -1 for an undeclared-length source (ADTS).
	req.cutSamples = track.Samples
	return plan
}
