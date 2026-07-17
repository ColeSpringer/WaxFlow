package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/cli"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/source"
)

const testPID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut strings.Builder
	code = cli.ExecuteFlavor("test", args, &out, &errOut, cli.Flavor{
		Name:         "catalog",
		OpenResolver: openResolver,
	})
	return code, out.String(), errOut.String()
}

// newCatalog writes a one-entry stub catalog and returns its directory.
func newCatalog(t *testing.T, pid, path string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, pid), []byte(path), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestSignResolvesPIDThroughCatalog runs the CLI this module builds
// against its own catalog. Exit 0 is the assertion that the hook fired:
// a build without one refuses a configured catalogDB outright, and the
// minted URL can only carry an identity the catalog resolved.
func TestSignResolvesPIDThroughCatalog(t *testing.T) {
	dir := t.TempDir()
	track := filepath.Join(dir, "track.wav")
	if err := os.WriteFile(track, []byte("not audio; sign only stats it"), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAXFLOW_DATA_DIR", dataDir)
	t.Setenv("WAXFLOW_CATALOG_DB", newCatalog(t, testPID, track))

	code, out, stderr := runCLI(t, "sign", "--src", "pid:"+testPID)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	for _, want := range []string{"src=pid%3A" + testPID, "id=", "sig=", "exp="} {
		if !strings.Contains(out, want) {
			t.Errorf("minted URL missing %q: %s", want, out)
		}
	}
}

// TestUnconfiguredCatalogOwnsRefusal pins the rule cli.Flavor documents:
// an implementation that is present but unconfigured answers its own
// schemes itself, rather than leaving pid: to the stock
// unsupported-source error.
func TestUnconfiguredCatalogOwnsRefusal(t *testing.T) {
	t.Setenv("WAXFLOW_DATA_DIR", t.TempDir())
	code, _, stderr := runCLI(t, "sign", "--src", "pid:"+testPID)
	if code == 0 {
		t.Fatal("exit = 0 with no catalog configured")
	}
	if !strings.Contains(stderr, "catalogDB configured") {
		t.Errorf("stderr = %q, want this build's own refusal", stderr)
	}
}

// TestDoctorOpensCatalog covers a second call site out-of-prefix: doctor
// opens its catalog check through the same seam that sign resolves
// through, and is the one caller that opens the resolver only to close
// it again. Asserting the catalog check alone, rather than the exit
// code, keeps this test about the seam: doctor also self-benches, and a
// slow box must not fail it here.
func TestDoctorOpensCatalog(t *testing.T) {
	root := t.TempDir()
	track := filepath.Join(root, "a.wav")
	if err := os.WriteFile(track, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAXFLOW_ROOTS", "lib="+root)
	t.Setenv("WAXFLOW_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	t.Setenv("WAXFLOW_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("WAXFLOW_SCRATCH_DIR", filepath.Join(t.TempDir(), "scratch"))
	t.Setenv("WAXFLOW_CATALOG_DB", newCatalog(t, testPID, track))

	_, out, stderr := runCLI(t, "doctor", "--json")
	var report struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("doctor --json: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	for _, c := range report.Checks {
		if c.Name == "catalog" {
			if c.Status != "ok" {
				t.Errorf("catalog check = %q (%s), want ok", c.Status, c.Detail)
			}
			return
		}
	}
	t.Errorf("no catalog check in doctor report:\n%s", out)
}

// TestServerConfigConstructs pins the other half of the extension
// surface an out-of-prefix module needs: server.Config, wired the way an
// embedder would wire it -- the catalog ahead of the roots, and
// PIDSources advertising the pid support this resolver actually has.
//
// Meta stays nil on purpose: it is typed from waxflow/internal/meta, so
// naming it here would not compile, and an embedder leaves it nil to
// disable metadata passthrough.
func TestServerConfigConstructs(t *testing.T) {
	track := filepath.Join(t.TempDir(), "track.wav")
	if err := os.WriteFile(track, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No roots configured: every path reference resolves not-found, and
	// the catalog delegates to it for everything that is not a pid.
	roots, err := source.OpenRoots(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	cat, err := openCatalog(context.Background(), cli.ResolverOptions{
		CatalogDB: newCatalog(t, testPID, track),
		Next:      roots,
	})
	if err != nil {
		t.Fatalf("openCatalog: %v", err)
	}
	defer cat.Close()

	srv, err := server.New(server.Config{
		Addr:       "127.0.0.1:0",
		Resolver:   cat,
		PIDSources: true,
		CacheDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	defer srv.Close()
}
