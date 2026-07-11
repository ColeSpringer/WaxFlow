package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/internal/cache"
	"github.com/colespringer/waxflow/internal/metrics"
	"github.com/colespringer/waxflow/internal/sign"
	"github.com/colespringer/waxflow/waxerr"
)

func (s *Server) handleCaps(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, buildCaps(s.jobs != nil, s.uploads != nil, s.cfg.PIDSources))
}

// probeRequest is the POST /probe body; GET uses src and strict query
// parameters with the same meaning.
type probeRequest struct {
	Src    string `json:"src"`
	Strict bool   `json:"strict"`
}

func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	var req probeRequest
	if r.Method == http.MethodPost {
		if err := decodeJSONBody(w, r, &req); err != nil {
			s.writeError(w, err)
			return
		}
	} else {
		req.Src = r.URL.Query().Get("src")
		req.Strict = boolish(r.URL.Query().Get("strict"))
	}
	if req.Src == "" {
		s.writeError(w, waxerr.New(waxerr.CodeInvalidRequest, "src is required"))
		return
	}
	f, err := s.resolver.Resolve(req.Src)
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer f.Close()
	info, err := s.eng.Probe(f, f.Ext, &waxflow.ProbeOptions{Strict: req.Strict})
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, ProbeJSON(info, s.readMeta(r.Context(), f, false)))
}

func (s *Server) handleSign(w http.ResponseWriter, r *http.Request) {
	if s.signer == nil {
		s.writeError(w, waxerr.New(waxerr.CodeInvalidRequest, "signing is not configured on this daemon"))
		return
	}
	var req SignRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		s.writeError(w, err)
		return
	}
	if req.Path == "" {
		req.Path = "/stream"
	}

	// Explicit TTLs are bounded on both sides: negative is meaningless,
	// and values past MaxTTL would overflow the duration arithmetic into
	// the past, minting a 200 OK URL that is already expired.
	if req.TTLSeconds < 0 || req.TTLSeconds > int64(sign.MaxTTL/time.Second) {
		s.writeError(w, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("ttlSeconds %d outside 0..%d", req.TTLSeconds, int64(sign.MaxTTL/time.Second))))
		return
	}

	var q url.Values
	var duration float64
	switch req.Path {
	case "/stream":
		q = make(url.Values, len(req.Params)+4)
		for k, v := range req.Params {
			q.Set(k, v)
		}
		// Reject junk at mint time, not at playback time.
		p, err := parseStreamParams(q, s.defaultGain)
		if err != nil {
			s.writeError(w, err)
			return
		}
		f, err := s.resolver.Resolve(p.src)
		if err != nil {
			s.writeError(w, err)
			return
		}
		q.Set("id", f.ID.String())
		// Duration comes from the probe; unknown lengths get the floor.
		duration = -1
		if info, err := s.eng.Probe(f, f.Ext, nil); err == nil {
			track := info.Default()
			duration = DurationSeconds(track.Samples, track.Fmt.Rate)
		}
		f.Close()
	case "/hls/master.m3u8":
		// The HLS surface signs one URL: master. Its children (media
		// playlists, init, segments) are signed by the daemon as it
		// emits them, inheriting this URL's expiry.
		desc, d, err := s.mintHLSDescriptor(req.Params)
		if err != nil {
			s.writeError(w, err)
			return
		}
		q = url.Values{"v": []string{desc.Encode()}}
		duration = d
	case "/art", "/lyrics":
		var err error
		if q, err = s.signMetaPath(req.Params); err != nil {
			s.writeError(w, err)
			return
		}
		duration = -1
	default:
		// Jobs paths carry the job id in the path itself, so the
		// signature pins it with no query parameters of its own.
		if err := s.signableJobPath(req.Path); err != nil {
			if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest || strings.HasPrefix(req.Path, "/jobs/") {
				s.writeError(w, err)
				return
			}
			s.writeError(w, waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("path %q: signable paths are /stream, /hls/master.m3u8, /art, /lyrics, and /jobs/<id>/{events,result}", req.Path)))
			return
		}
		if len(req.Params) > 0 {
			s.writeError(w, waxerr.New(waxerr.CodeInvalidRequest, "jobs paths take no params"))
			return
		}
		q = url.Values{}
		duration = -1
	}

	ttl := time.Duration(req.TTLSeconds) * time.Second
	if req.TTLSeconds == 0 {
		ttl = sign.DefaultTTLFor(duration)
	}
	exp := time.Now().Add(ttl)
	signed := s.signer.Sign(http.MethodGet, req.Path, q, exp)
	s.writeJSON(w, http.StatusOK, SignResponse{
		SchemaVersion: 1,
		URL:           req.Path + "?" + signed.Encode(),
		Exp:           exp.Unix(),
	})
}

// handleTranscode serves POST /transcode: the synchronous one-shot whose
// response body IS the transcode (CLI/scripting). Uncacheable by design:
// it runs through a ring-only entry, dies with its request, and holds a
// live admission slot while the client drains. The shared prepare path
// enforces the same parameter, identity, and bitrate-cap policies as
// /stream.
func (s *Server) handleTranscode(w http.ResponseWriter, r *http.Request) {
	reqStart := time.Now()
	req, err := s.prepareSource(r.Context(), r.URL.Query(), false)
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer req.Close()
	if err := s.planTranscode(req); err != nil {
		s.writeError(w, err)
		return
	}

	release, ok := s.pools.AcquireLive()
	if !ok {
		s.met.AdmissionRejects.Add(1)
		s.writeError(w, waxerr.New(waxerr.CodeOverloaded, "live transcode slots are full"))
		return
	}
	armed := false
	defer func() {
		if !armed {
			release()
		}
	}()
	entry := cache.NewMemEntry(s.cfg.RingBytes, cache.Meta{Ext: req.plan.Container})
	reader, err := entry.NewReader()
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.startPipeline(r.Context(), entry, req.p.src, req.src.ID, req.src.Ext, req.opts, release, true)
	armed = true

	w.Header().Set("Content-Disposition", "attachment; filename=\""+outputFilename(req.p.src, req.plan.Container)+"\"")
	// Downloads are not playback: no pacing.
	s.serveLive(w, r, reader, req.plan, false, reqStart)
}

// outputFilename derives the attachment name from the source reference.
func outputFilename(ref, ext string) string {
	base := path.Base(ref)
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	base = strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 {
			return '_'
		}
		return r
	}, base)
	return base + "." + ext
}

func (s *Server) handleCacheStats(w http.ResponseWriter, _ *http.Request) {
	st := s.store.Stats()
	s.writeJSON(w, http.StatusOK, CacheStatsResponse{
		SchemaVersion: 1,
		Entries:       st.Entries,
		Bytes:         st.Bytes,
		Hits:          st.Hits,
		Misses:        st.Misses,
	})
}

func (s *Server) handleCacheGC(w http.ResponseWriter, _ *http.Request) {
	removed, freed := s.store.GC()
	s.writeJSON(w, http.StatusOK, CacheGCResponse{SchemaVersion: 1, Removed: removed, FreedBytes: freed})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.metricsAuthed(r) {
		s.writeEnvelope(w, http.StatusUnauthorized, waxerr.CodeUnauthorized, "metrics need an API key or the metricsKey")
		return
	}
	st := s.store.Stats()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.met.WritePrometheus(w, s.cfg.Version, metrics.Gauges{
		CacheBytes:   st.Bytes,
		CacheEntries: st.Entries,
		CacheHits:    st.Hits,
		CacheMisses:  st.Misses,
		LiveInUse:    s.pools.LiveInUse(),
		JobInUse:     s.jobsRunning(),
	})
}

// decodeJSONBody strictly decodes a small JSON request body. The
// ResponseWriter goes to MaxBytesReader so an over-limit request also
// gets its connection closed after the reply (the unread remainder would
// otherwise desynchronize keep-alive); the 413 itself comes from the
// envelope code below, not the stdlib.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return waxerr.Wrap(waxerr.CodePayloadTooLarge, "request body", err)
		}
		return waxerr.Wrap(waxerr.CodeInvalidRequest, "request body", err)
	}
	if dec.More() {
		return waxerr.New(waxerr.CodeInvalidRequest, "request body: trailing data")
	}
	return nil
}

func boolish(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
