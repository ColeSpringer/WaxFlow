package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxflow/internal/jobs"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/waxerr"
)

// postJSON posts a raw JSON body to the keyed control plane.
func (e *testEnv) postJSON(t *testing.T, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.ts.URL+path, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func jobsEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnv(t, func(cfg *server.Config) {
		cfg.JobsDir = filepath.Join(t.TempDir(), "jobs")
	})
}

// TestAnalyzeRejectsTranscodeFields pins the per-type field policing behind
// "a 201 means the job will not fail on request shape later": a field that
// does not apply to the job's type is a 400 rather than a field silently
// ignored at run time.
//
// The rule it guards is a hand-maintained disjunction, one clause per field,
// which is exactly the shape that rots when a field is added: the new field
// gets a clause in validateJobRequest or it does not, and nothing else notices.
// Driving every field individually is what makes a missing clause fail here.
// analyzeRejectRows is one entry per transcode-only field on jobRequest,
// spelled as the wire does. The name is the json tag, which is what lets the
// coverage guard below derive from this table rather than restate it.
var analyzeRejectRows = []struct{ name, field string }{
	{"format", `"format":"flac"`},
	{"container", `"container":"adts"`},
	{"rate", `"rate":44100`},
	{"ch", `"ch":1`},
	{"bits", `"bits":16`},
	{"bitrate", `"bitrate":128`},
	{"gain", `"gain":"track"`},
	{"loudness", `"loudness":"analyze"`},
	{"flacLevel", `"flacLevel":5`},
}

// transcodeRejectRows is the mirror: one entry per analyze-only field, which
// a transcode job must refuse for the same reason and by the same rule.
var transcodeRejectRows = []struct{ name, field string }{
	{"silence", `"silence":true`},
	{"silenceThresholdDb", `"silenceThresholdDb":-60`},
	{"silenceMinSeconds", `"silenceMinSeconds":0.3`},
}

func TestAnalyzeRejectsTranscodeFields(t *testing.T) {
	env := jobsEnv(t)

	for _, tc := range analyzeRejectRows {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"type":"analyze","src":"lib/sine.wav",` + tc.field + `}`
			resp := env.postJSON(t, "/jobs", body)
			wantEnvelope(t, resp, http.StatusBadRequest, waxerr.CodeInvalidRequest)
		})
	}

	// The control: analyze with only src is accepted, or the test above would
	// pass trivially by rejecting everything.
	t.Run("bare analyze is accepted", func(t *testing.T) {
		resp := env.postJSON(t, "/jobs", `{"type":"analyze","src":"lib/sine.wav"}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("bare analyze: status = %d, want 201: %s", resp.StatusCode, readBody(t, resp))
		}
	})
}

func TestTranscodeRejectsAnalyzeFields(t *testing.T) {
	env := jobsEnv(t)

	for _, tc := range transcodeRejectRows {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"type":"transcode","src":"lib/sine.wav","format":"flac",` + tc.field + `}`
			resp := env.postJSON(t, "/jobs", body)
			wantEnvelope(t, resp, http.StatusBadRequest, waxerr.CodeInvalidRequest)
		})
	}

	// The control, for the same reason as above.
	t.Run("bare transcode is accepted", func(t *testing.T) {
		resp := env.postJSON(t, "/jobs", `{"type":"transcode","src":"lib/sine.wav","format":"flac"}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("bare transcode: status = %d, want 201: %s", resp.StatusCode, readBody(t, resp))
		}
	})
}

// TestSilenceFieldPolicing covers the silence surface's own shape rules: the
// parameters are legal on analyze, out-of-range values are refused at
// creation rather than at run time, and a parameter with no silence:true is
// refused rather than silently ignored (which is the shape of acceptance the
// honesty gate exists to prevent).
func TestSilenceFieldPolicing(t *testing.T) {
	env := jobsEnv(t)

	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"silence alone is accepted", `{"type":"analyze","src":"lib/sine.wav","silence":true}`, http.StatusCreated},
		{"silence with parameters", `{"type":"analyze","src":"lib/sine.wav","silence":true,"silenceThresholdDb":-60,"silenceMinSeconds":0.25}`, http.StatusCreated},
		{"threshold below the bound", `{"type":"analyze","src":"lib/sine.wav","silence":true,"silenceThresholdDb":-120}`, http.StatusBadRequest},
		{"threshold at full scale", `{"type":"analyze","src":"lib/sine.wav","silence":true,"silenceThresholdDb":0.5}`, http.StatusBadRequest},
		{"minSeconds past the bound", `{"type":"analyze","src":"lib/sine.wav","silence":true,"silenceMinSeconds":61}`, http.StatusBadRequest},
		{"minSeconds negative", `{"type":"analyze","src":"lib/sine.wav","silence":true,"silenceMinSeconds":-1}`, http.StatusBadRequest},
		// A float64 too large for int64 has an implementation-defined
		// conversion; the saturating helper must land it outside the bound.
		{"minSeconds absurd", `{"type":"analyze","src":"lib/sine.wav","silence":true,"silenceMinSeconds":1e30}`, http.StatusBadRequest},
		{"threshold without silence", `{"type":"analyze","src":"lib/sine.wav","silenceThresholdDb":-60}`, http.StatusBadRequest},
		{"minSeconds without silence", `{"type":"analyze","src":"lib/sine.wav","silenceMinSeconds":0.3}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := env.postJSON(t, "/jobs", tc.body)
			if tc.want == http.StatusBadRequest {
				wantEnvelope(t, resp, http.StatusBadRequest, waxerr.CodeInvalidRequest)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.want, readBody(t, resp))
			}
		})
	}
}

// TestSilenceMapServedAsResult drives the wire end of A12: a silence
// analyze job's result is the map file, while a bare analyze job's is still
// the analysis JSON. The two share one endpoint and the branch between them
// is whether the job produced an output at all.
func TestSilenceMapServedAsResult(t *testing.T) {
	env := jobsEnv(t)

	for _, tc := range []struct {
		name, body, wantType string
		wantMap              bool
	}{
		{"silence analyze serves the map", `{"type":"analyze","src":"lib/sine.wav","silence":true}`, "application/json", true},
		{"bare analyze serves the analysis", `{"type":"analyze","src":"lib/sine.wav"}`, "application/json", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := createJob(t, env, tc.body)
			job := awaitJob(t, env, id)

			if tc.wantMap {
				if job.Output == nil || job.Output.File != "silence.json" {
					t.Fatalf("job output = %+v, want silence.json", job.Output)
				}
				if job.Analysis == nil || job.Analysis.Silence == nil {
					t.Fatal("job carries no silence summary")
				}
			} else if job.Output != nil {
				t.Fatalf("bare analyze grew an output: %+v", job.Output)
			}

			resp := env.get(t, "/jobs/"+id+"/result", nil)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("result: status = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
			}
			body := readBody(t, resp)
			if tc.wantMap {
				var doc jobs.SilenceMap
				if err := json.Unmarshal(body, &doc); err != nil {
					t.Fatalf("result is not a silence map: %v\n%s", err, body)
				}
				if doc.Version == "" || doc.SchemaVersion != 1 {
					t.Errorf("result is JSON but not the map document: %s", body)
				}
				if doc.Rate <= 0 {
					t.Errorf("map rate = %d, want the analyzed rate", doc.Rate)
				}
			} else {
				var a jobs.Analysis
				if err := json.Unmarshal(body, &a); err != nil {
					t.Fatalf("result is not an analysis: %v\n%s", err, body)
				}
				if a.Rate <= 0 {
					t.Errorf("analysis rate = %d, want the analyzed rate", a.Rate)
				}
			}
		})
	}
}

// createJob posts a job body and returns the new job's id.
func createJob(t *testing.T, env *testEnv, body string) string {
	t.Helper()
	resp := env.postJSON(t, "/jobs", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201: %s", resp.StatusCode, readBody(t, resp))
	}
	var j jobs.Job
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
		t.Fatalf("decoding the created job: %v", err)
	}
	return j.ID
}

// awaitJob polls until the job is done, failing on any other terminal state.
func awaitJob(t *testing.T, env *testEnv, id string) *jobs.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp := env.get(t, "/jobs/"+id, nil)
		var j jobs.Job
		err := json.NewDecoder(resp.Body).Decode(&j)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("decoding job %s: %v", id, err)
		}
		switch j.State {
		case jobs.StateDone:
			return &j
		case jobs.StateFailed, jobs.StateCanceled:
			t.Fatalf("job %s landed on %s: %+v", id, j.State, j.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s still %s after 10s", id, j.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestJobFieldRejectionCoversEveryField guards the guards: the tables above
// only pin what they list, so a field added to jobRequest without a row in
// one of them would be silently unchecked. Every wire field that is not src
// or type belongs to exactly one job type and must appear in that type's
// table.
//
// covered is derived from the tables rather than restated. A hand-copied list
// would keep passing after a row was deleted, which is the exact failure this
// test exists to prevent.
func TestJobFieldRejectionCoversEveryField(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range analyzeRejectRows {
		covered[tc.name] = true
	}
	for _, tc := range transcodeRejectRows {
		if covered[tc.name] {
			t.Errorf("field %q is in both reject tables; it cannot be both transcode-only "+
				"and analyze-only", tc.name)
		}
		covered[tc.name] = true
	}
	for _, tag := range server.JobRequestJSONTags() {
		switch tag {
		case "type", "src":
			continue // what every job legitimately takes
		}
		if !covered[tag] {
			t.Errorf("jobRequest has field %q with no row in either reject table; "+
				"a job carrying it for the wrong type would go unchecked", tag)
		}
	}
}
