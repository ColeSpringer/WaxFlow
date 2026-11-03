package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRetainKeepsAFileOpenUntilEveryHolderCloses: a retained file serves
// reads after the first holder's Close and closes with the last.
func TestRetainKeepsAFileOpenUntilEveryHolderCloses(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.bin"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	roots, err := OpenRoots([]Root{{Name: "lib", Path: root}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	f, err := roots.Resolve(context.Background(), "lib/a.bin")
	if err != nil {
		t.Fatal(err)
	}
	held := f.Retain()
	if err := f.Close(); err != nil {
		t.Fatalf("the first holder's Close = %v", err)
	}
	if _, err := held.ReadAt(make([]byte, 1), 0); err != nil {
		t.Fatalf("a read through the remaining holder = %v, want the file still open", err)
	}
	if err := held.Close(); err != nil {
		t.Fatalf("the last holder's Close = %v", err)
	}
	if _, err := f.ReadAt(make([]byte, 1), 0); !errors.Is(err, os.ErrClosed) {
		t.Errorf("a read after the last Close = %v, want closed", err)
	}
}
