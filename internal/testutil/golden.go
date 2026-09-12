package testutil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Golden compares a writer's output against a committed file, or rewrites it
// when update is set (the `-update` flag behind `make goldens`). A missing
// golden is fatal rather than an implicit write, so a typo in the path reads
// as a missing file instead of a new one nobody reviewed.
//
// The mismatch message names the version constant as well as the regeneration
// command, because those bytes are what an ADR-0004 cache key stands for: a
// deliberate change has to bump the writer's Version in the same commit, and
// this failure is the only place that gets said out loud.
func Golden(t testing.TB, path string, got []byte, update bool) {
	t.Helper()
	if update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote golden %s (%d bytes)", path, len(got))
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (run `make goldens`): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s (%d vs %d bytes)%s\n"+
			"if the change is intentional: bump this writer's Version constant (ADR-0004), "+
			"then `make goldens` and review the diff",
			path, len(got), len(want), firstDiff(got, want))
	}
}
