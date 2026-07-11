package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	binconfig "github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/model"

	"github.com/colespringer/waxflow/cli"
)

func runFlavor(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut strings.Builder
	code = cli.ExecuteFlavor("test", args, &out, &errOut, cli.Flavor{
		Name:         "waxbin",
		OpenResolver: openResolver,
	})
	return code, out.String(), errOut.String()
}

// TestDoctorCatalogOpens is the doctor's catalog-opens check against a
// real WaxBin database: authored read-write here, opened read-only by the
// doctor through the same resolver chain the daemon uses.
func TestDoctorCatalogOpens(t *testing.T) {
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib, err := waxbin.Open(context.Background(), waxbin.Options{
		DBPath: db,
		Roots:  []binconfig.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("waxbin open: %v", err)
	}
	if _, err := lib.Scan(context.Background(), waxbin.ScanRequest{}); err != nil {
		t.Fatalf("waxbin scan: %v", err)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("WAXFLOW_ROOTS", "lib="+root)
	t.Setenv("WAXFLOW_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	t.Setenv("WAXFLOW_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("WAXFLOW_SCRATCH_DIR", filepath.Join(t.TempDir(), "scratch"))
	t.Setenv("WAXFLOW_CATALOG_DB", db)

	code, out, stderr := runFlavor(t, "doctor")
	if code != 0 {
		t.Fatalf("doctor exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, stderr)
	}
	if !strings.Contains(out, "catalog") || !strings.Contains(out, "opens read-only") {
		t.Errorf("doctor output missing ok catalog line:\n%s", out)
	}

	// A missing database must fail the check, not skip it.
	t.Setenv("WAXFLOW_CATALOG_DB", filepath.Join(t.TempDir(), "missing.db"))
	code, out, _ = runFlavor(t, "doctor")
	if code == 0 {
		t.Fatalf("doctor exit = 0 with a missing catalog\n%s", out)
	}
	if !strings.Contains(out, "FAIL") {
		t.Errorf("doctor output missing FAIL catalog line:\n%s", out)
	}
}
