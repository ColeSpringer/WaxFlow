package server

import (
	"context"
	"fmt"
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
// the plan and the run must be handed the same one.
//
// The server never crossfades, and this is where that is true rather than in
// the four literals it replaces. A blend is a caller's editorial decision
// about their material; the HTTP surface has no way to ask for one and no
// business inventing one, and a queue arriving over /hls/timeline is a gapless
// album as often as not, which is exactly the case a nonzero default would
// ruin. The zero value is what says so.
//
// It is also what keeps ADR-0009's identity section true. The digest covers
// the members and nothing else, so if the server ever did crossfade, the
// digest would name two different timelines by one name.
//
// # One construction site is deliberately not here
//
// The merge job's run builds its own ConcatOptions (internal/jobs' runMerge),
// and cannot call this: the dependency runs the other way. So a merge is the
// one plan/run pair this method does not join up, since validateMergeRequest
// plans its envelope through here. They agree because both are the daemon's
// profile and neither crossfades, and that holds only while nothing threads a
// crossfade to a job -- which nothing asks for, and which is where the digest
// question arrives for real. If that day comes, runMerge is the second place,
// and its own comment says so.
func (s *Server) timelineOptions() waxflow.ConcatOptions {
	return waxflow.ConcatOptions{Profile: s.profile}
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
// Both answers carry the same three values, so a client's two paths converge:
// the finished job's timeline field is this response's body.
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
		j, err := s.jobs.Create(jobs.Request{Type: jobs.TypeTimeline, Srcs: refs})
		if err != nil {
			s.writeError(w, err)
			return
		}
		s.writeJSON(w, http.StatusAccepted, j)
		return
	}
	// The handles are already open and identity-checked, so the mint reuses
	// them rather than resolving every member a second time.
	tl, err := s.mintTimelineFrom(r.Context(), srcs, nil)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, TimelineResponse{
		SchemaVersion:   1,
		Tl:              tl.Tl,
		Members:         tl.Members,
		DurationSeconds: tl.DurationSeconds,
	})
}

// mintTimelineJob is the runner's MintTimeline hook. It binds the job's own
// context, which is the runner's and not a request's, exactly as a live
// pipeline binds the server's base context.
func (s *Server) mintTimelineJob(ctx context.Context, refs []string, progress func(done, total int64)) (*jobs.Timeline, error) {
	if s.timelines == nil {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat, "multi-source timelines are not enabled on this daemon")
	}
	return s.mintTimeline(ctx, refs, progress)
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
func (s *Server) mintTimeline(ctx context.Context, refs []string, progress func(done, total int64)) (*jobs.Timeline, error) {
	srcs, err := s.resolveAll(ctx, refs)
	if err != nil {
		return nil, err
	}
	defer closeAll(srcs)
	return s.mintTimelineFrom(ctx, srcs, progress)
}

// mintTimelineFrom probes and measures every member, then stores the timeline
// and reports what it named. The caller owns the handles; progress may be nil.
//
// It is the one path both the fast handler and the job run through, so a
// timeline minted either way is the same timeline: the digest is a function
// of the members alone, and the duration comes from the same ConcatTrack the
// stream will later plan through.
func (s *Server) mintTimelineFrom(ctx context.Context, srcs []*source.File, progress func(done, total int64)) (*jobs.Timeline, error) {
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
	// Planning the envelope now is what makes a mint mean something: a queue
	// whose members cannot be concatenated (a member laid out for other
	// speakers, say) fails here rather than at the first segment request.
	env, err := waxflow.ConcatTrack(tracks, s.timelineOptions())
	if err != nil {
		return nil, err
	}
	duration := DurationSeconds(env.Samples, env.Fmt.Rate)
	// The store's rule is that a timeline outlives every URL minted against
	// it. No URL exists yet, so the floor is what a default-TTL URL for this
	// much audio would carry; /sign and every segment read extend it from
	// there.
	digest, err := s.timelines.Put(members, time.Now().Add(sign.DefaultTTLFor(duration)))
	if err != nil {
		return nil, err
	}
	return &jobs.Timeline{Tl: digest, Members: len(members), DurationSeconds: duration}, nil
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
