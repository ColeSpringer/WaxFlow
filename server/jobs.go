package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/cue"
	"github.com/colespringer/waxflow/internal/jobs"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/sign"
	"github.com/colespringer/waxflow/internal/timeline"
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
	Type string   `json:"type"`
	Src  string   `json:"src"`
	Srcs []string `json:"srcs,omitempty"`
	// Titles are optional per-member chapter titles for a merge, index-aligned
	// to Srcs. They stamp the QuickTime chapter track of an mp4-family merge,
	// overriding each member's own TITLE tag; see validateMergeRequest.
	Titles []string `json:"titles,omitempty"`
	Cuts   []int64  `json:"cuts,omitempty"`
	// Cue is a source reference naming a CUE sheet whose track boundaries
	// are this split's cut points, exclusive with Cuts. It resolves through
	// the same resolver src does, so a sheet can be uploaded
	// (upload:<id>) or sit in a library root beside its rip.
	//
	// It is resolved into Cuts at creation and does not reach the domain
	// Request: the job is its cut points, so re-reading a sheet at run time
	// would let an edit between creation and execution change what the job
	// was accepted as.
	Cue       string `json:"cue,omitempty"`
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
// The identity pins are deliberately not among them: SourceID and SourceIDs
// are absent from jobRequest so a client cannot forge one, and the caller
// fills them from the resolved sources instead. TestJobRequestCoverage pins
// both the mapping and those exemptions.
func requestFrom(body jobRequest) *jobs.Request {
	return &jobs.Request{
		Type:               jobs.Type(body.Type),
		Src:                body.Src,
		Srcs:               slices.Clone(body.Srcs),
		MemberTitles:       slices.Clone(body.Titles),
		Cuts:               slices.Clone(body.Cuts),
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

// validateJobRequest maps and validates the wire body. Every type plans
// against its probed source (gain 0: the resolved value never shapes plan
// validity), so a 201 means the job will not fail on request shape later.
//
// The per-type field policing below is that promise's front half, and it is a
// hand-maintained rule per field. It reads as bureaucracy until the
// alternative is spelled out: a field that does not apply to a type would be
// accepted, stored, broadcast on every progress event, and silently ignored,
// so the caller's evidence that their cuts were honored would be the request
// they sent. TestJobFieldPolicing drives every (type, field) pair against it.
func (s *Server) validateJobRequest(ctx context.Context, body jobRequest) (*jobs.Request, error) {
	bad := func(format string, args ...any) (*jobs.Request, error) {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(format, args...))
	}
	req := requestFrom(body)
	switch req.Type {
	case jobs.TypeAnalyze, jobs.TypeTranscode, jobs.TypeMerge, jobs.TypeSplit:
	default:
		// Timeline is a real type and still not one of these: POST
		// /hls/timeline creates it, because that endpoint answers with the
		// digest outright whenever nothing needs measuring, and a second front
		// door here would skip both that fast path and the mint's validation.
		return bad("type %q: want transcode, analyze, merge, or split", body.Type)
	}
	// The source side forks once: a merge names its members in srcs, and every
	// other type names one source in src. Neither spelling is tolerated on the
	// other, so a merge with a src is a 400 rather than a merge of one file
	// that quietly ignored a member list.
	if req.Type == jobs.TypeMerge {
		if body.Src != "" {
			return bad("type merge takes srcs, not src")
		}
		if len(body.Srcs) == 0 {
			return bad("type merge needs srcs: at least one member")
		}
		if len(body.Srcs) > timeline.MaxMembers {
			return bad("%d members is past the %d-member bound; a merge is a play queue, not a library",
				len(body.Srcs), timeline.MaxMembers)
		}
		for i, m := range body.Srcs {
			if m == "" {
				return bad("member %d has no src", i)
			}
		}
	} else {
		if body.Src == "" {
			return bad("src is required")
		}
		if len(body.Srcs) > 0 {
			return bad("srcs applies to merge jobs")
		}
	}
	if req.Type != jobs.TypeMerge && len(body.Titles) > 0 {
		return bad("titles applies to merge jobs")
	}
	if req.Type != jobs.TypeSplit && len(body.Cuts) > 0 {
		return bad("cuts applies to split jobs")
	}
	if req.Type != jobs.TypeSplit && body.Cue != "" {
		return bad("cue applies to split jobs")
	}
	if len(body.Cuts) > 0 && body.Cue != "" {
		return bad("cuts and cue are exclusive: a split's boundaries come from one place")
	}
	if req.Type != jobs.TypeAnalyze && (body.Silence || body.SilenceThresholdDB != 0 || body.SilenceMinSeconds != 0) {
		return bad("the silence fields apply to analyze jobs")
	}
	// Gain and loudness are transcode-only, and that is a real restriction
	// rather than an unfinished one. Both answer "how loud should this one
	// track be", against that track's own measurement or its own ReplayGain
	// tags: a merge has N tracks in and one file out, and a split has one in
	// and N out, so either would have to pick one source's number and apply it
	// to things it does not describe. A caller who wants normalized pieces
	// transcodes them after the cut, where the numbers name what they measure.
	if req.Type == jobs.TypeMerge || req.Type == jobs.TypeSplit {
		if body.Gain != "" || body.Loudness != "" {
			return bad("gain and loudness apply to transcode jobs; %s writes the samples it is given", req.Type)
		}
	}
	if req.Type == jobs.TypeMerge {
		return s.validateMergeRequest(ctx, body, req)
	}

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

	if req.Type == jobs.TypeAnalyze {
		if body.Format != "" || body.Container != "" || body.Rate != 0 || body.Ch != 0 ||
			body.Bits != 0 || body.Bitrate != 0 || body.Gain != "" || body.Loudness != "" ||
			body.FLACLevel != 0 {
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
	}
	if req.Type == jobs.TypeSplit {
		// A length no header declares is measured here, which is what makes
		// the refusal a 400. SplitSpans only bounds a cut when it is given a
		// total, so a source that declares none (total -1) has every cut
		// accepted: the overshoot check is skipped, the pieces run in order,
		// and the first one past the real end dies at read with its
		// predecessors already written to the job directory and no Output
		// naming them (the list is assigned after the loop, and a plain
		// failure leaves the directory).
		//
		// Not measured exactly, which a merge and a timeline mint both are.
		// The difference is that a split is not the last word on its own
		// lengths: every piece is a Slice, and SpanTrack refuses a window past
		// the length the piece's own freshly opened Media declares, from the
		// header alone and with nothing measured. So measuring exactly here
		// would override a declared total that the run then re-reads and holds
		// the cuts to anyway, and a source whose header under-declares would
		// take a 201 and fail at run: the opposite of what validating early
		// is for. Filling in an absent length adds a bound where there was
		// none; replacing a present one puts a third number in a chain that
		// needs one.
		track, err := s.trackFor(src, false)
		if err != nil {
			return nil, err
		}
		// A sheet becomes cut points here, once, so everything below (and
		// the runner, and the restart) sees the same list a caller who sent
		// cuts directly would have sent. The sheet is not carried into the
		// job: a job is its cut points, and re-reading a sheet at run time
		// would let an edit between creation and execution change what was
		// accepted.
		if body.Cue != "" {
			if req.Cuts, err = s.cutsFromCue(ctx, body.Cue, track.Fmt.Rate); err != nil {
				return nil, err
			}
		}
		// The cut arithmetic itself is the runner's own function, so the two
		// cannot disagree about which samples are piece 3. They are handed
		// different totals, though: the runner re-probes raw, and closing that
		// gap needs the runner to measure as this does.
		if _, err := req.SplitSpans(track.Samples); err != nil {
			return nil, err
		}
	}
	if err := s.checkOutputShape(req); err != nil {
		return nil, err
	}
	if _, err := s.eng.PlanTranscode(info.Default(), req.TranscodeOptions(0, s.profile)); err != nil {
		return nil, err
	}
	return req, nil
}

// checkOutputShape validates the shaping fields every audio-writing job
// carries, in one place because transcode, merge, and split take the same set
// and must refuse the same values.
func (s *Server) checkOutputShape(req *jobs.Request) error {
	bad := func(format string, args ...any) error {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(format, args...))
	}
	if req.Format == "" || req.Format == "auto" {
		return bad("%s jobs need an explicit format (%s)", req.Type, strings.Join(waxflow.OutputFormats(), ", "))
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
		return err
	}
	if req.Bitrate != 0 {
		if lossy, known := waxflow.LossyFormat(req.Format); known && !lossy {
			return waxerr.New(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("bitrate applies to lossy output; %s is lossless", req.Format))
		}
	}
	return nil
}

// mp4ProgressiveContainer is the container override that selects the flat
// (moov+mdat) MP4 form. The engine's own constant is unexported and this is a
// wire value either way, spelled the same by the CLI's --container flag.
const mp4ProgressiveContainer = "progressive"

// validateMergeRequest resolves, measures, and plans a merge's members. It
// pins their identities as it goes, which is where the merge's source-changed
// guarantee is minted.
//
// The members are measured rather than trusted, and that is why this can be
// slow enough to notice: a merge's positions are a prefix sum, so an advisory
// length that is two samples out desyncs every seam after it. The memo behind
// trackFor is what keeps it affordable, and it is the same memo the HLS
// timeline mint fills, so a queue that was timelined before measures nothing
// here and the merge job's own run measures nothing at all.
//
// One member is open at a time, for the reason resolveMember spells out: a
// thousand-member queue held open at once is a thousand descriptors against a
// default limit of 1024, and this is the path that would hold them longest,
// since the measure pass is the slow part. runMerge already proves one at a
// time suffices.
func (s *Server) validateMergeRequest(ctx context.Context, body jobRequest, req *jobs.Request) (*jobs.Request, error) {
	// Titles are index-aligned to the members, so the only shapes that mean
	// anything are none and one-per-member. A short or long list would leave
	// the alignment to guesswork at run time, which is the silent-mismatch this
	// refuses. A blank entry is legal: it falls through to the tag or fallback.
	if n := len(body.Titles); n != 0 && n != len(body.Srcs) {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
			"titles has %d entries but the merge has %d members; a title per member, or none", n, len(body.Srcs)))
	}
	tracks := make([]container.Track, len(body.Srcs))
	req.SourceIDs = make([]string, len(body.Srcs))
	for i, ref := range body.Srcs {
		track, id, err := s.measureMember(ctx, ref)
		if err != nil {
			return nil, err
		}
		tracks[i] = track
		req.SourceIDs[i] = id
	}
	// Planning the envelope now is what makes a 201 mean something: a queue
	// whose members cannot be concatenated (one laid out for other speakers,
	// say) fails here rather than after the caller has waited for the encode.
	env, err := waxflow.ConcatTrack(tracks, s.timelineOptions(0))
	if err != nil {
		return nil, err
	}
	if err := s.checkOutputShape(req); err != nil {
		return nil, err
	}
	plan, err := s.eng.PlanTranscode(env, req.TranscodeOptions(0, s.profile))
	if err != nil {
		return nil, err
	}
	// An mp4-family merge writes the flat form unless the caller said
	// otherwise, and it is decided here so the stored request says what will
	// be produced rather than the runner quietly meaning something else.
	//
	// The row's default is the fragmented muxer because /stream needs one that
	// streams; a job writes to a file, where the back-patch the flat form
	// needs is satisfiable and streaming buys nothing. The flat form is also
	// what Apple Books reads and the only shape that can carry a chapter text
	// track, which is the whole point of merging an audiobook. Asking the
	// caller to know the word "progressive" to get an M4B that opens is a
	// worse API than picking the form that works.
	//
	// The engine is asked whether this is MP4 rather than a format name being
	// matched here: the plan's media type is the same answer runTranscode's
	// own MP4 branch reads, so a new mp4-family row is covered without this
	// knowing it exists.
	if req.Container == "" && plan.MediaType == "audio/mp4" {
		req.Container = mp4ProgressiveContainer
		if _, err := s.eng.PlanTranscode(env, req.TranscodeOptions(0, s.profile)); err != nil {
			return nil, err
		}
	}
	// Titles write a QuickTime chapter track, which only the flat MP4 form
	// carries. If the resolved output is not that form the runner drops them, so
	// refuse here rather than accept a field that will be silently inert, the
	// way a lossless bitrate is refused (checkOutputShape): a field the request
	// carries but the output cannot honor is a 400 where the caller can still
	// fix it, not a clean success that quietly did less. The container is final
	// by now (the block above settled the mp4-family default), and the test is
	// the same one the runner gates on, so the two agree on which merges carry
	// chapters.
	if len(body.Titles) > 0 && req.Container != mp4ProgressiveContainer {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			"titles write a QuickTime chapter track, which only the flat MP4 form carries; this merge's "+
				"format and container produce none, so the titles would be dropped. Merge to an mp4-family "+
				"format (alac, or aac without an explicit container) to get chapters")
	}
	return req, nil
}

// measureMember resolves one merge member, measures it, and releases the
// handle before returning. The track and the identity are the whole product:
// nothing reads the member again at creation, and the run re-resolves from the
// reference anyway, so holding the descriptor past this point would buy
// nothing and cost one per member.
func (s *Server) measureMember(ctx context.Context, ref string) (container.Track, string, error) {
	f, err := s.resolver.Resolve(ctx, ref)
	if err != nil {
		return container.Track{}, "", err
	}
	defer f.Close()
	track, err := s.trackFor(f, true)
	if err != nil {
		return container.Track{}, "", err
	}
	return track, f.ID.String(), nil
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

// handleJobResult serves GET /jobs/{id}/result and /jobs/{id}/result/{n}: the
// job's nth output file (full ranges, immutable-per-id strong ETag), which is
// a transcode's or a merge's audio, one piece of a split, or an analyze job's
// silence map. A job whose product is not a file answers with the product as
// JSON instead. Sig auth is accepted so a browser can download without
// headers.
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
	n, err := jobResultIndex(j, r.PathValue("n"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	if n < 0 {
		s.writeJobProduct(w, j)
		return
	}
	f, err := s.jobs.OutputFile(j, n)
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
	out := j.Outputs[n]
	w.Header().Set("Content-Type", out.MediaType)
	// The ETag carries the index as well as the id, or a split's pieces would
	// all validate against each other: the id alone was strong only while a
	// job had one output to be.
	w.Header().Set("ETag", fmt.Sprintf(`"job-%s-%d"`, j.ID, n))
	w.Header().Set("Content-Disposition", "attachment; filename=\""+jobResultFilename(j, n)+"\"")
	setDurationHeader(w, out.Samples, out.Rate)
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// jobResultIndex resolves the {n} path segment against the job's outputs. It
// returns -1 for a job whose product is not a file, which the caller answers
// as JSON.
//
// The bare /result (no index) means the job's one output, and refuses to mean
// anything when the job has several. That reads like a hedge and is the one
// answer with no wrong case: for the three types that produce a single file it
// is what it has always meant, and for a split it is genuinely ambiguous, so
// handing back piece 1 of 12 would be a plausible-looking wrong answer to a
// caller who never learned the pieces existed. This daemon refuses the
// ambiguous case everywhere else it meets one (a span past the end of a
// source, a threshold with no silence:true) for the same reason: a 400 is read,
// and a quiet first-of-N is not.
func jobResultIndex(j *jobs.Job, raw string) (int, error) {
	if raw == "" {
		switch {
		case len(j.Outputs) == 0:
			return -1, nil
		case len(j.Outputs) > 1:
			return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"this job has %d outputs; ask for one by index, /jobs/%s/result/{0..%d}",
				len(j.Outputs), j.ID, len(j.Outputs)-1))
		}
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("result index %q: want a non-negative integer", raw))
	}
	if n >= len(j.Outputs) {
		return 0, waxerr.New(waxerr.CodeNotFound, fmt.Sprintf(
			"result index %d: this job has %d outputs", n, len(j.Outputs)))
	}
	return n, nil
}

// writeJobProduct answers for a done job that wrote no file, whose product is
// a document on the job itself.
//
// Both branches are policy rather than fallback, which is what the internal
// error they replace was not: a timeline job's product is its digest, and it
// answered "job has no output" here because Timeline is deliberately not an
// Output (it is not a file in the job directory; it lives in the timeline
// store under the digest that is its identity). That reads as a broken
// endpoint rather than as the decision it was. /result means this job's
// product, and an analyze job has served its numbers here since before any of
// this, so a timeline job serves its digest by exactly that precedent.
//
// A done job with neither is genuinely a bug, and keeps the internal error.
func (s *Server) writeJobProduct(w http.ResponseWriter, j *jobs.Job) {
	switch {
	case j.Type == jobs.TypeAnalyze && j.Analysis != nil:
		// An analyze job has an output only when it mapped silence; the
		// loudness numbers stay on the job itself either way.
		s.writeJSON(w, http.StatusOK, j.Analysis)
	case j.Type == jobs.TypeTimeline && j.Timeline != nil:
		s.writeJSON(w, http.StatusOK, j.Timeline)
	default:
		s.writeError(w, waxerr.New(waxerr.CodeInternal, "job has no output"))
	}
}

// maxCueBytes bounds a CUE sheet read. Real sheets are a few kilobytes; the
// bound is what stops "cue" naming a multi-gigabyte source and the daemon
// reading it into memory to find out it is not a sheet.
const maxCueBytes = 1 << 20

// cutsFromCue resolves a CUE sheet reference and turns its track boundaries
// into this split's interior cut points, at the source's own rate.
//
// The sheet's track starts and a split's cuts are not the same list, and
// cue.Cuts owns every rule about the difference: a leading 0 is dropped
// because a cut there would ask for an empty piece, and a nonzero first start
// is kept, because a sheet whose TRACK 01 begins past frame 0 (a pregap, or
// hidden-track-one audio, which can be a whole song) is describing a lead-in
// that is part of the file and must become the first piece rather than
// vanish. That funnel is shared with the CLI, which is what stops one sheet
// being cut two ways.
//
// The rate is the source's, not the sheet's, because a sheet has no rate:
// its MM:SS:FF times are CD frames of 1/75 s, and it is the audio that says
// how many samples a frame is. Every CD-family rate divides by 75 exactly,
// which is what makes the conversion exact rather than nearly so.
func (s *Server) cutsFromCue(ctx context.Context, ref string, rate int) ([]int64, error) {
	f, err := s.resolver.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if f.ID.Size > maxCueBytes {
		return nil, waxerr.New(waxerr.CodePayloadTooLarge, fmt.Sprintf(
			"cue %q is %d bytes; a CUE sheet is text and is bounded at %d", ref, f.ID.Size, maxCueBytes))
	}
	raw := make([]byte, f.ID.Size)
	if err := container.ReadFull(f, raw, 0); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeSourceUnreadable, "reading the CUE sheet", err)
	}
	sheet, err := cue.Parse(raw)
	if err != nil {
		return nil, err
	}
	file, err := sheet.SingleFile()
	if err != nil {
		return nil, err
	}
	return file.Cuts(rate)
}

// jobResultFilename names the download. A split's pieces carry the index the
// URL asked by, so that twelve chapters do not all arrive called the same
// thing and land on top of each other in a downloads folder.
//
// The extension comes from the output table rather than from the output's
// container name, because a container name is a muxer-form selector and not a
// file form: an mp4-family merge defaults to container "progressive", and
// handing back foo.progressive names a file Apple Books will not open, which
// is precisely the M4B the default exists to produce. The others (flac, mp3,
// wav, mka) only ever worked by coinciding with an extension.
//
// A job that wrote no audio has no row to ask: an analyze job's silence map is
// application/json, and its container name IS its extension. Format is the
// test because checkOutputShape requires an explicit one of every audio-writing
// type and analyze refuses it outright, so the two cases cannot blur.
func jobResultFilename(j *jobs.Job, n int) string {
	ref := j.Request.Src
	if ref == "" && len(j.Request.Srcs) > 0 {
		// A merge names no single source, and its first member is the closest
		// thing to a name the request carries.
		ref = j.Request.Srcs[0]
	}
	ext := j.Outputs[n].Container
	if j.Request.Format != "" {
		ext = waxflow.OutputExt(j.Request.Format, j.Request.Container)
	}
	name := outputFilename(ref, ext)
	if len(j.Outputs) == 1 {
		return name
	}
	// Re-derived from the name rather than reused: outputFilename may have
	// fallen back to a name of its own, and the index goes before whatever
	// extension actually landed.
	suffix := path.Ext(name)
	return fmt.Sprintf("%s.%d%s", strings.TrimSuffix(name, suffix), n, suffix)
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

// signableJobPath validates a /jobs/{id}/(events|result[/n]) signing target
// against the store, so minting fails on junk rather than the player.
//
// The index is checked for shape only, never against the job's outputs: a URL
// is signed while the job is still queued, which is the whole reason to want
// one, and at that point the job has no outputs to index. The handler bounds
// it when the download arrives.
func (s *Server) signableJobPath(path string) error {
	rest, ok := strings.CutPrefix(path, "/jobs/")
	if !ok {
		return waxerr.New(waxerr.CodeInvalidRequest, "not a jobs path")
	}
	id, tail, ok := strings.Cut(rest, "/")
	if !ok || !signableJobTail(tail) {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
			"path %q: jobs signing covers /jobs/<id>/events, /jobs/<id>/result, and /jobs/<id>/result/<n>", path))
	}
	if s.jobs == nil {
		return waxerr.New(waxerr.CodeInvalidRequest, "jobs are not enabled on this daemon")
	}
	if _, ok := s.jobs.Get(id); !ok {
		return waxerr.New(waxerr.CodeNotFound, "no such job")
	}
	return nil
}

// signableJobTail reports whether tail names a signable job sub-resource:
// events, result, or result/<n> for a non-negative n.
func signableJobTail(tail string) bool {
	if tail == "events" || tail == "result" {
		return true
	}
	n, ok := strings.CutPrefix(tail, "result/")
	if !ok {
		return false
	}
	i, err := strconv.Atoi(n)
	return err == nil && i >= 0
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
