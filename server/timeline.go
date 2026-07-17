package server

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/jobs"
	"github.com/colespringer/waxflow/internal/sign"
	"github.com/colespringer/waxflow/internal/timeline"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// timelineOptions is the ConcatOptions every timeline this server plans or
// runs is built with, in one place because ConcatOptions' own convention says
// the plan and the run must be handed the same one. crossfade is in envelope
// samples, already converted from the wire's seconds (waxflow.CrossfadeSamples).
//
// A crossfade is a rendering option, not a timeline-identity input (ADR-0009).
// It rides the signed HLS descriptor and the mint request beside from/to/format,
// and the tl= digest and the timeline store are untouched: the digest covers the
// members alone, so two renders of one timeline that differ only by crossfade
// share a digest and are separated downstream by the cache key (crossfadeVersion,
// which timelineVersions already emits when the crossfade is nonzero). The plan
// and the run agree because both read one signed descriptor and convert it here
// the same way, which is the property storing the samples would have bought,
// obtained instead by signing the descriptor.
//
// The default is still a butt-join: an omitted crossfade is 0, exactly what a
// gapless album needs and what every pre-crossfade caller got. Callers pass the
// render's crossfade (from the descriptor) or, at mint, the request's; the merge
// validation passes 0, because a merge butt-joins and never blends.
//
// # One construction site is deliberately not here
//
// The merge job's run builds its own ConcatOptions (internal/jobs' runMerge),
// and cannot call this: the dependency runs the other way. So a merge is the
// one plan/run pair this method does not join up, since validateMergeRequest
// plans its envelope through here with crossfade 0. They agree because both are
// the daemon's profile and neither blends, and that holds because a merge is not
// on the render path a wire crossfade reaches.
func (s *Server) timelineOptions(crossfade int64) waxflow.ConcatOptions {
	return waxflow.ConcatOptions{Profile: s.profile, Crossfade: crossfade}
}

// checkCrossfadeSeconds is the cheap early guard on a wire crossfade: it must be
// non-negative and finite. Whether it actually fits the timeline (the shortest
// member, the pool buffer) is checkCrossfade's call inside ConcatTrack, which
// needs the members measured; this is what a handler can refuse before resolving
// or measuring anything. The HLS descriptor applies the same rule at decode, so
// a signed URL and a fresh request are held to one contract.
func checkCrossfadeSeconds(x float64) error {
	if x < 0 || math.IsNaN(x) || math.IsInf(x, 0) {
		return waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("crossfadeSeconds %g must be non-negative and finite", x))
	}
	return nil
}

// handleTimelineCreate serves POST /hls/timeline: it resolves and probes the
// queue's members, stores them under the digest that is their identity, and
// answers with the digest a tl= parameter names.
//
// It answers 201 with the digest, or 202 with a job when the mint would be
// slow enough to be worth one (see timelineNeedsJob). The split exists
// because a timeline's positions are a prefix sum, so every member's length
// must be measured rather than trusted, and for the formats whose demuxer has
// to scan the whole file to find its end, a long cold queue is more work than
// a request should hold open. A timeout would not help: it would only turn a
// slow album into an album that cannot be timelined at all, which is the
// functional gap the job avoids.
//
// Both answers carry the same values, so a client's two paths converge: the
// finished job's timeline field is this response's body.
func (s *Server) handleTimelineCreate(w http.ResponseWriter, r *http.Request) {
	var body TimelineRequest
	if err := decodeJSONBody(w, r, &body); err != nil {
		s.writeError(w, err)
		return
	}
	refs, err := timelineRefs(body)
	if err != nil {
		s.writeError(w, err)
		return
	}
	// The cheap early guard, before anything is resolved or measured: a negative
	// or non-finite crossfade is refused now. Whether it actually fits the members
	// is checkCrossfade's call at mint, which needs them measured, so an oversized
	// crossfade shares the sync/async split below with an un-concatenatable queue:
	// a 400 on the synchronous path but a failed job on the 202 path, one input
	// with two failure modes by cache warmth. That asymmetry is inherent to
	// deferring the measure, not new to the crossfade.
	if err := checkCrossfadeSeconds(body.CrossfadeSeconds); err != nil {
		s.writeError(w, err)
		return
	}
	srcs, err := s.resolveAll(r.Context(), refs)
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer closeAll(srcs)

	slow, err := s.timelineNeedsJob(srcs)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if slow && s.jobs != nil {
		// A daemon with no jobs API falls through and measures inline. That
		// is the honest degradation: the work is the same either way, and
		// refusing would turn a slow album into an album that cannot be
		// timelined at all, which is the failure this split exists to avoid.
		j, err := s.jobs.Create(jobs.Request{Type: jobs.TypeTimeline, Srcs: refs, CrossfadeSeconds: body.CrossfadeSeconds})
		if err != nil {
			s.writeError(w, err)
			return
		}
		s.writeJSON(w, http.StatusAccepted, j)
		return
	}
	// The handles are already open and identity-checked, so the mint reuses
	// them rather than resolving every member a second time.
	tl, err := s.mintTimelineFrom(r.Context(), srcs, body.CrossfadeSeconds, nil)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, TimelineResponse{
		SchemaVersion:   1,
		Tl:              tl.Tl,
		Members:         tl.Members,
		DurationSeconds: tl.DurationSeconds,
		EnvelopeRate:    tl.EnvelopeRate,
		Boundaries:      tl.Boundaries,
	})
}

// mintTimelineJob is the runner's MintTimeline hook. It binds the job's own
// context, which is the runner's and not a request's, exactly as a live
// pipeline binds the server's base context. crossfadeSeconds is the request's,
// carried on the job so the 202 path shapes the same duration and boundaries the
// 201 fast path returns.
func (s *Server) mintTimelineJob(ctx context.Context, refs []string, crossfadeSeconds float64, progress func(done, total int64)) (*jobs.Timeline, error) {
	if s.timelines == nil {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat, "multi-source timelines are not enabled on this daemon")
	}
	return s.mintTimeline(ctx, refs, crossfadeSeconds, progress)
}

// timelineRefs validates the body and flattens it to source references.
func timelineRefs(body TimelineRequest) ([]string, error) {
	bad := func(format string, args ...any) ([]string, error) {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(format, args...))
	}
	switch {
	case len(body.Srcs) == 0:
		return bad("srcs is required: a timeline needs at least one member")
	case len(body.Srcs) > timeline.MaxMembers:
		return bad("%d members is past the %d-member bound; a timeline is a play queue, not a library",
			len(body.Srcs), timeline.MaxMembers)
	}
	refs := make([]string, len(body.Srcs))
	for i, m := range body.Srcs {
		if m.Src == "" {
			return bad("member %d has no src", i)
		}
		refs[i] = m.Src
	}
	return refs, nil
}

// mintTimeline resolves the references and mints. It is the job's entry
// point, which has only the references the request was created from; the
// handler has open handles already and calls mintTimelineFrom directly.
func (s *Server) mintTimeline(ctx context.Context, refs []string, crossfadeSeconds float64, progress func(done, total int64)) (*jobs.Timeline, error) {
	srcs, err := s.resolveAll(ctx, refs)
	if err != nil {
		return nil, err
	}
	defer closeAll(srcs)
	return s.mintTimelineFrom(ctx, srcs, crossfadeSeconds, progress)
}

// mintTimelineFrom probes and measures every member, then stores the timeline
// and reports what it named. The caller owns the handles; progress may be nil.
//
// It is the one path both the fast handler and the job run through, so a
// timeline minted either way is the same timeline: the digest is a function
// of the members alone, and the duration and boundaries come from the same
// concatLayout the stream will later plan through (ConcatBoundaries here,
// ConcatTrack in the segment plan, one funnel).
//
// crossfadeSeconds shapes the reported duration and boundaries only. It is a
// rendering option, not part of the members that make the digest, so the store
// is untouched and two mints of one queue at different crossfades name one tl=;
// see timelineOptions. A client that mints with a crossfade must render with the
// same one, since the boundaries it reads back reflect the value passed here.
func (s *Server) mintTimelineFrom(ctx context.Context, srcs []*source.File, crossfadeSeconds float64, progress func(done, total int64)) (*jobs.Timeline, error) {
	members := make([]timeline.Member, len(srcs))
	tracks := make([]container.Track, len(srcs))
	for i, f := range srcs {
		if err := ctx.Err(); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeCanceled, "minting a timeline canceled", err)
		}
		// The exact length is what a timeline needs and what makes this
		// possibly slow: a member whose headers cannot declare one is
		// decoded to its end here, once, and memoized by identity.
		track, err := s.trackFor(f, true)
		if err != nil {
			return nil, err
		}
		tracks[i] = track
		members[i] = timeline.Member{Src: f.Ref, ID: f.ID.String()}
		if progress != nil {
			progress(int64(i+1), int64(len(srcs)))
		}
	}
	// The wire spells the crossfade in seconds; convert it on the measured
	// members, whose formats fix the envelope rate the blend is counted on. A
	// crossfade too long for the shortest member (or the pool buffer) is refused
	// by ConcatBoundaries below, which is checkCrossfade's 400 on the sync path
	// and a failed job on the async one.
	crossfade, err := waxflow.CrossfadeSamples(tracks, crossfadeSeconds)
	if err != nil {
		return nil, err
	}
	// Planning the envelope now is what makes a mint mean something: a queue
	// whose members cannot be concatenated (a member laid out for other
	// speakers, say) fails here rather than at the first segment request. The
	// boundaries fall out of the same walk, so the response can report where
	// each member lands with no extra measurement.
	boundaries, env, err := waxflow.ConcatBoundaries(tracks, s.timelineOptions(crossfade))
	if err != nil {
		return nil, err
	}
	// The timeline's length is where the last member ends: it carries no tail
	// zone, so its end (offset + duration) is starts[N], the same total
	// ConcatTrack projects. Deriving it from the boundaries keeps every
	// reported length reading one concatLayout.
	var total int64
	if n := len(boundaries); n > 0 {
		last := boundaries[n-1]
		total = last.OffsetSamples + last.DurationSamples
	}
	duration := DurationSeconds(total, env.Rate)
	// The store's rule is that a timeline outlives every URL minted against
	// it. No URL exists yet, so the floor is what a default-TTL URL for this
	// much audio would carry; /sign and every segment read extend it from
	// there.
	digest, err := s.timelines.Put(members, time.Now().Add(sign.DefaultTTLFor(duration)))
	if err != nil {
		return nil, err
	}
	return &jobs.Timeline{
		Tl:              digest,
		Members:         len(members),
		DurationSeconds: duration,
		EnvelopeRate:    env.Rate,
		Boundaries:      boundaries,
	}, nil
}

// timelineNeedsJob reports whether minting these members would be slow
// enough to be worth a job rather than a handler.
//
// Whether a member must be measured and whether measuring it is slow are
// different questions, and they are easy to run together. Every member whose
// length is not authoritative is measured, always, because a timeline's
// prefix sum cannot survive a wrong one; that is not what this asks. This
// asks what the measuring costs, and the answer is not "a decode": an
// exact-length walk is a demuxer walk plus one frame, and where the demuxer
// can find its end from a table (FLAC's seek table, WAV's data chunk size, an
// Ogg last page, an mp4 stts) it is well under a millisecond for a
// three-minute track.
//
// What is slow is a format whose walk must scan every frame header to build
// an index, which reads the whole file. container.Indexer is exactly the
// tree's mark for that: a demuxer implements it when its index is expensive
// enough to be worth persisting, and MP3 is the only one that does. So a cold
// MP3 queue, which the walk reads end to end, becomes a job, and a FLAC album
// or any queue already measured mints in one round trip.
func (s *Server) timelineNeedsJob(srcs []*source.File) (bool, error) {
	for _, f := range srcs {
		if s.trackIsExact(f) {
			continue // already measured: the memo answers for free
		}
		// Opening is a header read, so asking is cheap even when the answer
		// is that the answer is expensive. It has to be an open rather than a
		// probe: the Indexer gate is on the media, not on the track.
		med, err := s.eng.OpenStream(f, f.Ext)
		if err != nil {
			return false, err
		}
		_, indexed := med.(container.Indexer)
		exact := med.Info().Default().SamplesExact
		med.Close()
		if !exact && indexed {
			return true, nil
		}
	}
	return false, nil
}

// resolveAll resolves every reference, closing what it opened if any fails.
func (s *Server) resolveAll(ctx context.Context, refs []string) ([]*source.File, error) {
	srcs := make([]*source.File, 0, len(refs))
	for _, ref := range refs {
		f, err := s.resolver.Resolve(ctx, ref)
		if err != nil {
			closeAll(srcs)
			return nil, err
		}
		srcs = append(srcs, f)
	}
	return srcs, nil
}

func closeAll(srcs []*source.File) {
	for _, f := range srcs {
		f.Close()
	}
}
