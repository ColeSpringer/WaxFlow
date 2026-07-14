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
	"github.com/colespringer/waxflow/internal/cache"
	"github.com/colespringer/waxflow/internal/hls"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/sign"
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
// (comma-separated kbit/s).
var hlsMintParamNames = map[string]bool{
	"src": true, "format": true, "bitrate": true, "bitrates": true,
	"bits": true, "rate": true, "ch": true, "gain": true, "segDur": true,
}

// hlsRequest is one parsed, resolved, identity-checked, planned HLS
// request for a single variant (media playlist, init header, or a
// segment).
type hlsRequest struct {
	desc hls.Descriptor
	src  *source.File
	opts waxflow.TranscodeOptions
	plan *waxflow.SegmentPlan
	key  cache.Key
	meta cache.Meta
	// exp is the request's own signature expiry (unix seconds), 0 when
	// key-authed; child URLs inherit it so one minting governs the whole
	// playback session's lifetime.
	exp int64
}

func (req *hlsRequest) Close() error { return req.src.Close() }

// prepareHLS runs the shared front half of the per-variant HLS handlers:
// auth, the closed parameter surface, descriptor decode, source identity
// (410 on mismatch, always: the descriptor embeds identity by
// construction), probe, the exact-length walk when the headers cannot
// provide one, and the variant plan plus its ADR-0004 cache key.
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
	src, err := s.resolver.Resolve(r.Context(), desc.Src)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			src.Close()
		}
	}()
	want, err := source.ParseIdentity(desc.ID)
	if err != nil {
		return nil, err
	}
	if want != src.ID {
		return nil, waxerr.New(waxerr.CodeSourceChanged,
			"the source changed since this URL was minted; request a fresh one")
	}
	track, err := s.trackFor(src)
	if err != nil {
		return nil, err
	}
	opts, plan, err := s.planHLSVariant(desc, track, s.readMeta(r.Context(), src, false))
	if err != nil {
		return nil, err
	}
	if plan.Segments > maxPlaylistSegments {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("segment duration yields %d segments (max %d); raise segDur", plan.Segments, maxPlaylistSegments))
	}
	canonical := canonicalHLSParams(plan, opts.GainDB)
	req := &hlsRequest{
		desc: desc,
		src:  src,
		opts: opts,
		plan: plan,
		key:  cache.NewKey(identityString(desc.Src, src.ID), canonical, plan.Versions),
		meta: cache.Meta{
			Ref:         desc.Src,
			Identity:    src.ID.String(),
			Params:      canonical,
			Ext:         "m4s",
			ContentType: hlsMediaType,
			Samples:     plan.Samples,
			Rate:        plan.Format.Rate,
		},
	}
	if sigAuthed {
		req.exp, _ = strconv.ParseInt(q.Get(sign.ParamExp), 10, 64)
	}
	ok = true
	return req, nil
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
// HLS cannot drift.
func (s *Server) planHLSVariant(desc hls.Descriptor, track container.Track, m *meta.Info) (waxflow.TranscodeOptions, *waxflow.SegmentPlan, error) {
	if desc.Bitrate != 0 {
		if lossy, known := waxflow.LossyFormat(desc.Format); known && !lossy {
			return waxflow.TranscodeOptions{}, nil, waxerr.New(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("bitrate applies to lossy output; %s is lossless", desc.Format))
		}
	}
	gain, err := parseGain(desc.Gain, s.defaultGain)
	if err != nil {
		return waxflow.TranscodeOptions{}, nil, err
	}
	opts := waxflow.TranscodeOptions{
		Format:          desc.Format,
		Rate:            desc.Rate,
		Channels:        desc.Ch,
		BitDepth:        desc.Bits,
		GainDB:          gain.resolveDB(m),
		ResampleProfile: s.profile,
		MP3Bitrate:      desc.Bitrate * 1000,
		OpusBitrate:     desc.Bitrate * 1000,
	}
	plan, err := s.eng.PlanSegments(track, opts, desc.SegDur)
	if err != nil {
		return waxflow.TranscodeOptions{}, nil, err
	}
	return opts, plan, nil
}

// canonicalHLSParams is the HLS analog of canonicalParams: every
// output-shaping parameter in one fixed order for the cache key, values
// from the resolved plan. segSamples pins the segment numbering; the hls
// prefix keeps the key space disjoint from progressive entries.
func canonicalHLSParams(plan *waxflow.SegmentPlan, gainDB float64) string {
	return fmt.Sprintf("hls&container=%s&rate=%d&ch=%d&type=%s&bits=%d&bitrate=%d&gain=%s&segSamples=%d",
		plan.Container, plan.Format.Rate, plan.Format.Channels, plan.Format.Type,
		plan.Format.BitDepth, plan.BitRate, strconv.FormatFloat(gainDB, 'g', -1, 64), plan.SegmentSamples)
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

	src, err := s.resolver.Resolve(r.Context(), desc.Src)
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer src.Close()
	want, err := source.ParseIdentity(desc.ID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if want != src.ID {
		s.writeError(w, waxerr.New(waxerr.CodeSourceChanged,
			"the source changed since this URL was minted; request a fresh one"))
		return
	}
	info, err := s.eng.Probe(src, src.Ext, nil)
	if err != nil {
		s.writeError(w, err)
		return
	}
	track := info.Default()

	exp := time.Now().Add(sign.DefaultTTLFor(DurationSeconds(track.Samples, track.Fmt.Rate)))
	if sigAuthed {
		if e, _ := strconv.ParseInt(q.Get(sign.ParamExp), 10, 64); e > 0 {
			exp = time.Unix(e, 0)
		}
	}
	m := s.readMeta(r.Context(), src, false)
	var variants []hls.MasterVariant
	for _, kbps := range desc.Ladder() {
		vdesc := desc.Variant(kbps)
		_, plan, err := s.planHLSVariant(vdesc, track, m)
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
	init, err := s.eng.InitSegment(req.plan, req.opts)
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
	// The worker outlives the request; capture values, never req.src.
	ref, id, ext := req.desc.Src, req.src.ID, req.src.Ext
	opts, plan, key := req.opts, req.plan, req.key
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
			exit(s.runHLSWorker(ctx, ref, id, ext, opts, plan, variant, start, notify))
		}()
		return cancel, nil
	}
}

// runHLSWorker is one variant worker: its own source handle, the init
// header if the variant lacks one, then segments from start to end of
// stream, each published atomically and announced.
func (s *Server) runHLSWorker(ctx context.Context, ref string, id source.Identity, ext string,
	opts waxflow.TranscodeOptions, plan *waxflow.SegmentPlan, variant *cache.Variant,
	start int64, notify func(int64)) error {
	src, err := s.resolver.Resolve(ctx, ref)
	if err != nil {
		return err
	}
	defer src.Close()
	if src.ID != id {
		return waxerr.New(waxerr.CodeSourceChanged, "source changed while starting the segment worker")
	}
	if !variant.Has("init.mp4") {
		init, err := s.eng.InitSegment(plan, opts)
		if err != nil {
			return err
		}
		if err := variant.WriteFile("init.mp4", init); err != nil {
			return err
		}
	}
	_, err = s.eng.TranscodeSegments(ctx, src, ext, opts,
		waxflow.SegmentedOptions{SegmentSamples: plan.SegmentSamples, StartSegment: start},
		func(seg mp4.Segment) error {
			if err := variant.WriteFile(segmentName(seg.Index), seg.Data); err != nil {
				return err
			}
			notify(seg.Index)
			return nil
		})
	if err != nil {
		s.log.Warn("hls worker failed", "src", ref, "start", start, "err", err)
	} else {
		s.log.Debug("hls worker finished", "src", ref, "start", start)
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
	d := hls.Descriptor{Ver: hls.DescriptorVersion, Src: params["src"], Format: params["format"], Gain: params["gain"]}
	if d.Src == "" {
		return bad("src is required")
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
	if d.Ch, err = atoi("ch"); err != nil {
		return hls.Descriptor{}, 0, err
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
	// The gain spelling must parse now, not at playback.
	if _, err := parseGain(d.Gain, s.defaultGain); err != nil {
		return hls.Descriptor{}, 0, err
	}

	f, err := s.resolver.Resolve(ctx, d.Src)
	if err != nil {
		return hls.Descriptor{}, 0, err
	}
	defer f.Close()
	d.ID = f.ID.String()

	// Round trip through the wire form: one validator for minting and
	// playback.
	out, err := hls.DecodeDescriptor(d.Encode())
	if err != nil {
		return hls.Descriptor{}, 0, err
	}
	// Every rung must plan (mirroring the master handler), so a URL that
	// mints is a URL that plays: a lossless format with a bitrate, or a
	// format with no segmented form, fails here and not in the player.
	info, err := s.eng.Probe(f, f.Ext, nil)
	if err != nil {
		return hls.Descriptor{}, 0, err
	}
	track := info.Default()
	for _, kbps := range out.Ladder() {
		// Mint-time validation plans with nil metadata: the resolved gain
		// value never shapes plan validity, only playback bytes.
		if _, _, err := s.planHLSVariant(out.Variant(kbps), track, nil); err != nil {
			return hls.Descriptor{}, 0, err
		}
	}
	return out, DurationSeconds(track.Samples, track.Fmt.Rate), nil
}
