package posixfs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReplaceSurfacesRealFailures: a rename that can never succeed still
// returns its error rather than being retried into silence.
func TestReplaceSurfacesRealFailures(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := Replace(filepath.Join(dir, "missing.tmp"), filepath.Join(dir, "out")); err == nil {
		t.Error("Replace of a missing source = nil, want an error")
	}
}

// TestCreateReplaceReadFileRoundTrip drives the publish idiom end to end on
// every platform.
func TestCreateReplaceReadFileRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tmp := filepath.Join(dir, "blob.tmp")
	target := filepath.Join(dir, "blob")

	f, err := Create(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// Create is exclusive: a second create of a live path must refuse.
	if g, err := Create(tmp); err == nil {
		g.Close()
		t.Fatal("Create re-created a live path; O_EXCL semantics lost")
	}
	if err := Replace(tmp, target); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(target)
	if err != nil || string(got) != "payload" {
		t.Fatalf("ReadFile = %q (%v), want %q", got, err, "payload")
	}
	if _, err := os.Stat(tmp); err == nil {
		t.Error("tmp still exists after Replace")
	}
}
