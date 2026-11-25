package server

import (
	"context"
	"fmt"
	"net/url"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/outshape"
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

	// Set by planTranscode for transcode-shaped requests. tags ride with the
	// run beside opts: they never shape the plan, and the cache key folds
	// them in on its own.
	opts waxflow.TranscodeOptions
	tags []container.Tag
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
	cut       *waxflow.CutPlan
	canonical string
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
	// A timeline parses with no src; routing it is the caller's job, so one
	// that reaches here is refused by name rather than resolved as "".
	if p.tl != "" {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "tl names a timeline, and this endpoint serves one src")
	}
	src, err := s.resolver.Resolve(ctx, p.src)
	if err != nil {
		return nil, err
	}
	if err := checkIdentity(p, sigAuthed, src); err != nil {
		src.Close()
		return nil, err
	}
	info, err := s.eng.Probe(ctx, src, src.Ext, nil)
	if err != nil {
		src.Close()
		return nil, err
	}
	track := info.Default()
	// An advisory total, or none at all, is measured, because everything below
	// plans from this track: plan.Samples is what X-Content-Duration
	// advertises, the segmented route's count is derived from it, and the
	// body's own Media is wrapped with this number (see stream.go), so the
	// transcode projects what was advertised. Advertising an estimate beside
	// audio the run measures a different way is the drift this closes.
	//
	// The absent case is narrower, and the narrowing is the point: only a
	// container that states its trims per packet can hide a DiscardPadding
	// inside its run, only a walk finds one, and without it the ladder plans a
	// copy that then refuses mid-body. So a Matroska file with no Info Duration
	// (it reports -1 with neither flag) is measured, and an untagged MP3 or a
	// bare ADTS stream is not.
	//
	// Measuring those too would be free duration headers and a real cost: this
	// runs before the direct-play rung, so every first play of a large
	// lengthless file would read the whole thing to answer a question serving
	// its own bytes never asks, and a walk that failed would fail a request
	// that needed no length at all.
	//
	// It costs less than it reads. A cold stream of a Matroska Opus used to
	// pay two walks, one to settle the track and one inside the transcode's
	// own open; now it pays this one, memoized, and a warm one pays none. A
	// source whose headers count is taken at its word here, as the playlist
	// does; what a header on a pipe would otherwise commit is confirmed by the
	// transcode itself.
	if track.SamplesAdvisory || (track.Samples < 0 && waxflow.StatesTrimsPerPacket(info.Container)) {
		if track, err = s.trackFor(ctx, src, true); err != nil {
			src.Close()
			return nil, err
		}
	}
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
		// dies part way through delivery. A source that counts its own samples
		// reaches the measure only here, since an ordinary stream of one
		// depends on no number and must not pay for a walk to be told it.
		if track, err = s.trackFor(ctx, src, true); err != nil {
			src.Close()
			return nil, err
		}
		if track, err = waxflow.SpanTrack(track, p.span.from, p.span.end()); err != nil {
			src.Close()
			return nil, err
		}
	}
	// The container's own tags are the floor and the mapper's win over them,
	// the same resolution ProbeJSON makes and for the same reason: without it
	// a daemon nobody wired a mapper into embeds no tags at all onto a stream
	// whose source plainly carries them. readMeta answers nil for exactly that
	// daemon, which the fold reads as a floor to build on.
	m := meta.WithContainerTags(s.readMeta(ctx, src, false), info.Tags)
	return &streamRequest{
		p:     p,
		src:   src,
		info:  info,
		track: track,
		// The seek is inside the span: t= addresses the virtual track's own
		// timeline, because that is the stream the caller is playing.
		from:   seekSample(p.t, track.Fmt.Rate),
		gainDB: resolveGainDB(p.gain, m, p.dynamics),
		meta:   m,
	}, nil
}

// planTranscode runs the shared transcode-shaped back half: resolve
// format=auto, plan the pipeline, and enforce the streaming-form and
// bitrate-cap policies identically for /stream and /transcode.
func (s *Server) planTranscode(ctx context.Context, req *streamRequest) error {
	opts, err := s.streamOptions(req.p, req.gainDB, req.from)
	if err != nil {
		return err
	}
	// The live passthrough: the minimal descriptive set, embedded by
	// muxers with a stream-form tag representation.
	// Trimmed for the reason internal/jobs does it: a source's own tags
	// carried onto a stream must not turn an oversized one into a refusal.
	req.tags = embeddableTags(req.meta)
	req.opts = opts
	// Rung 1 (directPlayable) already declined, or we would not be here.
	if err := s.planStreamRung(ctx, req); err != nil {
		return err
	}
	plan := req.plan
	if err := checkLivePlan(plan, req.p); err != nil {
		return err
	}
	req.canonical = canonicalParams(plan, req.gainDB, req.p.dynamics, req.p.span, req.opts.HEAACv2, req.from, req.opts.VorbisQuality)
	// Keyed for every output, the few that embed no tags included (ADTS, WAV,
	// Matroska): a term too many costs a re-encode, a term too few serves
	// another mapping's tags.
	if len(req.tags) > 0 {
		req.canonical += "&tags=" + tagsFingerprint(req.tags)
	}
	return nil
}

// streamOptions maps a progressive request's parameters onto engine options,
// resolving format=auto, then q for that format, and refusing what the options
// alone rule out, a knob the format does not take among them. Tags are left to
// the caller.
func (s *Server) streamOptions(p *streamParams, gainDB float64, from int64) (waxflow.TranscodeOptions, error) {
	shape := p.shape
	if shape.Format == "auto" {
		shape.Format = waxflow.DefaultLiveFormat()
	}
	if p.q != "" {
		var err error
		if shape.Bitrate, shape.VorbisQuality, err = outshape.Preset(shape.Format, p.q); err != nil {
			// A container error comes before any knob refusal, q's included.
			form := outshape.Params{Format: shape.Format, Container: shape.Container}
			if cerr := form.Check(outshape.Stream); cerr != nil {
				return waxflow.TranscodeOptions{}, cerr
			}
			return waxflow.TranscodeOptions{}, err
		}
	}
	// The refusal also keeps the cache key's hev2 term honest: true only
	// where it shaped the stream.
	if err := shape.Check(outshape.Stream); err != nil {
		return waxflow.TranscodeOptions{}, err
	}
	opts := shape.Options(gainDB, p.dynamics, s.profile)
	opts.FromSample = from
	return opts, nil
}

// checkLivePlan enforces what a progressive plan must promise: a streaming
// form, and the request's maxBitRate cap.
func checkLivePlan(plan *waxflow.TranscodePlan, p *streamParams) error {
	if !plan.Live {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("%s has no streaming form; request it as a job output (POST /jobs)", plan.Container))
	}
	if kbit := p.maxBitRate; kbit > 0 {
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
	return nil
}

// planStreamRung plans the rung walkLadder picks for a prepared request. The
// cut plans from the full source track, whose trims and length a spanned
// req.track does not carry.
func (s *Server) planStreamRung(ctx context.Context, req *streamRequest) error {
	var rp *waxflow.RemuxPlan
	var cp *waxflow.CutPlan
	r, err := s.walkLadder(ladderQuery{
		src:      req.src,
		delivery: "progressive",
		span:     req.p.span,
		capped:   req.p.maxBitRate > 0,
		track:    func() (container.Track, error) { return s.trackFor(ctx, req.src, true) },
		grid:     func() (int, error) { return s.gridFor(ctx, req.src) },
	}, func(func() (int, error)) (bool, error) {
		p, err := s.eng.PlanRemux(req.track, req.opts)
		rp = p
		return p != nil, err
	}, func(track container.Track, grid func() (int, error)) (bool, error) {
		p, err := s.eng.PlanCut(track, req.opts,
			[]waxflow.Span{{From: req.p.span.from, To: req.p.span.end()}}, grid)
		cp = p
		return p != nil, err
	})
	if err != nil {
		return err
	}
	switch r {
	case rungRemux:
		req.remux, req.plan = rp, &rp.TranscodePlan
	case rungCut:
		req.cut, req.plan = cp, &cp.TranscodePlan
	default:
		plan, err := s.eng.PlanTranscode(req.track, req.opts)
		if err != nil {
			return err
		}
		req.plan = plan
	}
	return nil
}

// embeddableTags is MinimalTags trimmed to what the stream's muxer can embed.
// A /stream request names no tags of its own, so every tag here is the
// source's and a value too large for the comment header is dropped rather than
// refused. Nothing is logged: the streaming path drops the same tags on every
// request for the same source, and a per-request line would say it every time.
func embeddableTags(info *meta.Info) []container.Tag {
	tags, _ := meta.EmbeddableTags(meta.MinimalTags(info))
	return tags
}
