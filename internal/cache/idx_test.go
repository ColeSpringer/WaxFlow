package cache

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/colespringer/waxflow/internal/posixfs"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

func TestIdxStoreRoundTrip(t *testing.T) {
	s, err := OpenIdx(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Load("missing"); got != nil {
		t.Errorf("Load(missing) = %d bytes, want nil", len(got))
	}
	blob := []byte("WXMPAIDX1\x00 pretend index payload")
	s.Save("lib/a.mp3|123-456", blob)
	if got := s.Load("lib/a.mp3|123-456"); !bytes.Equal(got, blob) {
		t.Errorf("round trip mismatch: %q", got)
	}
	// A different identity misses.
	if got := s.Load("lib/a.mp3|123-457"); got != nil {
		t.Error("changed identity still hits")
	}
	// Empty and oversized blobs are refused.
	s.Save("empty", nil)
	if s.Load("empty") != nil {
		t.Error("empty blob was stored")
	}
	// Drop removes a blob for good (the rejected-blob path).
	s.Drop("lib/a.mp3|123-456")
	if s.Load("lib/a.mp3|123-456") != nil {
		t.Error("dropped blob still loads")
	}
	s.Drop("never-existed") // must not panic or disturb the total
}

func TestIdxStoreTrim(t *testing.T) {
	// Cap small enough that only some blobs survive; the survivors must
	// be the most recently touched ones.
	s, err := OpenIdx(t.TempDir(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, 1024)
	for i := range 8 {
		s.Save(fmt.Sprint("id-", i), blob)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		total += fi.Size()
	}
	if total > 4096 {
		t.Errorf("directory holds %d bytes past the %d cap", total, 4096)
	}
	// The last write always survives its own trim.
	if s.Load("id-7") == nil {
		t.Error("most recent blob was evicted")
	}
}

// TestIdxStoreConcurrentSaves exercises the write path from many
// goroutines, same identity included: the race detector proves the
// serialization and Load must always see a whole blob.
func TestIdxStoreConcurrentSaves(t *testing.T) {
	s, err := OpenIdx(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	blob := bytes.Repeat([]byte("idx"), 4096)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 25 {
				s.Save("shared", blob)
				s.Save(fmt.Sprint("own-", w, "-", i), blob)
				if got := s.Load("shared"); got != nil && !bytes.Equal(got, blob) {
					t.Errorf("torn blob: %d bytes", len(got))
					return
				}
			}
		})
	}
	wg.Wait()
	if got := s.Load("shared"); !bytes.Equal(got, blob) {
		t.Error("final blob does not round-trip")
	}
}

// TestIdxStoreTrimSweepsTemps: the first Save's scan removes a temp a crashed
// Save left behind, in both spellings a blob has been staged under, and
// neither counts against the cap. The blobs beside them stay.
func TestIdxStoreTrimSweepsTemps(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenIdx(dir, 4096)
	if err != nil {
		t.Fatal(err)
	}
	blob := bytes.Repeat([]byte("idx"), 300)
	s.Save("kept", blob)
	if name := filepath.Base(s.path("kept")); posixfs.IsTemp(name) {
		t.Fatalf("a blob is published as %s, which reads as a temp", name)
	}

	temps := []string{
		filepath.Base(s.path("kept")) + ".tmp",
		filepath.Base(s.path("kept")) + ".tmp-0123456789abcdef",
	}
	for _, name := range temps {
		// Each is large enough to evict a blob if it were counted as one.
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s, err = OpenIdx(dir, 4096)
	if err != nil {
		t.Fatal(err)
	}
	s.Save("new", blob)
	for _, name := range temps {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived the scan: %v", name, err)
		}
	}
	for _, id := range []string{"kept", "new"} {
		if got := s.Load(id); !bytes.Equal(got, blob) {
			t.Errorf("blob %q after the scan is %d bytes, want %d", id, len(got), len(blob))
		}
	}
}

// A relative directory that cannot be resolved, the working directory being
// gone, is refused with a code as the stores refuse it.
func TestOpenIdxCodesADirectoryItCannotResolve(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.Chdir(t, gone)
	if err := os.Remove(gone); err != nil {
		t.Skipf("the working directory cannot be removed here: %v", err)
	}
	// Linux's getcwd fails on a removed directory, as getcwd(2) documents;
	// darwin's still resolves it, which leaves nothing to refuse.
	if wd, err := os.Getwd(); err == nil && runtime.GOOS != "linux" {
		t.Skipf("the removed working directory still resolves, to %s", wd)
	}
	_, err := OpenIdx("idx", 0)
	if e, ok := errors.AsType[*waxerr.Error](err); !ok || e.Code != waxerr.CodeInternal || !strings.HasPrefix(e.Error(), "cache: resolving idx dir: ") {
		t.Errorf("OpenIdx = %v, want %s from resolving the directory", err, waxerr.CodeInternal)
	}
}
