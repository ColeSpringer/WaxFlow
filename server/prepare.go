package server

import (
	"fmt"
	"net/url"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
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

	// Set by planTranscode for transcode-shaped requests.
	opts      waxflow.TranscodeOptions
	plan      *waxflow.TranscodePlan
	canonical string
}

// Close releases the request's source handle.
func (req *streamRequest) Close() error { return req.src.Close() }

// prepareSource runs the shared request front half. On success the caller
// owns req.Close.
func (s *Server) prepareSource(q url.Values, sigAuthed bool) (*streamRequest, error) {
	p, err := parseStreamParams(q, s.defaultGain)
	if err != nil {
		return nil, err
	}
	src, err := s.resolver.Resolve(p.src)
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
	return &streamRequest{
		p:      p,
		src:    src,
		info:   info,
		track:  track,
		from:   int64(p.t * float64(track.Fmt.Rate)),
		gainDB: p.gain.resolveDB(),
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
	req.opts = waxflow.TranscodeOptions{
		Format:          outFormat,
		Rate:            req.p.rate,
		Channels:        req.p.ch,
		BitDepth:        req.p.bits,
		GainDB:          req.gainDB,
		FromSample:      req.from,
		ResampleProfile: s.profile,
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
	req.canonical = canonicalParams(plan, req.gainDB, req.from)
	return nil
}
