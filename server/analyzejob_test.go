package server_test

import (
	"bytes"
	"net/http"
	"path/filepath"
	"testing"

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
// "a 201 means the job will not fail on request shape later": an analyze job
// takes only src, so any transcode field on one is a 400 rather than a field
// silently ignored at run time.
//
// The rule it guards is a hand-maintained disjunction, one clause per field,
// which is exactly the shape that rots when a field is added: the new field
// gets a clause in validateJobRequest or it does not, and nothing else notices.
// Driving every field individually is what makes a missing clause fail here.
// analyzeRejectRows is one entry per transcode field on jobRequest, spelled as
// the wire does. The name is the json tag, which is what lets the coverage
// guard below derive from this table rather than restate it.
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

// TestAnalyzeRejectionCoversEveryTranscodeField guards the guard: the table
// above only pins what it lists, so a field added to jobRequest without a row
// there would be silently unchecked. Every wire field that is not src or type
// must appear.
//
// covered is derived from analyzeRejectRows rather than restated. A hand-copied
// list would keep passing after a row was deleted from the table, which is the
// exact failure this test exists to prevent.
func TestAnalyzeRejectionCoversEveryTranscodeField(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range analyzeRejectRows {
		covered[tc.name] = true
	}
	for _, tag := range server.JobRequestJSONTags() {
		switch tag {
		case "type", "src":
			continue // what an analyze job legitimately takes
		}
		if !covered[tag] {
			t.Errorf("jobRequest has field %q with no row in TestAnalyzeRejectsTranscodeFields; "+
				"an analyze job carrying it would go unchecked", tag)
		}
	}
}
