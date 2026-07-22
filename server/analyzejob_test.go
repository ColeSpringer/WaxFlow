package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow/client"
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

// jobField is one wire field: how it is spelled in a body, and which job types
// legitimately take it.
type jobField struct {
	// json is the field's wire name, which is what lets the coverage guard
	// below derive from this table rather than restate it.
	json string
	// spelling is the field as a JSON body fragment, with a value that would
	// be accepted by a type that does take it. A value the owning type would
	// reject anyway would make a rejection here prove nothing.
	spelling string
	// owners are the types the field is legal on. Every other type must
	// refuse it.
	owners []string
}

// jobFields is the whole POST /jobs wire surface, one row per field, and the
// single statement of which type takes what.
//
// It is one table rather than a reject list per type because the rule is
// ownership and the rejections are its consequence: with four creatable types,
// per-type lists would restate the same fact up to four times and could
// disagree. The tests below derive both directions from this: every type
// refuses every field it does not own, and every type accepts a body of the
// fields it does.
//
// What it guards is the promise that a 201 means the job will not fail on
// request shape later. validateJobRequest enforces that with a hand-maintained
// clause per field, which is exactly the shape that rots when a field is
// added: the new field gets a clause or it does not, and nothing else notices.
// Driving every (type, field) pair is what makes a missing clause fail here.
var jobFields = []jobField{
	{"srcs", `"srcs":["lib/sine.wav","lib/sine.wav"]`, []string{"merge"}},
	// Per-member chapter titles: merge's alone, one per member. A two-title
	// spelling matches the two-member merge base above, so a type that does not
	// own it refuses a well-formed value rather than a malformed one.
	{"titles", `"titles":["Intro","Chapter Two"]`, []string{"merge"}},
	{"cuts", `"cuts":[1000]`, []string{"split"}},
	// cue is split's other way of saying cuts. It is owned by split alone
	// for the same reason cuts is, and the two being exclusive of each
	// other is a separate rule this table cannot express (it describes
	// which type owns a field, not which fields conflict), so
	// TestSplitJobCueRejects carries that one.
	{"cue", `"cue":"lib/rip.cue"`, []string{"split"}},
	{"format", `"format":"flac"`, []string{"transcode", "merge", "split"}},
	{"container", `"container":"mka"`, []string{"transcode", "merge", "split"}},
	{"rate", `"rate":44100`, []string{"transcode", "merge", "split"}},
	{"ch", `"ch":1`, []string{"transcode", "merge", "split"}},
	{"bits", `"bits":16`, []string{"transcode", "merge", "split"}},
	{"bitrate", `"bitrate":128`, []string{"transcode", "merge", "split"}},
	// Gain and loudness stop at transcode: both answer "how loud is this one
	// track", which a merge (N in, one out) and a split (one in, N out) have
	// no honest way to apply.
	{"gain", `"gain":"track"`, []string{"transcode"}},
	{"loudness", `"loudness":"analyze"`, []string{"transcode"}},
	{"flacLevel", `"flacLevel":5`, []string{"transcode", "merge", "split"}},
	{"silence", `"silence":true`, []string{"analyze"}},
	{"silenceThresholdDb", `"silenceThresholdDb":-60`, []string{"analyze"}},
	{"silenceMinSeconds", `"silenceMinSeconds":0.3`, []string{"analyze"}},
}

// jobTypeBodies is a minimal accepted body per creatable type: exactly the
// fields that type requires, and nothing optional. Injecting a non-owned field
// into one of these is what each rejection case does, so the base must never
// contain a field it is about to be given (a duplicate JSON key decodes to the
// last one, and the test would be checking the wrong value).
var jobTypeBodies = map[string]string{
	"analyze":   `{"type":"analyze","src":"lib/sine.wav"}`,
	"transcode": `{"type":"transcode","src":"lib/sine.wav","format":"flac"}`,
	"merge":     `{"type":"merge","srcs":["lib/sine.wav","lib/sine.wav"],"format":"flac"}`,
	"split":     `{"type":"split","src":"lib/ramp.wav","format":"flac","cuts":[1000]}`,
}

// TestJobFieldPolicing drives every (type, field) pair the table describes: a
// field on a type that does not own it is a 400, never a field quietly ignored
// at run time. The per-type control (a bare body of only what that type needs)
// is what stops the whole thing passing trivially by rejecting everything.
func TestJobFieldPolicing(t *testing.T) {
	env := jobsEnv(t)

	for typ, base := range jobTypeBodies {
		t.Run(typ, func(t *testing.T) {
			t.Run("the bare body is accepted", func(t *testing.T) {
				resp := env.postJSON(t, "/jobs", base)
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusCreated {
					t.Fatalf("bare %s: status = %d, want 201: %s", typ, resp.StatusCode, readBody(t, resp))
				}
			})
			for _, f := range jobFields {
				if slices.Contains(f.owners, typ) {
					continue
				}
				t.Run("rejects "+f.json, func(t *testing.T) {
					body := strings.TrimSuffix(base, "}") + "," + f.spelling + "}"
					resp := env.postJSON(t, "/jobs", body)
					wantEnvelope(t, resp, http.StatusBadRequest, waxerr.CodeInvalidRequest)
				})
			}
		})
	}

	// src is the one field the table cannot describe, because it is not
	// owned-or-refused: three types require it and merge refuses it. Both
	// halves are checked, since a merge that took a src would be a merge that
	// silently ignored either it or its member list.
	t.Run("merge rejects src", func(t *testing.T) {
		resp := env.postJSON(t, "/jobs",
			`{"type":"merge","src":"lib/sine.wav","srcs":["lib/sine.wav"],"format":"flac"}`)
		wantEnvelope(t, resp, http.StatusBadRequest, waxerr.CodeInvalidRequest)
	})
	for _, tc := range []struct{ typ, body string }{
		{"analyze", `{"type":"analyze"}`},
		{"transcode", `{"type":"transcode","format":"flac"}`},
		{"split", `{"type":"split","format":"flac","cuts":[1000]}`},
	} {
		t.Run(tc.typ+" needs src", func(t *testing.T) {
			resp := env.postJSON(t, "/jobs", tc.body)
			wantEnvelope(t, resp, http.StatusBadRequest, waxerr.CodeInvalidRequest)
		})
	}
	// Timeline is a real job type and still not creatable here: POST
	// /hls/timeline owns it, and a second front door would skip its fast path.
	t.Run("timeline is not creatable", func(t *testing.T) {
		resp := env.postJSON(t, "/jobs", `{"type":"timeline","srcs":["lib/sine.wav"]}`)
		wantEnvelope(t, resp, http.StatusBadRequest, waxerr.CodeInvalidRequest)
	})
}

// probeSamples asks the daemon how long a source really is. It is how the cut
// cases below learn the number they are written against, rather than restating
// it: a literal compared to a literal is a guard that holds nothing, and the
// cases that name the end stop naming it the moment the fixture moves.
func probeSamples(t *testing.T, env *testEnv, ref string) int64 {
	t.Helper()
	var out struct {
		Tracks []struct {
			Samples int64 `json:"samples"`
		} `json:"tracks"`
	}
	body := readBody(t, env.get(t, "/probe?src="+ref, nil))
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("probing %s: %v: %s", ref, err, body)
	}
	if len(out.Tracks) == 0 {
		t.Fatalf("probing %s returned no tracks: %s", ref, body)
	}
	return out.Tracks[0].Samples
}

// TestSplitCutPolicing pins the cut list's own shape rules at creation, which
// is where the honesty gate has to catch them: a cut list that does not
// describe this source must be a 400 and not a job that dies on piece 7.
//
// Every boundary case derives from the fixture's own probed length rather than
// from a literal. "The last sample" and "a cut at the end" are only those
// things relative to a real end, and a fixture swapped under them would
// otherwise leave the names describing cases the table no longer runs.
func TestSplitCutPolicing(t *testing.T) {
	env := jobsEnv(t)
	total := probeSamples(t, env, "lib/ramp.wav")
	if total < 3 {
		t.Fatalf("lib/ramp.wav probes as %d samples; these cases need a source with an interior to cut", total)
	}

	for _, tc := range []struct {
		name string
		cuts []int64
		want int
	}{
		{"one interior cut", []int64{1000}, http.StatusCreated},
		{"several ascending cuts", []int64{1000, 2000, total - 12000}, http.StatusCreated},
		{"the last sample", []int64{total - 1}, http.StatusCreated},
		// A leading 0 asks for an empty first piece, which is what makes the
		// implicit-0 convention the only one where every cut does something.
		{"a leading zero", []int64{0, 1000}, http.StatusBadRequest},
		{"a duplicate cut", []int64{1000, 1000}, http.StatusBadRequest},
		{"descending cuts", []int64{2000, 1000}, http.StatusBadRequest},
		{"a negative cut", []int64{-1}, http.StatusBadRequest},
		// At the end is as empty as past it: both ask for a piece of nothing.
		{"a cut at the end", []int64{total}, http.StatusBadRequest},
		{"a cut past the end", []int64{total * 2}, http.StatusBadRequest},
		{"no cuts at all", []int64{}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cuts, err := json.Marshal(tc.cuts)
			if err != nil {
				t.Fatal(err)
			}
			body := `{"type":"split","src":"lib/ramp.wav","format":"flac","cuts":` + string(cuts) + `}`
			resp := env.postJSON(t, "/jobs", body)
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

// adtsFixture transcodes lib/ramp.wav into an ADTS-wrapped AAC file in the
// library root and returns its reference.
//
// ADTS is the point rather than an incidental choice of codec: it declares no
// length at all (the stream simply runs to EOF), so its probed total is -1.
// That is the one source shape a cut list cannot be policed against without
// measuring, and every length-declaring fixture in this suite hides it.
func adtsFixture(t *testing.T, env *testEnv, name string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		env.ts.URL+"/transcode?src=lib/ramp.wav&format=aac&container=adts", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("building the ADTS fixture: status %d: %s", resp.StatusCode, readBody(t, resp))
	}
	if err := os.WriteFile(filepath.Join(env.root, name), readBody(t, resp), 0o644); err != nil {
		t.Fatal(err)
	}
	ref := "lib/" + name
	if n := probeSamples(t, env, ref); n >= 0 {
		t.Fatalf("%s probes as %d samples; this fixture is only worth anything while it "+
			"declares no length (-1)", ref, n)
	}
	return ref
}

// TestSplitCutsAreMeasuredNotDeclared covers the source the case above cannot:
// one whose headers declare no length.
//
// SplitSpans only bounds a cut when it is handed a total, so a source that
// declares none (-1) skips the overshoot check entirely and every cut is
// accepted, however far past the end it sits. The damage is not just a late
// failure: the pieces are written in order, so a job cut past the end lands
// piece 0..k-1 in the job directory and then dies at read, and job.Outputs is
// only assigned after the loop, so the pieces persist with no Output naming
// them and nothing to clean them up by. Measuring the source at creation is
// what turns that into a 400.
func TestSplitCutsAreMeasuredNotDeclared(t *testing.T) {
	env := jobsEnv(t)
	ref := adtsFixture(t, env, "ramp.aac")
	// The fixture is a transcode of a 4 s source, so its real length is within
	// an AAC frame of 192000 samples. 400000 is past any of that, and 1000 is
	// inside all of it, which is what lets these bound the truth without
	// restating a number no header declares.
	for _, tc := range []struct {
		name string
		cut  int64
		want int
	}{
		{"a cut past the real end", 400000, http.StatusBadRequest},
		{"a cut inside it", 1000, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"type":"split","src":%q,"format":"flac","cuts":[%d]}`, ref, tc.cut)
			resp := env.postJSON(t, "/jobs", body)
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

	// The accepted cut must also still run to completion. Without this the
	// refusal above would be satisfied just as well by a version that measured
	// wrongly and refused everything.
	body := fmt.Sprintf(`{"type":"split","src":%q,"format":"flac","cuts":[1000]}`, ref)
	job := awaitJob(t, env, createJob(t, env, body))
	if len(job.Outputs) != 2 {
		t.Fatalf("splitting %s at one cut made %d pieces, want 2: %+v", ref, len(job.Outputs), job.Outputs)
	}
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
				if len(job.Outputs) != 1 || job.Outputs[0].File != "silence.json" {
					t.Fatalf("job outputs = %+v, want one silence.json", job.Outputs)
				}
				if job.Analysis == nil || job.Analysis.Silence == nil {
					t.Fatal("job carries no silence summary")
				}
			} else if len(job.Outputs) != 0 {
				t.Fatalf("bare analyze grew an output: %+v", job.Outputs)
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

// TestJobFieldsCoversEveryField guards the guard: jobFields only pins what it
// lists, so a field added to jobRequest without a row there would be silently
// unchecked, and an owner naming a type that does not exist would quietly
// check nothing.
//
// Both sides are derived from the code rather than restated. A hand-copied
// list would keep passing after a row was deleted, which is the exact failure
// this exists to prevent.
func TestJobFieldsCoversEveryField(t *testing.T) {
	owned := map[string]bool{}
	for _, f := range jobFields {
		if owned[f.json] {
			t.Errorf("field %q has two rows in jobFields; the owners of one of them are a lie", f.json)
		}
		owned[f.json] = true
		if len(f.owners) == 0 {
			t.Errorf("field %q is owned by no type; drop it from jobRequest rather than "+
				"leaving a field no job may send", f.json)
		}
		for _, o := range f.owners {
			if _, ok := jobTypeBodies[o]; !ok {
				t.Errorf("field %q names owner %q, which is not a creatable job type", f.json, o)
			}
		}
	}
	for _, tag := range server.JobRequestJSONTags() {
		switch tag {
		case "type", "src":
			// The two the table cannot describe: type selects the rule, and src
			// is required by three types and refused by merge, both checked by
			// name in TestJobFieldPolicing.
			continue
		}
		if !owned[tag] {
			t.Errorf("jobRequest has field %q with no row in jobFields; a job carrying it "+
				"for the wrong type would go unchecked", tag)
		}
	}
}

// TestClientJobsLifecycle drives the whole jobs surface through the client
// package, which is what makes docs/api.md's reference-consumer claim true
// for jobs: create an analyze job, poll it to done, fetch the result, read
// the event stream to its terminal event (the one pin of SSE framing through
// the client), delete, and classify an unknown id by its typed code. The
// typed-error half is the claim a raw-HTTP bridge cannot make: every failure
// here must come back as a waxerr code, not a status to re-parse.
func TestClientJobsLifecycle(t *testing.T) {
	env := jobsEnv(t)
	c, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	created, err := c.CreateJob(ctx, client.JobRequest{Type: "analyze", Src: "lib/sine.wav", Silence: true})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Type != "analyze" || created.SchemaVersion != jobs.SchemaVersion {
		t.Fatalf("created job = %+v, want an analyze job with an id at schema %d", created, jobs.SchemaVersion)
	}
	if created.Request.Src != "lib/sine.wav" || !created.Request.Silence {
		t.Fatalf("echoed request = %+v; the client's fields did not survive the round trip", created.Request)
	}
	if created.Request.SourceID == "" {
		t.Error("echoed request carries no sourceId; the echo-only mirror is missing or mistagged")
	}

	// Poll through the client to done, the loop every daemon manager runs.
	deadline := time.Now().Add(10 * time.Second)
	var job *client.Job
	for {
		if job, err = c.Job(ctx, created.ID); err != nil {
			t.Fatal(err)
		}
		if job.State == "done" {
			break
		}
		if job.State == "failed" || job.State == "canceled" {
			t.Fatalf("job landed on %s: %+v", job.State, job.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("job still %s after 10s", job.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The pointer mirrors are presence: a done analyze job has all three.
	if job.Finished == nil {
		t.Error("done job has a nil Finished; the pointer mirror lost the timestamp")
	}
	if job.Analysis == nil || job.Analysis.Silence == nil {
		t.Fatalf("done job carries no silence summary: %+v", job.Analysis)
	}
	if job.Analysis.Silence.Version == "" {
		t.Error("silence summary carries no detector version")
	}

	// The listing mirrors the server's envelope and holds the job.
	list, err := c.Jobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if list.SchemaVersion != 1 {
		t.Errorf("jobs list schemaVersion = %d, want 1", list.SchemaVersion)
	}
	if !slices.ContainsFunc(list.Jobs, func(j *client.Job) bool { return j.ID == created.ID }) {
		t.Fatalf("Jobs() does not list %s", created.ID)
	}

	// The result through the client is byte-identical to a raw GET.
	resp, err := c.JobResult(ctx, created.ID, -1)
	if err != nil {
		t.Fatal(err)
	}
	viaClient := readBody(t, resp)
	if want := readBody(t, env.get(t, "/jobs/"+created.ID+"/result", nil)); !bytes.Equal(viaClient, want) {
		t.Fatalf("JobResult differs from a raw GET:\n client: %s\n    raw: %s", viaClient, want)
	}

	// The event stream: SSE headers, "event: job" framing, data lines that
	// decode as job documents, and the stream ending on its own after the
	// terminal event (on a done job the daemon replays the terminal snapshot
	// and closes, so reading to EOF is bounded and proves the ending).
	evCtx, evCancel := context.WithTimeout(ctx, 10*time.Second)
	defer evCancel()
	ev, err := c.JobEvents(evCtx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ct := ev.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("events Content-Type = %q, want text/event-stream", ct)
	}
	stream := readBody(t, ev)
	if !bytes.Contains(stream, []byte("event: job\ndata: ")) {
		t.Fatalf("event stream carries no event: job frame: %q", stream)
	}
	var last client.Job
	for _, line := range strings.Split(string(stream), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			if err := json.Unmarshal([]byte(data), &last); err != nil {
				t.Fatalf("event data does not decode as a job document: %v\n%s", err, data)
			}
		}
	}
	if last.ID != created.ID || last.State != "done" {
		t.Fatalf("the stream's last event is job %q in state %q, want %s done", last.ID, last.State, created.ID)
	}

	// The result endpoint serves ranges, and WithHTTPClient can bring a
	// range-requesting transport (a resumable download), so a 206 is a
	// success JobResult must pass through, not an envelope to decode. The
	// default transport never asks for one, which is why reaching this
	// takes a header-injecting round tripper.
	ranged, err := client.New(env.ts.URL, testKey,
		client.WithHTTPClient(&http.Client{Transport: rangeInjector{next: http.DefaultTransport}}))
	if err != nil {
		t.Fatal(err)
	}
	part, err := ranged.JobResult(ctx, created.ID, -1)
	if err != nil {
		t.Fatalf("ranged JobResult: %v", err)
	}
	if part.StatusCode != http.StatusPartialContent {
		t.Fatalf("ranged JobResult = %d, want 206", part.StatusCode)
	}
	if got := readBody(t, part); !bytes.Equal(got, viaClient[:10]) {
		t.Fatalf("ranged JobResult = %q, want the full result's first 10 bytes %q", got, viaClient[:10])
	}

	if err := c.DeleteJob(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Job(ctx, created.ID); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Fatalf("fetching a deleted job errs %v, want code %s", err, waxerr.CodeNotFound)
	}

	// The typed-error claim, on every surface an unknown id can reach.
	if _, err := c.JobResult(ctx, "01NOSUCHJOB", -1); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Fatalf("unknown job's result errs %v, want code %s", err, waxerr.CodeNotFound)
	}
	if err := c.DeleteJob(ctx, "01NOSUCHJOB"); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Fatalf("deleting an unknown job errs %v, want code %s", err, waxerr.CodeNotFound)
	}

	// An empty id is refused client-side before it can misroute: "/jobs/"
	// is not a job URL, and the mux's path cleaning would turn
	// "/jobs//result" into a lookup of a job literally named "result",
	// whose not-found would read as the daemon's word on a job that was
	// never named.
	for name, call := range map[string]func() error{
		"Job":       func() error { _, err := c.Job(ctx, ""); return err },
		"DeleteJob": func() error { return c.DeleteJob(ctx, "") },
		"JobResult": func() error { _, err := c.JobResult(ctx, "", -1); return err },
		"JobEvents": func() error { _, err := c.JobEvents(ctx, ""); return err },
	} {
		if err := call(); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
			t.Errorf("%s with an empty id errs %v, want code %s", name, err, waxerr.CodeInvalidRequest)
		}
	}

	// A context the caller ended reports canceled, not a fabricated
	// network failure: a slow daemon and an unreachable one need
	// different operator responses.
	gone, cancelGone := context.WithCancel(ctx)
	cancelGone()
	if _, err := c.Jobs(gone); waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Errorf("a canceled ctx errs %v, want code %s", err, waxerr.CodeCanceled)
	}
}

// rangeInjector adds a fixed 10-byte Range header to every request,
// standing in for the resumable-download transport a caller could bring
// through WithHTTPClient. It clones before writing, per the RoundTripper
// contract.
type rangeInjector struct{ next http.RoundTripper }

func (r rangeInjector) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Range", "bytes=0-9")
	return r.next.RoundTrip(req)
}
