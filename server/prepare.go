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
	opts      waxflow.TranscodeOptions
	plan      *waxflow.TranscodePlan
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
	plan, err := s.eng.PlanTranscode(req.track, req.opts)
	if err != nil {
		return err
	}
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
	req.plan = plan
	req.canonical = canonicalParams(plan, req.gainDB, req.p.dynamics, req.p.span, req.from)
	if len(req.opts.Tags) > 0 {
		req.canonical += "&tags=" + tagsFingerprint(req.opts.Tags)
	}
	return nil
}
