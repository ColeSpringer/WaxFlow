package jobs

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxflow/internal/ulid"
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
	if got.Output == nil || *got.Output != *done.Output {
		t.Errorf("output after reopen = %+v, want %+v", got.Output, done.Output)
	}
	if got.Finished == nil || !got.Finished.Equal(*done.Finished) {
		t.Errorf("finished after reopen = %v, want %v", got.Finished, done.Finished)
	}
	fi, err := os.Stat(filepath.Join(dir, j.ID, got.Output.File))
	if err != nil {
		t.Fatalf("output file after reopen: %v", err)
	}
	if fi.Size() != got.Output.Bytes {
		t.Errorf("output file is %d bytes after reopen, Output.Bytes says %d", fi.Size(), got.Output.Bytes)
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
				SchemaVersion: 1,
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
			if done.Output == nil {
				t.Fatal("requeued job finished without an output")
			}
			if _, err := os.Stat(filepath.Join(jdir, "foo.tmp")); !os.IsNotExist(err) {
				t.Errorf("stray foo.tmp survived the requeue: %v", err)
			}
			out, err := os.ReadFile(filepath.Join(jdir, done.Output.File))
			if err != nil {
				t.Fatalf("reading rerun output: %v", err)
			}
			if !bytes.HasPrefix(out, []byte("fLaC")) {
				t.Error("rerun output is not a FLAC stream; the junk partial survived")
			}
			if int64(len(out)) != done.Output.Bytes {
				t.Errorf("rerun output is %d bytes, Output.Bytes says %d", len(out), done.Output.Bytes)
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
		SchemaVersion: 1,
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

func TestUnreadableJobDirsQuarantined(t *testing.T) {
	res, _, _ := openLib(t)
	dir := t.TempDir()

	garbageID := mustULID(t)
	mismatchID := mustULID(t)
	emptyID := mustULID(t)
	fileID := mustULID(t)

	// A directory whose job.json names another job entirely.
	writeJob(t, dir, &Job{SchemaVersion: 1, ID: mustULID(t), Type: TypeAnalyze, State: StateQueued, Created: time.Now().UTC()})
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
