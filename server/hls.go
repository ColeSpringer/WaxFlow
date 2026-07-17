package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/cache"
	"github.com/colespringer/waxflow/internal/hls"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/sign"
	"github.com/colespringer/waxflow/internal/timeline"
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

// measureCeiling is the past-any-stream seek target that forces a
// demuxer's exact-length walk (an IO-bound frame-index build, no
// decode): Media.SeekSample lands at the true end of stream.
const measureCeiling = int64(1) << 61

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
	"bits": true, "rate": true, "ch": true, "gain": true, "dynamics": true,
	"segDur": true, "from": true, "to": true, "crossfadeSeconds": true,
}

// hlsSource is one resolved source as a worker needs it. Workers outlive
// their requests, so they re-resolve from these values rather than borrowing
// a handle the request is about to close.
type hlsSource struct {
	Ref string
	ID  source.Identity
	Ext string
	// Track is the probed, measured track the plan was computed from. A
	// timeline hands it back to Concat, so the run's members are declared by
	// exactly what the plan projected: plan and run cannot disagree about a
	// member's format or length, because the number is not computed twice.
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
	members []hlsSource
	opts    waxflow.TranscodeOptions
	plan    *waxflow.SegmentPlan
	// remux is the ladder's rung 2 when this variant can be served by rewriting
	// the container around the source's own packets, nil when it takes rung 3.
	// plan points at its embedded SegmentPlan when it is set, so the playlist,
	// the key, and the worker all read one shape.
	remux *waxflow.RemuxSegmentPlan
	key   cache.Key
	meta  cache.Meta
	// exp is the request's own signature expiry (unix seconds), 0 when
	// key-authed; child URLs inherit it so one minting governs the whole
	// playback session's lifetime.
	exp int64
}

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
		m = s.readMeta(r.Context(), req.srcs[0], false)
		rmx = req.srcs[0]
	}
	if req.opts, req.plan, req.remux, err = s.planHLSVariant(desc, req.tracks(), m, rmx); err != nil {
		return nil, err
	}
	if req.plan.Segments > maxPlaylistSegments {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("segment duration yields %d segments (max %d); raise segDur", req.plan.Segments, maxPlaylistSegments))
	}
	canonical := canonicalHLSParams(req.plan, req.opts.GainDB, req.opts.Dynamics, descSpan(desc))
	identity := hlsIdentity(desc, req.members)
	req.key = cache.NewKey(identity, canonical, req.plan.Versions)
	req.meta = cache.Meta{
		Ref:         hlsRef(desc),
		Identity:    identity,
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
// exactly as for a single source. A timeline's digest pins its members'
// identities by covering them, so a member replaced on disk cannot match the
// digest minted against it, and the mismatch is the 410 the URL promises.
//
// A timeline's members are measured rather than trusted, which a single
// source is not. A timeline's positions are a prefix sum, so a member that
// delivers two samples fewer than its headers declare desyncs every position
// after it and makes the playlist promise segments the stream cannot fill.
// One file's advisory length is a tolerated oddity because nothing
// downstream of it depends on the number.
func (s *Server) resolveHLSSources(ctx context.Context, req *hlsRequest) error {
	tl := req.desc.Tl != ""
	refs := []timeline.Member{{Src: req.desc.Src, ID: req.desc.ID}}
	if tl {
		if s.timelines == nil {
			return waxerr.New(waxerr.CodeUnsupportedFormat,
				"multi-source timelines are not enabled on this daemon")
		}
		var err error
		if refs, err = s.timelines.Members(req.desc.Tl); err != nil {
			return err
		}
	}
	for _, ref := range refs {
		if err := s.resolveMember(ctx, req, ref, tl); err != nil {
			return err
		}
	}
	return nil
}

// resolveMember resolves one member, enforces the identity the URL pins,
// probes it, and records what the plan and the worker need.
//
// It measures for a timeline's member, and for a span. A span is the case that
// makes an advisory length load-bearing on a single source: SpanTrack checks
// to against the total, so an under-declared source refuses a legitimate span
// at mint, and an over-declared one mints a span whose segments run past the
// stream. Neither is a length nobody depended on, which is the only condition
// under which trusting the headers was free.
//
// It keeps the open handle only for a single source, whose metadata read still
// needs one. A timeline's handles are scaffolding: nothing reads a member
// again once it is checked and probed, and every position past that comes from
// the recorded track. Holding them would put one open file per member on every
// segment request, for the life of the request, which for a thousand-member
// queue is a thousand descriptors against a default limit of 1024. It would
// also undo the point of opening members lazily: the timeline exists so a queue
// of any length costs one descriptor, and eagerly holding them here is exactly
// what Concat is built not to do.
func (s *Server) resolveMember(ctx context.Context, req *hlsRequest, ref timeline.Member, tl bool) error {
	want, err := source.ParseIdentity(ref.ID)
	if err != nil {
		return err
	}
	f, err := s.resolver.Resolve(ctx, ref.Src)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			f.Close()
		}
	}()
	if want != f.ID {
		return waxerr.New(waxerr.CodeSourceChanged,
			"the source changed since this URL was minted; request a fresh one")
	}
	track, err := s.trackFor(f, tl || descSpan(req.desc).narrowed())
	if err != nil {
		return err
	}
	req.members = append(req.members, hlsSource{Ref: ref.Src, ID: f.ID, Ext: f.Ext, Track: track})
	if !tl {
		// The metadata read that resolves tag-based gain still needs it, and
		// req.Close owns it from here.
		req.srcs = append(req.srcs, f)
		keep = true
	}
	return nil
}

// tracks are the members' tracks in order: what the variant plans from.
func (req *hlsRequest) tracks() []container.Track {
	out := make([]container.Track, len(req.members))
	for i, m := range req.members {
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
	return desc.Src
}

// hlsIdentity is the ADR-0004 identity a variant's entries key on: the
// source's own for a single-track stream, the digest for a timeline.
//
// The digest is enough by itself, which is the whole point of content-
// addressing it. It covers every member's reference and identity, so any
// change to any member is a different digest and therefore a different key,
// and there is no second list identity that could disagree with it.
func hlsIdentity(desc hls.Descriptor, members []hlsSource) string {
	if desc.Tl != "" {
		return "tl:" + desc.Tl
	}
	return identityString(desc.Src, members[0].ID)
}

// measureSamples forces a source's exact length: seek past any possible
// end and read where the stream really stops.
func (s *Server) measureSamples(src *source.File) (int64, error) {
	med, err := s.eng.OpenStream(src, src.Ext)
	if err != nil {
		return 0, err
	}
	defer med.Close()
	return med.SeekSample(measureCeiling)
}

// planHLSVariant maps one variant descriptor onto engine options and its
// segment plan, mirroring planTranscode's policy checks so /stream and
// HLS cannot drift. tracks is the single source's track, or the timeline's
// members' tracks in order.
// rmx is the source to try the remux rung against, nil to skip it: the master
// playlist plans every variant to advertise it and runs none, so it has no
// reason to pay for a packet walk, and the rung it would find changes nothing
// it prints.
func (s *Server) planHLSVariant(desc hls.Descriptor, tracks []container.Track, m *meta.Info,
	rmx *source.File) (waxflow.TranscodeOptions, *waxflow.SegmentPlan, *waxflow.RemuxSegmentPlan, error) {
	fail := func(err error) (waxflow.TranscodeOptions, *waxflow.SegmentPlan, *waxflow.RemuxSegmentPlan, error) {
		return waxflow.TranscodeOptions{}, nil, nil, err
	}
	if desc.Bitrate != 0 {
		if lossy, known := waxflow.LossyFormat(desc.Format); known && !lossy {
			return fail(waxerr.New(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("bitrate applies to lossy output; %s is lossless", desc.Format)))
		}
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
	opts := waxflow.TranscodeOptions{
		Format:          desc.Format,
		Rate:            desc.Rate,
		Channels:        desc.Ch,
		BitDepth:        desc.Bits,
		GainDB:          g.resolveDB(m, dyn),
		Dynamics:        dyn,
		ResampleProfile: s.profile,
		MP3Bitrate:      desc.Bitrate * 1000,
		OpusBitrate:     desc.Bitrate * 1000,
	}
	// The timeline plan is not the single-source plan over a synthetic
	// track: it also carries the members' decoder revisions and the
	// normalization each member runs through, neither of which the synthetic
	// track can name. See PlanSegmentsTimeline.
	var plan *waxflow.SegmentPlan
	if desc.Tl != "" {
		// The descriptor spells the crossfade in seconds; convert it on the
		// members' fixed formats, exactly as the run's Concat will (openHLSMedia),
		// so the plan's length and the run's cannot disagree. A crossfade too long
		// for the members is refused here at plan time (checkCrossfade inside
		// ConcatTrack), so an oversized or tampered value 400s rather than serving
		// a playlist the stream cannot fill.
		var crossfade int64
		if crossfade, err = waxflow.CrossfadeSamples(tracks, desc.CrossfadeSeconds); err != nil {
			return fail(err)
		}
		plan, err = s.eng.PlanSegmentsTimeline(tracks, s.timelineOptions(crossfade), opts, desc.SegDur)
	} else {
		// The span narrows the track before it is planned, which is what
		// makes a virtual track a stream in its own right: its segment 0 is
		// the span's first sample and its segment count covers to-from, not
		// the whole rip. Planning the file's track and bounding afterward
		// would promise a playlist the span cannot fill.
		track := tracks[0]
		spanned := descSpan(desc).narrowed()
		if spanned {
			if track, err = waxflow.SpanTrack(track, descSpan(desc).from, descSpan(desc).end()); err != nil {
				return fail(err)
			}
		}
		// Ladder rung 2, the segmented spelling: rewrite the container around
		// the source's own packets. This is where format=opus on an Opus source
		// stops re-encoding, which is the case that motivated the rung.
		//
		// The two declines are structural and neither is visible to the engine,
		// which is handed a track and options. A timeline cannot remux: one
		// fMP4 timeline carries one edit list, and per-member delay and padding
		// cannot be trimmed at a seam where the packets overlap. A span cannot
		// remux: cutting at an arbitrary sample means cutting mid-packet. Both
		// always take rung 3.
		if rmx != nil && !spanned {
			rp, err := s.remuxSegmentPlanFor(rmx, track, opts, desc.SegDur)
			if err != nil {
				return fail(err)
			}
			if rp != nil {
				return opts, &rp.SegmentPlan, rp, nil
			}
		}
		plan, err = s.eng.PlanSegments(track, opts, desc.SegDur)
	}
	if err != nil {
		return fail(err)
	}
	return opts, plan, nil, nil
}

// remuxSegmentPlanFor tries the segmented remux rung for one variant, returning
// nil when the source's packets have no uniform grid to lay segment boundaries
// on and the variant must therefore be transcoded.
//
// The header-only check runs first and the packet walk second, which is the
// whole reason this is not a single call into PlanRemuxSegments. gridFor walks
// every packet in the source; PlanRemux reads the track and the options and
// nothing else. A variant that cannot remux whatever the packets look like (a
// bitrate ladder rung, a codec mismatch) must not pay for a walk to be told what
// its options already say, and a ladder is exactly where that bites: every rung
// of it declines, and every rung of it would have walked.
func (s *Server) remuxSegmentPlanFor(src *source.File, track container.Track,
	opts waxflow.TranscodeOptions, segDur float64) (*waxflow.RemuxSegmentPlan, error) {
	// An error is not this rung's to report; PlanSegments is about to hit the
	// same one with the same words.
	if rp, err := s.eng.PlanRemux(track, opts); err != nil || rp == nil {
		return nil, nil
	}
	grid, err := s.gridFor(src)
	if err != nil {
		return nil, err
	}
	if grid <= 0 {
		return nil, nil
	}
	return s.eng.PlanRemuxSegments(track, opts, segDur, grid)
}

// descSpan is the descriptor's span in the server's own spelling.
func descSpan(desc hls.Descriptor) span { return span{from: desc.From, to: desc.To} }

// canonicalHLSParams is the segmented cache key's parameter string: the
// core canonicalParams shares, plus the segment length that pins the
// numbering. The hls prefix keeps the key space disjoint from progressive
// entries.
func canonicalHLSParams(plan *waxflow.SegmentPlan, gainDB float64, dyn gain.Preset, sp span) string {
	return fmt.Sprintf("hls&%s&segSamples=%d",
		canonicalCore(&plan.TranscodePlan, gainDB, dyn, sp), plan.SegmentSamples)
}

// hlsChildExp is the expiry child URLs (media playlists, init, segments)
// are signed with: the requesting URL's own exp when it has one, so one
// minting governs the session, else the default TTL policy.
func (req *hlsRequest) childExp() time.Time {
	if req.exp > 0 {
		return time.Unix(req.exp, 0)
	}
	return time.Now().Add(sign.DefaultTTLFor(DurationSeconds(req.plan.Samples, req.plan.Format.Rate)))
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
	} else {
		if !s.keyAuthed(r) {
			s.writeError(w, waxerr.New(waxerr.CodeUnauthorized, "master.m3u8 without v= needs an API key"))
			return
		}
		params := make(map[string]string, len(q))
		for k := range q {
			params[k] = q.Get(k)
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
	tracks := req.tracks()
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

	exp := time.Now().Add(sign.DefaultTTLFor(duration))
	if sigAuthed {
		if e, _ := strconv.ParseInt(q.Get(sign.ParamExp), 10, 64); e > 0 {
			exp = time.Unix(e, 0)
		}
	}
	if desc.Tl != "" {
		// The master is a URL-minting path, like /sign: every child URL below
		// is signed with exp, so the timeline has to outlive them. This is the
		// one minting path that does not go through /sign, and without this it
		// would be the one that can hand out URLs whose timeline is swept
		// before they expire.
		s.timelines.Touch(desc.Tl, exp)
	}
	var m *meta.Info
	if desc.Tl == "" {
		m = s.readMeta(r.Context(), req.srcs[0], false)
	}
	var variants []hls.MasterVariant
	for _, kbps := range desc.Ladder() {
		vdesc := desc.Variant(kbps)
		_, plan, _, err := s.planHLSVariant(vdesc, tracks, m, nil)
		if err != nil {
			s.writeError(w, err) // one bad rung fails the master honestly
			return
		}
		variants = append(variants, hls.MasterVariant{
			URI:       "media.m3u8?" + s.hlsChildQuery("/hls/media.m3u8", vdesc, exp),
			Bandwidth: plan.Bandwidth,
			Codecs:    plan.Codecs,
		})
	}
	s.servePlaylist(w, hls.Master(variants))
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
	plan := req.plan
	exp := req.childExp()
	rate := float64(plan.Format.Rate)
	segments := make([]hls.MediaSegment, plan.Segments)
	for n := range segments {
		path := fmt.Sprintf("/hls/seg/%d.m4s", n)
		segments[n] = hls.MediaSegment{
			URI:     fmt.Sprintf("seg/%d.m4s?%s", n, s.hlsChildQuery(path, req.desc, exp)),
			Seconds: float64(plan.SegmentDuration(int64(n))) / rate,
		}
	}
	initURI := "init.mp4?" + s.hlsChildQuery("/hls/init.mp4", req.desc, exp)
	s.servePlaylist(w, hls.Media(initURI, segments))
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
// either way) and cached best effort.
func (s *Server) handleHLSInit(w http.ResponseWriter, r *http.Request) {
	reqStart := time.Now()
	s.applyCORS(w, r)
	req, err := s.prepareHLS(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer req.Close()
	variant, err := s.store.HLS(req.key, req.meta)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("ETag", `"`+string(req.key)+`-init"`)
	w.Header().Set("Content-Type", hlsMediaType)
	if c, ok := variant.Open("init.mp4"); ok {
		defer c.File.Close()
		s.met.TTFB.Observe(time.Since(reqStart).Seconds())
		http.ServeContent(w, r, "", c.ModTime, c.File)
		return
	}
	init, err := s.initSegmentFor(req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if err := variant.WriteFile("init.mp4", init); err != nil {
		s.log.Warn("hls: caching init header failed", "key", req.key, "err", err)
	}
	s.met.TTFB.Observe(time.Since(reqStart).Seconds())
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(init))
}

// initSegmentFor builds the variant's init header on whichever rung the plan
// chose, so the handler's on-the-spot build and the worker's cannot disagree
// about what describes the segments. A remuxed variant's header carries the
// source's own sample entry; a transcoded one's carries the encoder's.
func (s *Server) initSegmentFor(req *hlsRequest) ([]byte, error) {
	if req.remux != nil {
		return s.eng.RemuxInitSegment(req.remux)
	}
	return s.eng.InitSegment(req.plan, req.opts)
}

// handleHLSSegment serves one media segment: cache hit, wait on the
// running worker when it is close, else restart a worker at the segment
// (decoder pre-rolled sample-exact, encoder primed; §8 of the plan).
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
	variant, err := s.store.HLS(req.key, req.meta)
	if err != nil {
		s.writeError(w, err)
		return
	}
	name := segmentName(n)
	w.Header().Set("ETag", `"`+string(req.key)+"-"+name+`"`)
	w.Header().Set("Content-Type", hlsMediaType)

	// Two passes: eviction can race the manager's ground-truth check, so
	// one honest re-drive; a second miss is a real failure.
	for attempt := 0; attempt < 2; attempt++ {
		if c, ok := variant.Open(name); ok {
			defer c.File.Close()
			s.met.HLSSegments.Add(1)
			s.met.TTFB.Observe(time.Since(reqStart).Seconds())
			http.ServeContent(w, r, "", c.ModTime, c.File)
			return
		}
		err := s.hlsMgr.Segment(r.Context(), string(req.key), n, hls.Ops{
			Has:   func(i int64) bool { return variant.Has(segmentName(i)) },
			Spawn: s.hlsSpawner(req, variant),
		})
		if err != nil {
			s.writeError(w, err)
			return
		}
	}
	s.writeError(w, waxerr.New(waxerr.CodeInternal, "segment evicted while being served"))
}

func segmentName(n int64) string { return fmt.Sprintf("seg-%d.m4s", n) }

// hlsSpawner builds the manager's Spawn callback: admission (workers
// hold live slots), pinning (gc must not evict a variant mid-fill), and
// the worker goroutine bounded by the server's base context (workers
// outlive their requests, like read-behind pipelines).
func (s *Server) hlsSpawner(req *hlsRequest, variant *cache.Variant) func(int64, func(int64), func(error)) (context.CancelFunc, error) {
	// The worker outlives the request; capture values, never req.srcs.
	members, tl := req.members, req.desc.Tl != ""
	opts, plan, key, rmx := req.opts, req.plan, req.key, req.remux
	sp := descSpan(req.desc)
	xfade := req.desc.CrossfadeSeconds
	return func(start int64, notify func(int64), exit func(error)) (context.CancelFunc, error) {
		release, ok := s.pools.AcquireLive()
		if !ok {
			s.met.AdmissionRejects.Add(1)
			return nil, waxerr.New(waxerr.CodeOverloaded, "live transcode slots are full")
		}
		ctx, cancel := context.WithCancel(s.baseCtx)
		s.store.Pin(key)
		s.met.SessionsHLS.Add(1)
		s.met.SessionsActive.Add(1)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.met.SessionsActive.Add(-1)
			defer s.store.Unpin(key)
			defer release()
			defer cancel()
			exit(s.runHLSWorker(ctx, members, tl, sp, xfade, opts, plan, rmx, variant, start, notify))
		}()
		return cancel, nil
	}
}

// logHLSWorker records a finished worker and passes its error through, so both
// rungs report the same way and a session can be filtered by which one ran it.
func (s *Server) logHLSWorker(ref, rung string, members int, start int64, err error) error {
	if err != nil {
		s.log.Warn("hls worker failed", "src", ref, "rung", rung, "members", members, "start", start, "err", err)
	} else {
		s.log.Debug("hls worker finished", "src", ref, "rung", rung, "members", members, "start", start)
	}
	return err
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
func (s *Server) runHLSRemuxWorker(ctx context.Context, m hlsSource, opts waxflow.TranscodeOptions,
	plan *waxflow.RemuxSegmentPlan, variant *cache.Variant, start int64, publish func(mp4.Segment) error) error {
	src, err := s.resolver.Resolve(ctx, m.Ref)
	if err != nil {
		return err
	}
	defer src.Close()
	if src.ID != m.ID {
		return waxerr.New(waxerr.CodeSourceChanged, "source changed while starting the worker")
	}
	if !variant.Has("init.mp4") {
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

// runHLSWorker is one variant worker: its own input, opened afresh, the init
// header if the variant lacks one, then segments from start to end of
// stream, each published atomically and announced.
func (s *Server) runHLSWorker(ctx context.Context, members []hlsSource, tl bool, sp span, xfadeSeconds float64,
	opts waxflow.TranscodeOptions, plan *waxflow.SegmentPlan, rmx *waxflow.RemuxSegmentPlan,
	variant *cache.Variant, start int64, notify func(int64)) error {
	publish := func(seg mp4.Segment) error {
		if err := variant.WriteFile(segmentName(seg.Index), seg.Data); err != nil {
			return err
		}
		notify(seg.Index)
		return nil
	}
	ref := members[0].Ref
	// Ladder rung 2, which the plan already chose: the source's own packets,
	// resegmented. It is passed rather than re-derived so the bytes a worker
	// writes are the ones the playlist's segment count and the cache key were
	// computed for.
	if rmx != nil {
		return s.logHLSWorker(ref, rungName(rungRemux), len(members), start,
			s.runHLSRemuxWorker(ctx, members[0], opts, rmx, variant, start, publish))
	}
	med, err := s.openHLSMedia(ctx, members, tl, sp, xfadeSeconds)
	if err != nil {
		return err
	}
	defer med.Close()
	if !variant.Has("init.mp4") {
		init, err := s.eng.InitSegment(plan, opts)
		if err != nil {
			return err
		}
		if err := variant.WriteFile("init.mp4", init); err != nil {
			return err
		}
	}
	_, err = s.eng.TranscodeSegmentsMedia(ctx, med, opts,
		waxflow.SegmentedOptions{SegmentSamples: plan.SegmentSamples, StartSegment: start},
		publish)
	return s.logHLSWorker(ref, rungName(rungTranscode), len(members), start, err)
}

// openHLSMedia opens a worker's input: the source itself for a single-track
// stream, a lazily opened timeline for a multi-source one. Both are one
// format.Media, so the segmented engine has a single entry point behind the
// branch.
//
// ctx is the worker's, which is derived from the server's base context and
// not from any request. That is the rule a timeline makes easy to break: its
// members open mid-stream, minutes after the request that planned them
// returned, so a request context threaded in here would kill a member's
// first read the moment the client disconnected, which is precisely what
// read-behind exists not to do.
// xfadeSeconds is the descriptor's crossfade, converted here on the members the
// same way planHLSVariant did, so plan and run describe one blended length. It
// is ignored for a single-track stream, which has no seam.
func (s *Server) openHLSMedia(ctx context.Context, members []hlsSource, tl bool,
	sp span, xfadeSeconds float64) (format.Media, error) {
	if !tl {
		med, err := s.openMember(ctx, members[0])
		if err != nil || !sp.narrowed() {
			return med, err
		}
		// The slice takes ownership, so a failure here has to close what it
		// did not take.
		sl, err := waxflow.Slice(med, sp.from, sp.end())
		if err != nil {
			med.Close()
			return nil, err
		}
		return sl, nil
	}
	srcs := make([]waxflow.ConcatSource, len(members))
	tracks := make([]container.Track, len(members))
	for i, m := range members {
		srcs[i] = waxflow.ConcatSource{
			Track: m.Track,
			Open:  func() (format.Media, error) { return s.openMember(ctx, m) },
		}
		tracks[i] = m.Track
	}
	// The run converts the descriptor's crossfade the same way the plan did
	// (planHLSVariant), on the same members, so the blended length the worker
	// delivers is the one the playlist promised.
	crossfade, err := waxflow.CrossfadeSamples(tracks, xfadeSeconds)
	if err != nil {
		return nil, err
	}
	return waxflow.Concat(srcs, s.timelineOptions(crossfade))
}

// openMember resolves one source, enforces the identity the plan was made
// against, and opens it. The returned Media owns the handle, so closing it
// releases the descriptor: that is what makes a timeline's lazy opening
// worth having, since a 500-track queue then holds one open file and not
// 500.
func (s *Server) openMember(ctx context.Context, m hlsSource) (format.Media, error) {
	f, err := s.resolver.Resolve(ctx, m.Ref)
	if err != nil {
		return nil, err
	}
	if f.ID != m.ID {
		f.Close()
		return nil, waxerr.New(waxerr.CodeSourceChanged, "source changed while starting the segment worker")
	}
	med, err := s.eng.OpenStream(f, f.Ext)
	if err != nil {
		f.Close()
		return nil, err
	}
	return closingMedia{Media: med, f: f}, nil
}

// closingMedia releases the source handle with the media.
type closingMedia struct {
	format.Media
	f *source.File
}

func (m closingMedia) Close() error {
	err := m.Media.Close()
	if cerr := m.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// mintHLSDescriptor builds the canonical descriptor from raw mint
// parameters (the keyed master form and POST /sign), resolving the
// source identity. The encode/decode round trip applies the descriptor's
// own validation, so mint-time junk is rejected exactly like
// playback-time junk. It also returns the source duration in seconds
// (-1 unknown) for the TTL policy.
func (s *Server) mintHLSDescriptor(ctx context.Context, params map[string]string) (hls.Descriptor, float64, error) {
	bad := func(format string, args ...any) (hls.Descriptor, float64, error) {
		return hls.Descriptor{}, 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(format, args...))
	}
	for k := range params {
		if !hlsMintParamNames[k] {
			return bad("unknown parameter %q", k)
		}
	}
	d := hls.Descriptor{Ver: hls.DescriptorVersion, Src: params["src"], Tl: params["tl"],
		Format: params["format"], Gain: params["gain"], Dynamics: params["dynamics"]}
	switch {
	case d.Src == "" && d.Tl == "":
		return bad("src or tl is required")
	case d.Src != "" && d.Tl != "":
		return bad("src and tl are exclusive: a URL names one stream or one timeline")
	}
	if d.Format == "" {
		d.Format = waxflow.SegmentedFormats()[0]
	}
	var err error
	atoi := func(name string) (int, error) {
		v := params[name]
		if v == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("%s %q: want a non-negative integer", name, v))
		}
		return n, nil
	}
	if d.Bitrate, err = atoi("bitrate"); err != nil {
		return hls.Descriptor{}, 0, err
	}
	if d.Bits, err = atoi("bits"); err != nil {
		return hls.Descriptor{}, 0, err
	}
	if d.Rate, err = atoi("rate"); err != nil {
		return hls.Descriptor{}, 0, err
	}
	// The span is parsed by the same function /stream uses, so the two
	// surfaces cannot come to disagree about what from and to mean.
	sp, err := parseSpan(params["from"], params["to"])
	if err != nil {
		return hls.Descriptor{}, 0, err
	}
	d.From, d.To = sp.from, sp.to
	if d.Ch, err = atoi("ch"); err != nil {
		return hls.Descriptor{}, 0, err
	}
	if v := params["crossfadeSeconds"]; v != "" {
		if d.CrossfadeSeconds, err = strconv.ParseFloat(v, 64); err != nil {
			return bad("crossfadeSeconds %q: want seconds", v)
		}
		// Guard before Encode: json.Marshal rejects a NaN or Inf float, so an
		// unvalidated one would panic rather than 400. This is the same rule the
		// descriptor decode applies, applied a step earlier on the raw value.
		if err := checkCrossfadeSeconds(d.CrossfadeSeconds); err != nil {
			return hls.Descriptor{}, 0, err
		}
	}
	if v := params["segDur"]; v != "" {
		if d.SegDur, err = strconv.ParseFloat(v, 64); err != nil {
			return bad("segDur %q: want seconds", v)
		}
	}
	if v := params["bitrates"]; v != "" {
		for _, part := range strings.Split(v, ",") {
			kbps, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil {
				return bad("bitrates %q: want comma-separated kbit/s values", v)
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

	// A timeline's members already carry their identities inside the digest,
	// so only a single source needs one pinned here.
	if d.Src != "" {
		f, err := s.resolver.Resolve(ctx, d.Src)
		if err != nil {
			return hls.Descriptor{}, 0, err
		}
		d.ID = f.ID.String()
		f.Close()
	}

	// Round trip through the wire form: one validator for minting and
	// playback.
	out, err := hls.DecodeDescriptor(d.Encode())
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
	tracks := req.tracks()
	for _, kbps := range out.Ladder() {
		// Mint-time validation plans with nil metadata: the resolved gain
		// value never shapes plan validity, only playback bytes. It hands the
		// remux rung no source for the same reason: a mint validates that some
		// rung can serve the variant, and rung 3 always can wherever rung 2
		// could, so paying for a packet walk here would buy nothing.
		if _, _, _, err := s.planHLSVariant(out.Variant(kbps), tracks, nil, nil); err != nil {
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
