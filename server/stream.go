package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/cache"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// handleStream serves GET/HEAD /stream: auth, parameter parsing, source
// identity, then the decision ladder: direct play, (transmux, once a
// pair exists), transcode via the cache.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	reqStart := time.Now()
	s.applyCORS(w, r)

	q := r.URL.Query()
	sigAuthed, err := s.playbackAuth(r, q)
	if err != nil {
		s.writeError(w, err)
		return
	}
	req, err := s.prepareSource(r.Context(), q, sigAuthed)
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer req.Close()

	// Ladder rung 1: the source already satisfies the request, so serve
	// the original bytes: zero CPU, full range support.
	if directPlayable(req) {
		s.serveDirect(w, r, req, reqStart)
		return
	}
	if err := s.planTranscode(req); err != nil {
		s.writeError(w, err)
		return
	}
	plan := req.plan

	key := cache.NewKey(identityString(req.p.src, req.src.ID), req.canonical, plan.Versions)
	meta := cache.Meta{
		Ref:         req.p.src,
		Identity:    req.src.ID.String(),
		Params:      req.canonical,
		Ext:         plan.Container,
		ContentType: plan.MediaType,
		Samples:     plan.Samples,
		Rate:        plan.Format.Rate,
	}

	// HEAD never spawns a pipeline: headers come from the plan, or from
	// cache metadata when the entry is already complete.
	if r.Method == http.MethodHead {
		if c := s.store.Lookup(key); c != nil {
			s.serveCached(w, r, c, key, reqStart)
			return
		}
		s.liveHeaders(w, plan)
		w.WriteHeader(http.StatusOK)
		return
	}

	for range 3 {
		// Fast path: complete entry, own descriptor, full range support.
		// Lookup owns all hit/miss accounting (store counters feed both
		// /cache/stats and /metrics), so no separate bookkeeping here.
		if c := s.store.Lookup(key); c != nil {
			s.serveCached(w, r, c, key, reqStart)
			return
		}
		// Not cached, so the response would be a live transcode. Range
		// policy (RFC 9110 permission to ignore): Safari and AVPlayer
		// attach "bytes=0-" to essentially every media request, so that
		// gets the plain 200 full stream; a nonzero offset cannot be
		// honored live and gets 416 plus a hint, before any pipeline
		// spawns. No Content-Range rides along: RFC 9110's
		// unsatisfied-range form is "*/" complete-length with digits
		// required, and a live transcode has no complete length to state.
		if rng := r.Header.Get("Range"); rng != "" && !isZeroRange(rng) {
			w.Header().Set("Accept-Ranges", "none")
			s.writeEnvelope(w, http.StatusRequestedRangeNotSatisfiable, waxerr.CodeInvalidRequest,
				"live transcodes are not byte-addressable; seek with t= seconds (or HLS, once available)")
			return
		}
		// The flight result is the in-flight entry to attach to, or nil
		// meaning the entry completed and the caller should re-run its
		// own Lookup (each request needs its own file descriptor;
		// sharing one across responses would race on Seek).
		entry, err := s.fl.Do(string(key), func() (*cache.Entry, error) {
			if e := s.store.InFlight(key); e != nil {
				return e, nil
			}
			if s.store.Contains(key) {
				return nil, nil // raced a completion; the caller re-looks-up
			}
			release, ok := s.pools.AcquireLive()
			if !ok {
				s.met.AdmissionRejects.Add(1)
				return nil, waxerr.New(waxerr.CodeOverloaded, "live transcode slots are full")
			}
			// The slot must not leak if anything below panics before the
			// pipeline goroutine takes ownership of release.
			armed := false
			defer func() {
				if !armed {
					release()
				}
			}()
			entry, err := s.store.Begin(key, meta)
			if err != nil {
				// Cache volume unusable from the start: ring-fed
				// client-only streaming, never a dead request.
				s.log.Warn("cache unavailable, session is ring-fed", "key", key, "err", err)
				s.met.Degradations.Add(1)
				entry = cache.NewMemEntry(s.cfg.RingBytes, meta)
			}
			s.startPipeline(s.baseCtx, entry, req.p.src, req.src.ID, req.src.Ext, req.opts, release, false)
			armed = true
			return entry, nil
		})
		if err != nil {
			s.writeError(w, err)
			return
		}
		if entry == nil {
			continue // completed under us: re-run the Lookup
		}
		reader, err := entry.NewReader()
		if err != nil {
			// The entry died between Do and attach. A pipeline failure
			// carries the real error (a source deleted or changed under
			// us): surface it instead of respawning doomed pipelines.
			if ferr := entry.Err(); ferr != nil {
				s.writeError(w, ferr)
				return
			}
			continue // released or unjoinable after completion: retry
		}
		s.serveLive(w, r, reader, plan, true, reqStart)
		return
	}
	s.writeError(w, waxerr.New(waxerr.CodeInternal, "stream slot flapping"))
}

// startPipeline runs the transcode into entry on its own goroutine. Live
// pipelines pass the server's base context, not the request's: the
// read-behind model finishes the encode (and the cache entry) even when
// the first client leaves. Sync one-shots pass their request context to
// die with it.
func (s *Server) startPipeline(ctx context.Context, entry *cache.Entry, ref string, id source.Identity, hint string,
	opts waxflow.TranscodeOptions, release func(), sync bool) {
	if sync {
		s.met.SessionsSync.Add(1)
	} else {
		s.met.SessionsLive.Add(1)
	}
	s.met.SessionsActive.Add(1)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer release()
		defer s.met.SessionsActive.Add(-1)

		// The pipeline owns its own source handle; the request's closes
		// with the handler.
		src, err := s.resolver.Resolve(ctx, ref)
		if err != nil {
			entry.Fail(err)
			return
		}
		defer src.Close()
		if src.ID != id {
			entry.Fail(waxerr.New(waxerr.CodeSourceChanged, "source changed while starting the pipeline"))
			return
		}
		res, err := s.eng.Transcode(ctx, src, hint, entry, opts)
		if err != nil {
			s.log.Warn("pipeline failed", "src", ref, "err", err)
			entry.Fail(err)
			return
		}
		if err := entry.Complete(res.Samples); err != nil {
			s.log.Warn("entry completion failed", "src", ref, "err", err)
		}
		// After Complete: promotion failures (rename, meta write) also
		// degrade, and they must count too.
		if entry.FileBacked() && entry.Degraded() {
			s.met.Degradations.Add(1)
		}
		s.log.Debug("pipeline finished", "src", ref, "samples", res.Samples, "degraded", entry.Degraded())
	}()
}

// serveDirect is ladder rung 1: the original bytes with full range
// support, strong identity ETag, and conditional-request handling via
// http.ServeContent.
func (s *Server) serveDirect(w http.ResponseWriter, r *http.Request, req *streamRequest, reqStart time.Time) {
	s.met.DirectPlays.Add(1)
	w.Header().Set("Content-Type", format.MediaTypeFor(req.info.Container))
	w.Header().Set("ETag", `"`+directETag(req.src)+`"`)
	setDurationHeader(w, req.track.Samples, req.track.Fmt.Rate)
	s.met.TTFB.Observe(time.Since(reqStart).Seconds())
	http.ServeContent(w, r, "", req.src.ModTime(), req.src.ReadSeeker())
}

// serveCached serves a completed cache entry: real Content-Length, full
// RFC 7233 ranges, strong ETag (the cache key), If-None-Match for free.
func (s *Server) serveCached(w http.ResponseWriter, r *http.Request, c *cache.Cached, key cache.Key, reqStart time.Time) {
	defer c.File.Close()
	w.Header().Set("Content-Type", c.Meta.ContentType)
	w.Header().Set("ETag", `"`+string(key)+`"`)
	setDurationHeader(w, c.Meta.Samples, c.Meta.Rate)
	s.met.TTFB.Observe(time.Since(reqStart).Seconds())
	http.ServeContent(w, r, "", c.ModTime, c.File)
}

// liveHeaders are the transcode-shape response headers: chunked, not
// byte-addressable, with duration and size hints for players.
func (s *Server) liveHeaders(w http.ResponseWriter, plan *waxflow.TranscodePlan) {
	h := w.Header()
	h.Set("Content-Type", plan.MediaType)
	h.Set("Accept-Ranges", "none")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // reverse proxies must not buffer TTFA away
	setDurationHeader(w, plan.Samples, plan.Format.Rate)
	if plan.EstimatedBytes >= 0 {
		h.Set("X-Estimated-Content-Length", fmt.Sprint(plan.EstimatedBytes))
	}
}

// serveLive streams a read-behind reader: burst-then-pace delivery (when
// paced), per-chunk write deadlines, first flush at the first mux write.
// The status line waits for the priming read, so a pipeline that fails
// before producing a byte yields its real error code instead of a
// truncated 200.
func (s *Server) serveLive(w http.ResponseWriter, r *http.Request, reader *cache.Reader, plan *waxflow.TranscodePlan, paced bool, reqStart time.Time) {
	defer reader.Close()

	// A client that disconnects mid-read must unblock the reader; the
	// pipeline itself keeps running into the cache.
	stop := context.AfterFunc(r.Context(), func() { reader.Close() })
	defer stop()

	buf := make([]byte, 64<<10)
	n, err := reader.Read(buf)
	if n == 0 && err != nil && err != io.EOF {
		if r.Context().Err() != nil {
			return // client gone before the first byte
		}
		s.writeError(w, err)
		return
	}

	s.liveHeaders(w, plan)
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	// Bytes per second: PCM derives it from the per-sample byte cost, a
	// compressed CBR stream (MP3) from its fixed bit rate. Without this
	// fallback the primary streaming format would never be paced.
	byteRate := float64(plan.BytesPerFrame * plan.Format.Rate)
	if byteRate == 0 && plan.BitRate > 0 {
		byteRate = float64(plan.BitRate) / 8
	}
	if !paced {
		byteRate = 0 // disables pace() below
	}
	burstBytes := int64(s.cfg.PaceBurst.Seconds() * byteRate)
	start := time.Now()
	var sent int64
	first := true

	for {
		if n > 0 {
			if first {
				s.met.TTFB.Observe(time.Since(reqStart).Seconds())
				first = false
			}
			rc.SetWriteDeadline(time.Now().Add(writeStallTimeout))
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // client gone or stalled past the deadline
			}
			rc.Flush()
			sent += int64(n)
			s.pace(r.Context(), sent, burstBytes, byteRate, start)
		}
		switch {
		case err == io.EOF:
			return
		case err != nil:
			// Headers are long gone; the truncated body is the signal.
			s.log.Warn("live stream truncated", "err", err)
			return
		}
		n, err = reader.Read(buf)
	}
}

// pace enforces burst-then-cap delivery: protects limited home uplinks
// from track-skippers pulling whole files into client buffers. Factor 0
// disables.
func (s *Server) pace(ctx context.Context, sent, burstBytes int64, byteRate float64, start time.Time) {
	factor := s.cfg.PaceFactor
	if factor <= 0 || byteRate <= 0 || sent <= burstBytes {
		return
	}
	target := time.Duration(float64(sent-burstBytes) / (byteRate * factor) * float64(time.Second))
	sleep := target - time.Since(start)
	if sleep <= 0 {
		return
	}
	t := time.NewTimer(sleep)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// idxCache adapts the sidecar store to waxflow.IndexCache: blobs key by
// source identity (ADR-0003: ref plus size plus mtimeNS), so an edited
// file misses and its stale index ages out of the store.
type idxCache struct {
	store *cache.IdxStore
}

func (c *idxCache) Load(src container.Source) []byte {
	f, ok := src.(*source.File)
	if !ok {
		return nil // uploads and test sources: nothing durable to key on
	}
	return c.store.Load(identityString(f.Ref, f.ID))
}

func (c *idxCache) Save(src container.Source, blob []byte) {
	f, ok := src.(*source.File)
	if !ok {
		return
	}
	c.store.Save(identityString(f.Ref, f.ID), blob)
}

func (c *idxCache) Drop(src container.Source) {
	f, ok := src.(*source.File)
	if !ok {
		return
	}
	c.store.Drop(identityString(f.Ref, f.ID))
}

// directPlayable decides ladder rung 1: the source itself satisfies the
// request (codec allowed, bitrate under the cap, container acceptable)
// and no parameter transforms audio.
//
// The format= comparison deliberately bridges two namespaces: the value
// names an output format for transcodes, but here it is matched against
// the input container so an explicit format= short-circuits to the
// original bytes whenever the source already complies, including for
// containers no encoder writes yet (format=flac on a FLAC source).
func directPlayable(req *streamRequest) bool {
	p, track := req.p, req.track
	switch {
	case req.from > 0:
		return false // t= means a transcode timeline
	case p.bitrate != 0:
		return false // bitrate/q asks for a lossy re-encode, not the original
	case p.format != "auto" && p.format != req.info.Container:
		return false
	case p.rate != 0 && p.rate != track.Fmt.Rate:
		return false
	case p.ch != 0 && p.ch != track.Fmt.Channels:
		return false
	case p.bits != 0 && (track.Fmt.Type != audio.Int || p.bits != track.Fmt.BitDepth):
		return false
	case req.gainDB != 0:
		return false
	case p.dynamics != gain.PresetOff:
		// Note the asymmetry with the gain check above, which is what makes
		// this easy to miss: gain=track on an untagged file resolves to 0 dB
		// and direct-plays correctly, because 0 dB is a genuine no-op. A
		// dynamics preset has no no-op state. Without this clause a
		// dynamics=voice request on a format-matching source would serve the
		// original bytes with no compression at all.
		return false
	}
	if p.maxBitRate > 0 {
		if track.Samples <= 0 {
			return false // unverifiable cap: fall through to the encoder check
		}
		// Whole-file bytes over duration, deliberately including tags and
		// embedded art: direct play ships the entire file, so the wire
		// cost the cap protects (a limited mobile uplink) is exactly
		// this, not the audio-only bitrate.
		dur := float64(track.Samples) / float64(track.Fmt.Rate)
		if float64(req.src.ID.Size)*8/dur > float64(p.maxBitRate)*1000 {
			return false
		}
	}
	return true
}

// checkIdentity enforces ADR-0003 source identity: a signed URL must
// carry id and it must match the resolved file; a key-authed request may
// pin identity voluntarily. Identity pinning is orthogonal to auth: it
// guards which bytes are served, not who may ask, so a keyed request
// carrying id still gets 410 when the file changed. That is the contract
// the id parameter exists for; drop id to opt out.
func checkIdentity(p *streamParams, sigAuthed bool, f *source.File) error {
	if p.identity == "" {
		if sigAuthed {
			return waxerr.New(waxerr.CodeSignatureInvalid, "signed URLs must carry the id parameter")
		}
		return nil
	}
	want, err := source.ParseIdentity(p.identity)
	if err != nil {
		return err
	}
	if want != f.ID {
		return waxerr.New(waxerr.CodeSourceChanged,
			"the source changed since this URL was minted; request a fresh one")
	}
	return nil
}

// directETag derives the strong ETag for original bytes from the source
// identity (ADR-0003: identity is what pins bytes).
func directETag(f *source.File) string {
	sum := cache.NewKey(identityString(f.Ref, f.ID), "direct", nil)
	return string(sum[:32])
}

func setDurationHeader(w http.ResponseWriter, samples int64, rate int) {
	if d := DurationSeconds(samples, rate); d >= 0 {
		w.Header().Set("X-Content-Duration", fmt.Sprintf("%.3f", d))
	}
}

// isZeroRange reports whether a Range header is exactly the harmless
// whole-resource form "bytes=0-".
func isZeroRange(rng string) bool {
	return strings.TrimSpace(rng) == "bytes=0-"
}
