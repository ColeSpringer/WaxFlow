package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/internal/jobs"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/sign"
	"github.com/colespringer/waxflow/waxerr"
)

// jobsRunning feeds the job gauge; the runner owns job concurrency.
func (s *Server) jobsRunning() int {
	if s.jobs == nil {
		return 0
	}
	return s.jobs.Running()
}

// handleUploadCreate serves POST /uploads: the raw request body spools
// under a fresh id, referenced as src=upload:<id>. The optional name
// query parameter supplies the original filename (its extension is the
// probe hint).
func (s *Server) handleUploadCreate(w http.ResponseWriter, r *http.Request) {
	for k := range r.URL.Query() {
		if k != "name" {
			s.writeError(w, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("unknown parameter %q", k)))
			return
		}
	}
	item, err := s.uploads.Put(r.Body, r.URL.Query().Get("name"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	resp := UploadResponse{
		SchemaVersion: 1,
		ID:            item.ID,
		Ref:           "upload:" + item.ID,
		Name:          item.Name,
		Bytes:         item.Bytes,
	}
	if s.cfg.UploadTTL > 0 {
		resp.ExpiresAt = item.Created.Add(s.cfg.UploadTTL).Unix()
	}
	s.writeJSON(w, http.StatusCreated, resp)
}

// handleUploadDelete serves DELETE /uploads/{id}.
func (s *Server) handleUploadDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.uploads.Delete(r.PathValue("id")); err != nil {
		s.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// jobRequest is the POST /jobs body.
type jobRequest struct {
	Type      string `json:"type"`
	Src       string `json:"src"`
	Format    string `json:"format,omitempty"`
	Container string `json:"container,omitempty"`
	Rate      int    `json:"rate,omitempty"`
	Ch        int    `json:"ch,omitempty"`
	Bits      int    `json:"bits,omitempty"`
	Bitrate   int    `json:"bitrate,omitempty"`
	Gain      string `json:"gain,omitempty"`
	Loudness  string `json:"loudness,omitempty"`
	FLACLevel int    `json:"flacLevel,omitempty"`

	Silence            bool    `json:"silence,omitempty"`
	SilenceThresholdDB float64 `json:"silenceThresholdDb,omitempty"`
	SilenceMinSeconds  float64 `json:"silenceMinSeconds,omitempty"`
}

// requestFrom maps the wire body onto a job request, field for field.
//
// SourceID is deliberately not among them: it is absent from jobRequest so a
// client cannot forge the identity pin, and the caller fills it from the
// resolved source instead. TestJobRequestCoverage pins both the mapping and
// that exemption.
func requestFrom(body jobRequest) *jobs.Request {
	return &jobs.Request{
		Type:               jobs.Type(body.Type),
		Src:                body.Src,
		Format:             body.Format,
		Container:          body.Container,
		Rate:               body.Rate,
		Channels:           body.Ch,
		Bits:               body.Bits,
		Bitrate:            body.Bitrate,
		Gain:               body.Gain,
		Loudness:           body.Loudness,
		FLACLevel:          body.FLACLevel,
		Silence:            body.Silence,
		SilenceThresholdDB: body.SilenceThresholdDB,
		SilenceMinSeconds:  body.SilenceMinSeconds,
	}
}

// handleJobCreate serves POST /jobs: validate everything a queued job
// will need (source, identity, plan) so acceptance means the job can
// run, then persist and enqueue it.
func (s *Server) handleJobCreate(w http.ResponseWriter, r *http.Request) {
	var body jobRequest
	if err := decodeJSONBody(w, r, &body); err != nil {
		s.writeError(w, err)
		return
	}
	req, err := s.validateJobRequest(r.Context(), body)
	if err != nil {
		s.writeError(w, err)
		return
	}
	j, err := s.jobs.Create(*req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, j)
}

// validateJobRequest maps and validates the wire body. Transcode jobs
// plan against the probed source (gain 0: the resolved value never
// shapes plan validity), so a 201 means the job will not fail on
// request shape later.
func (s *Server) validateJobRequest(ctx context.Context, body jobRequest) (*jobs.Request, error) {
	bad := func(format string, args ...any) (*jobs.Request, error) {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(format, args...))
	}
	if body.Src == "" {
		return bad("src is required")
	}
	req := requestFrom(body)
	src, err := s.resolver.Resolve(ctx, body.Src)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	req.SourceID = src.ID.String()

	info, err := s.eng.Probe(src, src.Ext, nil)
	if err != nil {
		return nil, err
	}

	switch req.Type {
	case jobs.TypeAnalyze:
		if body.Format != "" || body.Container != "" || body.Rate != 0 || body.Ch != 0 ||
			body.Bits != 0 || body.Bitrate != 0 || body.Gain != "" || body.Loudness != "" || body.FLACLevel != 0 {
			return bad("type analyze takes src and the silence fields")
		}
		// A threshold with no silence:true would be silently ignored, which
		// is the shape of acceptance this gate exists to refuse.
		if !body.Silence && (body.SilenceThresholdDB != 0 || body.SilenceMinSeconds != 0) {
			return bad("silenceThresholdDb and silenceMinSeconds need silence:true")
		}
		// Named sil, not s: the receiver is the Server.
		if sil := req.SilenceOptions(); sil != nil {
			if err := checkSilenceOptions(sil); err != nil {
				return nil, err
			}
		}
		return req, nil
	case jobs.TypeTranscode:
		if body.Silence || body.SilenceThresholdDB != 0 || body.SilenceMinSeconds != 0 {
			return bad("the silence fields apply to analyze jobs")
		}
	default:
		return bad("type %q: want transcode or analyze", body.Type)
	}

	if req.Format == "" || req.Format == "auto" {
		return bad("transcode jobs need an explicit format (%s)", strings.Join(waxflow.OutputFormats(), ", "))
	}
	switch req.Bits {
	case 0, 16, 24:
	default:
		return bad("bits %d: want 16 or 24", req.Bits)
	}
	if req.Rate < 0 || req.Channels < 0 || req.Bitrate < 0 {
		return bad("rate, ch, and bitrate must be non-negative")
	}
	if req.Loudness != "" && req.Loudness != "analyze" {
		return bad("loudness %q: want analyze (or omit)", req.Loudness)
	}
	if req.Loudness == "analyze" && req.Gain != "" {
		return bad("loudness analyze replaces gain; drop the gain field")
	}
	if _, err := parseGain(req.Gain, s.defaultGain); err != nil {
		return nil, err
	}
	if req.Bitrate != 0 {
		if lossy, known := waxflow.LossyFormat(req.Format); known && !lossy {
			return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("bitrate applies to lossy output; %s is lossless", req.Format))
		}
	}
	if _, err := s.eng.PlanTranscode(info.Default(), req.TranscodeOptions(0, s.profile)); err != nil {
		return nil, err
	}
	return req, nil
}

// JobsList is the GET /jobs body.
type JobsList struct {
	SchemaVersion int         `json:"schemaVersion"`
	Jobs          []*jobs.Job `json:"jobs"`
}

func (s *Server) handleJobList(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, JobsList{SchemaVersion: 1, Jobs: s.jobs.List()})
}

func (s *Server) handleJobGet(w http.ResponseWriter, r *http.Request) {
	j, ok := s.jobs.Get(r.PathValue("id"))
	if !ok {
		s.writeError(w, waxerr.New(waxerr.CodeNotFound, "no such job"))
		return
	}
	s.writeJSON(w, http.StatusOK, j)
}

func (s *Server) handleJobDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.jobs.Delete(r.PathValue("id")); err != nil {
		s.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sseHeartbeat keeps intermediaries from timing an idle event stream out.
const sseHeartbeat = 15 * time.Second

// handleJobEvents serves GET /jobs/{id}/events: a server-sent event
// stream of job snapshots (event type "job"), one per state or progress
// update, ending after the terminal event. Sig auth is accepted because
// a browser EventSource cannot set headers.
func (s *Server) handleJobEvents(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)
	q := r.URL.Query()
	if _, err := s.playbackAuth(r, q); err != nil {
		s.writeError(w, err)
		return
	}
	sub, cancel, ok := s.jobs.Subscribe(r.PathValue("id"))
	if !ok {
		s.writeError(w, waxerr.New(waxerr.CodeNotFound, "no such job"))
		return
	}
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)

	beat := time.NewTicker(sseHeartbeat)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-beat.C:
			rc.SetWriteDeadline(time.Now().Add(writeStallTimeout))
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			rc.Flush()
		case j, live := <-sub:
			if !live {
				return
			}
			data, err := json.Marshal(j)
			if err != nil {
				return
			}
			rc.SetWriteDeadline(time.Now().Add(writeStallTimeout))
			if _, err := fmt.Fprintf(w, "event: job\ndata: %s\n\n", data); err != nil {
				return
			}
			rc.Flush()
		}
	}
}

// handleJobResult serves GET /jobs/{id}/result: the job's output file
// (full ranges, immutable-per-id strong ETag), which is a transcode's audio
// or an analyze job's silence map, and the analysis JSON for an analyze job
// that produced no file. Sig auth is accepted so a browser can download
// without headers.
func (s *Server) handleJobResult(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)
	q := r.URL.Query()
	if _, err := s.playbackAuth(r, q); err != nil {
		s.writeError(w, err)
		return
	}
	j, ok := s.jobs.Get(r.PathValue("id"))
	if !ok {
		s.writeError(w, waxerr.New(waxerr.CodeNotFound, "no such job"))
		return
	}
	switch j.State {
	case jobs.StateDone:
	case jobs.StateFailed:
		s.writeEnvelope(w, statusFor(waxerr.Code(j.Error.Code)), waxerr.Code(j.Error.Code),
			"job failed: "+j.Error.Message)
		return
	default:
		s.writeError(w, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("job is %s; the result is not ready", j.State)))
		return
	}
	// An analyze job has an output only when it mapped silence; the
	// loudness numbers stay on the job itself either way.
	if j.Output == nil {
		if j.Type == jobs.TypeAnalyze {
			s.writeJSON(w, http.StatusOK, j.Analysis)
			return
		}
		s.writeError(w, waxerr.New(waxerr.CodeInternal, "job has no output"))
		return
	}
	f, err := s.jobs.OutputFile(j)
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		s.writeError(w, waxerr.Wrap(waxerr.CodeInternal, "job output", err))
		return
	}
	w.Header().Set("Content-Type", j.Output.MediaType)
	w.Header().Set("ETag", `"job-`+j.ID+`"`)
	w.Header().Set("Content-Disposition", "attachment; filename=\""+outputFilename(j.Request.Src, j.Output.Container)+"\"")
	setDurationHeader(w, j.Output.Samples, j.Output.Rate)
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// artLyricsParams is the closed parameter surface of /art and /lyrics.
var artLyricsParams = map[string]bool{
	"src": true, "id": true,
	sign.ParamExp: true, sign.ParamKID: true, sign.ParamSig: true,
}

// prepareMetaRequest is the shared front half of /art and /lyrics:
// playback auth, the closed parameter set, source resolution with
// identity pinning, and the metadata read.
func (s *Server) prepareMetaRequest(w http.ResponseWriter, r *http.Request, pictures bool) (*meta.Info, *streamRequest, bool) {
	s.applyCORS(w, r)
	q := r.URL.Query()
	sigAuthed, err := s.playbackAuth(r, q)
	if err != nil {
		s.writeError(w, err)
		return nil, nil, false
	}
	for k := range q {
		if !artLyricsParams[k] {
			s.writeError(w, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("unknown parameter %q", k)))
			return nil, nil, false
		}
	}
	if s.cfg.Meta == nil {
		s.writeError(w, waxerr.New(waxerr.CodeUnsupportedFormat, "metadata mapping is not configured on this daemon"))
		return nil, nil, false
	}
	p := &streamParams{src: q.Get("src"), identity: q.Get("id")}
	if p.src == "" {
		s.writeError(w, waxerr.New(waxerr.CodeInvalidRequest, "src is required"))
		return nil, nil, false
	}
	src, err := s.resolver.Resolve(r.Context(), p.src)
	if err != nil {
		s.writeError(w, err)
		return nil, nil, false
	}
	if err := checkIdentity(p, sigAuthed, src); err != nil {
		src.Close()
		s.writeError(w, err)
		return nil, nil, false
	}
	info := s.readMeta(r.Context(), src, pictures)
	return info, &streamRequest{p: p, src: src}, true
}

// handleArt serves GET /art: the source's embedded cover art, verbatim
// (no resizing), with a strong identity-derived ETag. A remote player
// streaming through WaxFlow has no other channel for artwork.
func (s *Server) handleArt(w http.ResponseWriter, r *http.Request) {
	info, req, ok := s.prepareMetaRequest(w, r, true)
	if !ok {
		return
	}
	defer req.Close()
	pic := info.FrontPicture()
	if pic == nil {
		s.writeError(w, waxerr.New(waxerr.CodeNotFound, "the source embeds no artwork"))
		return
	}
	mime := pic.MIME
	if mime == "" || !strings.Contains(mime, "/") {
		mime = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("ETag", `"`+directETag(req.src)+`-art"`)
	http.ServeContent(w, r, "", req.src.ModTime(), bytes.NewReader(pic.Data))
}

// handleLyrics serves GET /lyrics: unsynced lyrics text, or an LRC
// rendering of embedded synced lyrics when that is all the source has.
func (s *Server) handleLyrics(w http.ResponseWriter, r *http.Request) {
	info, req, ok := s.prepareMetaRequest(w, r, false)
	if !ok {
		return
	}
	defer req.Close()
	text := info.Lyrics()
	if text == "" {
		text = info.SyncedLRC()
	}
	if text == "" {
		s.writeError(w, waxerr.New(waxerr.CodeNotFound, "the source embeds no lyrics"))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("ETag", `"`+directETag(req.src)+`-lyrics"`)
	http.ServeContent(w, r, "", req.src.ModTime(), strings.NewReader(text))
}

// signableJobPath validates a /jobs/{id}/(events|result) signing target
// against the store, so minting fails on junk rather than the player.
func (s *Server) signableJobPath(path string) error {
	rest, ok := strings.CutPrefix(path, "/jobs/")
	if !ok {
		return waxerr.New(waxerr.CodeInvalidRequest, "not a jobs path")
	}
	id, tail, ok := strings.Cut(rest, "/")
	if !ok || (tail != "events" && tail != "result") {
		return waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("path %q: jobs signing covers /jobs/<id>/events and /jobs/<id>/result", path))
	}
	if s.jobs == nil {
		return waxerr.New(waxerr.CodeInvalidRequest, "jobs are not enabled on this daemon")
	}
	if _, ok := s.jobs.Get(id); !ok {
		return waxerr.New(waxerr.CodeNotFound, "no such job")
	}
	return nil
}

// signMetaPath prepares the query for signing /art and /lyrics: src plus
// the pinned identity, exactly like /stream.
func (s *Server) signMetaPath(ctx context.Context, params map[string]string) (url.Values, error) {
	src := params["src"]
	if src == "" {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "src is required")
	}
	for k := range params {
		if k != "src" {
			return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("unknown parameter %q", k))
		}
	}
	f, err := s.resolver.Resolve(ctx, src)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return url.Values{"src": []string{src}, "id": []string{f.ID.String()}}, nil
}
