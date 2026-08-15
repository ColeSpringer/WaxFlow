//go:build windows

package posixfs

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestRetryable pins which failures earn a retry. Anything but a transient
// handle must surface at once instead of after the full backoff.
func TestRetryable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"ERROR_ACCESS_DENIED", syscall.ERROR_ACCESS_DENIED, true},
		{"ERROR_SHARING_VIOLATION", errnoSharingViolation, true},
		{"ERROR_FILE_NOT_FOUND", syscall.ERROR_FILE_NOT_FOUND, false},
		// os.Rename wraps in a *os.LinkError.
		{"wrapped ERROR_ACCESS_DENIED", &os.LinkError{Err: syscall.ERROR_ACCESS_DENIED}, true},
		{"wrapped ERROR_FILE_NOT_FOUND", &os.LinkError{Err: syscall.ERROR_FILE_NOT_FOUND}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryable(tc.err); got != tc.want {
				t.Errorf("retryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestReplaceTakesAnOpenSharedTarget pins the POSIX rung and that it was
// taken, with no timing involved: os.Rename fails against a target held
// through Open for the whole call, so only FILE_RENAME_POSIX_SEMANTICS can
// land the replace. This is the HLS shape: a worker re-publishing a segment
// a client is mid-GET through.
func TestReplaceTakesAnOpenSharedTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tmp := filepath.Join(dir, "seg-1.m4s.tmp")
	target := filepath.Join(dir, "seg-1.m4s")
	if err := os.WriteFile(tmp, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	serving, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer serving.Close()

	// The rung is needed: the classic rename cannot take the held name.
	if err := os.Rename(tmp, target); err == nil {
		t.Fatal("os.Rename replaced a held target; this cell can no longer prove Replace's rung was taken")
	}
	if err := Replace(tmp, target); err != nil {
		t.Fatalf("Replace over a delete-shared-held target: %v", err)
	}

	// The in-flight response keeps its bytes; the name serves the new ones.
	buf := make([]byte, 3)
	if _, err := serving.ReadAt(buf, 0); err != nil || string(buf) != "old" {
		t.Errorf("held handle reads %q (%v), want %q", buf, err, "old")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "new" {
		t.Errorf("target holds %q (%v) after the replace, want %q", got, err, "new")
	}
}

// TestReplaceOutlastsATransientHold drives the hold POSIX semantics cannot
// beat, a handle without delete sharing, and asserts the backoff was needed:
// os.Rename fails outright under the hold, and Replace succeeds only because
// a retry lands after the release.
func TestReplaceOutlastsATransientHold(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tmp := filepath.Join(dir, "meta.json.tmp")
	target := filepath.Join(dir, "meta.json")
	if err := os.WriteFile(tmp, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	held, err := os.Open(target) // no delete sharing: blocks every rename form
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, target); err == nil {
		held.Close()
		t.Fatal("os.Rename replaced a plain-held target; the transient hold proves nothing here")
	}

	// The hold outlives the first attempt and clears well inside the 75ms
	// backoff budget, so only a retry can land the rename.
	var wg sync.WaitGroup
	wg.Go(func() {
		time.Sleep(15 * time.Millisecond)
		held.Close()
	})
	defer wg.Wait()

	if err := Replace(tmp, target); err != nil {
		t.Fatalf("Replace under a transient hold: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "new" {
		t.Errorf("target holds %q (%v) after the replace, want %q", got, err, "new")
	}
}

// TestCreateSurvivesReplaceRename pins what cache promotion depends on: the
// entry file renames to its final name while the write handle stays open,
// and that handle keeps serving reads afterward. With a plain os.OpenFile
// handle the rename fails ACCESS_DENIED, so promotion never succeeded on
// Windows and every write-through entry silently degraded.
func TestCreateSurvivesReplaceRename(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tmp := filepath.Join(dir, "out.flac.tmp-1")
	final := filepath.Join(dir, "out.flac")

	f, err := Create(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := Replace(tmp, final); err != nil {
		t.Fatalf("rename with the write handle open: %v", err)
	}

	// The old handle follows the renamed file, like a POSIX fd.
	buf := make([]byte, 7)
	if _, err := f.ReadAt(buf, 0); err != nil || string(buf) != "payload" {
		t.Fatalf("ReadAt through the pre-rename handle: %q (%v)", buf, err)
	}
	if _, err := os.Stat(final); err != nil {
		t.Errorf("final file missing after the rename: %v", err)
	}
	if _, err := os.Stat(tmp); err == nil {
		t.Error("tmp still exists after the rename")
	}
}

// TestOpenDoesNotBlockEviction pins the unlink half: an eviction racing an
// in-flight response deletes the file, NTFS unlinks the name at once, and
// the handle drains the unlinked file.
func TestOpenDoesNotBlockEviction(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	final := filepath.Join(dir, "out.flac")
	if err := os.WriteFile(final, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	serving, err := Open(final)
	if err != nil {
		t.Fatal(err)
	}
	defer serving.Close()

	if err := os.Remove(final); err != nil {
		t.Fatalf("eviction while a shared read handle is open: %v", err)
	}
	if _, err := os.Stat(final); err == nil {
		t.Error("the name still resolves after the eviction")
	}
	buf := make([]byte, 3)
	if _, err := serving.ReadAt(buf, 0); err != nil || string(buf) != "old" {
		t.Errorf("evicted handle reads %q (%v), want %q", buf, err, "old")
	}
}

// TestLongPaths guards the fixLongPath job sharedFile takes over from
// os.OpenFile: on a host without long-path support a >MAX_PATH cache path
// would otherwise fail ERROR_PATH_NOT_FOUND and silently degrade every
// entry. Trivially green where long paths are enabled; the guard is for the
// hosts where they are not.
func TestLongPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	deep := filepath.Join(dir, strings.Repeat("d", 100), strings.Repeat("e", 100), strings.Repeat("f", 100))
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(deep, "out.flac.tmp-1")
	final := filepath.Join(deep, "out.flac")
	if len(final) <= 260 {
		t.Fatalf("fixture path is %d chars, not past MAX_PATH", len(final))
	}

	f, err := Create(tmp)
	if err != nil {
		t.Fatalf("Create on a long path: %v", err)
	}
	if _, err := f.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Replace(tmp, final); err != nil {
		t.Fatalf("Replace on a long path: %v", err)
	}
	g, err := Open(final)
	if err != nil {
		t.Fatalf("Open on a long path: %v", err)
	}
	g.Close()
}
