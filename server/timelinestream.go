package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/internal/cache"
	"github.com/colespringer/waxflow/internal/sign"
)

// timelineStream is a prepared /stream?tl= request: the timeline's members and
// the options it is built and transcoded with, planned. It holds values only,
// as memberSource does, since the run outlives the request.
type timelineStream struct {
	members []memberSource
	opts    waxflow.TranscodeOptions
	copts   waxflow.ConcatOptions
	plan    *waxflow.TranscodePlan
	// canonical is the progressive cache key's parameter string. The crossfade
	// is not in it: it rides plan.Versions, as on HLS.
	canonical string
	// seconds is the whole timeline's duration, whatever t= seeks to: what a
	// URL for it is sized from.
	seconds float64
}

// serveTimelineStream serves GET/HEAD /stream?tl=: a minted timeline as one
// live encode over every member, through the cache like any other transcode.
// No rung but the transcode can serve one, since a copy cannot join sources.
func (s *Server) serveTimelineStream(w http.ResponseWriter, r *http.Request, q url.Values,
	sigAuthed bool, reqStart time.Time) {
	p, err := parseStreamParams(q, s.defaultGain)
	if err != nil {
		s.writeError(w, err)
		return
	}
	ts, err := s.prepareTimelineStream(r.Context(), p)
	if err != nil {
		s.writeError(w, err)
		return
	}
	// Every read extends the timeline to cover the URL it came in on, as HLS
	// reads do (ADR-0009).
	var exp int64
	if sigAuthed {
		exp, _ = strconv.ParseInt(q.Get(sign.ParamExp), 10, 64)
	}
	s.timelines.Touch(p.tl, readExpiry(exp, ts.seconds))

	// The digest is the identity, as for a timeline's HLS variants, whose
	// canonical params carry an hls prefix this key space lacks.
	identity := "tl:" + p.tl
	key := s.newKey("timeline", rungName(rungTranscode), identity, ts.canonical, ts.plan.Versions)
	meta := cache.Meta{
		Ref:         identity,
		Identity:    identity,
		Params:      ts.canonical,
		Ext:         waxflow.OutputExt(ts.opts.Format, ts.opts.Container),
		ContentType: ts.plan.MediaType,
		Samples:     ts.plan.Samples,
		Rate:        ts.plan.Format.Rate,
	}
	s.serveViaCache(w, r, key, meta, ts.plan, s.timelinePipeline(identity, ts), reqStart)
}

// prepareTimelineStream resolves and plans a /stream?tl= request: 415 on a
// daemon without timelines, then what the query alone decides (the gain policy
// and the options /stream shares with a single source), then 404 for a digest
// the store does not hold, 410 for a member changed since the mint, and the
// plan. The query comes before the members so a refusal costs no member open or
// measure and no 410 hides it. /sign runs it too, so a URL that signs is one
// that plays.
//
// The plan is the timeline's own (Engine.PlanTranscodeTimeline), with copts
// from timelineOptionsFor, and the run builds its Concat from those same copts,
// so the plan and the run cannot disagree about the timeline's length, width or
// key.
func (s *Server) prepareTimelineStream(ctx context.Context, p *streamParams) (*timelineStream, error) {
	if err := s.checkTimelines(); err != nil {
		return nil, err
	}
	if err := checkTimelineGain(p.gainParam, p.gain); err != nil {
		return nil, err
	}
	// No metadata and no tags: only off and a dB number pass the gain check,
	// and a timeline carries no one source's tags, as a merge job does not.
	gainDB := p.gain.resolveDB(nil, p.dynamics)
	opts, err := s.streamOptions(p, gainDB, 0)
	if err != nil {
		return nil, err
	}
	members, err := s.resolveTimelineMembers(ctx, p.tl)
	if err != nil {
		return nil, err
	}
	tracks := memberTracks(members)
	copts, err := s.timelineOptionsFor(tracks, p.crossfade, opts)
	if err != nil {
		return nil, err
	}
	env, err := waxflow.ConcatTrack(tracks, copts)
	if err != nil {
		return nil, err
	}
	// t= seeks on the timeline's own clock, which runs at the envelope's rate.
	opts.FromSample = seekSample(p.t, env.Fmt.Rate)
	plan, err := s.eng.PlanTranscodeTimeline(tracks, copts, opts)
	if err != nil {
		return nil, err
	}
	if err := checkLivePlan(plan, p); err != nil {
		return nil, err
	}
	return &timelineStream{
		members:   members,
		opts:      opts,
		copts:     copts,
		plan:      plan,
		canonical: canonicalParams(plan, gainDB, p.dynamics, span{}, opts.HEAACv2, opts.FromSample),
		seconds:   DurationSeconds(env.Samples, env.Fmt.Rate),
	}, nil
}

// timelinePipeline is the run that serves a planned timeline: one encode over a
// Concat whose members open lazily under the run's own context, each checked
// against the identity it was planned with (openTimelineMedia).
func (s *Server) timelinePipeline(ref string, ts *timelineStream) pipeline {
	members, copts, opts := ts.members, ts.copts, ts.opts
	return pipeline{ref: ref, rung: rungTranscode, run: func(ctx context.Context, dst io.Writer) (*waxflow.TranscodeResult, error) {
		med, err := s.openTimelineMedia(ctx, members, copts)
		if err != nil {
			return nil, err
		}
		defer med.Close()
		return s.eng.TranscodeMedia(ctx, med, dst, opts)
	}}
}
