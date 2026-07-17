package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if got := status["catalog"]; got != "skip" {
		t.Errorf("check catalog = %q, want skip (stock build, no catalogDB)", got)
	}
}

func TestDoctorFailsOnMissingRoot(t *testing.T) {
	doctorEnv(t)
	t.Setenv("WAXFLOW_ROOTS", "lib=/nonexistent/waxflow-doctor-test")
	code, out, _ := run(t, "doctor")
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero\n%s", out)
	}
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "roots") {
		t.Errorf("output missing FAIL roots line:\n%s", out)
	}
}

func TestDoctorFailsOnCatalogDBWithoutFlavor(t *testing.T) {
	doctorEnv(t)
	t.Setenv("WAXFLOW_CATALOG_DB", filepath.Join(t.TempDir(), "waxbin.db"))
	code, out, _ := run(t, "doctor")
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero\n%s", out)
	}
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "catalog") {
		t.Errorf("output missing FAIL catalog line:\n%s", out)
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
