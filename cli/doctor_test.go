package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/source"
)

// doctorEnv points every directory the doctor touches at t.TempDir so a
// test run never writes outside the sandbox.
func doctorEnv(t *testing.T) (rootDir string) {
	t.Helper()
	rootDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDir, "a.wav"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAXFLOW_ROOTS", "lib="+rootDir)
	t.Setenv("WAXFLOW_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	t.Setenv("WAXFLOW_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("WAXFLOW_SCRATCH_DIR", filepath.Join(t.TempDir(), "scratch"))
	return rootDir
}

func TestDoctorHealthy(t *testing.T) {
	doctorEnv(t)
	code, out, stderr := run(t, "doctor", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	var report struct {
		SchemaVersion int  `json:"schemaVersion"`
		Healthy       bool `json:"healthy"`
		Checks        []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decoding doctor JSON: %v\n%s", err, out)
	}
	if !report.Healthy || report.SchemaVersion != 1 {
		t.Fatalf("healthy = %v, schemaVersion = %d; want true, 1", report.Healthy, report.SchemaVersion)
	}
	status := map[string]string{}
	for _, c := range report.Checks {
		status[c.Name] = c.Status
	}
	for _, name := range []string{"config", "root:lib", "cache", "data", "scratch", "bench:opus", "bench:flac", "ffmpeg"} {
		if got := status[name]; got != "ok" {
			t.Errorf("check %s = %q, want ok", name, got)
		}
	}
	if got := status["pid"]; got != "skip" {
		t.Errorf("check pid = %q, want skip (no pid lookup or catalogDB)", got)
	}
}

// TestDoctorProbesThePIDLookup asks the configured lookup host for the probe
// id: a host that answers at all passes, one that refuses or is down fails.
func TestDoctorProbesThePIDLookup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		closed bool
		want   string
		detail string
	}{
		{name: "a 404 is a reachable host", status: http.StatusNotFound, want: "ok", detail: "answers lookups"},
		{name: "a 401 is a refused key", status: http.StatusUnauthorized, want: "fail", detail: "401"},
		{name: "a closed host is down", closed: true, want: "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doctorEnv(t)
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			t.Setenv("WAXFLOW_PID_LOOKUP_URL", host.URL+"/pid")
			if tc.closed {
				host.Close()
			} else {
				defer host.Close()
			}
			code, out, stderr := run(t, "doctor", "--json")
			var report struct {
				Checks []struct {
					Name, Status, Detail string
				} `json:"checks"`
			}
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatalf("decoding doctor JSON: %v\n%s\n%s", err, out, stderr)
			}
			var found bool
			for _, c := range report.Checks {
				if c.Name != "pid" {
					continue
				}
				found = true
				if c.Status != tc.want || !strings.Contains(c.Detail, tc.detail) {
					t.Errorf("check pid = %s (%s), want %s with %q", c.Status, c.Detail, tc.want, tc.detail)
				}
			}
			if !found {
				t.Fatalf("no pid check in the report:\n%s", out)
			}
			if (code != 0) != (tc.want == "fail") {
				t.Errorf("exit = %d with the pid check %s", code, tc.want)
			}
		})
	}
}

func TestDoctorFailsOnMissingRoot(t *testing.T) {
	doctorEnv(t)
	t.Setenv("WAXFLOW_ROOTS", "lib=/nonexistent/waxflow-doctor-test")
	code, out, _ := run(t, "doctor")
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero\n%s", out)
	}
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "root:lib") {
		t.Errorf("output missing FAIL root:lib line:\n%s", out)
	}
}

func TestDoctorFailsOnCatalogDBWithoutFlavor(t *testing.T) {
	doctorEnv(t)
	t.Setenv("WAXFLOW_CATALOG_DB", filepath.Join(t.TempDir(), "waxbin.db"))
	code, out, _ := run(t, "doctor")
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero\n%s", out)
	}
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "pid") {
		t.Errorf("output missing FAIL pid line:\n%s", out)
	}
	if !strings.Contains(out, "catalog resolver") {
		t.Errorf("catalog failure should point at the missing catalog resolver:\n%s", out)
	}
}

func TestDoctorWarnsOnEmptyRoot(t *testing.T) {
	doctorEnv(t)
	t.Setenv("WAXFLOW_ROOTS", "lib="+t.TempDir())
	code, out, _ := run(t, "doctor")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (warn is not fail)\n%s", code, out)
	}
	if !strings.Contains(out, "warn") || !strings.Contains(out, "empty") {
		t.Errorf("output missing warn empty-root line:\n%s", out)
	}
}

func TestDoctorBenchUsesConfiguredProfile(t *testing.T) {
	doctorEnv(t)
	t.Setenv("WAXFLOW_RESAMPLE_PROFILE", "fast")
	code, out, stderr := run(t, "doctor")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	// The plumbing cannot be observed through the ok detail (the note
	// only appears under 2x), so at minimum the bench must run and pass
	// under the fast profile; the note-text split is covered by reading
	// benchChecks with a warn on a slow box.
	if !strings.Contains(out, "bench:opus") || !strings.Contains(out, "x realtime") {
		t.Errorf("bench line missing under fast profile:\n%s", out)
	}
}

// TestDoctorProbesTheHostNotTheFlavor: the pid check asks the configured host
// itself, so a Flavor that would answer the probe cannot hide a host that is
// down.
func TestDoctorProbesTheHostNotTheFlavor(t *testing.T) {
	root := doctorEnv(t)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Setenv("WAXFLOW_PID_LOOKUP_URL", host.URL+"/pid")
	host.Close()
	flavor := Flavor{
		Name: "answers-everything",
		OpenResolver: func(_ context.Context, o ResolverOptions) (source.Resolver, io.Closer, error) {
			return fakePIDResolver{next: o.Next, path: filepath.Join(root, "a.wav")}, nil, nil
		},
	}
	var out, errOut strings.Builder
	code := ExecuteFlavor("test", []string{"doctor", "--json"}, &out, &errOut, flavor)
	var report struct {
		Checks []struct{ Name, Status string } `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out.String()), &report); err != nil {
		t.Fatalf("decoding doctor JSON: %v\n%s\n%s", err, out.String(), errOut.String())
	}
	for _, c := range report.Checks {
		if c.Name == "pid" && c.Status != "fail" {
			t.Fatalf("pid check = %s with the host down, want fail (exit %d)", c.Status, code)
		}
	}
	if code == 0 {
		t.Fatal("exit = 0 with the lookup host down")
	}
}
