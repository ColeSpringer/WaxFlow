package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/colespringer/waxflow/internal/config"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// writeReloadConfig writes a config file naming the given roots plus temp
// daemon dirs, and returns its path. Rewriting it with a new root set is how
// the tests "edit the config" a reload then re-reads.
func writeReloadConfig(t *testing.T, base string, roots []config.Root) string {
	return writeReloadConfigCap(t, base, roots, 0)
}

// writeReloadConfigCap is writeReloadConfig plus an explicit sourceMaxBytes
// (0 omits it), for tests that reconcile the cap.
func writeReloadConfigCap(t *testing.T, base string, roots []config.Root, sourceMaxBytes int64) string {
	t.Helper()
	cfg := struct {
		Roots          []config.Root `json:"roots"`
		SourceMaxBytes int64         `json:"sourceMaxBytes,omitempty"`
		DataDir        string        `json:"dataDir"`
		CacheDir       string        `json:"cacheDir"`
		ScratchDir     string        `json:"scratchDir"`
	}{
		Roots:          roots,
		SourceMaxBytes: sourceMaxBytes,
		DataDir:        filepath.Join(base, "data"),
		CacheDir:       filepath.Join(base, "cache"),
		ScratchDir:     filepath.Join(base, "scratch"),
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "config.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func makeRoot(t *testing.T, base, name, file string) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte("RIFFdata"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// unsetenv deterministically removes an env var for the test and restores
// it afterward. t.Setenv can only set a value, not clear one, so these
// gating tests (whose outcome turns on WAXFLOW_ROOTS being genuinely absent)
// cannot rely on the ambient shell not exporting it.
func unsetenv(t *testing.T, key string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if had {
			os.Setenv(key, prev)
		} else {
			os.Unsetenv(key)
		}
	})
}

// TestReloadRootsClosureReReadsFile confirms the server's ReloadRoots
// closure re-reads the config file with startup precedence: a root added to
// the file after startup resolves through the same live resolver once the
// closure runs, no restart.
func TestReloadRootsClosureReReadsFile(t *testing.T) {
	base := t.TempDir()
	// Don't depend on the ambient env: an exported WAXFLOW_ROOTS would gate
	// the closure off (and reshape config.Load's roots), and an exported
	// WAXFLOW_CATALOG_DB would make the stock flavor refuse to build.
	unsetenv(t, "WAXFLOW_ROOTS")
	unsetenv(t, "WAXFLOW_CATALOG_DB")
	dirA := makeRoot(t, base, "A", "a.wav")
	dirB := makeRoot(t, base, "B", "b.wav")
	path := writeReloadConfig(t, base, []config.Root{{Name: "A", Path: dirA}})

	cfg, err := config.Load(path, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	logger, err := newLogger(&strings.Builder{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	srvCfg, cleanup, err := buildServerConfig(context.Background(), cfg, path, "test", logger, Flavor{})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if srvCfg.ReloadRoots == nil {
		t.Fatal("ReloadRoots not wired for a file-configured daemon with WAXFLOW_ROOTS unset")
	}

	ctx := context.Background()
	if f, err := srvCfg.Resolver.Resolve(ctx, "A/a.wav"); err != nil {
		t.Fatalf("A resolve before reload = %v", err)
	} else {
		f.Close()
	}
	if _, err := srvCfg.Resolver.Resolve(ctx, "B/b.wav"); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Fatalf("B resolve before reload = %v, want not-found", err)
	}

	// Edit the file to add B, then run the closure.
	writeReloadConfig(t, base, []config.Root{{Name: "A", Path: dirA}, {Name: "B", Path: dirB}})
	res, err := srvCfg.ReloadRoots()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 1 || res.Added[0] != "B" {
		t.Fatalf("reload delta = %+v, want B added", res)
	}

	// B now resolves through the same resolver the daemon already holds.
	if f, err := srvCfg.Resolver.Resolve(ctx, "B/b.wav"); err != nil {
		t.Fatalf("B resolve after reload = %v, want success", err)
	} else {
		f.Close()
	}
}

// TestReloadRootsMissingConfigIs400 pins the closure's error remap: a config
// file that vanished after startup is an invalid-request (400), not the
// not-found (404) config.Load reports for a missing file, so a wired
// endpoint's failure never masquerades as the not-wired 404.
func TestReloadRootsMissingConfigIs400(t *testing.T) {
	base := t.TempDir()
	unsetenv(t, "WAXFLOW_ROOTS")
	unsetenv(t, "WAXFLOW_CATALOG_DB")
	dirA := makeRoot(t, base, "A", "a.wav")
	path := writeReloadConfig(t, base, []config.Root{{Name: "A", Path: dirA}})

	cfg, err := config.Load(path, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	logger, err := newLogger(&strings.Builder{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	srvCfg, cleanup, err := buildServerConfig(context.Background(), cfg, path, "test", logger, Flavor{})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if srvCfg.ReloadRoots == nil {
		t.Fatal("ReloadRoots not wired")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, err = srvCfg.ReloadRoots()
	if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Fatalf("reload with a deleted config = %v (code %s), want invalid-request", err, waxerr.CodeOf(err))
	}
}

// capReloadable stands in for a Flavor resolver that caps its own sources:
// it records the source cap a reload hands it via ReloadSourceMaxBytes.
type capReloadable struct {
	source.Resolver
	mu       sync.Mutex
	maxBytes int64
	calls    int
}

func (c *capReloadable) ReloadSourceMaxBytes(maxBytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxBytes = maxBytes
	c.calls++
}

func (c *capReloadable) get() (int64, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxBytes, c.calls
}

// TestReloadReconcilesFlavorSourceMaxBytes pins the ReloadableResolver hook:
// a reload that re-reads a changed sourceMaxBytes reconciles a Flavor
// resolver's own cap, not only the library roots, so the byte-for-byte-as-
// restart promise holds for a Flavor build too.
func TestReloadReconcilesFlavorSourceMaxBytes(t *testing.T) {
	base := t.TempDir()
	unsetenv(t, "WAXFLOW_ROOTS")
	unsetenv(t, "WAXFLOW_CATALOG_DB")
	unsetenv(t, "WAXFLOW_SOURCE_MAX_BYTES") // don't let ambient env pin the cap
	dirA := makeRoot(t, base, "A", "a.wav")
	path := writeReloadConfigCap(t, base, []config.Root{{Name: "A", Path: dirA}}, 1<<20)

	var rec *capReloadable
	flavor := Flavor{
		Name: "catalog",
		OpenResolver: func(_ context.Context, o ResolverOptions) (source.Resolver, io.Closer, error) {
			rec = &capReloadable{Resolver: o.Next, maxBytes: o.MaxBytes}
			return rec, nil, nil
		},
	}
	cfg, err := config.Load(path, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	logger, err := newLogger(&strings.Builder{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	srvCfg, cleanup, err := buildServerConfig(context.Background(), cfg, path, "test", logger, flavor)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if srvCfg.ReloadRoots == nil {
		t.Fatal("ReloadRoots not wired")
	}
	if got, _ := rec.get(); got != 1<<20 {
		t.Fatalf("open cap = %d, want %d", got, 1<<20)
	}

	// Raise sourceMaxBytes in the file, reload, and assert the flavor
	// resolver was reconciled, exactly once.
	writeReloadConfigCap(t, base, []config.Root{{Name: "A", Path: dirA}}, 2<<20)
	if _, err := srvCfg.ReloadRoots(); err != nil {
		t.Fatal(err)
	}
	if got, calls := rec.get(); got != 2<<20 || calls != 1 {
		t.Fatalf("after reload cap = %d, calls = %d; want %d and 1", got, calls, 2<<20)
	}
}

// TestReloadRootsGating pins the "reloadability is a gated capability"
// rule: the closure is wired only when a config file is set and
// WAXFLOW_ROOTS is not pinning roots (its set-but-empty == unset rule
// mirrored from config.Load), so /caps never advertises a reload that could
// not do anything.
func TestReloadRootsGating(t *testing.T) {
	base := t.TempDir()
	dirA := makeRoot(t, base, "A", "a.wav")
	// Keep the daemon dirs in temp even for the no-config-file case, and
	// clear WAXFLOW_CATALOG_DB so the stock flavor never refuses to build.
	t.Setenv("WAXFLOW_DATA_DIR", filepath.Join(base, "data"))
	t.Setenv("WAXFLOW_CACHE_DIR", filepath.Join(base, "cache"))
	t.Setenv("WAXFLOW_SCRATCH_DIR", filepath.Join(base, "scratch"))
	t.Setenv("WAXFLOW_CATALOG_DB", "")
	path := writeReloadConfig(t, base, []config.Root{{Name: "A", Path: dirA}})

	empty, pinned := "", "A="+dirA
	tests := []struct {
		name      string
		path      string
		rootsEnv  *string // nil: genuinely unset; else set to this value
		wantWired bool
	}{
		{"file, WAXFLOW_ROOTS unset", path, nil, true},
		{"file, WAXFLOW_ROOTS pins roots", path, &pinned, false},
		{"file, WAXFLOW_ROOTS set-but-empty", path, &empty, true},
		{"no config file", "", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Establish WAXFLOW_ROOTS explicitly so the outcome never turns
			// on the ambient shell.
			if tc.rootsEnv == nil {
				unsetenv(t, "WAXFLOW_ROOTS")
			} else {
				t.Setenv("WAXFLOW_ROOTS", *tc.rootsEnv)
			}
			cfg, err := config.Load(tc.path, os.LookupEnv)
			if err != nil {
				t.Fatal(err)
			}
			logger, err := newLogger(&strings.Builder{}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			srvCfg, cleanup, err := buildServerConfig(context.Background(), cfg, tc.path, "test", logger, Flavor{})
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if wired := srvCfg.ReloadRoots != nil; wired != tc.wantWired {
				t.Errorf("wired = %v, want %v", wired, tc.wantWired)
			}
		})
	}
}
