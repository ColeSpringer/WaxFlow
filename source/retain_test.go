package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/waxerr"
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

// TestAFailedCloseIsUnreadableAndNamesNoPath: a close the system refuses is
// the source's fault, and its message holds no path of the daemon's.
func TestAFailedCloseIsUnreadableAndNamesNoPath(t *testing.T) {
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
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// Closed under its holder, the file refuses the next close.
	err = f.Close()
	if waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("a refused close = %v, want %s over the system's error", err, waxerr.CodeSourceUnreadable)
	}
	if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "a.bin") {
		t.Errorf("the error %q names the file's path", err)
	}
}
