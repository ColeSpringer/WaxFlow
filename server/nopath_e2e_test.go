package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/internal/jobs"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/waxerr"
)

// eachSpelling runs test with a store directory configured as an absolute
// path and as one relative to the working directory. The relative cell works
// from a temp directory, so it has a relative path to every other one even
// where the package's directory is on another volume.
func eachSpelling(t *testing.T, test func(t *testing.T, spell func(abs string) string)) {
	t.Run("an absolute directory", func(t *testing.T) {
		test(t, func(abs string) string { return abs })
	})
	t.Run("a relative directory", func(t *testing.T) {
		wd := t.TempDir()
		testutil.Chdir(t, wd)
		test(t, func(abs string) string {
			rel, err := filepath.Rel(wd, abs)
			if err != nil {
				t.Fatalf("no relative path from %s to %s: %v", wd, abs, err)
			}
			return rel
		})
	})
}

// fileInPlaceOf puts a file where dir was, so nothing can be made under it.
func fileInPlaceOf(t *testing.T, dir string) {
	t.Helper()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// refusal reads an error envelope with the wanted code and returns its text.
func refusal(t *testing.T, resp *http.Response, code waxerr.Code) string {
	t.Helper()
	raw := readBody(t, resp)
	var body server.ErrorBody
	if err := json.Unmarshal(raw, &body); err != nil || body.Code != code {
		t.Fatalf("status %d with %s, want a %s envelope", resp.StatusCode, raw, code)
	}
	return body.Error
}

// wantNoDir checks that a message a client read starts with prefix, which
// ends in the failed operation, and holds dir in no spelling.
func wantNoDir(t *testing.T, msg, prefix, abs, configured string) {
	t.Helper()
	if !strings.HasPrefix(msg, prefix) {
		t.Errorf("the client read %q, want it to start %q", msg, prefix)
	}
	for _, dir := range []string{abs, configured} {
		if strings.Contains(msg, dir) {
			t.Errorf("the client read %q, which holds the directory %s", msg, dir)
		}
	}
}

// An upload the spool cannot take answers with the failed operation and no
// part of the spool directory.
func TestAnUploadTheSpoolRefusesNamesNoPath(t *testing.T) {
	eachSpelling(t, func(t *testing.T, spell func(string) string) {
		abs := filepath.Join(t.TempDir(), "uploads")
		env := newTestEnv(t, func(cfg *server.Config) { cfg.UploadDir = spell(abs) })
		fileInPlaceOf(t, abs)
		req, err := http.NewRequest(http.MethodPost, env.ts.URL+"/uploads?name=a.wav", bytes.NewReader([]byte("RIFF")))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-API-Key", testKey)
		resp, err := env.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		msg := refusal(t, resp, waxerr.CodeInternal)
		wantNoDir(t, msg, "uploads: creating spool file: open: ", abs, spell(abs))
	})
}

// A timeline the store cannot write answers with the failed operation and no
// part of the timeline directory, from the inline mint and from the job a
// slow mint becomes.
func TestATimelineTheStoreRefusesNamesNoPath(t *testing.T) {
	const prefix = "timeline: writing a timeline: open: "
	eachSpelling(t, func(t *testing.T, spell func(string) string) {
		abs := filepath.Join(t.TempDir(), "timelines")
		env := newTestEnv(t, func(cfg *server.Config) {
			cfg.TimelineDir = spell(abs)
			cfg.JobsDir = filepath.Join(t.TempDir(), "jobs")
		})
		fileInPlaceOf(t, abs)

		inline := env.postJSON(t, "/hls/timeline", `{"srcs":[{"src":"lib/ramp.wav"},{"src":"lib/sine.wav"}]}`)
		msg := refusal(t, inline, waxerr.CodeOutputUnwritable)
		wantNoDir(t, msg, prefix, abs, spell(abs))

		// A WebM member's length is a cluster walk, so its mint is a job.
		slow := env.postJSON(t, "/hls/timeline", `{"srcs":[{"src":"lib/seed.webm"},{"src":"lib/seed.webm"}]}`)
		raw := readBody(t, slow)
		var created jobs.Job
		if err := json.Unmarshal(raw, &created); err != nil || slow.StatusCode != http.StatusAccepted {
			t.Fatalf("a cold WebM mint = %d, want a 202 job: %s", slow.StatusCode, raw)
		}
		failed := awaitJobTerminal(t, env, created.ID)
		if failed.Error == nil || failed.Error.Code != string(waxerr.CodeOutputUnwritable) {
			t.Fatalf("the timeline job ended %s with %+v, want %s", failed.State, failed.Error, waxerr.CodeOutputUnwritable)
		}
		wantNoDir(t, failed.Error.Message, prefix, abs, spell(abs))
	})
}

// A job the store cannot write is refused with the failed operation and no
// part of the jobs directory.
func TestAJobTheStoreRefusesNamesNoPath(t *testing.T) {
	eachSpelling(t, func(t *testing.T, spell func(string) string) {
		abs := filepath.Join(t.TempDir(), "jobs")
		env := newTestEnv(t, func(cfg *server.Config) { cfg.JobsDir = spell(abs) })
		fileInPlaceOf(t, abs)
		resp := env.postJSON(t, "/jobs", `{"type":"transcode","src":"lib/sine.wav","format":"flac"}`)
		msg := refusal(t, resp, waxerr.CodeOutputUnwritable)
		wantNoDir(t, msg, "jobs: creating job dir: mkdir: ", abs, spell(abs))
	})
}

// A finished job whose output is no longer on disk answers its result with
// the failed open and no part of the jobs directory.
func TestAJobResultThatIsGoneNamesNoPath(t *testing.T) {
	eachSpelling(t, func(t *testing.T, spell func(string) string) {
		abs := filepath.Join(t.TempDir(), "jobs")
		env := newTestEnv(t, func(cfg *server.Config) { cfg.JobsDir = spell(abs) })
		id := createJob(t, env, `{"type":"transcode","src":"lib/sine.wav","format":"flac"}`)
		awaitJob(t, env, id)
		if err := os.Remove(filepath.Join(abs, id, "out.flac")); err != nil {
			t.Fatal(err)
		}
		msg := refusal(t, env.get(t, "/jobs/"+id+"/result", nil), waxerr.CodeInternal)
		wantNoDir(t, msg, "jobs: opening output: open: ", abs, spell(abs))
		if strings.Contains(msg, id) || strings.Contains(msg, "out.flac") {
			t.Errorf("the client read %q, which names the job's file", msg)
		}
	})
}

// An HLS request needs no key, only a signed URL. What it can still be
// refused while the cache cannot be written says nothing of the cache's
// directory or the library's, however the cache was configured.
func TestAnHLSRefusalOnABrokenCacheNamesNoPath(t *testing.T) {
	eachSpelling(t, func(t *testing.T, spell func(string) string) {
		var configured string
		env := newTestEnv(t, func(cfg *server.Config) {
			cfg.CacheDir = spell(cfg.CacheDir)
			configured = cfg.CacheDir
			cfg.LiveSlots = 1
		})
		_, segURIs := hlsURIs(t, env, rampSegments)
		fileInPlaceOf(t, filepath.Join(env.cache, "v1"))
		clean := func(what, msg string) {
			t.Helper()
			for _, dir := range []string{env.cache, configured, env.root} {
				if msg == "" || strings.Contains(msg, dir) {
					t.Errorf("%s: the client read %q, which is empty or holds the directory %s", what, msg, dir)
				}
			}
		}

		release, ok := env.srv.HoldLiveSlot()
		if !ok {
			t.Fatal("the one live slot is taken before any stream")
		}
		clean("no live slot", refusal(t, keyless(t, env, "/hls/"+segURIs[2]), waxerr.CodeOverloaded))
		release()

		// With a key the descriptor alone is asked, of any segment.
		_, desc, _ := strings.Cut(segURIs[0], "&v=")
		clean("a segment past the end", refusal(t, env.get(t, "/hls/seg/99999.m4s?v="+desc, nil), waxerr.CodeNotFound))

		if err := os.WriteFile(filepath.Join(env.root, "ramp.wav"), rampWAV(t, 48000, 2, 5*48000), 0o644); err != nil {
			t.Fatal(err)
		}
		clean("a source that changed", refusal(t, keyless(t, env, "/hls/"+segURIs[2]), waxerr.CodeSourceChanged))
	})
}
