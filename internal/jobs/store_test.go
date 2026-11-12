package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/colespringer/waxflow/internal/ulid"
	"github.com/colespringer/waxflow/source"
)

func mustULID(t *testing.T) string {
	t.Helper()
	id, err := ulid.New()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// writeJob hand-writes a job directory's job.json, standing in for
// state a crashed daemon left behind.
func writeJob(t *testing.T, storeDir string, j *Job) {
	t.Helper()
	dir := filepath.Join(storeDir, j.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, jobFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRestartCompletedJobSurvives(t *testing.T) {
	res, ref, srcID := openLib(t)
	dir := t.TempDir()

	r1 := openRunner(t, Config{Dir: dir, Resolver: res})
	j, err := r1.Create(transcodeReq(ref, srcID))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	done := waitJob(t, r1, j.ID, StateDone)
	r1.Close()

	r2 := openRunner(t, Config{Dir: dir, Resolver: res})
	got, ok := r2.Get(j.ID)
	if !ok {
		t.Fatal("Get after reopen: job not found")
	}
	if got.State != StateDone {
		t.Fatalf("state after reopen = %s, want %s", got.State, StateDone)
	}
	if len(got.Outputs) != 1 || got.Outputs[0] != *soleOutput(t, done) {
		t.Errorf("outputs after reopen = %+v, want %+v", got.Outputs, done.Outputs)
	}
	if got.Finished == nil || !got.Finished.Equal(*done.Finished) {
		t.Errorf("finished after reopen = %v, want %v", got.Finished, done.Finished)
	}
	out := soleOutput(t, got)
	fi, err := os.Stat(filepath.Join(dir, j.ID, out.File))
	if err != nil {
		t.Fatalf("output file after reopen: %v", err)
	}
	if fi.Size() != out.Bytes {
		t.Errorf("output file is %d bytes after reopen, Output.Bytes says %d", fi.Size(), out.Bytes)
	}
}

func TestRestartRequeuesIncomplete(t *testing.T) {
	res, ref, srcID := openLib(t)
	for _, state := range []State{StateRunning, StateQueued} {
		t.Run(string(state), func(t *testing.T) {
			dir := t.TempDir()
			id := mustULID(t)
			now := time.Now().UTC()
			j := &Job{
				SchemaVersion: SchemaVersion,
				ID:            id,
				Type:          TypeTranscode,
				State:         state,
				Request:       transcodeReq(ref, srcID),
				Created:       now,
			}
			if state == StateRunning {
				started := now
				j.Started = &started
			}
			writeJob(t, dir, j)
			// Debris of the interrupted run: a partial output and a torn
			// temp file, both of which the requeue must sweep.
			jdir := filepath.Join(dir, id)
			if err := os.WriteFile(filepath.Join(jdir, "out.flac"), []byte("junk from the interrupted encode"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(jdir, "foo.tmp"), []byte("stray"), 0o600); err != nil {
				t.Fatal(err)
			}

			r := openRunner(t, Config{Dir: dir, Resolver: res})
			done := waitJob(t, r, id, StateDone)
			rerun := soleOutput(t, done)
			if _, err := os.Stat(filepath.Join(jdir, "foo.tmp")); !os.IsNotExist(err) {
				t.Errorf("stray foo.tmp survived the requeue: %v", err)
			}
			out, err := os.ReadFile(filepath.Join(jdir, rerun.File))
			if err != nil {
				t.Fatalf("reading rerun output: %v", err)
			}
			if !bytes.HasPrefix(out, []byte("fLaC")) {
				t.Error("rerun output is not a FLAC stream; the junk partial survived")
			}
			if int64(len(out)) != rerun.Bytes {
				t.Errorf("rerun output is %d bytes, Output.Bytes says %d", len(out), rerun.Bytes)
			}
		})
	}
}

func TestRestartLeavesTerminalUntouched(t *testing.T) {
	res, ref, srcID := openLib(t)
	dir := t.TempDir()
	id := mustULID(t)
	created := time.Now().UTC().Add(-time.Hour)
	started := created.Add(time.Second)
	finished := created.Add(2 * time.Second)
	j := &Job{
		SchemaVersion: SchemaVersion,
		ID:            id,
		Type:          TypeTranscode,
		State:         StateFailed,
		Request:       transcodeReq(ref, srcID),
		Created:       created,
		Started:       &started,
		Finished:      &finished,
		Error:         &ErrInfo{Code: "internal", Message: "disk fell over"},
	}
	writeJob(t, dir, j)
	jdir := filepath.Join(dir, id)
	if err := os.WriteFile(filepath.Join(jdir, "leftover.bin"), []byte("terminal residue"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(jdir, jobFile))
	if err != nil {
		t.Fatal(err)
	}

	r := openRunner(t, Config{Dir: dir, Resolver: res})
	got, ok := r.Get(id)
	if !ok {
		t.Fatal("Get: failed job not found after reopen")
	}
	if got.State != StateFailed {
		t.Fatalf("state = %s, want %s (a terminal job must not requeue)", got.State, StateFailed)
	}
	if got.Error == nil || got.Error.Code != "internal" || got.Error.Message != "disk fell over" {
		t.Errorf("error = %+v, want the failure exactly as written", got.Error)
	}
	if got.Finished == nil || !got.Finished.Equal(finished) {
		t.Errorf("finished = %v, want %v", got.Finished, finished)
	}
	after, err := os.ReadFile(filepath.Join(jdir, jobFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("job.json was rewritten for a terminal job")
	}
	if _, err := os.Stat(filepath.Join(jdir, "leftover.bin")); err != nil {
		t.Errorf("terminal job's directory was swept: %v", err)
	}
}

// TestOlderSchemaQuarantined pins the schema gate against the version that
// made it necessary: v1 carried one "output" object where v2 carries an
// "outputs" list.
//
// Nothing but the version tells the two apart, which is the danger. A v1
// document unmarshals cleanly into this build's Job (the key it names is
// simply one nothing reads), leaving a done job with no products at all, and
// done is terminal, so the requeue that recreates outputs never runs: the
// finished file below would sit on disk unreachable for good. Refusing hands
// the directory to openStore's quarantine, which keeps it for the operator.
func TestOlderSchemaQuarantined(t *testing.T) {
	res, _, _ := openLib(t)
	dir := t.TempDir()
	id := mustULID(t)
	jdir := filepath.Join(dir, id)
	if err := os.MkdirAll(jdir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Hand-written rather than marshaled from a Job: the shape it pins is one
	// this build can no longer express.
	v1 := fmt.Sprintf(`{
  "schemaVersion": 1,
  "id": %q,
  "type": "transcode",
  "state": "done",
  "request": {"type": "transcode", "src": "lib/sine-s16.wav", "sourceId": "1-1", "format": "flac"},
  "created": "2026-01-01T00:00:00Z",
  "output": {"file": "out.flac", "mediaType": "audio/flac", "container": "flac", "bytes": 8, "samples": 1, "rate": 44100}
}`, id)
	if err := os.WriteFile(filepath.Join(jdir, jobFile), []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jdir, "out.flac"), []byte("survivor"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := openRunner(t, Config{Dir: dir, Resolver: res})
	if got, ok := r.Get(id); ok {
		t.Errorf("a v1 job.json loaded as a live job (%+v); its product list is empty and its "+
			"state terminal, so its finished output is unreachable forever", got)
	}
	if _, err := os.Stat(jdir); !os.IsNotExist(err) {
		t.Errorf("the v1 dir survived at its id: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, id+".unreadable", "out.flac")); err != nil || string(b) != "survivor" {
		t.Errorf("quarantine did not preserve the finished output: %v %q", err, b)
	}
}

func TestUnreadableJobDirsQuarantined(t *testing.T) {
	res, _, _ := openLib(t)
	dir := t.TempDir()

	garbageID := mustULID(t)
	mismatchID := mustULID(t)
	emptyID := mustULID(t)
	fileID := mustULID(t)

	// A directory whose job.json names another job entirely.
	writeJob(t, dir, &Job{SchemaVersion: SchemaVersion, ID: mustULID(t), Type: TypeAnalyze, State: StateQueued, Created: time.Now().UTC()})
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("planting the mismatched dir: %v (%d entries)", err, len(entries))
	}
	if err := os.Rename(filepath.Join(dir, entries[0].Name()), filepath.Join(dir, mismatchID)); err != nil {
		t.Fatal(err)
	}
	// A directory holding torn JSON, plus a finished output the
	// quarantine must preserve.
	if err := os.MkdirAll(filepath.Join(dir, garbageID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, garbageID, jobFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, garbageID, "out.flac"), []byte("survivor"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A leftover quarantine from a previous run collides with the
	// mismatch dir's target name; the retry must land under a suffix.
	if err := os.MkdirAll(filepath.Join(dir, mismatchID+".unreadable", "old"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A ulid-named directory with no job.json at all.
	if err := os.MkdirAll(filepath.Join(dir, emptyID), 0o700); err != nil {
		t.Fatal(err)
	}
	// Non-job residents the scan must skip, not remove: a foreign
	// directory name and a plain file at a ulid name.
	if err := os.MkdirAll(filepath.Join(dir, "not-a-ulid"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileID), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := openRunner(t, Config{Dir: dir, Resolver: res})
	if jobs := r.List(); len(jobs) != 0 {
		t.Errorf("List = %+v, want none", jobs)
	}
	for _, id := range []string{garbageID, mismatchID, emptyID} {
		if _, err := os.Stat(filepath.Join(dir, id)); !os.IsNotExist(err) {
			t.Errorf("unreadable dir %s survived the scan at its id: %v", id, err)
		}
		// Quarantined, not deleted: the directory may hold a finished
		// output the operator can still recover. A colliding leftover
		// pushes the quarantine to a suffixed name, so match by prefix.
		matches, err := filepath.Glob(filepath.Join(dir, id+".unreadable*"))
		if err != nil || len(matches) == 0 {
			t.Errorf("unreadable dir %s was not quarantined: %v %v", id, matches, err)
		}
	}
	for _, name := range []string{"not-a-ulid", fileID} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("non-job resident %s was removed: %v", name, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, garbageID+".unreadable", "out.flac")); err != nil || string(b) != "survivor" {
		t.Errorf("quarantined output not preserved: %v %q", err, b)
	}
	// A second open must tolerate the quarantine debris and stay empty.
	r2 := openRunner(t, Config{Dir: dir, Resolver: res})
	if jobs := r2.List(); len(jobs) != 0 {
		t.Errorf("reopen List = %+v, want none", jobs)
	}
}

func TestListOrder(t *testing.T) {
	res, ref, srcID := openLib(t)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res})

	var ids []string
	for i := range 3 {
		if i > 0 {
			// Not synchronization: ULIDs order by millisecond timestamp,
			// so back-to-back creations need distinct millis to have a
			// defined creation order at all.
			time.Sleep(3 * time.Millisecond)
		}
		j, err := r.Create(analyzeReq(ref, srcID))
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		ids = append(ids, j.ID)
	}

	list := r.List()
	if len(list) != len(ids) {
		t.Fatalf("List returned %d jobs, want %d", len(list), len(ids))
	}
	for i, j := range list {
		if j.ID != ids[i] {
			t.Errorf("List[%d] = %s, want %s (creation order)", i, j.ID, ids[i])
		}
		if i > 0 && list[i-1].ID >= j.ID {
			t.Errorf("List ids out of order: %s before %s", list[i-1].ID, j.ID)
		}
	}
}

// TestRestartKeepsAnyReference: job.json is JSON, which would turn a name
// that is not UTF-8 into another file's, and the requeued job would run
// against that one.
func TestRestartKeepsAnyReference(t *testing.T) {
	dir := t.TempDir()
	s1, err := openStore(dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Type: TypeMerge, Src: "lib/caf\xe9.flac", Srcs: []source.Ref{"lib/100% \xff.flac", "lib/a.flac"}}
	j, err := s1.create(req)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := openStore(dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.get(j.ID)
	if !ok {
		t.Fatal("the job did not survive the reopen")
	}
	if got.Request.Src != req.Src || !slices.Equal(got.Request.Srcs, req.Srcs) {
		t.Fatalf("after the reopen the job names %q and %q, want %q and %q",
			got.Request.Src, got.Request.Srcs, req.Src, req.Srcs)
	}
}

// aliasResolver serves one file under any reference, so a test can name a
// source with bytes no fixture's name holds on every platform.
type aliasResolver struct {
	next source.Resolver
	to   string
}

func (a aliasResolver) Resolve(ctx context.Context, ref string) (*source.File, error) {
	f, err := a.next.Resolve(ctx, a.to)
	if err == nil {
		f.Ref = ref
	}
	return f, err
}

// TestJobErrorNamesAnyReference: a job's error is stored and served as JSON,
// where a stray byte becomes U+FFFD and the member named is another file.
func TestJobErrorNamesAnyReference(t *testing.T) {
	res, ref, _ := openLib(t)
	dir := t.TempDir()
	r := openRunner(t, Config{Dir: dir, Resolver: aliasResolver{res, ref}, MeasureTrack: measureTrack()})
	j, err := r.Create(Request{Type: TypeMerge, Srcs: []source.Ref{"lib/caf\xe9.wav"}, SourceIDs: []string{"1-1"},
		Format: "flac", FLACLevel: -1})
	if err != nil {
		t.Fatal(err)
	}
	failed := waitJob(t, r, j.ID, StateFailed)
	if failed.Error == nil || !strings.Contains(failed.Error.Message, "pct:lib/caf%E9.wav") {
		t.Fatalf("the error does not name the member in text form: %+v", failed.Error)
	}

	// An error the file system wrote quotes the name's own bytes.
	r2 := openRunner(t, Config{Dir: t.TempDir(), Resolver: res})
	j, err = r2.Create(Request{Type: TypeTranscode, Src: "lib/gone\xe9.wav", Format: "flac", FLACLevel: -1})
	if err != nil {
		t.Fatal(err)
	}
	failed = waitJob(t, r2, j.ID, StateFailed)
	if failed.Error == nil || !utf8.ValidString(failed.Error.Message) || !strings.Contains(failed.Error.Message, `gone\xe9.wav`) {
		t.Fatalf("the error does not show the name's bytes: %+v", failed.Error)
	}
}

// TestStoreKeepsEveryMessageLegible: whatever text a caller puts on a job,
// the store makes it something JSON holds, so no caller has to remember to.
// The reference is not text: its bytes are its value, and it marshals itself.
func TestStoreKeepsEveryMessageLegible(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.create(Request{Type: TypeTranscode, Src: "lib/caf\xe9.flac"})
	if err != nil {
		t.Fatal(err)
	}
	got := s.update(j.ID, true, func(job *Job) {
		job.Warnings = append(job.Warnings, "input damage: chunk \"\xff\xfe\" skipped")
		job.Error = &ErrInfo{Code: "internal", Message: "open /m/caf\xe9: gone"}
	})
	if got.Warnings[0] != `input damage: chunk "\xff\xfe" skipped` || got.Error.Message != `open /m/caf\xe9: gone` {
		t.Errorf("update left %q and %q", got.Warnings, got.Error.Message)
	}
	if got.Request.Src != "lib/caf\xe9.flac" {
		t.Errorf("the reference was rewritten: %q", got.Request.Src)
	}
	queued, _, _ := s.requeue(j.ID, time.Second, "root-unavailable", "root at /m/caf\xe9 is down")
	if queued.Waiting == nil || queued.Waiting.Message != `root at /m/caf\xe9 is down` {
		t.Errorf("requeue left %+v", queued.Waiting)
	}
	doc, err := os.ReadFile(filepath.Join(dir, j.ID, jobFile))
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(doc) || bytes.Contains(doc, []byte(`�`)) || !bytes.Contains(doc, []byte(`caf\\xe9`)) {
		t.Errorf("job.json lost a message's bytes:\n%s", doc)
	}
}
