package source

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/waxerr"
)

// resolveHello resolves a five-byte file under a fresh root and returns it
// with the root's path.
func resolveHello(t *testing.T) (*File, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.bin"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	roots, err := OpenRoots([]Root{{Name: "lib", Path: root}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { roots.Close() })
	f, err := roots.Resolve(context.Background(), "lib/a.bin")
	if err != nil {
		t.Fatal(err)
	}
	return f, root
}

// TestAFailedReadNamesNoPath: a read the system refuses keeps the system's
// error and drops the path it quotes.
func TestAFailedReadNamesNoPath(t *testing.T) {
	f, root := resolveHello(t)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	n, err := f.ReadAt(make([]byte, 1), 0)
	if n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("a read of a closed file = %d, %v; want the system's refusal", n, err)
	}
	if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "a.bin") {
		t.Errorf("the error %q names the file's path", err)
	}
}

// TestReadAtPassesTheEndThrough: the end of the file is io.EOF itself, with
// the bytes a short read got, and a read that succeeds allocates nothing.
func TestReadAtPassesTheEndThrough(t *testing.T) {
	f, _ := resolveHello(t)
	defer f.Close()
	buf := make([]byte, 8)
	if n, err := f.ReadAt(buf, 2); n != 3 || err != io.EOF {
		t.Errorf("a short read = %d, %v; want 3 and io.EOF itself", n, err)
	}
	if n, err := f.ReadAt(buf, 5); n != 0 || err != io.EOF {
		t.Errorf("a read at the end = %d, %v; want 0 and io.EOF itself", n, err)
	}
	if allocs := testing.AllocsPerRun(100, func() { f.ReadAt(buf[:5], 0) }); allocs != 0 {
		t.Errorf("a read that succeeds allocates %v times, want none", allocs)
	}
}

// TestOpenLocalNamesNoPath: a local path is the daemon's own, so a file that
// is not there is reported without it.
func TestOpenLocalNamesNoPath(t *testing.T) {
	dir := t.TempDir()
	_, err := OpenLocal("upload:01J", filepath.Join(dir, "01J"), "song.wav")
	if waxerr.CodeOf(err) != waxerr.CodeNotFound || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenLocal of a missing file = %v, want %s", err, waxerr.CodeNotFound)
	}
	if strings.Contains(err.Error(), dir) {
		t.Errorf("the error %q names the daemon's directory", err)
	}
}
