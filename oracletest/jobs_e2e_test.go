// The M16 acceptance suite: uploads, the async job API (creation,
// progress events, results, cancel, restart safety), loudness analysis
// with ReplayGain tags on analyzed outputs, the metadata passthrough
// matrix (live minimal tags, job full tags), tag-based gain resolution,
// and the /art and /lyrics passthrough endpoints.
package oracletest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	waxlabel "github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"

	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/internal/jobs"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// jobsEnvDirs holds the durable directories a restart test reuses across
// server instances.
type jobsEnvDirs struct {
	root, cache, jobs, uploads string
}

// withJobs enables the M16 surfaces on a test server config.
func withJobs(d jobsEnvDirs) func(*server.Config) {
	return func(cfg *server.Config) {
		cfg.JobsDir = d.jobs
		cfg.UploadDir = d.uploads
		cfg.Meta = label.New()
	}
}

func newJobsEnv(t *testing.T) (*testEnv, jobsEnvDirs) {
	t.Helper()
	d := jobsEnvDirs{jobs: t.TempDir(), uploads: t.TempDir()}
	env := newTestEnv(t, withJobs(d))
	d.root, d.cache = env.root, env.cache
	return env, d
}

// tinyPNG is a valid 1x1 image, the smallest artwork the mapper accepts.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	b, err := hex.DecodeString(
		"89504e470d0a1a0a0000000d4948445200000001000000010806000000" +
			"1f15c4890000000a49444154789c63000100000500010d0a2db40000000049454e44ae426082")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tagFixture writes metadata onto a library file via the tag library
// (the same one the daemon maps with, used here as the fixture author).
func tagFixture(t *testing.T, path string, set map[tag.Key]string, art []byte, lyrics string) {
	t.Helper()
	ctx := context.Background()
	doc, err := waxlabel.ParseFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	ed := doc.Edit()
	for k, v := range set {
		ed.Set(k, v)
	}
	if lyrics != "" {
		ed.Set(tag.Lyrics, lyrics)
	}
	if art != nil {
		ed.AddPicture(waxlabel.Picture{Type: waxlabel.PicFrontCover, MIME: "image/png", Data: art})
	}
	plan, err := ed.Prepare()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
		t.Fatal(err)
	}
}

// parseTags reads output bytes back through the tag library.
func parseTags(t *testing.T, raw []byte) *waxlabel.Document {
	t.Helper()
	doc, err := waxlabel.Parse(context.Background(), bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("output not parseable for tags: %v", err)
	}
	return doc
}

func (e *testEnv) postJSON(t *testing.T, path string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, e.ts.URL+path, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-Key", testKey)
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeJob(t *testing.T, resp *http.Response, wantStatus int) *jobs.Job {
	t.Helper()
	body := readBody(t, resp)
	if resp.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d (body: %s)", resp.StatusCode, wantStatus, body)
	}
	var j jobs.Job
	if err := json.Unmarshal(body, &j); err != nil {
		t.Fatalf("not a job: %s", body)
	}
	return &j
}

// waitJob polls until the job reaches a terminal state.
func waitJob(t *testing.T, env *testEnv, id string) *jobs.Job {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		j := decodeJob(t, env.get(t, "/jobs/"+id, nil), http.StatusOK)
		if j.State.Terminal() {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s stuck in %s", id, j.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestUploadsE2E(t *testing.T) {
	env, _ := newJobsEnv(t)

	wav, err := os.ReadFile(filepath.Join(env.root, "sine.wav"))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, env.ts.URL+"/uploads?name=sine.wav", bytes.NewReader(wav))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-Key", testKey)
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d: %s", resp.StatusCode, body)
	}
	var up server.UploadResponse
	if err := json.Unmarshal(body, &up); err != nil {
		t.Fatal(err)
	}
	if up.Ref != "upload:"+up.ID || up.Bytes != int64(len(wav)) || up.Name != "sine.wav" {
		t.Fatalf("upload response: %+v", up)
	}

	t.Run("probe and stream the upload", func(t *testing.T) {
		resp := env.get(t, "/probe?src="+up.Ref, nil)
		b := readBody(t, resp)
		if resp.StatusCode != http.StatusOK || !bytes.Contains(b, []byte(`"container":"wav"`)) {
			t.Fatalf("probe upload: %d %s", resp.StatusCode, b)
		}
		resp = env.get(t, "/stream?src="+up.Ref+"&format=flac", nil)
		out := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream upload: %d", resp.StatusCode)
		}
		ref := decodePCM(t, out)
		if ref.N == 0 {
			t.Fatal("streamed upload decoded empty")
		}
	})

	t.Run("job over an upload source", func(t *testing.T) {
		j := decodeJob(t, env.postJSON(t, "/jobs", map[string]any{
			"type": "transcode", "src": up.Ref, "format": "flac", "flacLevel": -1,
		}), http.StatusCreated)
		if got := waitJob(t, env, j.ID); got.State != jobs.StateDone {
			t.Fatalf("upload job ended %s: %+v", got.State, got.Error)
		}
	})

	t.Run("delete removes the upload", func(t *testing.T) {
		resp := env.req(t, http.MethodDelete, "/uploads/"+up.ID, nil)
		readBody(t, resp)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete = %d", resp.StatusCode)
		}
		wantEnvelope(t, env.get(t, "/probe?src="+up.Ref, nil), http.StatusNotFound, waxerr.CodeNotFound)
	})

	t.Run("unknown parameter rejected", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/uploads?nom=x", strings.NewReader("y"))
		req.Header.Set("X-API-Key", testKey)
		resp, err := env.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		wantEnvelope(t, resp, http.StatusBadRequest, waxerr.CodeInvalidRequest)
	})
}

func TestJobTranscodeE2E(t *testing.T) {
	env, _ := newJobsEnv(t)
	tagged := filepath.Join(env.root, "tagged.flac")
	src, err := os.ReadFile(filepath.Join(env.root, "album", "track.flac"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tagged, src, 0o644); err != nil {
		t.Fatal(err)
	}
	tagFixture(t, tagged, map[tag.Key]string{
		tag.Title:  "Job Title",
		tag.Artist: "Job Artist",
	}, nil, "")

	j := decodeJob(t, env.postJSON(t, "/jobs", map[string]any{
		"type": "transcode", "src": "lib/tagged.flac", "format": "opus", "bitrate": 64,
	}), http.StatusCreated)
	if j.State != jobs.StateQueued && j.State != jobs.StateRunning {
		t.Fatalf("fresh job state %s", j.State)
	}
	done := waitJob(t, env, j.ID)
	if done.State != jobs.StateDone || done.Output == nil {
		t.Fatalf("job ended %s: %+v", done.State, done.Error)
	}
	if done.Output.Container != "opus" || done.Output.MediaType != "audio/ogg" {
		t.Fatalf("output: %+v", done.Output)
	}

	t.Run("list contains it", func(t *testing.T) {
		var list server.JobsList
		if err := json.Unmarshal(readBody(t, env.get(t, "/jobs", nil)), &list); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, item := range list.Jobs {
			found = found || item.ID == j.ID
		}
		if !found {
			t.Fatalf("job %s missing from list", j.ID)
		}
	})

	var result []byte
	t.Run("result serves the file with ranges", func(t *testing.T) {
		resp := env.get(t, "/jobs/"+j.ID+"/result", nil)
		result = readBody(t, resp)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "audio/ogg" {
			t.Fatalf("result: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if int64(len(result)) != done.Output.Bytes {
			t.Fatalf("result bytes %d, job says %d", len(result), done.Output.Bytes)
		}
		resp = env.get(t, "/jobs/"+j.ID+"/result", map[string]string{"Range": "bytes=0-99"})
		part := readBody(t, resp)
		if resp.StatusCode != http.StatusPartialContent || len(part) != 100 || !bytes.Equal(part, result[:100]) {
			t.Fatalf("range: %d, %d bytes", resp.StatusCode, len(part))
		}
	})

	t.Run("output decodes and carries the full tag set", func(t *testing.T) {
		pcm := decodePCM(t, result)
		if pcm.N == 0 {
			t.Fatal("empty decode")
		}
		doc := parseTags(t, result)
		if got := doc.Fields().Title; got != "Job Title" {
			t.Errorf("output TITLE = %q", got)
		}
		if got := doc.Fields().Artists; len(got) != 1 || got[0] != "Job Artist" {
			t.Errorf("output ARTIST = %q", got)
		}
	})

	t.Run("delete removes the job", func(t *testing.T) {
		resp := env.req(t, http.MethodDelete, "/jobs/"+j.ID, nil)
		readBody(t, resp)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete = %d", resp.StatusCode)
		}
		wantEnvelope(t, env.get(t, "/jobs/"+j.ID, nil), http.StatusNotFound, waxerr.CodeNotFound)
	})
}

func TestJobValidation(t *testing.T) {
	env, _ := newJobsEnv(t)
	cases := []struct {
		name string
		body map[string]any
		code waxerr.Code
	}{
		{"missing src", map[string]any{"type": "transcode", "format": "opus"}, waxerr.CodeInvalidRequest},
		{"unknown type", map[string]any{"type": "remix", "src": "lib/sine.wav"}, waxerr.CodeInvalidRequest},
		{"missing format", map[string]any{"type": "transcode", "src": "lib/sine.wav"}, waxerr.CodeInvalidRequest},
		{"bitrate on lossless", map[string]any{"type": "transcode", "src": "lib/sine.wav", "format": "flac", "bitrate": 128}, waxerr.CodeUnsupportedFormat},
		{"analyze with extras", map[string]any{"type": "analyze", "src": "lib/sine.wav", "format": "opus"}, waxerr.CodeInvalidRequest},
		{"bad loudness", map[string]any{"type": "transcode", "src": "lib/sine.wav", "format": "opus", "loudness": "no"}, waxerr.CodeInvalidRequest},
		{"gain with analyze", map[string]any{"type": "transcode", "src": "lib/sine.wav", "format": "opus", "loudness": "analyze", "gain": "3"}, waxerr.CodeInvalidRequest},
		{"no such source", map[string]any{"type": "analyze", "src": "lib/absent.wav"}, waxerr.CodeNotFound},
		{"unknown field", map[string]any{"type": "analyze", "src": "lib/sine.wav", "frobnicate": 1}, waxerr.CodeInvalidRequest},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			resp := env.postJSON(t, "/jobs", tt.body)
			if resp.StatusCode == http.StatusCreated {
				t.Fatalf("accepted: %s", readBody(t, resp))
			}
			var env2 server.ErrorBody
			body := readBody(t, resp)
			if err := json.Unmarshal(body, &env2); err != nil || env2.Code != tt.code {
				t.Fatalf("envelope %s, want %s", body, tt.code)
			}
		})
	}
}

func TestJobAnalyzeE2E(t *testing.T) {
	env, _ := newJobsEnv(t)
	// The ramp fixture is 4 s: integrated loudness needs at least one
	// 400 ms gating block, which the sub-second sine fixtures cannot fill.
	j := decodeJob(t, env.postJSON(t, "/jobs", map[string]any{
		"type": "analyze", "src": "lib/ramp.wav",
	}), http.StatusCreated)
	done := waitJob(t, env, j.ID)
	if done.State != jobs.StateDone || done.Analysis == nil {
		t.Fatalf("analyze ended %s: %+v", done.State, done.Error)
	}
	a := done.Analysis
	if a.IntegratedLUFS == nil || math.IsNaN(*a.IntegratedLUFS) || *a.IntegratedLUFS > 0 || *a.IntegratedLUFS < -70 {
		t.Fatalf("integrated = %v", a.IntegratedLUFS)
	}
	if a.TruePeakDB == nil || a.Samples <= 0 || a.DurationSeconds <= 0 {
		t.Fatalf("analysis: %+v", a)
	}

	resp := env.get(t, "/jobs/"+j.ID+"/result", nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("analyze result: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var again jobs.Analysis
	if err := json.Unmarshal(body, &again); err != nil {
		t.Fatalf("result JSON: %v (%s)", err, body)
	}
	if again.IntegratedLUFS == nil || *again.IntegratedLUFS != *a.IntegratedLUFS {
		t.Fatalf("result disagrees with job: %s", body)
	}
}

// TestJobLoudnessAnalyzeRG is the "RG tags written on analyzed outputs"
// exit criterion: a loudness:analyze transcode measures the source,
// applies the exact gain to the ReplayGain reference, and writes
// measured RG2 tags describing the output.
func TestJobLoudnessAnalyzeRG(t *testing.T) {
	env, _ := newJobsEnv(t)
	j := decodeJob(t, env.postJSON(t, "/jobs", map[string]any{
		"type": "transcode", "src": "lib/ramp.wav", "format": "flac", "loudness": "analyze",
	}), http.StatusCreated)
	done := waitJob(t, env, j.ID)
	if done.State != jobs.StateDone {
		t.Fatalf("job ended %s: %+v", done.State, done.Error)
	}
	a := done.Analysis
	if a == nil || a.AppliedGainDB == nil || a.ReplayGainTrackGain == "" || a.ReplayGainTrackPeak == "" {
		t.Fatalf("analysis: %+v", a)
	}
	if *a.AppliedGainDB != -18-*a.IntegratedLUFS {
		t.Fatalf("applied gain %v does not match measurement %v", *a.AppliedGainDB, *a.IntegratedLUFS)
	}

	result := readBody(t, env.get(t, "/jobs/"+j.ID+"/result", nil))
	doc := parseTags(t, result)
	gainVals, _ := doc.Get(tag.ReplayGainTrackGain)
	peakVals, _ := doc.Get(tag.ReplayGainTrackPeak)
	if len(gainVals) != 1 || gainVals[0] != a.ReplayGainTrackGain {
		t.Fatalf("output RG gain %v, job says %q", gainVals, a.ReplayGainTrackGain)
	}
	if len(peakVals) != 1 || peakVals[0] != a.ReplayGainTrackPeak {
		t.Fatalf("output RG peak %v, job says %q", peakVals, a.ReplayGainTrackPeak)
	}
	// The output sits at the reference by construction, so its own gain
	// tag must be near zero (lossless transcode, exact gain).
	var outGain float64
	if _, err := fmt.Sscanf(gainVals[0], "%f dB", &outGain); err != nil {
		t.Fatal(err)
	}
	if math.Abs(outGain) > 0.2 {
		t.Fatalf("analyzed output reads %s from the reference, want ~0", gainVals[0])
	}

	t.Run("fragmented mp4 output derives and patches", func(t *testing.T) {
		// The engine cannot decode its own fragmented MP4 back, so the
		// output values derive from the source measurement and patch the
		// placeholder atoms in place.
		j := decodeJob(t, env.postJSON(t, "/jobs", map[string]any{
			"type": "transcode", "src": "lib/ramp.wav", "format": "aac", "loudness": "analyze",
		}), http.StatusCreated)
		done := waitJob(t, env, j.ID)
		if done.State != jobs.StateDone || done.Analysis == nil {
			t.Fatalf("aac job ended %s: %+v", done.State, done.Error)
		}
		a := done.Analysis
		// Derivation contract: output loudness is the source measurement
		// plus the applied gain, which lands exactly on the reference, so
		// the derived gain tag is exactly zero (the lossy delta is not
		// measurable without an fMP4 read path, by design). The peak tag
		// is the patch witness: its derived value cannot equal the
		// placeholder for a non-silent source.
		if a.ReplayGainTrackGain != "+00.00 dB" {
			t.Fatalf("derived RG gain %q, want the reference exactly", a.ReplayGainTrackGain)
		}
		if a.ReplayGainTrackPeak == "" || a.ReplayGainTrackPeak == meta.FormatPeak(0) {
			t.Fatalf("derived RG peak %q", a.ReplayGainTrackPeak)
		}
		result := readBody(t, env.get(t, "/jobs/"+j.ID+"/result", nil))
		if !bytes.Contains(result, []byte(a.ReplayGainTrackPeak)) {
			t.Fatalf("output lacks the patched RG peak %q", a.ReplayGainTrackPeak)
		}
		if !bytes.Contains(result, []byte("iTunSMPB")) {
			t.Fatal("output lacks the gapless atom")
		}
	})
}

// TestJobEventsSSE is the SSE exit criterion: the events endpoint
// replays the current state, streams updates, and ends cleanly after
// the terminal event, for both key and signed auth.
func TestJobEventsSSE(t *testing.T) {
	env, _ := newJobsEnv(t)
	// A long enough job that subscription races cannot miss every
	// intermediate state: the 4 s ramp fixture through opus.
	j := decodeJob(t, env.postJSON(t, "/jobs", map[string]any{
		"type": "transcode", "src": "lib/ramp.wav", "format": "opus",
	}), http.StatusCreated)

	events := readSSE(t, env, "/jobs/"+j.ID+"/events", map[string]string{"X-API-Key": testKey})
	if len(events) == 0 {
		t.Fatal("no events")
	}
	last := events[len(events)-1]
	if last.State != jobs.StateDone {
		t.Fatalf("stream ended on %s", last.State)
	}

	t.Run("signed events and result", func(t *testing.T) {
		var signed server.SignResponse
		resp := env.postJSON(t, "/sign", map[string]any{"path": "/jobs/" + j.ID + "/events"})
		if err := json.Unmarshal(readBody(t, resp), &signed); err != nil {
			t.Fatal(err)
		}
		events := readSSE(t, env, signed.URL, map[string]string{"X-API-Key": ""})
		if len(events) != 1 || events[0].State != jobs.StateDone {
			t.Fatalf("signed replay: %d events", len(events))
		}

		resp = env.postJSON(t, "/sign", map[string]any{"path": "/jobs/" + j.ID + "/result"})
		if err := json.Unmarshal(readBody(t, resp), &signed); err != nil {
			t.Fatal(err)
		}
		rr := env.get(t, signed.URL, map[string]string{"X-API-Key": ""})
		readBody(t, rr)
		if rr.StatusCode != http.StatusOK {
			t.Fatalf("signed result = %d", rr.StatusCode)
		}
	})
}

// readSSE consumes a job event stream to its end and returns the parsed
// job snapshots.
func readSSE(t *testing.T, env *testEnv, path string, hdr map[string]string) []*jobs.Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, env.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("events content type %q", ct)
	}
	var out []*jobs.Job
	scan := bufio.NewScanner(resp.Body)
	scan.Buffer(make([]byte, 1<<20), 1<<20)
	for scan.Scan() {
		line := scan.Text()
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var j jobs.Job
			if err := json.Unmarshal([]byte(data), &j); err != nil {
				t.Fatalf("event payload: %v (%s)", err, data)
			}
			out = append(out, &j)
		}
	}
	if err := scan.Err(); err != nil && err != io.ErrUnexpectedEOF {
		t.Fatalf("event stream: %v", err)
	}
	return out
}

// TestJobRestartSafety is the restart-safety exit criterion: completed
// results survive a daemon restart, and an interrupted job restarts
// cleanly from zero, idempotent.
func TestJobRestartSafety(t *testing.T) {
	// Durable dirs shared by both server generations.
	d := jobsEnvDirs{jobs: t.TempDir(), uploads: t.TempDir()}
	envA := newTestEnv(t, withJobs(d))
	d.root, d.cache = envA.root, envA.cache

	j := decodeJob(t, envA.postJSON(t, "/jobs", map[string]any{
		"type": "transcode", "src": "lib/sine.wav", "format": "flac",
	}), http.StatusCreated)
	done := waitJob(t, envA, j.ID)
	if done.State != jobs.StateDone {
		t.Fatalf("job ended %s", done.State)
	}
	resultA := readBody(t, envA.get(t, "/jobs/"+j.ID+"/result", nil))

	// Plant an interrupted job: running state on disk, a partial output,
	// and a stray temp file. The next start must requeue it from zero.
	interruptedID := "01AAAAAAAAAAAAAAAAAAAAAAAA"
	idRoots, err := source.OpenRoots([]source.Root{{Name: "lib", Path: d.root}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	f, err := idRoots.Resolve(context.Background(), "lib/sine.wav")
	if err != nil {
		t.Fatal(err)
	}
	sourceID := f.ID.String()
	f.Close()
	idRoots.Close()
	interrupted := jobs.Job{
		SchemaVersion: 1,
		ID:            interruptedID,
		Type:          jobs.TypeTranscode,
		State:         jobs.StateRunning,
		Request: jobs.Request{
			Type: jobs.TypeTranscode, Src: "lib/sine.wav", SourceID: sourceID,
			Format: "flac",
		},
		Created: time.Now().UTC(),
	}
	dir := filepath.Join(d.jobs, interruptedID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(&interrupted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "job.json"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out.flac"), []byte("partial junk"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Stop generation A, start generation B over the same directories.
	envA.ts.Close()
	if err := envA.srv.Close(); err != nil {
		t.Fatal(err)
	}
	roots, err := source.OpenRoots([]source.Root{{Name: "lib", Path: d.root}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	cfgB := server.Config{
		Addr:        "127.0.0.1:4418",
		APIKeys:     []string{testKey},
		SigningKeys: []server.SigningKey{{ID: "1", Secret: []byte(testSecret)}},
		Resolver:    roots,
		CacheDir:    d.cache,
		Version:     "test",
	}
	withJobs(d)(&cfgB)
	srvB, err := server.New(cfgB)
	if err != nil {
		t.Fatal(err)
	}
	tsB := httptest.NewServer(srvB)
	envB := &testEnv{ts: tsB, srv: srvB, root: d.root, cache: d.cache}
	t.Cleanup(func() {
		tsB.Close()
		srvB.Close()
		roots.Close()
	})

	t.Run("completed job survives", func(t *testing.T) {
		got := decodeJob(t, envB.get(t, "/jobs/"+j.ID, nil), http.StatusOK)
		if got.State != jobs.StateDone || got.Output == nil {
			t.Fatalf("survivor: %+v", got)
		}
		resultB := readBody(t, envB.get(t, "/jobs/"+j.ID+"/result", nil))
		if !bytes.Equal(resultA, resultB) {
			t.Fatal("result bytes changed across restart")
		}
	})

	t.Run("interrupted job reruns from zero", func(t *testing.T) {
		got := waitJob(t, envB, interruptedID)
		if got.State != jobs.StateDone || got.Output == nil {
			t.Fatalf("requeued job: state %s, %+v", got.State, got.Error)
		}
		out := readBody(t, envB.get(t, "/jobs/"+interruptedID+"/result", nil))
		if bytes.Contains(out, []byte("partial junk")) || len(out) != int(got.Output.Bytes) {
			t.Fatal("partial output survived the requeue")
		}
		if decodePCM(t, out).N == 0 {
			t.Fatal("rerun output does not decode")
		}
	})
}

// TestStreamMinimalTags pins the live half of the passthrough matrix:
// a progressive transcode carries the minimal descriptive set in its
// stream form and never the source's ReplayGain tags.
func TestStreamMinimalTags(t *testing.T) {
	env, _ := newJobsEnv(t)
	tagged := filepath.Join(env.root, "tagged.flac")
	src, err := os.ReadFile(filepath.Join(env.root, "album", "track.flac"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tagged, src, 0o644); err != nil {
		t.Fatal(err)
	}
	tagFixture(t, tagged, map[tag.Key]string{
		tag.Title:               "Live Title",
		tag.Artist:              "Live Artist",
		tag.ReplayGainTrackGain: "-6.00 dB",
	}, nil, "")

	for _, format := range []string{"opus", "flac", "mp3"} {
		t.Run(format, func(t *testing.T) {
			// bits=24 forces the flac case through the transcode rung: the
			// same format over a compliant source direct-plays the original
			// bytes, whose tags are the source's own business.
			extra := ""
			if format == "flac" {
				extra = "&bits=24"
			}
			resp := env.get(t, "/stream?src=lib/tagged.flac&format="+format+"&gain=off"+extra, nil)
			raw := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("stream = %d", resp.StatusCode)
			}
			doc := parseTags(t, raw)
			if got := doc.Fields().Title; got != "Live Title" {
				t.Errorf("TITLE = %q", got)
			}
			if vals, ok := doc.Get(tag.ReplayGainTrackGain); ok {
				t.Errorf("live stream carries ReplayGain %v", vals)
			}
		})
	}
}

// TestGainTrackResolvesReplayGain pins tag-based gain resolution: the
// same source streams 6 dB quieter under gain=track when its tags say
// -6.00 dB.
func TestGainTrackResolvesReplayGain(t *testing.T) {
	env, _ := newJobsEnv(t)
	tagged := filepath.Join(env.root, "quiet.flac")
	src, err := os.ReadFile(filepath.Join(env.root, "album", "track.flac"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tagged, src, 0o644); err != nil {
		t.Fatal(err)
	}
	tagFixture(t, tagged, map[tag.Key]string{tag.ReplayGainTrackGain: "-6.00 dB"}, nil, "")

	peakOf := func(gain string) float64 {
		resp := env.get(t, "/stream?src=lib/quiet.flac&format=wav&gain="+gain, nil)
		raw := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream gain=%s: %d", gain, resp.StatusCode)
		}
		buf := decodePCM(t, raw)
		peak := 0.0
		for c := 0; c < buf.Fmt.Channels; c++ {
			for _, v := range buf.ChanI(c) {
				peak = math.Max(peak, math.Abs(float64(v)))
			}
		}
		return peak
	}
	off, track := peakOf("off"), peakOf("track")
	ratio := track / off
	want := math.Pow(10, -6.0/20)
	if math.Abs(ratio-want) > 0.01 {
		t.Fatalf("gain=track peak ratio %.4f, want %.4f", ratio, want)
	}
}

func TestArtAndLyrics(t *testing.T) {
	env, _ := newJobsEnv(t)
	png := tinyPNG(t)
	tagged := filepath.Join(env.root, "artful.flac")
	src, err := os.ReadFile(filepath.Join(env.root, "album", "track.flac"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tagged, src, 0o644); err != nil {
		t.Fatal(err)
	}
	tagFixture(t, tagged, map[tag.Key]string{tag.Title: "Arty"}, png, "la la la\nsecond line")

	t.Run("art round trip", func(t *testing.T) {
		resp := env.get(t, "/art?src=lib/artful.flac", nil)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" {
			t.Fatalf("art: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if !bytes.Equal(body, png) {
			t.Fatalf("art bytes differ (%d vs %d)", len(body), len(png))
		}
		if resp.Header.Get("ETag") == "" {
			t.Fatal("no ETag")
		}
	})

	t.Run("lyrics round trip", func(t *testing.T) {
		resp := env.get(t, "/lyrics?src=lib/artful.flac", nil)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/plain") {
			t.Fatalf("lyrics: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if string(body) != "la la la\nsecond line" {
			t.Fatalf("lyrics = %q", body)
		}
	})

	t.Run("absent art is not found", func(t *testing.T) {
		wantEnvelope(t, env.get(t, "/art?src=lib/sine.wav", nil), http.StatusNotFound, waxerr.CodeNotFound)
	})

	t.Run("signed art", func(t *testing.T) {
		var signed server.SignResponse
		resp := env.postJSON(t, "/sign", map[string]any{"path": "/art", "params": map[string]string{"src": "lib/artful.flac"}})
		if err := json.Unmarshal(readBody(t, resp), &signed); err != nil {
			t.Fatal(err)
		}
		rr := env.get(t, signed.URL, map[string]string{"X-API-Key": ""})
		body := readBody(t, rr)
		if rr.StatusCode != http.StatusOK || !bytes.Equal(body, png) {
			t.Fatalf("signed art: %d", rr.StatusCode)
		}
	})

	t.Run("probe reports the metadata", func(t *testing.T) {
		var info server.ProbeInfo
		if err := json.Unmarshal(readBody(t, env.get(t, "/probe?src=lib/artful.flac", nil)), &info); err != nil {
			t.Fatal(err)
		}
		if !info.HasArt || !info.HasLyrics || len(info.Tags["TITLE"]) != 1 {
			t.Fatalf("probe: hasArt=%v hasLyrics=%v tags=%v", info.HasArt, info.HasLyrics, info.Tags)
		}
		// The probe is a tag summary: the lyric sheet itself is /lyrics
		// business and must not inflate every probe body.
		if _, ok := info.Tags["LYRICS"]; ok {
			t.Fatal("probe body carries the full lyrics text")
		}
	})
}

func TestCapsAdvertisesJobsAndUploads(t *testing.T) {
	env, _ := newJobsEnv(t)
	var caps server.Caps
	if err := json.Unmarshal(readBody(t, env.get(t, "/caps", nil)), &caps); err != nil {
		t.Fatal(err)
	}
	if !caps.Delivery.Jobs || !caps.Delivery.Uploads {
		t.Fatalf("caps: %+v", caps.Delivery)
	}

	// The default environment (no dirs) must keep advertising false; the
	// committed caps golden pins the same fact.
	bare := newTestEnv(t, nil)
	if err := json.Unmarshal(readBody(t, bare.get(t, "/caps", nil)), &caps); err != nil {
		t.Fatal(err)
	}
	if caps.Delivery.Jobs || caps.Delivery.Uploads {
		t.Fatalf("bare caps: %+v", caps.Delivery)
	}
}
