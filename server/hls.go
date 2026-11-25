package server

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/cache"
	"github.com/colespringer/waxflow/internal/hls"
	"github.com/colespringer/waxflow/internal/lru"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/outshape"
	"github.com/colespringer/waxflow/internal/sign"
	"github.com/colespringer/waxflow/internal/timeline"
	"github.com/colespringer/waxflow/m3u8"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// hlsMediaType covers the init header and media segments: audio-only
// fMP4, so audio/mp4 is the honest type (players key on the boxes, not
// the header).
const hlsMediaType = "audio/mp4"

// playlistMediaType is RFC 8216's type for m3u8 playlists.
const playlistMediaType = "application/vnd.apple.mpegurl"

// maxPlaylistSegments bounds a VOD playlist: past it the requested
// segment duration is degenerate for the source (a one-frame segDur on
// an audiobook), and the playlist itself becomes the payload.
const maxPlaylistSegments = 1 << 16

// hlsParamNames is the closed parameter surface of the descriptor-form
// HLS endpoints: the descriptor carries everything else.
var hlsParamNames = map[string]bool{
	"v": true, sign.ParamExp: true, sign.ParamKID: true, sign.ParamSig: true,
}

// hlsMintParamNames is the surface that mints descriptors: the raw-query
// master form and POST /sign's params. bitrates is the ladder
// (comma-separated kbit/s); src and tl are mutually exclusive, one stream or
// one timeline.
var hlsMintParamNames = map[string]bool{
	"src": true, "tl": true, "format": true, "bitrate": true, "bitrates": true,
	"hev2": true, "bits": true, "rate": true, "ch": true, "gain": true,
	"dynamics": true, "segDur": true, "from": true, "to": true, "crossfadeSeconds": true,
}

// memberSource is one resolved source as a run needs it: a timeline's member,
// or a single stream's one source. Runs outlive their requests, so they
// re-resolve from these values rather than borrowing a handle the request is
// about to close.
type memberSource struct {
	Ref string
	ID  source.Identity
	Ext string
	// From and To are a timeline member's stored sample window on its own
	// source (the zero pair is the whole file). The worker outlives the
	// request and rebuilds its input from these values: openTimelineMedia
	// slices the member's media by them at open, matching the narrowed Track
	// below.
	From int64
	To   int64
	// SrcTrack is the source's own pre-narrow track, measured whenever this
	// member can be sliced (a timeline member, or a spanned single source).
	// The worker re-anchors a freshly opened member's advisory declaration to
	// it before slicing, so the run's bound check runs against the same total
	// the mint and the plan validated the window on; see
	// waxflow.SliceMeasured.
	SrcTrack container.Track
	// Track is the probed, measured track the plan was computed from (for a
	// windowed member, already narrowed through SpanTrack). A timeline hands
	// it back to Concat, so the run's members are declared by exactly what
	// the plan projected: plan and run cannot disagree about a member's
	// format or length, because the number is not computed twice.
	Track container.Track
}

// hlsRequest is one parsed, resolved, identity-checked, planned HLS
// request for a single variant (media playlist, init header, or a
// segment).
type hlsRequest struct {
	desc hls.Descriptor
	// srcs are the open handles the request still needs: exactly one for a
	// single-track stream (the metadata read wants it), and none for a
	// timeline, whose members are closed as soon as they are checked and
	// probed. The request owns them and Close releases them.
	srcs []*source.File
	// members is every source this URL names, in order, as values: what the
	// plan reads and what a worker (which outlives the request, and so cannot
	// borrow a handle) re-resolves from.
	members []memberSource
	variantPlan
	key  cache.Key
	meta cache.Meta
	// exp is the request's own signature expiry (unix seconds), 0 when
	// key-authed; child URLs inherit it so one minting governs the whole
	// playback session's lifetime.
	exp int64
}

// variantPlan is what planning a variant produced: its options and its plan on
// the rung that serves it.
type variantPlan struct {
	opts waxflow.TranscodeOptions
	// copts are the ConcatOptions a timeline was planned with, and so the ones
	// its worker builds the timeline with; zero for a single source.
	copts waxflow.ConcatOptions
	plan  *waxflow.SegmentPlan
	// remux is the ladder's rung 2 when this variant can be served by rewriting
	// the container around the source's own packets, nil when it takes rung 3.
	// plan points at its embedded SegmentPlan when it is set, so the playlist,
	// the key, and the worker all read one shape.
	remux *waxflow.RemuxSegmentPlan
	// cut is the cut rung, between rung 2 and rung 3, when a spanned variant can be
	// served by moving the source's own packets, nil otherwise. Like remux, plan
	// points at its embedded SegmentPlan, so one shape reaches the playlist, key,
	// and worker; a variant is at most one of remux or cut (a span declines remux).
	cut *waxflow.CutSegmentPlan
}

// rung reports which ladder rung planned this variant. The key, the init
// segment and the worker all read it.
func (vp variantPlan) rung() rung { return rungOf(vp.remux != nil, vp.cut != nil) }

func (req *hlsRequest) Close() error {
	closeAll(req.srcs)
	req.srcs = nil
	return nil
}

// prepareHLS runs the shared front half of the per-variant HLS handlers:
// auth, the closed parameter surface, descriptor decode, source identity
// (410 on mismatch, always: the descriptor embeds identity by
// construction), probe, the exact-length walk when the headers cannot
// provide one, and the variant plan plus its ADR-0004 cache key.
//
// A single-track URL and a timeline URL diverge only at the front door, in
// what they resolve. Everything past it (the identity checks, the tracks,
// the plan, the key) runs over a list of sources that happens to hold one.
func (s *Server) prepareHLS(r *http.Request) (*hlsRequest, error) {
	q := r.URL.Query()
	sigAuthed, err := s.playbackAuth(r, q)
	if err != nil {
		return nil, err
	}
	for k := range q {
		if !hlsParamNames[k] {
			return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("unknown parameter %q", k))
		}
	}
	desc, err := hls.DecodeDescriptor(q.Get("v"))
	if err != nil {
		return nil, err
	}
	if len(desc.Bitrates) > 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			"this URL is per-variant; the bitrates ladder belongs to master.m3u8")
	}
	if err := s.refuseHLSQuery(desc); err != nil {
		return nil, err
	}
	req := &hlsRequest{desc: desc}
	ok := false
	defer func() {
		if !ok {
			req.Close()
		}
	}()
	if err := s.resolveHLSSources(r.Context(), req); err != nil {
		return nil, err
	}
	// Metadata resolves tag-based gain, which only a single-track URL can
	// have: a timeline refuses the tag-derived modes at mint, because one
	// chain has one gain and N members have N answers (see checkTimelineGain).
	// A timeline hands no source to the remux rung either: it cannot take one,
	// and resolving a member's grid would be a walk for an answer nobody reads.
	var m *meta.Info
	var rmx *source.File
	if desc.Tl == "" {
		// Folded exactly as prepareSource folds it, and for the reason that
		// matters here: gain=track resolves against REPLAYGAIN_* tags, so
		// without the container floor the same source answers one gain on
		// /stream and another on /hls/master.m3u8 whenever the tag library
		// cannot read the file (or none is wired at all).
		m = meta.WithContainerTags(s.readMeta(r.Context(), req.srcs[0], false), s.containerTagsFor(r.Context(), req.srcs[0]))
		rmx = req.srcs[0]
	}
	if req.variantPlan, err = s.planHLSVariant(r.Context(), desc, memberTracks(req.members), m, rmx); err != nil {
		return nil, err
	}
	canonical := canonicalHLSParams(req.plan, req.opts.GainDB, req.opts.Dynamics, descSpan(desc), req.opts.HEAACv2)
	identity, shown := hlsIdentity(desc, req.members)
	req.key = s.newKey("hls", rungName(req.rung()), identity, canonical, req.plan.Versions)
	req.meta = cache.Meta{
		Ref:         hlsRef(desc),
		Identity:    shown,
		Params:      canonical,
		Ext:         "m4s",
		ContentType: hlsMediaType,
		Samples:     req.plan.Samples,
		Rate:        req.plan.Format.Rate,
	}
	if sigAuthed {
		req.exp, _ = strconv.ParseInt(q.Get(sign.ParamExp), 10, 64)
	}
	if desc.Tl != "" {
		// Every read touches the timeline, not just the mint, and it extends
		// the expiry to cover the URLs this request is about to hand out. A
		// long session (an audiobook, or one paused across the window) keeps
		// fetching segments against a timeline nobody re-mints, and evicting
		// it mid-playback would 404 the player at a buffer refill.
		s.timelines.Touch(desc.Tl, req.childExp())
	}
	ok = true
	return req, nil
}

// resolveHLSSources resolves, identity-checks, and probes everything the
// descriptor names: the one source, or the timeline's members in order.
//
// The identity check runs per request and is never skipped, for a timeline
// exactly as for a single source (see resolveTimelineMembers).
func (s *Server) resolveHLSSources(ctx context.Context, req *hlsRequest) error {
	if req.desc.Tl != "" {
		members, err := s.resolveTimelineMembers(ctx, req.desc.Tl)
		if err != nil {
			return err
		}
		req.members = members
		return nil
	}
	m, f, err := s.resolveMember(ctx, 0, timeline.Member{Src: req.desc.Src, ID: req.desc.ID},
		descSpan(req.desc).narrowed())
	if err != nil {
		return err
	}
	// The metadata read that resolves tag-based gain still needs the handle,
	// and req.Close owns it from here.
	req.members = append(req.members, m)
	req.srcs = append(req.srcs, f)
	return nil
}

// resolveTimelineMembers resolves, identity-checks, and measures the members
// of the timeline the digest names, in order: 404 for a digest the store does
// not hold, 410 for a member that changed since the mint.
//
// The digest pins its members' identities by covering them, so a member
// replaced on disk cannot match the digest minted against it, and the mismatch
// is the 410 the URL promises. Every member is measured rather than trusted,
// which a single source is not: a timeline's positions are a prefix sum, so a
// member that delivers two samples fewer than its headers declare desyncs every
// position after it and makes the playlist promise segments the stream cannot
// fill.
//
// It holds no handle once it returns. A timeline's handles are scaffolding:
// nothing reads a member again once it is checked and probed, and every
// position past that comes from the recorded track. Holding them would put one
// open file per member on every segment request, which for a thousand-member
// queue is a thousand descriptors against a default limit of 1024, and would
// undo the point of Concat opening members lazily.
//
// Members resolve together, memberResolvers at a time: a pid member is a
// round trip to the lookup host, and a thousand of them one after another
// would make every segment request wait on the sum. The bound keeps the
// handles open at once to that many. The first failure ends the rest.
func (s *Server) resolveTimelineMembers(ctx context.Context, digest string) ([]memberSource, error) {
	if err := s.checkTimelines(); err != nil {
		return nil, err
	}
	refs, err := s.timelines.Members(digest)
	if err != nil {
		return nil, err
	}
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	members := make([]memberSource, len(refs))
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		slots = make(chan struct{}, memberResolvers)
	)
	for i, ref := range refs {
		if rctx.Err() != nil {
			break
		}
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			m, f, err := s.resolveMember(rctx, i, ref, true)
			if err != nil {
				mu.Lock()
				if first == nil {
					first = err
					stop()
				}
				mu.Unlock()
				return
			}
			f.Close()
			members[i] = m
		})
	}
	wg.Wait()
	if first != nil {
		return nil, first
	}
	if err := ctx.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeCanceled, "resolving a timeline canceled", err)
	}
	return members, nil
}

// memberResolvers bounds how many timeline members resolve at once.
const memberResolvers = 8

// resolveMember resolves member i, enforces the identity the URL pins, probes
// it, and returns what the plan and the worker need beside its open handle,
// which the caller owns.
//
// exact measures the length. A timeline's members always are, and so is a
// spanned single source: SpanTrack checks to against the total, so an
// under-declared source refuses a legitimate span at mint, and an
// over-declared one mints a span whose segments run past the stream.
func (s *Server) resolveMember(ctx context.Context, i int, ref timeline.Member,
	exact bool) (memberSource, *source.File, error) {
	want, err := source.ParseIdentity(ref.ID)
	if err != nil {
		return memberSource{}, nil, err
	}
	f, err := s.resolver.Resolve(ctx, string(ref.Src))
	if err != nil {
		return memberSource{}, nil, err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()
	if want != f.ID {
		return memberSource{}, nil, waxerr.New(waxerr.CodeSourceChanged,
			"the source changed since this URL was minted; request a fresh one")
	}
	track, err := s.trackFor(ctx, f, exact)
	if err != nil {
		return memberSource{}, nil, err
	}
	// A rounded total cannot be divided into segments. trackFor hands a
	// non-exact caller the headers' number, and for ASF and for a Matroska,
	// which no open walks, that number is an estimate: taking it at its word
	// is the tail 404 or early ENDLIST trackFor's own comment names. So a
	// playlist asks again for a measured one. The re-ask is a memo hit
	// whenever the first call already measured, and the cheap measure below is
	// what makes it a walk rather than a decode.
	if track.SamplesAdvisory {
		if track, err = s.trackFor(ctx, f, true); err != nil {
			return memberSource{}, nil, err
		}
	}
	// The pre-narrow total, recorded so the worker can re-anchor a freshly
	// opened member's advisory declaration to the measure the window was
	// validated against (waxflow.SliceMeasured). Whenever a member is sliced,
	// the exact branch above ran, so this is measured in every consulted case.
	srcTrack := track
	// A timeline member's own stored window narrows its track through the
	// same funnel the mint used (narrowMemberTrack), so the plan reads the
	// window's length and the two refuse identically. The re-check against
	// the freshly measured track is not redundant with the mint's: the
	// identity check above pins the bytes, not the measure, and the track
	// memo is deliberately re-derived per binary revision while the store is
	// durable, so a window minted valid under an old measure must fail here,
	// loudly, rather than desync the prefix sum (or, for the empty-window
	// shape from == total, mint the zero-sample member nothing downstream
	// pins). Corruption is not the concern: a hand-edited window fails
	// load's digest re-check before it ever resolves.
	if track, err = narrowMemberTrack(i, track, span{from: ref.From, to: ref.To}); err != nil {
		return memberSource{}, nil, err
	}
	ok = true
	return memberSource{Ref: string(ref.Src), ID: f.ID, Ext: f.Ext,
		From: ref.From, To: ref.To, SrcTrack: srcTrack, Track: track}, f, nil
}

// memberTracks are the members' tracks in order: what a variant or a timeline
// plans from.
func memberTracks(members []memberSource) []container.Track {
	out := make([]container.Track, len(members))
	for i, m := range members {
		out[i] = m.Track
	}
	return out
}

// hlsDuration is the stream duration a URL's TTL policy reads: the span's for
// a single source, the whole concatenated timeline's for a timeline. A signed
// URL is meant to outlive one playthrough, and a twelve-track album's
// playthrough is not its first track's, just as a two-minute span's is not the
// hour-long rip it was cut from.
//
// The span narrows the track through the same funnel planHLSVariant uses, so
// the duration the TTL is sized from is the duration the playlist promises.
// copts is what the timeline is built with for the same reason: a crossfade
// shortens the stream, and a TTL sized from a length nothing delivers is one
// more way for the plan and the run to disagree.
func hlsDuration(desc hls.Descriptor, tracks []container.Track, copts waxflow.ConcatOptions) (float64, error) {
	if desc.Tl == "" {
		track := tracks[0]
		if sp := descSpan(desc); sp.narrowed() {
			var err error
			if track, err = waxflow.SpanTrack(track, sp.from, sp.end()); err != nil {
				return 0, err
			}
		}
		return DurationSeconds(track.Samples, track.Fmt.Rate), nil
	}
	env, err := waxflow.ConcatTrack(tracks, copts)
	if err != nil {
		return 0, err
	}
	return DurationSeconds(env.Samples, env.Fmt.Rate), nil
}

// hlsRef is the source reference a variant's cache metadata records, for a
// human reading the cache directory.
func hlsRef(desc hls.Descriptor) string {
	if desc.Tl != "" {
		return "tl:" + desc.Tl
	}
	return desc.Src.Text()
}

// hlsIdentity is the ADR-0004 identity a variant's entries key on: the
// source's own for a single-track stream, the digest for a timeline.
//
// The digest is enough by itself, which is the whole point of content-
// addressing it. It covers every member's reference and identity, so any
// change to any member is a different digest and therefore a different key,
// and there is no second list identity that could disagree with it.
//
// shown is the same identity as the sidecar records it: JSON, so the
// reference is in text form where key hashes its own bytes.
func hlsIdentity(desc hls.Descriptor, members []memberSource) (key, shown string) {
	if desc.Tl != "" {
		return "tl:" + desc.Tl, "tl:" + desc.Tl
	}
	id := members[0].ID
	return identityString(string(desc.Src), id), identityString(desc.Src.Text(), id)
}

// measureSamples forces a source's exact length, by the cheapest route the
// opened media offers (see waxflow.MeasureLength). It returns the whole track,
// because a walk settles the trims beside the length and the memo this fills
// is what the copy rungs plan from (see container.Track.MidPadding).
func (s *Server) measureSamples(ctx context.Context, src *source.File) (container.Track, error) {
	med, err := s.eng.OpenStream(src, src.Ext)
	if err != nil {
		return container.Track{}, err
	}
	defer med.Close()
	return waxflow.MeasureLength(ctx, med)
}

// planHLSVariant maps one variant descriptor onto engine options and its
// segment plan, mirroring planTranscode's policy checks so /stream and
// HLS cannot drift. tracks is the single source's track, or the timeline's
// members' tracks in order.
// rmx is the source to try the remux rung against, nil to skip it. The rung
// changes what a plan prints (an HE-AAC copy answers the stream's own
// signalling, mp4a.40.5 or mp4a.40.29, where the transcode rung answers an
// LC re-encode), so every path that prints a plan passes the source; only
// mint-time validation, which prints nothing and needs any rung to answer,
// skips the packet walk.
//
// The playlist cap is checked here, where the mint, the master and every
// per-variant handler all plan, so a URL that mints is one whose media
// playlist lists.
func (s *Server) planHLSVariant(ctx context.Context, desc hls.Descriptor, tracks []container.Track, m *meta.Info,
	rmx *source.File) (variantPlan, error) {
	vp, err := s.planHLSRung(ctx, desc, tracks, m, rmx)
	if err == nil && vp.plan.Segments > maxPlaylistSegments {
		err = waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("segment duration yields %d segments (max %d); raise segDur", vp.plan.Segments, maxPlaylistSegments))
	}
	if err != nil {
		return variantPlan{}, err
	}
	return vp, nil
}

// hlsVariantOptions maps a variant descriptor onto engine options, refusing
// what the descriptor alone rules out. It reads nothing but the descriptor and
// m, which resolves tag-based gain (nil at mint and for a timeline), so a
// caller can run it before resolving anything (refuseHLSQuery).
func (s *Server) hlsVariantOptions(desc hls.Descriptor, m *meta.Info) (waxflow.TranscodeOptions, error) {
	fail := func(err error) (waxflow.TranscodeOptions, error) { return waxflow.TranscodeOptions{}, err }
	shape := descriptorShape(desc)
	if err := shape.Check(outshape.HLS); err != nil {
		return fail(err)
	}
	g, err := parseGain(desc.Gain, s.defaultGain)
	if err != nil {
		return fail(err)
	}
	if desc.Tl != "" {
		if err := checkTimelineGain(desc.Gain, g); err != nil {
			return fail(err)
		}
	}
	dyn, err := parseDynamics(desc.Dynamics)
	if err != nil {
		return fail(err)
	}
	return shape.Options(resolveGainDB(g, m, dyn), dyn, s.profile), nil
}

// descriptorShape is the output shape d carries, and setDescriptorShape
// writes one into d: the one mapping between the two, so hls stays free of
// the engine.
func descriptorShape(d hls.Descriptor) outshape.Params {
	return outshape.Params{Format: d.Format, Rate: d.Rate, Channels: d.Ch, Bits: d.Bits,
		Bitrate: d.Bitrate, HEv2: d.HEv2}
}

func setDescriptorShape(d *hls.Descriptor, p outshape.Params) {
	d.Format, d.Rate, d.Ch, d.Bits, d.Bitrate, d.HEv2 = p.Format, p.Rate, p.Channels, p.Bits, p.Bitrate, p.HEv2
}

// refuseHLSQuery refuses what desc alone rules out, before anything resolves:
// a timeline on a daemon without them, and each ladder rung's options. A
// refusal then costs no source read, and no 404 or 410 hides it.
func (s *Server) refuseHLSQuery(desc hls.Descriptor) error {
	if desc.Tl != "" {
		if err := s.checkTimelines(); err != nil {
			return err
		}
	}
	for _, kbps := range desc.Ladder() {
		if _, err := s.hlsVariantOptions(desc.Variant(kbps), nil); err != nil {
			return err
		}
	}
	return nil
}

// planHLSRung is planHLSVariant short of the playlist cap: the first rung that
// serves the variant, and its plan.
func (s *Server) planHLSRung(ctx context.Context, desc hls.Descriptor, tracks []container.Track, m *meta.Info,
	rmx *source.File) (variantPlan, error) {
	fail := func(err error) (variantPlan, error) { return variantPlan{}, err }
	opts, err := s.hlsVariantOptions(desc, m)
	if err != nil {
		return fail(err)
	}
	// The timeline plan is not the single-source plan over a synthetic
	// track: it also carries the members' decoder revisions and the
	// normalization each member runs through, neither of which the synthetic
	// track can name. See PlanSegmentsTimeline.
	var plan *waxflow.SegmentPlan
	var copts waxflow.ConcatOptions
	if desc.Tl != "" {
		// The descriptor spells the crossfade in seconds; convert it on the
		// members' fixed formats, once: the worker builds its Concat from these
		// copts, so the plan's length and the run's cannot disagree. A crossfade
		// too long for the members is refused here at plan time (checkCrossfade
		// inside ConcatTrack), so an oversized or tampered value 400s rather than
		// serving a playlist the stream cannot fill. The delivered width rides
		// the same helper, so the timeline is planned at the width it is encoded
		// at.
		if copts, err = s.timelineOptionsFor(tracks, desc.CrossfadeSeconds, opts); err != nil {
			return fail(err)
		}
		plan, err = s.eng.PlanSegmentsTimeline(tracks, copts, opts, desc.SegDur)
	} else {
		// The full source track, measured exact for a span (resolveMember). The
		// copy rungs plan from it directly; only the transcode fallback narrows it
		// to a virtual track, because only a decode addresses the span by re-timing.
		// With no source to read (mint-time validation) only the transcode plans.
		src := tracks[0]
		if rmx != nil {
			if vp, ok, err := s.planHLSCopy(ctx, desc, rmx, src, opts); err != nil || ok {
				return vp, err
			}
		}
		// Rung 3: decode, DSP, encode. The span narrows the track first, which is
		// what makes a virtual track a stream in its own right: its segment 0 is the
		// span's first sample and its count covers to-from, not the whole rip.
		track := src
		if sp := descSpan(desc); sp.narrowed() {
			if track, err = waxflow.SpanTrack(track, sp.from, sp.end()); err != nil {
				return fail(err)
			}
		}
		plan, err = s.eng.PlanSegments(track, opts, desc.SegDur)
	}
	if err != nil {
		return fail(err)
	}
	return variantPlan{opts: opts, copts: copts, plan: plan}, nil
}

// planHLSCopy walks the copy rungs for one single-source variant: the
// segmented remux for a whole source, the segmented cut for a span. ok is
// false when the variant takes the transcode.
func (s *Server) planHLSCopy(ctx context.Context, desc hls.Descriptor, src *source.File, track container.Track,
	opts waxflow.TranscodeOptions) (vp variantPlan, ok bool, err error) {
	sp := descSpan(desc)
	var rp *waxflow.RemuxSegmentPlan
	var cp *waxflow.CutSegmentPlan
	r, err := s.walkLadder(ladderQuery{
		src:      src,
		delivery: "hls",
		span:     sp,
		track:    func() (container.Track, error) { return track, nil },
		grid:     func() (int, error) { return s.gridFor(ctx, src) },
	}, func(grid func() (int, error)) (bool, error) {
		p, err := s.eng.PlanRemuxSegments(track, opts, desc.SegDur, grid)
		rp = p
		return p != nil, err
	}, func(track container.Track, grid func() (int, error)) (bool, error) {
		p, err := s.eng.PlanCutSegments(track, opts, []waxflow.Span{{From: sp.from, To: sp.end()}}, grid, desc.SegDur)
		cp = p
		return p != nil, err
	})
	switch {
	case err != nil:
		return variantPlan{}, false, err
	case r == rungRemux:
		return variantPlan{opts: opts, plan: &rp.SegmentPlan, remux: rp}, true, nil
	case r == rungCut:
		return variantPlan{opts: opts, plan: &cp.SegmentPlan, cut: cp}, true, nil
	}
	return variantPlan{}, false, nil
}

// descSpan is the descriptor's span in the server's own spelling.
func descSpan(desc hls.Descriptor) span { return span{from: desc.From, to: desc.To} }

// canonicalHLSParams is the segmented cache key's parameter string: the
// core canonicalParams shares, plus the segment length that pins the
// numbering. The hls prefix keeps the key space disjoint from progressive
// entries.
func canonicalHLSParams(plan *waxflow.SegmentPlan, gainDB float64, dyn gain.Preset, sp span, hev2 bool) string {
	return fmt.Sprintf("hls&%s&segSamples=%d",
		canonicalCore(&plan.TranscodePlan, gainDB, dyn, sp, hev2), plan.SegmentSamples)
}

// childExp is the expiry child URLs (media playlists, init, segments) are
// signed with, and the one a timeline read extends the store to: the
// requesting URL's own exp when it has one, so one minting governs the
// session, else the default TTL policy rounded up to the hour (readExpiry), so
// a key-authed child URL can outlive max(6 h, 2x duration) by up to an hour.
func (req *hlsRequest) childExp() time.Time {
	return readExpiry(req.exp, DurationSeconds(req.plan.Samples, req.plan.Format.Rate))
}

// hlsChildQuery renders a child URL's query: the variant descriptor,
// signed when signing is configured (players fetch playlist children
// without headers, so the signature IS their auth).
func (s *Server) hlsChildQuery(path string, desc hls.Descriptor, exp time.Time) string {
	q := url.Values{"v": []string{desc.Encode()}}
	if s.signer != nil {
		q = s.signer.Sign(http.MethodGet, path, q, exp)
	}
	return q.Encode()
}

// handleHLSMaster serves the master playlist. It accepts the signed v=
// descriptor form, or (with an API key) raw /stream-style parameters as
// a convenience: the server builds the canonical descriptor itself,
// identity included, and the emitted child URLs carry it.
func (s *Server) handleHLSMaster(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)
	q := r.URL.Query()
	sigAuthed, err := s.playbackAuth(r, q)
	if err != nil {
		s.writeError(w, err)
		return
	}
	var desc hls.Descriptor
	if q.Has("v") {
		for k := range q {
			if !hlsParamNames[k] {
				s.writeError(w, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("unknown parameter %q", k)))
				return
			}
		}
		if desc, err = hls.DecodeDescriptor(q.Get("v")); err != nil {
			s.writeError(w, err)
			return
		}
		if err := s.refuseHLSQuery(desc); err != nil {
			s.writeError(w, err)
			return
		}
	} else {
		if !s.keyAuthed(r) {
			s.writeError(w, waxerr.New(waxerr.CodeUnauthorized, "master.m3u8 without v= needs an API key"))
			return
		}
		params, err := signParamsOf(q)
		if err != nil {
			s.writeError(w, err)
			return
		}
		if desc, _, err = s.mintHLSDescriptor(r.Context(), params); err != nil {
			s.writeError(w, err)
			return
		}
	}

	req := &hlsRequest{desc: desc}
	defer req.Close()
	if err := s.resolveHLSSources(r.Context(), req); err != nil {
		s.writeError(w, err)
		return
	}
	tracks := memberTracks(req.members)
	crossfade, err := waxflow.CrossfadeSamples(tracks, desc.CrossfadeSeconds)
	if err != nil {
		s.writeError(w, err)
		return
	}
	// The TTL is sized from the delivered duration, which a crossfade shortens;
	// hlsDuration reads the same copts the variants plan with so the lifetime and
	// the playlists agree about how long the stream is.
	duration, err := hlsDuration(desc, tracks, s.timelineOptions(crossfade))
	if err != nil {
		s.writeError(w, err)
		return
	}

	var sigExp int64
	if sigAuthed {
		sigExp, _ = strconv.ParseInt(q.Get(sign.ParamExp), 10, 64)
	}
	exp := readExpiry(sigExp, duration)
	if desc.Tl != "" {
		// The master is a URL-minting path, like /sign: every child URL below
		// is signed with exp, so the timeline has to outlive them. This is the
		// one minting path that does not go through /sign, and without this it
		// would be the one that can hand out URLs whose timeline is swept
		// before they expire.
		s.timelines.Touch(desc.Tl, exp)
	}
	// Folded exactly as prepareHLS folds it, remux source included: the
	// master's CODECS and BANDWIDTH must come from the same rung the media
	// path will serve, or an HE-AAC copy advertises mp4a.40.2 in the master
	// while the segments carry AOT-5 or AOT-29 packets.
	var m *meta.Info
	var rmx *source.File
	if desc.Tl == "" {
		m = meta.WithContainerTags(s.readMeta(r.Context(), req.srcs[0], false), s.containerTagsFor(r.Context(), req.srcs[0]))
		rmx = req.srcs[0]
	}
	var variants []m3u8.MasterVariant
	for _, kbps := range desc.Ladder() {
		vdesc := desc.Variant(kbps)
		vp, err := s.planHLSVariant(r.Context(), vdesc, tracks, m, rmx)
		if err != nil {
			s.writeError(w, err) // one bad rung fails the master honestly
			return
		}
		variants = append(variants, vp.plan.MasterVariant("media.m3u8?"+s.hlsChildQuery("/hls/media.m3u8", vdesc, exp)))
	}
	master, err := m3u8.Master(m3u8.MasterPlaylist{Variants: variants})
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.servePlaylist(w, master)
}

// handleHLSMedia serves a variant's VOD media playlist: every segment
// listed with its exact duration (instant player-driven seek), the init
// map, ENDLIST.
func (s *Server) handleHLSMedia(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)
	req, err := s.prepareHLS(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer req.Close()
	exp := req.childExp()
	segs, err := req.plan.MediaSegments(func(n int64) string {
		return fmt.Sprintf("seg/%d.m4s?%s", n, s.hlsChildQuery(fmt.Sprintf("/hls/seg/%d.m4s", n), req.desc, exp))
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	media, err := m3u8.Media("init.mp4?"+s.hlsChildQuery("/hls/init.mp4", req.desc, exp), segs)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.servePlaylist(w, media)
}

func (s *Server) servePlaylist(w http.ResponseWriter, playlist string) {
	h := w.Header()
	h.Set("Content-Type", playlistMediaType)
	// Playlists embed freshly signed URLs; caching one would pin a stale
	// signature lifetime, and regeneration is a string build.
	h.Set("Cache-Control", "no-store")
	io.WriteString(w, playlist)
}

// handleHLSInit serves the variant's init header, from cache when a
// worker already wrote it, else computed on the spot (deterministic
// either way) and cached best effort. In an outage it keeps to the
// segments' rules: a refused write marks the variant, a variant known to
// refuse writes is answered from memory with no write tried, and the
// outage is one Warn.
func (s *Server) handleHLSInit(w http.ResponseWriter, r *http.Request) {
	reqStart := time.Now()
	s.applyCORS(w, r)
	req, err := s.prepareHLS(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer req.Close()
	variant, cacheErr := s.hlsVariant(req)
	if cacheErr != nil && waxerr.CodeOf(cacheErr) != waxerr.CodeOutputUnwritable {
		s.writeError(w, cacheErr)
		return
	}
	w.Header().Set("ETag", `"`+string(req.key)+`-init"`)
	w.Header().Set("Content-Type", hlsMediaType)
	if variant != nil {
		if c, ok := variant.Open("init.mp4"); ok {
			defer c.File.Close()
			s.met.TTFB.Observe(time.Since(reqStart).Seconds())
			http.ServeContent(w, r, "", c.ModTime, c.File)
			return
		}
	}
	init, err := s.initSegmentFor(req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	cause := s.hlsBroken.cause(req.key)
	if variant != nil && cause == nil {
		switch err := variant.WriteFile("init.mp4", init); waxerr.CodeOf(err) {
		case "":
		case waxerr.CodeOutputUnwritable:
			s.hlsBroken.mark(req.key, err)
			cause = err
		default:
			s.log.Warn("hls: caching init header failed", "key", req.key, "err", err)
		}
	}
	if cause != nil {
		level := slog.LevelDebug
		if s.hlsBroken.warnOnce(req.key, hlsInitUncached) {
			level = slog.LevelWarn
		}
		s.log.Log(r.Context(), level, hlsInitUncached, "key", req.key, "src", source.Ref(req.members[0].Ref), "err", cause)
	}
	s.met.TTFB.Observe(time.Since(reqStart).Seconds())
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(init))
}

// initSegmentFor builds the variant's init header on whichever rung the plan
// chose, so the handler's on-the-spot build and the worker's cannot disagree
// about what describes the segments. A remuxed variant's header carries the
// source's own sample entry; a transcoded one's carries the encoder's.
func (s *Server) initSegmentFor(req *hlsRequest) ([]byte, error) {
	switch req.rung() {
	case rungRemux:
		return s.eng.RemuxInitSegment(req.remux)
	case rungCut:
		return s.eng.CutInitSegment(req.cut)
	default:
		return s.eng.InitSegment(req.plan, req.opts)
	}
}

// hlsVariant opens req's variant in the cache. A cache that cannot hold it
// leaves it nil with the refusal, which marks the variant. While the mark
// stands and the store has no such variant, making it is tried again only
// once a probe is due.
func (s *Server) hlsVariant(req *hlsRequest) (*cache.Variant, error) {
	if cause := s.hlsBroken.cause(req.key); cause != nil && !s.store.Contains(req.key) && !s.hlsBroken.due(req.key) {
		return nil, cause
	}
	variant, err := s.store.HLS(req.key, req.meta)
	if waxerr.CodeOf(err) == waxerr.CodeOutputUnwritable {
		s.hlsBroken.mark(req.key, err)
	}
	return variant, err
}

// handleHLSSegment serves one media segment: cache hit, wait on the
// running worker when it is close, else restart a worker at the segment
// (decoder pre-rolled sample-exact, encoder primed). While the cache cannot
// be written the segment is made for this request alone.
func (s *Server) handleHLSSegment(w http.ResponseWriter, r *http.Request) {
	reqStart := time.Now()
	s.applyCORS(w, r)
	numStr, isSeg := strings.CutSuffix(r.PathValue("seg"), ".m4s")
	n, err := strconv.ParseInt(numStr, 10, 64)
	if !isSeg || err != nil || n < 0 {
		s.writeError(w, waxerr.New(waxerr.CodeNotFound, "segment URLs are seg/<n>.m4s"))
		return
	}
	req, err := s.prepareHLS(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer req.Close()
	if n >= req.plan.Segments {
		s.writeError(w, waxerr.New(waxerr.CodeNotFound,
			fmt.Sprintf("segment %d past the end (%d segments)", n, req.plan.Segments)))
		return
	}
	variant, cacheErr := s.hlsVariant(req)
	if cacheErr != nil && waxerr.CodeOf(cacheErr) != waxerr.CodeOutputUnwritable {
		s.writeError(w, cacheErr)
		return
	}
	name := segmentName(n)
	w.Header().Set("ETag", `"`+string(req.key)+"-"+name+`"`)
	w.Header().Set("Content-Type", hlsMediaType)

	if variant == nil {
		// The cache cannot hold the variant, so no worker can fill it.
		s.serveHLSSegmentUncached(w, r, req, n, cacheErr, nil, reqStart)
		return
	}
	ask := func(ctx context.Context, probe *hlsProbe) error {
		return s.hlsMgr.Segment(ctx, string(req.key), n, hls.Ops{
			Has:   func(i int64) bool { return variant.Has(segmentName(i)) },
			Spawn: s.hlsSpawner(req, variant, probe),
		})
	}
	// probe has a worker try the cache, for no longer than the probe's
	// bound, and ends a worker that has not written by then or by the time
	// the listener leaves. A worker that wrote as the bound passed healed
	// the variant all the same. The first probe of an outage given up at
	// the bound is a Warn: a write that hangs says nothing else until it
	// ends.
	probe := func() (healed bool) {
		wait := s.hlsBroken.probeWait(req.plan)
		ctx, cancel := s.hlsBroken.withDeadline(r.Context(), wait)
		defer cancel()
		var worker hlsProbe
		if err := ask(ctx, &worker); err == nil || worker.end() {
			return true
		}
		if ctx.Err() != nil && r.Context().Err() == nil {
			level := slog.LevelDebug
			if s.hlsBroken.warnOnce(req.key, hlsProbeGivenUp) {
				level = slog.LevelWarn
			}
			s.log.Log(r.Context(), level, hlsProbeGivenUp, "key", req.key, "segment", n,
				"src", source.Ref(req.members[0].Ref), "wait", wait)
		}
		return false
	}
	// Two passes: eviction can race the manager's ground-truth check, so
	// one honest re-drive; a second miss is a real failure.
	for range 2 {
		if c, ok := variant.Open(name); ok {
			defer c.File.Close()
			// A hit never reaches the manager, so it says the listener is
			// still there.
			s.hlsMgr.Touch(string(req.key))
			s.met.HLSSegments.Add(1)
			s.met.TTFB.Observe(time.Since(reqStart).Seconds())
			http.ServeContent(w, r, "", c.ModTime, c.File)
			return
		}
		if cause := s.hlsBroken.cause(req.key); cause != nil {
			// Known to refuse writes: the request takes the variant's turn
			// before anything else. A probe that finds the cache healed
			// leaves the segment on disk for the next pass.
			if !s.serveHLSSegmentUncached(w, r, req, n, cause, probe, reqStart) {
				return
			}
			continue
		}
		err := ask(r.Context(), nil)
		switch {
		case err == nil:
		case r.Context().Err() != nil:
			return // the client left while waiting; nobody reads an envelope
		case waxerr.CodeOf(err) == waxerr.CodeOutputUnwritable:
			// A worker could not write the segment, or none was started
			// for a variant already marked.
			s.serveHLSSegmentUncached(w, r, req, n, err, nil, reqStart)
			return
		default:
			s.writeError(w, err)
			return
		}
	}
	s.writeError(w, waxerr.New(waxerr.CodeInternal, "segment evicted while being served"))
}

// errOneSegment ends a run that was started for a single segment.
var errOneSegment = errors.New("hls: the one segment is made")

// serveHLSSegmentUncached answers segment n from memory: what a listener gets
// while the cache cannot be written, cause being why. The body goes out
// whole and is not to be stored, as a degraded progressive stream's is. It
// reports true, having written nothing, when probe found the cache healed.
func (s *Server) serveHLSSegmentUncached(w http.ResponseWriter, r *http.Request, req *hlsRequest, n int64, cause error,
	probe func() bool, reqStart time.Time) (healed bool) {
	ctx := r.Context()
	data, healed, err := s.hlsSegmentUncached(ctx, req, n, probe)
	switch {
	case ctx.Err() != nil:
		return false // the client left; nobody reads an answer
	case err != nil:
		s.writeError(w, err)
		return false
	case healed:
		return true
	}
	// A variant says so once at Warn; a full disk serves many segments.
	level := slog.LevelDebug
	if s.hlsBroken.warnOnce(req.key, hlsServedUncached) {
		level = slog.LevelWarn
	}
	s.log.Log(ctx, level, hlsServedUncached, "key", req.key, "segment", n, "src", source.Ref(req.members[0].Ref),
		"rung", rungName(req.rung()), "members", len(req.members), "err", cause)
	s.met.HLSSegments.Add(1)
	s.met.Degradations.Add(1)
	s.met.TTFB.Observe(time.Since(reqStart).Seconds())
	h := w.Header()
	// No validator: a conditional request would cost the encode it asks to skip.
	h.Del("ETag")
	h.Set("Accept-Ranges", "none")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method != http.MethodHead {
		w.Write(data)
	}
	return false
}

// hlsSegmentUncached makes segment n for one request: the rung a worker
// would run, started at n and stopped after n, on one live slot. All of it
// happens under the variant's turn, so a variant holds one live slot at a
// time whoever asks, and a request waiting its turn for the segment just
// made takes those bytes.
//
// probe, when the variant is due one, has a worker try the segment first,
// under the same turn: a write that succeeds heals the variant, reported
// as healed with the segment on disk. A probe that fails in any way costs
// the listener its bound at most, and the segment is made here as between
// probes.
func (s *Server) hlsSegmentUncached(ctx context.Context, req *hlsRequest, n int64, probe func() bool) (data []byte, healed bool, err error) {
	limit := s.hlsMgr.WaitTimeout
	if limit <= 0 {
		limit = hls.DefaultWaitTimeout
	}
	turn, err := s.hlsUncached.enter(ctx, req.key, limit)
	if err != nil {
		return nil, false, err
	}
	defer s.hlsUncached.leave(req.key, turn)
	if turn.data != nil && turn.n == n {
		return turn.data, false, nil
	}
	if probe != nil && s.hlsBroken.probeDue(req.key) {
		if probe() {
			return nil, true, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, false, err // the listener left during the probe
		}
	}
	release, ok := s.pools.AcquireLive()
	if !ok {
		s.met.AdmissionRejects.Add(1)
		return nil, false, waxerr.New(waxerr.CodeOverloaded, "live transcode slots are full")
	}
	// Deferred, so a run that panics gives the slot back.
	defer release()
	_, err = s.runHLSRung(ctx, req.members, req.desc.Tl != "", descSpan(req.desc), req.variantPlan, nil, n,
		func(seg mp4.Segment) error {
			if gate := s.hooks.hlsPublish; gate != nil {
				gate(ctx, seg.Index)
			}
			if seg.Index != n {
				return waxerr.New(waxerr.CodeInternal, fmt.Sprintf("hls: a run started at segment %d made segment %d", n, seg.Index))
			}
			data = bytes.Clone(seg.Data)
			return errOneSegment
		})
	switch {
	case errors.Is(err, errOneSegment):
		turn.n, turn.data = n, data
		return data, false, nil
	case err != nil:
		return nil, false, err
	default:
		return nil, false, waxerr.New(waxerr.CodeNotFound, fmt.Sprintf("hls: segment %d is past the end of the stream", n))
	}
}

// uncachedTurns gives each variant one turn at making a segment for a
// request. The zero value is ready.
type uncachedTurns struct {
	mu   sync.Mutex
	keys map[cache.Key]*uncachedTurn
}

// uncachedTurn is one variant's turn. Its holder alone reads and sets the
// segment the last run made, which lives while requests still wait.
type uncachedTurn struct {
	held    chan struct{}
	waiting int
	n       int64
	data    []byte
}

// enter waits for key's turn, for ctx to end, or for limit to pass, which is
// answered as a full pool is.
func (u *uncachedTurns) enter(ctx context.Context, key cache.Key, limit time.Duration) (*uncachedTurn, error) {
	u.mu.Lock()
	turn := u.keys[key]
	if turn == nil {
		if u.keys == nil {
			u.keys = make(map[cache.Key]*uncachedTurn)
		}
		turn = &uncachedTurn{held: make(chan struct{}, 1)}
		u.keys[key] = turn
	}
	turn.waiting++
	u.mu.Unlock()
	late := time.NewTimer(limit)
	defer late.Stop()
	select {
	case turn.held <- struct{}{}:
		return turn, nil
	case <-ctx.Done():
		u.forget(key, turn)
		return nil, ctx.Err()
	case <-late.C:
		u.forget(key, turn)
		return nil, waxerr.New(waxerr.CodeOverloaded, "hls: timed out waiting to make the segment")
	}
}

// leave ends the holder's turn.
func (u *uncachedTurns) leave(key cache.Key, turn *uncachedTurn) {
	<-turn.held
	u.forget(key, turn)
}

func (u *uncachedTurns) forget(key cache.Key, turn *uncachedTurn) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if turn.waiting--; turn.waiting == 0 {
		delete(u.keys, key)
	}
}

// brokenVariants remembers the variants whose cache refused a write: why,
// when a write was last tried, and which log lines said so at Warn, so a
// cache that stays unwritable warns once a variant and not once a segment.
// A worker's next good write forgets the variant: a second outage is news.
// Marking a variant and warning once are separate: only a refused write
// marks. The zero value is ready.
//
// At most max variants are remembered. Past that the oldest is dropped
// and the outage is taken to be the store's until a worker's write lands:
// a variant not remembered is then served as one known to refuse writes,
// on one probe schedule and one Warn shared by all of them. After such a
// write, the next dropped variant asked for pays one failing worker, whose
// mark brings that state back.
type brokenVariants struct {
	mu   sync.Mutex
	seen *lru.Cache[cache.Key, brokenVariant]
	// spill stands for every variant dropped from seen in this outage.
	spill *brokenVariant
	// now is the clock, time.Now when nil.
	now func() time.Time
	// wait bounds a probe, hlsProbeWait when zero.
	wait time.Duration
	// deadline makes a probe's context from its request's and its bound,
	// context.WithTimeout when nil.
	deadline func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	// max is how many variants are remembered, hlsBrokenMax when zero.
	max int
}

type brokenVariant struct {
	cause  error
	said   []string
	probed time.Time // when a write was last tried
}

// hlsBrokenMax is how many variants known to refuse writes are remembered.
const hlsBrokenMax = 1024

func (b *brokenVariants) withDeadline(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if b.deadline != nil {
		return b.deadline(parent, d)
	}
	return context.WithTimeout(parent, d)
}

// probeWait is the bound of a probe of plan's variant: the floor, or the
// duration of one of its segments when that is longer.
func (b *brokenVariants) probeWait(plan *waxflow.SegmentPlan) time.Duration {
	wait := hlsProbeWait
	if b.wait > 0 {
		wait = b.wait
	}
	if plan != nil && plan.Format.Rate > 0 {
		wait = max(wait, time.Duration(plan.SegmentSamples)*time.Second/time.Duration(plan.Format.Rate))
	}
	return wait
}

func (b *brokenVariants) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// mark records that key's cache refused a write just now, with cause. A
// cause that is no refused write marks nothing.
func (b *brokenVariants) mark(key cache.Key, cause error) {
	if waxerr.CodeOf(cause) != waxerr.CodeOutputUnwritable {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	limit := cmp.Or(b.max, hlsBrokenMax)
	if b.seen == nil {
		b.seen = lru.New[cache.Key, brokenVariant](limit)
	}
	now := b.clock()
	v, known := b.seen.Get(key)
	if !known {
		v.cause = cause
		if b.seen.Len() >= limit {
			// Full: the oldest goes, and what it said the outage has said.
			_, dropped, _ := b.seen.RemoveOldest()
			if b.spill == nil {
				b.spill = &brokenVariant{cause: cause}
			}
			for _, line := range dropped.said {
				if !slices.Contains(b.spill.said, line) {
					b.spill.said = append(b.spill.said, line)
				}
			}
			b.spill.probed = now
		}
	}
	v.probed = now
	b.seen.Put(key, v)
}

// held is what is remembered of key: its own entry, or the one standing
// for the variants dropped. Locked.
func (b *brokenVariants) held(key cache.Key) (v *brokenVariant, own bool) {
	if b.seen != nil {
		if v, known := b.seen.Get(key); known {
			return &v, true
		}
	}
	return b.spill, false
}

// warnOnce reports whether line has not been said in key's outage, and
// counts it said. It marks nothing.
func (b *brokenVariants) warnOnce(key cache.Key, line string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	said := false
	if v, own := b.held(key); own {
		if said = slices.Contains(v.said, line); !said {
			v.said = append(v.said, line)
			b.seen.Put(key, *v)
		}
	}
	if b.spill != nil {
		if slices.Contains(b.spill.said, line) {
			said = true
		} else {
			b.spill.said = append(b.spill.said, line)
		}
	}
	return !said
}

// cause is the refused write that began key's outage, nil for a variant
// not known to refuse writes.
func (b *brokenVariants) cause(key cache.Key) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v, _ := b.held(key); v != nil {
		return v.cause
	}
	return nil
}

// due reports whether a write should be tried for key again: at most once
// every hlsProbeEvery for a variant known to refuse them, and at once for
// any other.
func (b *brokenVariants) due(key cache.Key) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, _ := b.held(key)
	return v == nil || b.clock().Sub(v.probed) >= hlsProbeEvery
}

// probeDue is due, counting the try as made.
func (b *brokenVariants) probeDue(key cache.Key) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, own := b.held(key)
	if v == nil {
		return true
	}
	now := b.clock()
	if now.Sub(v.probed) < hlsProbeEvery {
		return false
	}
	v.probed = now
	if own {
		b.seen.Put(key, *v)
	}
	return true
}

// forget drops what is remembered of key, and of the variants dropped:
// a write landed, so the store takes them.
func (b *brokenVariants) forget(key cache.Key) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen != nil {
		b.seen.Delete(key)
	}
	b.spill = nil
}

// hlsProbeWait is the least a probe is given. A worker's first segment takes
// tens of milliseconds, a few hundred behind a seek that walks, so two
// seconds tells a write that hangs from one that is slow, and the probing
// listener still has its segment well inside a player's patience. A variant
// whose segments last longer is given that long: no host that can serve the
// stream needs more than a segment's duration to make one.
const hlsProbeWait = 2 * time.Second

// hlsProbe is the worker a probe started, if it started one, and whether
// it wrote before the probe ended.
type hlsProbe struct {
	mu      sync.Mutex
	stop    func()
	written bool
}

// wrote records the worker's first good write.
func (p *hlsProbe) wrote() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.written = true
}

func (p *hlsProbe) started(stop func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stop = stop
}

// end stops the probe's worker and gives its live slot back at once,
// unless the worker has written: it then reports true and leaves a healthy
// worker alone.
func (p *hlsProbe) end() (written bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.written {
		return true
	}
	if p.stop != nil {
		p.stop()
		p.stop = nil
	}
	return false
}

// errProbeEnded is the cause a probe's worker is canceled with.
var errProbeEnded = errors.New("hls: the probe ended before its worker wrote")

// hlsProbeEvery is how often a worker is tried for a variant known to
// refuse writes: how long a healed cache can go unnoticed, against how
// often a broken one pays for a worker that fails. Ten seconds is two or
// three segments of listening.
const hlsProbeEvery = 10 * time.Second

// hlsInitUncached is the log line of an init header served with no cache.
const hlsInitUncached = "hls: init header served uncached"

// hlsWorkerRefused names a worker's refused write among the lines an
// outage says once.
const hlsWorkerRefused = "hls worker"

// hlsProbeGivenUp is the log line of a probe given up at its bound.
const hlsProbeGivenUp = "hls: probe given up"

// hlsServedUncached is the log line of a segment made for one request.
const hlsServedUncached = "hls: segment served uncached"

func segmentName(n int64) string { return fmt.Sprintf("seg-%d.m4s", n) }

// hlsSpawner builds the manager's Spawn callback: admission (workers
// hold live slots), pinning (gc must not evict a variant mid-fill), and
// the worker goroutine bounded by the server's base context (workers
// outlive their requests, like read-behind pipelines).
//
// A variant known to refuse writes gets a worker from its probe alone: any
// other request for one, probe being nil, is refused with the variant's
// cause, a refused write as the mark only takes one, taking no slot. A
// probe is handed the means to end its worker and is told of its first
// good write.
func (s *Server) hlsSpawner(req *hlsRequest, variant *cache.Variant, probe *hlsProbe) func(int64, func(int64), func(error)) (context.CancelCauseFunc, error) {
	// The worker outlives the request; capture values, never req.srcs.
	members, tl := req.members, req.desc.Tl != ""
	vp, key := req.variantPlan, req.key
	sp := descSpan(req.desc)
	return func(start int64, notify func(int64), exit func(error)) (context.CancelCauseFunc, error) {
		if cause := s.hlsBroken.cause(key); cause != nil && probe == nil {
			return nil, cause
		}
		release, ok := s.pools.AcquireLive()
		if !ok {
			s.met.AdmissionRejects.Add(1)
			return nil, waxerr.New(waxerr.CodeOverloaded, "live transcode slots are full")
		}
		if !s.startWork() {
			release()
			return nil, waxerr.New(waxerr.CodeOverloaded, "the daemon is shutting down")
		}
		ctx, cancel := context.WithCancelCause(s.baseCtx)
		// An idle cancel gives the slot back at once, stalled worker or not; a
		// restart keeps it for the seek waiting on this worker's exit.
		stop := func(cause error) {
			if errors.Is(cause, hls.ErrIdle) {
				release()
			}
			cancel(cause)
		}
		if probe != nil {
			// Ended by its request, the slot comes back at once, as on
			// an idle cancel.
			probe.started(func() {
				release()
				cancel(errProbeEnded)
			})
		}
		s.store.Pin(key)
		s.met.SessionsHLS.Add(1)
		s.met.SessionsActive.Add(1)
		go func() {
			defer s.wg.Done()
			// The session stops counting last, so idle means retired.
			defer s.met.SessionsActive.Add(-1)
			// Unpinned only after exit: the manager reads a running worker's
			// progress as proof its segments are on disk.
			defer s.store.Unpin(key)
			var wrote func()
			if probe != nil {
				wrote = probe.wrote
			}
			err := s.runHLSWorker(ctx, key, members, tl, sp, vp, variant, start, notify, wrote)
			if err != nil && errors.Is(context.Cause(ctx), hls.ErrIdle) {
				s.met.AbandonedHLS.Add(1)
			}
			// The slot goes before exit wakes the waiters, one of which may
			// spawn the next worker into it at once.
			cancel(nil)
			release()
			exit(err)
		}()
		return stop, nil
	}
}

// logHLSWorkerResult records a finished worker and passes its error through,
// so every rung reports the same way and a session can be filtered by which
// one ran it. res is the run's result on the rung that has one. A variant
// worker has no caller to warn, so the log line is the whole surface; the
// clipped/truePeak keys appear only when the rung measured (the count spans
// the priming feed, hence per worker, not summed).
func (s *Server) logHLSWorkerResult(ctx context.Context, key cache.Key, ref, rung string, members int, start int64,
	res *waxflow.SegmentedResult, err error) error {
	if err != nil {
		// A seek restart cancels the worker it replaces; that is routine.
		level, why := stopReason(ctx, err)
		// A refused write makes the variant known to refuse them, and only
		// the first of an outage is a Warn: a probe refused again is routine.
		if level == slog.LevelWarn && waxerr.CodeOf(err) == waxerr.CodeOutputUnwritable {
			s.hlsBroken.mark(key, err)
			if !s.hlsBroken.warnOnce(key, hlsWorkerRefused) {
				level = slog.LevelDebug
			}
		}
		s.log.Log(ctx, level, "hls worker "+why, "src", source.Ref(ref), "rung", rung, "members", members, "start", start, "err", err)
		return err
	}
	if res == nil {
		s.log.Debug("hls worker finished", "src", source.Ref(ref), "rung", rung, "members", members, "start", start)
		return nil
	}
	s.log.Debug("hls worker finished", "src", source.Ref(ref), "rung", rung, "members", members,
		"start", start, "clipped", res.ClippedSamples, "truePeak", res.TruePeak)
	return nil
}

// runHLSRemuxWorker is one variant worker on the ladder's middle rung: the
// source's own packets, resegmented, with no decoder and no encoder anywhere in
// it.
//
// It re-resolves and re-checks identity exactly as the transcoding worker does
// through openHLSMedia, and for the same reason: a worker outlives its request,
// so it cannot borrow a handle, and the file it opens must still be the file the
// plan was computed against or its segments describe something else.
//
// It takes one source by construction. A timeline and a span both decline this
// rung at plan time (see planHLSVariant), so there is no member list to walk and
// no window to apply.
func (s *Server) runHLSRemuxWorker(ctx context.Context, m memberSource, opts waxflow.TranscodeOptions,
	plan *waxflow.RemuxSegmentPlan, variant *cache.Variant, start int64, publish func(mp4.Segment) error) error {
	src, err := s.resolver.Resolve(ctx, m.Ref)
	if err != nil {
		return err
	}
	defer src.Close()
	if src.ID != m.ID {
		return waxerr.New(waxerr.CodeSourceChanged, "source changed while starting the worker")
	}
	if variant != nil && !variant.Has("init.mp4") {
		// The source's own sample entry, not an encoder's: the packets these
		// segments carry are the ones this header must describe.
		init, err := s.eng.RemuxInitSegment(plan)
		if err != nil {
			return err
		}
		if err := variant.WriteFile("init.mp4", init); err != nil {
			return err
		}
	}
	s.met.Remuxes.Add(1)
	_, err = s.eng.RemuxSegments(ctx, src, m.Ext, opts,
		waxflow.SegmentedOptions{SegmentSamples: plan.SegmentSamples, StartSegment: start}, publish)
	return err
}

// runHLSCutWorker is one variant worker on the cut rung: a span of the source's
// own packets, resegmented, with no decoder and no encoder. It is the segmented
// sibling of the progressive cut worker and the mirror of runHLSRemuxWorker,
// re-resolving and re-checking identity for the same reason (a worker outlives
// its request and cannot borrow a handle). It takes one source by construction: a
// timeline never reaches the cut rung.
//
// The span, grid, and source length are the plan's own, threaded to CutSegments
// so the worker cuts on the boundaries and against the length the cache key was
// computed for. The init segment is the cut's, carrying the synthesized trims the
// source never had.
func (s *Server) runHLSCutWorker(ctx context.Context, m memberSource, opts waxflow.TranscodeOptions, sp span,
	plan *waxflow.CutSegmentPlan, variant *cache.Variant, start int64, publish func(mp4.Segment) error) error {
	src, err := s.resolver.Resolve(ctx, m.Ref)
	if err != nil {
		return err
	}
	defer src.Close()
	if src.ID != m.ID {
		return waxerr.New(waxerr.CodeSourceChanged, "source changed while starting the worker")
	}
	if variant != nil && !variant.Has("init.mp4") {
		init, err := s.eng.CutInitSegment(plan)
		if err != nil {
			return err
		}
		if err := variant.WriteFile("init.mp4", init); err != nil {
			return err
		}
	}
	s.met.Cuts.Add(1)
	_, err = s.eng.CutSegments(ctx, src, m.Ext, opts,
		[]waxflow.Span{{From: sp.from, To: sp.end()}}, plan.Grid, plan.Source,
		waxflow.SegmentedOptions{SegmentSamples: plan.SegmentSamples, StartSegment: start}, publish)
	return err
}

// runHLSWorker is one variant worker: its own input, opened afresh, the init
// header if the variant lacks one, then segments from start to end of
// stream, each published atomically and announced. wrote, when set, is told
// of each good write before it is announced.
func (s *Server) runHLSWorker(ctx context.Context, key cache.Key, members []memberSource, tl bool, sp span, vp variantPlan,
	variant *cache.Variant, start int64, notify func(int64), wrote func()) error {
	publish := func(seg mp4.Segment) error {
		if gate := s.hooks.hlsPublish; gate != nil {
			gate(ctx, seg.Index)
		}
		if err := variant.WriteFile(segmentName(seg.Index), seg.Data); err != nil {
			return err
		}
		// The probe hears of the write first: its bound may pass at any
		// moment, and must find a worker that wrote.
		if wrote != nil {
			wrote()
		}
		// The cache takes this variant's writes: its next refusal warns.
		s.hlsBroken.forget(key)
		if gate := s.hooks.hlsPublished; gate != nil {
			gate(ctx, seg.Index)
		}
		notify(seg.Index)
		return nil
	}
	res, err := s.runHLSRung(ctx, members, tl, sp, vp, variant, start, publish)
	return s.logHLSWorkerResult(ctx, key, members[0].Ref, rungName(vp.rung()), len(members), start, res, err)
}

// runHLSRung produces segments from start on the plan's rung, handing each to
// publish, until the stream ends or publish refuses one. A nil variant is a
// run with no cache behind it, which writes no init header.
func (s *Server) runHLSRung(ctx context.Context, members []memberSource, tl bool, sp span, vp variantPlan,
	variant *cache.Variant, start int64, publish func(mp4.Segment) error) (*waxflow.SegmentedResult, error) {
	// The rung is the plan's, passed rather than re-derived, so the bytes a
	// run makes are the ones the playlist's segment count and the cache key
	// were computed for.
	switch vp.rung() {
	case rungRemux:
		// Ladder rung 2: the source's own packets, resegmented.
		return nil, s.runHLSRemuxWorker(ctx, members[0], vp.opts, vp.remux, variant, start, publish)
	case rungCut:
		// Between rung 2 and rung 3: a span whose codec survives being moved.
		return nil, s.runHLSCutWorker(ctx, members[0], vp.opts, sp, vp.cut, variant, start, publish)
	default:
		return s.runHLSTranscodeWorker(ctx, members, tl, sp, vp.copts, vp.opts, vp.plan, variant, start, publish)
	}
}

// runHLSTranscodeWorker is one variant worker on rung 3: the input decoded,
// the init header if the variant lacks one, then the encoded segments.
func (s *Server) runHLSTranscodeWorker(ctx context.Context, members []memberSource, tl bool, sp span,
	copts waxflow.ConcatOptions, opts waxflow.TranscodeOptions, plan *waxflow.SegmentPlan, variant *cache.Variant,
	start int64, publish func(mp4.Segment) error) (*waxflow.SegmentedResult, error) {
	med, err := s.openHLSMedia(ctx, members, tl, sp, copts)
	if err != nil {
		return nil, err
	}
	defer med.Close()
	if variant != nil && !variant.Has("init.mp4") {
		init, err := s.eng.InitSegment(plan, opts)
		if err != nil {
			return nil, err
		}
		if err := variant.WriteFile("init.mp4", init); err != nil {
			return nil, err
		}
	}
	return s.eng.TranscodeSegmentsMedia(ctx, med, opts,
		waxflow.SegmentedOptions{SegmentSamples: plan.SegmentSamples, StartSegment: start},
		publish)
}

// openHLSMedia opens a worker's input: the source itself for a single-track
// stream, a lazily opened timeline for a multi-source one, built with the
// copts its plan was (a single-track stream has no seam and ignores them).
// Both are one format.Media, so the segmented engine has a single entry point
// behind the branch.
func (s *Server) openHLSMedia(ctx context.Context, members []memberSource, tl bool,
	sp span, copts waxflow.ConcatOptions) (format.Media, error) {
	if !tl {
		return s.openSlicedMember(ctx, members[0], -1, sp)
	}
	return s.openTimelineMedia(ctx, members, copts)
}

// openTimelineMedia opens members as one timeline whose members open lazily,
// each as its own stored window (the zero pair is the whole file), so the media
// Concat reads matches the narrowed track the plan projected. copts must be
// the ones the plan was built with.
//
// ctx is the run's, which is derived from the server's base context and not
// from any request. That is the rule a timeline makes easy to break: its
// members open mid-stream, minutes after the request that planned them
// returned, so a request context threaded in here would kill a member's
// first read the moment the client disconnected, which is precisely what
// read-behind exists not to do.
func (s *Server) openTimelineMedia(ctx context.Context, members []memberSource,
	copts waxflow.ConcatOptions) (format.Media, error) {
	srcs := make([]waxflow.ConcatSource, len(members))
	for i, m := range members {
		srcs[i] = waxflow.ConcatSource{
			Track: m.Track,
			Open: func() (format.Media, error) {
				return s.openSlicedMember(ctx, m, i, span{from: m.From, to: m.To})
			},
		}
	}
	return waxflow.Concat(srcs, copts)
}

// openSlicedMember opens m, bounded to sp when sp narrows it: a timeline
// member's own stored window, or the descriptor span on a single source.
// The zero span opens the member untouched. index is openMember's.
func (s *Server) openSlicedMember(ctx context.Context, m memberSource, index int, sp span) (format.Media, error) {
	med, err := s.openMember(ctx, m, index)
	if err != nil || !sp.narrowed() {
		return med, err
	}
	// Against the measured total the mint and plan validated the window with,
	// so a window edge in an advisory gap opens here too.
	return waxflow.SliceMeasured(ctx, med, m.SrcTrack, sp.from, sp.end())
}

// openMember resolves one source, enforces the identity the plan was made
// against, and opens it. The returned Media owns the handle, so closing it
// releases the descriptor: that is what makes a timeline's lazy opening
// worth having, since a 500-track queue then holds one open file and not
// 500. index is m's place in a timeline, negative for a single source: a
// timeline member's close error names it.
func (s *Server) openMember(ctx context.Context, m memberSource, index int) (format.Media, error) {
	f, err := s.resolver.Resolve(ctx, m.Ref)
	if err != nil {
		return nil, err
	}
	if f.ID != m.ID {
		f.Close()
		return nil, waxerr.New(waxerr.CodeSourceChanged, "source changed since the stream was planned")
	}
	med, err := s.eng.OpenStream(f, f.Ext)
	if err != nil {
		f.Close()
		return nil, err
	}
	var c io.Closer = f
	if index >= 0 {
		c = f.NamedCloser(fmt.Sprintf("member %d", index))
	}
	member, err := format.WithCloser(med, c)
	if err != nil {
		med.Close()
		f.Close()
		return nil, err
	}
	return member, nil
}

// mintHLSDescriptor builds the canonical descriptor from raw mint
// parameters (the keyed master form and POST /sign), resolving the
// source identity. The encode/decode round trip applies the descriptor's
// own validation, so mint-time junk is rejected exactly like
// playback-time junk. It also returns the source duration in seconds
// (-1 unknown) for the TTL policy.
func (s *Server) mintHLSDescriptor(ctx context.Context, params SignParams) (hls.Descriptor, float64, error) {
	bad := func(format string, args ...any) (hls.Descriptor, float64, error) {
		return hls.Descriptor{}, 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(format, args...))
	}
	for k := range params {
		if !hlsMintParamNames[k] {
			return bad("unknown parameter %q", k)
		}
	}
	// By presence, as /stream judges the pair: an empty src beside a tl, or an
	// empty tl, is refused, not dropped.
	src, hasSrc := params.src()
	tl, hasTl := params["tl"]
	d := hls.Descriptor{Ver: hls.DescriptorVersion, Src: source.Ref(src), Tl: tl,
		Gain: params["gain"], Dynamics: params["dynamics"]}
	switch {
	case hasTl && tl == "":
		return bad("tl is present but empty; omit it to stream one src")
	case d.Src == "" && d.Tl == "":
		return bad("src or tl is required")
	case hasSrc && d.Tl != "":
		return bad("src and tl are exclusive: a URL names one stream or one timeline")
	}
	get := func(name string) (string, bool) {
		v, ok := params[name]
		return v, ok
	}
	// q is not a mint parameter (the check above refused it), so it is empty.
	shape, _, err := outshape.ParseQuery(get)
	if err != nil {
		return hls.Descriptor{}, 0, err
	}
	if shape.Format == "" {
		shape.Format = waxflow.SegmentedFormats()[0]
	}
	setDescriptorShape(&d, shape)
	// The span is parsed by the same function /stream uses, so the two
	// surfaces cannot come to disagree about what from and to mean.
	sp, err := parseSpan(get)
	if err != nil {
		return hls.Descriptor{}, 0, err
	}
	d.From, d.To = sp.from, sp.to
	// Each duration is refused here when it is not finite, before anything
	// resolves; Canonical below refuses one for any float the descriptor
	// holds, since it could not be encoded.
	v, ok := get("crossfadeSeconds")
	if d.CrossfadeSeconds, err = parseSeconds("crossfadeSeconds", "for a butt-join", v, ok); err != nil {
		return hls.Descriptor{}, 0, err
	}
	v, ok = get("segDur")
	if d.SegDur, err = parseSeconds("segDur", "for the default segment duration", v, ok); err != nil {
		return hls.Descriptor{}, 0, err
	}
	if v, ok := get("bitrates"); ok {
		if v == "" {
			return bad("bitrates is present but empty; omit it for a single variant")
		}
		for part := range strings.SplitSeq(v, ",") {
			kbps, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil {
				return bad("bitrates %q: want comma-separated kbit/s values", v)
			}
			if err := hls.CheckRung(kbps); err != nil {
				return hls.Descriptor{}, 0, err
			}
			d.Bitrates = append(d.Bitrates, kbps)
		}
	}
	// The gain and dynamics spellings must parse now, not at playback.
	if _, err := parseGain(d.Gain, s.defaultGain); err != nil {
		return hls.Descriptor{}, 0, err
	}
	if _, err := parseDynamics(d.Dynamics); err != nil {
		return hls.Descriptor{}, 0, err
	}
	// What the query alone rules out is refused before the source resolves,
	// so no 404 hides it.
	if err := s.refuseHLSQuery(d); err != nil {
		return hls.Descriptor{}, 0, err
	}

	// A timeline's members already carry their identities inside the digest,
	// so only a single source needs one pinned here.
	if d.Src != "" {
		f, err := s.resolver.Resolve(ctx, string(d.Src))
		if err != nil {
			return hls.Descriptor{}, 0, err
		}
		d.ID = f.ID.String()
		f.Close()
	}

	// Round trip through the wire form: one validator for minting and
	// playback.
	out, err := d.Canonical()
	if err != nil {
		return hls.Descriptor{}, 0, err
	}
	// Every rung must plan (mirroring the master handler), so a URL that
	// mints is a URL that plays: a lossless format with a bitrate, a format
	// with no segmented form, or a queue whose members cannot be
	// concatenated fails here and not in the player.
	req := &hlsRequest{desc: out}
	defer req.Close()
	if err := s.resolveHLSSources(ctx, req); err != nil {
		return hls.Descriptor{}, 0, err
	}
	tracks := memberTracks(req.members)
	for _, kbps := range out.Ladder() {
		// Mint-time validation plans with nil metadata: the resolved gain
		// value never shapes plan validity, only playback bytes. It hands the
		// remux rung no source for the same reason: a mint validates that some
		// rung can serve the variant, and rung 3 always can wherever rung 2
		// could, so paying for a packet walk here would buy nothing.
		if _, err := s.planHLSVariant(ctx, out.Variant(kbps), tracks, nil, nil); err != nil {
			return hls.Descriptor{}, 0, err
		}
	}
	crossfade, err := waxflow.CrossfadeSamples(tracks, out.CrossfadeSeconds)
	if err != nil {
		return hls.Descriptor{}, 0, err
	}
	duration, err := hlsDuration(out, tracks, s.timelineOptions(crossfade))
	if err != nil {
		return hls.Descriptor{}, 0, err
	}
	return out, duration, nil
}
