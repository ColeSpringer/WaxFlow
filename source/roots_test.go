package source

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxflow/waxerr"
)

// rootDir makes a temp dir populated with the given rel->content files,
// for the extra roots the reload tests add, remove, and re-point.
func rootDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// readAll reads the whole file via ReadAt (position-independent, so it is
// valid even after the root it came from is closed).
func readAll(t *testing.T, f *File) string {
	t.Helper()
	buf := make([]byte, f.Size())
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	return string(buf)
}

func TestReloadAddsRoot(t *testing.T) {
	ctx := context.Background()
	r, libDir := newRoots(t, 0)
	dirB := rootDir(t, map[string]string{"track.wav": "RIFFdata"})

	// B is unknown before the reload.
	if _, err := r.Resolve(ctx, "B/track.wav"); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Fatalf("pre-reload B resolve = %v, want not-found", err)
	}

	res, err := r.Reload([]Root{{Name: "lib", Path: libDir}, {Name: "B", Path: dirB}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Added, []string{"B"}) || len(res.Removed) != 0 || len(res.Changed) != 0 {
		t.Fatalf("delta = %+v, want only B added", res)
	}
	if !slices.Equal(res.Roots, []string{"lib", "B"}) {
		t.Fatalf("roots = %v, want [lib B]", res.Roots)
	}

	f, err := r.Resolve(ctx, "B/track.wav")
	if err != nil {
		t.Fatalf("post-reload B resolve = %v, want success", err)
	}
	f.Close()
	// The unchanged root keeps resolving throughout.
	if f, err := r.Resolve(ctx, "lib/sub/a.wav"); err != nil {
		t.Fatalf("lib resolve after add = %v", err)
	} else {
		f.Close()
	}
}

func TestReloadRemovesRoot(t *testing.T) {
	ctx := context.Background()
	r, libDir := newRoots(t, 0)
	dirB := rootDir(t, map[string]string{"track.wav": "RIFFdata0123"})
	if _, err := r.Reload([]Root{{Name: "lib", Path: libDir}, {Name: "B", Path: dirB}}, 0); err != nil {
		t.Fatal(err)
	}

	// Open a file from B before it is removed; its fd must survive the close.
	f, err := r.Resolve(ctx, "B/track.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	res, err := r.Reload([]Root{{Name: "lib", Path: libDir}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Removed, []string{"B"}) || len(res.Added) != 0 || len(res.Changed) != 0 {
		t.Fatalf("delta = %+v, want only B removed", res)
	}

	// New requests for B 404.
	if _, err := r.Resolve(ctx, "B/track.wav"); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Fatalf("post-removal B resolve = %v, want not-found", err)
	}
	// The already-open file reads to completion (independent fd).
	if got := readAll(t, f); got != "RIFFdata0123" {
		t.Fatalf("pre-removal file read %q, want RIFFdata0123", got)
	}
}

func TestReloadRepointsRoot(t *testing.T) {
	ctx := context.Background()
	r, libDir := newRoots(t, 0)
	dir1 := rootDir(t, map[string]string{"x.wav": "AAAAAAAA"})
	dir2 := rootDir(t, map[string]string{"x.wav": "BBBBBBBB"})
	if _, err := r.Reload([]Root{{Name: "lib", Path: libDir}, {Name: "B", Path: dir1}}, 0); err != nil {
		t.Fatal(err)
	}

	// Open from the old path; the re-point closes the replaced handle, so
	// this is the dangerous case.
	old, err := r.Resolve(ctx, "B/x.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()

	res, err := r.Reload([]Root{{Name: "lib", Path: libDir}, {Name: "B", Path: dir2}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Changed, []string{"B"}) || len(res.Added) != 0 || len(res.Removed) != 0 {
		t.Fatalf("delta = %+v, want only B changed", res)
	}

	// New resolves hit the new path.
	fresh, err := r.Resolve(ctx, "B/x.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if got := readAll(t, fresh); got != "BBBBBBBB" {
		t.Fatalf("post-repoint read %q, want BBBBBBBB (new path)", got)
	}
	// The file opened before the re-point still reads the old path's bytes.
	if got := readAll(t, old); got != "AAAAAAAA" {
		t.Fatalf("pre-repoint read %q, want AAAAAAAA (old path)", got)
	}
}

func TestReloadReconcilesMaxBytes(t *testing.T) {
	ctx := context.Background()
	r, libDir := newRoots(t, 0) // lib/sub/a.wav is 8 bytes, default cap

	if f, err := r.Resolve(ctx, "lib/sub/a.wav"); err != nil {
		t.Fatalf("pre-reload resolve = %v", err)
	} else {
		f.Close()
	}

	// A smaller cap newly rejects the over-cap file, proving sourceMaxBytes
	// is reconciled and read under the lock.
	if _, err := r.Reload([]Root{{Name: "lib", Path: libDir}}, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(ctx, "lib/sub/a.wav"); waxerr.CodeOf(err) != waxerr.CodePayloadTooLarge {
		t.Fatalf("resolve under 4-byte cap = %v, want payload-too-large", err)
	}
	// Raising it again admits the file.
	if _, err := r.Reload([]Root{{Name: "lib", Path: libDir}}, 0); err != nil {
		t.Fatal(err)
	}
	if f, err := r.Resolve(ctx, "lib/sub/a.wav"); err != nil {
		t.Fatalf("resolve after raising the cap = %v", err)
	} else {
		f.Close()
	}
}

// TestReloadEqualsRestart pins the invariant the whole design rests on: a
// reloaded set is indistinguishable from one a fresh OpenRoots would build
// from the same desired roots (same names, same order).
func TestReloadEqualsRestart(t *testing.T) {
	r, libDir := newRoots(t, 0)
	dirB := rootDir(t, map[string]string{"track.wav": "RIFFdata"})
	dirC := rootDir(t, map[string]string{"track.wav": "RIFFdata"})
	// Deliberately not in the original order, to pin ordering too; D never
	// opens, which a restart and a reload must agree on as well.
	desired := []Root{{Name: "C", Path: dirC}, {Name: "lib", Path: libDir}, {Name: "D", Path: filepath.Join(t.TempDir(), "missing")}, {Name: "B", Path: dirB}}

	if _, err := r.Reload(desired, 0); err != nil {
		t.Fatal(err)
	}
	fresh, err := OpenRoots(desired, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()

	if got, want := r.Names(), fresh.Names(); !slices.Equal(got, want) {
		t.Fatalf("reloaded Names() = %v, restart Names() = %v", got, want)
	}
	shape := func(st []RootStatus) []string {
		var out []string
		for _, s := range st {
			out = append(out, fmt.Sprintf("%s %s %v", s.Name, s.Path, s.Mounted))
		}
		return out
	}
	if got, want := shape(r.Status()), shape(fresh.Status()); !slices.Equal(got, want) {
		t.Fatalf("reloaded Status() = %v, restart Status() = %v", got, want)
	}
}

// TestReloadReportsUnopenable pins the tolerant reconcile: an unopenable path
// becomes an unmounted root and the rest of the reload lands (the valid add
// and the cap), where it used to refuse the whole reload. A later reload
// retries the unmounted root.
func TestReloadReportsUnopenable(t *testing.T) {
	ctx := context.Background()
	r, libDir := newRoots(t, 0) // lib/sub/a.wav is 8 bytes
	dirC := rootDir(t, map[string]string{"track.wav": "RIFF"})
	later := filepath.Join(t.TempDir(), "later")

	desired := []Root{{Name: "lib", Path: libDir}, {Name: "C", Path: dirC}, {Name: "B", Path: later}}
	res, err := r.Reload(desired, 4)
	if err != nil {
		t.Fatalf("reload with an unopenable path = %v, want success", err)
	}
	if !slices.Equal(res.Added, []string{"C", "B"}) || !slices.Equal(res.Unmounted, []string{"B"}) ||
		len(res.Remounted) != 0 || len(res.Changed) != 0 || len(res.Removed) != 0 {
		t.Fatalf("result = %+v, want C and B added and B unmounted", res)
	}
	if f, err := r.Resolve(ctx, "C/track.wav"); err != nil {
		t.Fatalf("C resolve beside the unopenable root = %v", err)
	} else {
		f.Close()
	}
	if _, err := r.Resolve(ctx, "lib/sub/a.wav"); waxerr.CodeOf(err) != waxerr.CodePayloadTooLarge {
		t.Fatalf("lib under the 4-byte cap = %v, want payload-too-large (the cap applied)", err)
	}
	if _, err := r.Resolve(ctx, "B/x.wav"); waxerr.CodeOf(err) != waxerr.CodeRootUnavailable {
		t.Fatalf("B resolve = %v, want root-unavailable", err)
	}

	// A retry that still fails keeps the time B went down.
	down := statusOf(t, r, "B").Since
	fixClock(r, down.Add(time.Hour))
	if res, err = r.Reload(desired, 4); err != nil || !slices.Equal(res.Unmounted, []string{"B"}) {
		t.Fatalf("second reload = %+v, %v; want B still unmounted", res, err)
	}
	if got := statusOf(t, r, "B").Since; !got.Equal(down) {
		t.Fatalf("B Since after a failed retry = %v, want %v", got, down)
	}

	// The same reload once the folder exists mounts B and says so.
	if err := os.MkdirAll(later, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(later, "x.wav"), []byte("RIFF"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = r.Reload(desired, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Remounted, []string{"B"}) || len(res.Unmounted) != 0 || len(res.Added) != 0 {
		t.Fatalf("retry result = %+v, want only B remounted", res)
	}
	if st := statusOf(t, r, "B"); !st.Mounted || st.Err != nil {
		t.Fatalf("B after the retry = %+v, want mounted", st)
	}
}

// TestReloadRepointToAnUnopenablePath pins that a re-pointed root whose new
// path will not open stops serving its old one, which would be the wrong
// library. A file resolved before the reload keeps reading.
func TestReloadRepointToAnUnopenablePath(t *testing.T) {
	ctx := context.Background()
	r, libDir := newRoots(t, 0)
	dir1 := rootDir(t, map[string]string{"x.wav": "AAAAAAAA"})
	if _, err := r.Reload([]Root{{Name: "lib", Path: libDir}, {Name: "B", Path: dir1}}, 0); err != nil {
		t.Fatal(err)
	}
	old, err := r.Resolve(ctx, "B/x.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()

	res, err := r.Reload([]Root{{Name: "lib", Path: libDir}, {Name: "B", Path: filepath.Join(t.TempDir(), "missing")}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Changed, []string{"B"}) || !slices.Equal(res.Unmounted, []string{"B"}) {
		t.Fatalf("result = %+v, want B changed and unmounted", res)
	}
	if _, err := r.Resolve(ctx, "B/x.wav"); waxerr.CodeOf(err) != waxerr.CodeRootUnavailable {
		t.Fatalf("B after the re-point = %v, want root-unavailable, not the old path's file", err)
	}
	if got := readAll(t, old); got != "AAAAAAAA" {
		t.Fatalf("pre-reload file read %q, want AAAAAAAA", got)
	}
}

// statusOf returns the named root's entry in r.Status().
func statusOf(t *testing.T, r *Roots, name string) RootStatus {
	t.Helper()
	for _, st := range r.Status() {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("no status for root %q", name)
	return RootStatus{}
}

// fixClock pins r's clock, so a test can tell which operation set a Since.
func fixClock(r *Roots, at time.Time) { r.now = func() time.Time { return at } }

// skipIfHeldDirsResistRemoval skips tests that remove a mounted root's
// directory: Go opens Windows handles without delete sharing, so there the
// directory cannot go while the root holds it.
func skipIfHeldDirsResistRemoval(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows will not remove a directory an open os.Root holds")
	}
}

// TestResolveMountsOnDemand pins that a folder appearing under an unmounted
// root serves on the first request, with no sweep between: what makes the
// 503's Retry-After honest.
func TestResolveMountsOnDemand(t *testing.T) {
	ctx := context.Background()
	later := filepath.Join(t.TempDir(), "later")
	r, err := OpenRoots([]Root{{Name: "B", Path: later}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Resolve(ctx, "B/x.wav"); waxerr.CodeOf(err) != waxerr.CodeRootUnavailable {
		t.Fatalf("resolve before mkdir = %v, want root-unavailable", err)
	}

	if err := os.MkdirAll(later, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(later, "x.wav"), []byte("RIFFdata"), 0o644); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	fixClock(r, at)
	f, err := r.Resolve(ctx, "B/x.wav")
	if err != nil {
		t.Fatalf("first resolve after mkdir = %v, want success", err)
	}
	f.Close()
	if st := statusOf(t, r, "B"); !st.Mounted || st.Err != nil || !st.Since.Equal(at) {
		t.Fatalf("B after the on-demand mount = %+v, want mounted since %v", st, at)
	}
}

// TestResolveVanishedRootIsRootUnavailable pins the request-time health check:
// a mounted root whose directory was removed answers root-unavailable, not
// the not-found its dead handle reports, and is demoted so a later request
// mounts it again on demand.
func TestResolveVanishedRootIsRootUnavailable(t *testing.T) {
	skipIfHeldDirsResistRemoval(t)
	ctx := context.Background()
	dir := rootDir(t, map[string]string{"x.wav": "AAAAAAAA"})
	r, err := OpenRoots([]Root{{Name: "B", Path: dir}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	_, err = r.Resolve(ctx, "B/x.wav")
	if waxerr.CodeOf(err) != waxerr.CodeRootUnavailable || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("resolve on a vanished root = %v, want root-unavailable with a not-exist cause", err)
	}
	if st := statusOf(t, r, "B"); st.Mounted || !errors.Is(st.Err, fs.ErrNotExist) {
		t.Fatalf("B after the failed resolve = %+v, want demoted to unmounted", st)
	}
}

// TestResolveRemountsAReplacedRoot pins the stale-handle path: a library
// directory removed and recreated under its path (a sync tool's swap) serves
// the new bytes on the first request, not a 404 through the deleted one.
func TestResolveRemountsAReplacedRoot(t *testing.T) {
	skipIfHeldDirsResistRemoval(t)
	ctx := context.Background()
	dir := rootDir(t, map[string]string{"x.wav": "AAAAAAAA"})
	r, err := OpenRoots([]Root{{Name: "B", Path: dir}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	stale := r.roots["B"].root

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.wav"), []byte("BBBBBBBB"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := r.Resolve(ctx, "B/x.wav")
	if err != nil {
		t.Fatalf("resolve on a replaced root = %v, want success", err)
	}
	defer f.Close()
	if got := readAll(t, f); got != "BBBBBBBB" {
		t.Fatalf("read %q, want BBBBBBBB (the new directory)", got)
	}
	if _, err := stale.Stat("."); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("the replaced handle is still open (%v)", err)
	}
}

// TestResolveRetriesAClosedHandle pins the answer to a handle a writer closed
// between Resolve's lookup and its open: a fresh mount, not a 404. No test can
// schedule that window, so this one closes the handle in place.
func TestResolveRetriesAClosedHandle(t *testing.T) {
	r, _ := newRoots(t, 0)
	r.roots["lib"].root.Close()
	f, err := r.Resolve(context.Background(), "lib/sub/a.wav")
	if err != nil {
		t.Fatalf("resolve through a closed handle = %v, want a fresh mount", err)
	}
	f.Close()
}

// TestRemountAfterMkdir pins the sweep's mount: Remount reports a root whose
// folder appeared, once. A retry that fails for the same reason is no change;
// one that fails for a new reason is, and keeps the time the root went down.
func TestRemountAfterMkdir(t *testing.T) {
	ctx := context.Background()
	later := filepath.Join(t.TempDir(), "later")
	r, err := OpenRoots([]Root{{Name: "B", Path: later}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	down := statusOf(t, r, "B").Since

	if got := r.Remount(); len(got) != 0 {
		t.Fatalf("Remount with the folder missing = %+v, want no change", got)
	}
	// A file where the folder should be: still unmounted, a different reason.
	if err := os.WriteFile(later, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := r.Remount()
	if len(got) != 1 || got[0].Mounted || got[0].Err == nil || errors.Is(got[0].Err, fs.ErrNotExist) || !got[0].Since.Equal(down) {
		t.Fatalf("Remount with a file at the path = %+v, want B down since %v for the file's reason", got, down)
	}
	if got := r.Remount(); len(got) != 0 {
		t.Fatalf("Remount again = %+v, want no change", got)
	}

	if err := os.Remove(later); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(later, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(later, "x.wav"), []byte("RIFFdata"), 0o644); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	fixClock(r, at)
	got = r.Remount()
	if len(got) != 1 || got[0].Name != "B" || !got[0].Mounted || got[0].Err != nil || !got[0].Since.Equal(at) {
		t.Fatalf("Remount after mkdir = %+v, want B mounted since %v", got, at)
	}
	if got := r.Remount(); len(got) != 0 {
		t.Fatalf("second Remount = %+v, want no change", got)
	}
	if f, err := r.Resolve(ctx, "B/x.wav"); err != nil {
		t.Fatalf("resolve after the sweep mounted B = %v", err)
	} else {
		f.Close()
	}
}

// TestRemountDemotesAVanishedRoot pins the sweep's other half: a mounted root
// whose directory is removed is demoted with the reason and mounted again once
// the directory is back, and one replaced between sweeps is reopened in one.
func TestRemountDemotesAVanishedRoot(t *testing.T) {
	skipIfHeldDirsResistRemoval(t)
	ctx := context.Background()
	dir := rootDir(t, map[string]string{"x.wav": "AAAAAAAA"})
	r, err := OpenRoots([]Root{{Name: "B", Path: dir}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := r.Remount(); len(got) != 0 {
		t.Fatalf("Remount over a healthy root = %+v, want no change", got)
	}

	held := r.roots["B"].root
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	gone := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	fixClock(r, gone)
	got := r.Remount()
	if len(got) != 1 || got[0].Mounted || !errors.Is(got[0].Err, fs.ErrNotExist) || !got[0].Since.Equal(gone) {
		t.Fatalf("Remount after removal = %+v, want B unmounted since %v", got, gone)
	}
	if _, err := held.Stat("."); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("the demoted root's handle is still open (%v)", err)
	}
	if _, err := r.Resolve(ctx, "B/x.wav"); waxerr.CodeOf(err) != waxerr.CodeRootUnavailable {
		t.Fatalf("resolve on the demoted root = %v, want root-unavailable", err)
	}

	for _, content := range []string{"BBBBBBBB", "CCCCCCCC"} {
		held := r.roots["B"].root // nil while demoted
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "x.wav"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := r.Remount(); len(got) != 1 || !got[0].Mounted {
			t.Fatalf("Remount with %s in place = %+v, want B mounted", content, got)
		}
		if held != nil {
			if _, err := held.Stat("."); !errors.Is(err, fs.ErrClosed) {
				t.Errorf("the replaced handle is still open (%v)", err)
			}
		}
		f, err := r.Resolve(ctx, "B/x.wav")
		if err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, f); got != content {
			t.Fatalf("read %q, want %s", got, content)
		}
		f.Close()
	}
}

// TestCommitAfterReload pins the commit guard against a reload: a check that
// opened a root's old folder cannot install it once a reload re-pointed the
// root, and its handle is closed rather than installed or leaked.
func TestCommitAfterReload(t *testing.T) {
	ctx := context.Background()
	pathA := filepath.Join(t.TempDir(), "a")
	dirB := rootDir(t, map[string]string{"x.wav": "BBBBBBBB"})
	r, err := OpenRoots([]Root{{Name: "X", Path: pathA}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := os.MkdirAll(pathA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pathA, "x.wav"), []byte("AAAAAAAA"), 0o644); err != nil {
		t.Fatal(err)
	}

	seen := r.roots["X"]      // unmounted at pathA
	stale := r.mountAt(pathA) // a check's open, not yet committed
	if stale.root == nil {
		t.Fatalf("opening pathA = %v", stale.err)
	}
	if _, err := r.Reload([]Root{{Name: "X", Path: dirB}}, 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.commit("X", seen, stale); ok {
		t.Fatal("a check from before the reload committed over it")
	}
	if _, err := stale.root.Stat("."); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("the losing handle is still open (%v)", err)
	}
	f, err := r.Resolve(ctx, "X/x.wav")
	if err != nil {
		t.Fatalf("X resolve = %v", err)
	}
	defer f.Close()
	if got := readAll(t, f); got != "BBBBBBBB" {
		t.Fatalf("X read %q, want the reload's directory", got)
	}
}

// TestCommitNeedsTheSameState pins the guard against a root that went round
// to an equal-looking state: a check that saw it unmounted cannot install its
// handle once a mount and an unmount came between, though the path and the
// missing handle match again.
func TestCommitNeedsTheSameState(t *testing.T) {
	skipIfHeldDirsResistRemoval(t)
	later := filepath.Join(t.TempDir(), "later")
	r, err := OpenRoots([]Root{{Name: "B", Path: later}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := os.MkdirAll(later, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(later, "x.wav"), []byte("RIFFdata"), 0o644); err != nil {
		t.Fatal(err)
	}

	seen := r.roots["B"]
	stale := r.mountAt(later)
	if _, ok := r.commit("B", seen, r.mountAt(later)); !ok {
		t.Fatal("the mount did not commit")
	}
	mounted := r.roots["B"]
	if err := os.RemoveAll(later); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.commit("B", mounted, r.mountAt(later)); !ok {
		t.Fatal("the demotion did not commit")
	}
	if _, ok := r.commit("B", seen, stale); ok {
		t.Fatal("a check from before a mount and an unmount installed a handle to a deleted folder")
	}
	if _, err := stale.root.Stat("."); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("the losing handle is still open (%v)", err)
	}
	if st := statusOf(t, r, "B"); st.Mounted {
		t.Fatalf("B = %+v, want unmounted", st)
	}
}

// TestCommitAfterClose pins the closed-set guard Reload has: a check that
// opened a folder before Close closes what it opened instead of resurrecting
// the set, and a sweep after Close changes nothing.
func TestCommitAfterClose(t *testing.T) {
	later := filepath.Join(t.TempDir(), "later")
	r, err := OpenRoots([]Root{{Name: "B", Path: later}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(later, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(later, "x.wav"), []byte("RIFFdata"), 0o644); err != nil {
		t.Fatal(err)
	}
	seen := r.roots["B"]
	stale := r.mountAt(later)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.commit("B", seen, stale); ok {
		t.Fatal("a check committed after Close")
	}
	if _, err := stale.root.Stat("."); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("the check's handle outlived Close (%v)", err)
	}
	if got := r.Remount(); len(got) != 0 {
		t.Fatalf("Remount after Close = %+v, want no change", got)
	}
	if st := r.Status(); len(st) != 0 {
		t.Fatalf("Status after Close = %+v, want empty", st)
	}
	if _, err := r.Resolve(context.Background(), "B/x.wav"); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Fatalf("resolve after Close = %v, want not-found", err)
	}
}

// TestResolveSurvivesARetiredHandle races requests against reloads that only
// re-point their root: it stays configured, so a request on it never answers
// not-found or a retired handle's error, however many reloads retire the
// handle during one resolve. A re-pointed root is still opening until its
// reload's check commits; a request waits on that, and only one that meets
// back-to-back re-points more times than it looks gives up. Run under -race.
func TestResolveSurvivesARetiredHandle(t *testing.T) {
	ctx := context.Background()
	r, libDir := newRoots(t, 0)
	dir1 := rootDir(t, map[string]string{"x.wav": "AAAAAAAA"})
	dir2 := rootDir(t, map[string]string{"x.wav": "BBBBBBBB"})
	states := [][]Root{
		{{Name: "lib", Path: libDir}, {Name: "B", Path: dir1}},
		{{Name: "lib", Path: libDir}, {Name: "B", Path: dir2}},
	}
	if _, err := r.Reload(states[0], 0); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				f, err := r.Resolve(ctx, "B/x.wav")
				switch {
				case err == nil:
					f.Close()
				case !errors.Is(err, errOpening) && !errors.Is(err, errChanging):
					t.Errorf("resolve on a re-pointed root = %v", err)
					return
				}
			}
		})
	}
	for i := range 400 {
		if _, err := r.Reload(states[i%2], 0); err != nil {
			t.Errorf("reload %d failed: %v", i, err)
			break
		}
	}
	close(done)
	readers.Wait()
}

// TestRemountConcurrent runs the sweep against reloads, requests and a library
// directory that keeps vanishing. The stable root must never fail; the rest
// may answer anything but a race or a use of a closed handle. Run under -race.
func TestRemountConcurrent(t *testing.T) {
	skipIfHeldDirsResistRemoval(t)
	ctx := context.Background()
	r, libDir := newRoots(t, 0)
	dir1 := rootDir(t, map[string]string{"x.wav": "AAAAAAAA"})
	dir2 := rootDir(t, map[string]string{"x.wav": "BBBBBBBB"})

	done := make(chan struct{})
	var wg sync.WaitGroup
	// loop runs body until done closes or body returns false.
	loop := func(body func() bool) {
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				if !body() {
					return
				}
			}
		})
	}
	for range 4 {
		loop(func() bool {
			f, err := r.Resolve(ctx, "lib/sub/a.wav")
			if err != nil {
				t.Errorf("stable root failed: %v", err)
				return false
			}
			f.Close()
			return true
		})
		loop(func() bool {
			if f, err := r.Resolve(ctx, "B/x.wav"); err == nil {
				f.ReadAt(make([]byte, 8), 0)
				f.Close()
			}
			return true
		})
	}
	loop(func() bool { r.Remount(); return true })
	loop(func() bool { r.Status(); return true })
	loop(func() bool {
		os.RemoveAll(dir1)
		os.MkdirAll(dir1, 0o755)
		os.WriteFile(filepath.Join(dir1, "x.wav"), []byte("AAAAAAAA"), 0o644)
		return true
	})

	states := [][]Root{
		{{Name: "lib", Path: libDir}, {Name: "B", Path: dir1}},
		{{Name: "lib", Path: libDir}, {Name: "B", Path: dir2}},
		{{Name: "lib", Path: libDir}},
	}
	for i := range 300 {
		if _, err := r.Reload(states[i%len(states)], 0); err != nil {
			t.Errorf("reload %d failed: %v", i, err)
			break
		}
	}
	close(done)
	wg.Wait()
}

func TestReloadRejectsInvalidAndDuplicate(t *testing.T) {
	ctx := context.Background()
	r, libDir := newRoots(t, 0)
	dirB := rootDir(t, map[string]string{"track.wav": "RIFFdata"})

	cases := []struct {
		name    string
		desired []Root
	}{
		{"empty name", []Root{{Name: "lib", Path: libDir}, {Name: "", Path: dirB}}},
		{"slash in name", []Root{{Name: "lib", Path: libDir}, {Name: "a/b", Path: dirB}}},
		{"colon in name", []Root{{Name: "lib", Path: libDir}, {Name: "a:b", Path: dirB}}},
		{"duplicate name", []Root{{Name: "lib", Path: libDir}, {Name: "lib", Path: dirB}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.Reload(tc.desired, 0); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
				t.Fatalf("Reload(%s) = %v, want invalid-request", tc.name, err)
			}
			// The refusal left the live set intact.
			if f, err := r.Resolve(ctx, "lib/sub/a.wav"); err != nil {
				t.Fatalf("live set disturbed by a rejected reload: %v", err)
			} else {
				f.Close()
			}
		})
	}
}

func TestCloseClearsTheSet(t *testing.T) {
	ctx := context.Background()
	r, _ := newRoots(t, 0)

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// A Resolve after Close is an ordinary unknown-root, not a use of a
	// closed handle.
	if _, err := r.Resolve(ctx, "lib/sub/a.wav"); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Fatalf("resolve after close = %v, want not-found", err)
	}
	// A second Close is a no-op (newRoots also closes at cleanup).
	if err := r.Close(); err != nil {
		t.Fatalf("double close = %v, want nil", err)
	}
}

// TestReloadAfterClose pins the symmetric-safety guard: a reload racing (or
// following) Close must not resurrect the torn-down set, and must close what
// it opened rather than leak those fds.
func TestReloadAfterClose(t *testing.T) {
	ctx := context.Background()
	r, libDir := newRoots(t, 0)
	dirB := rootDir(t, map[string]string{"b.wav": "RIFFdata"})

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// Reload on a closed set short-circuits (phase 1) with an
	// fs.ErrClosed-matchable error, opening nothing and swapping nothing.
	_, err := r.Reload([]Root{{Name: "lib", Path: libDir}, {Name: "B", Path: dirB}}, 0)
	if err == nil {
		t.Fatal("Reload after Close succeeded, want an error")
	}
	if !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("Reload after Close = %v, want an fs.ErrClosed-matchable error", err)
	}
	// Not resurrected: had the swap happened, lib would resolve again. It
	// stays an unknown root.
	if _, err := r.Resolve(ctx, "lib/sub/a.wav"); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Fatalf("resolve after reload-after-close = %v, want not-found (no resurrection)", err)
	}
}

// TestReloadConcurrent is the point of the whole change: a previously
// lock-free hot path stays safe under mutation. Readers hammer a stable
// root (which must never fail) and a churning second root (any outcome
// tolerated but never a use-after-close) while one goroutine loops a reload
// that adds, re-points, and removes the second root and varies the cap. Run
// under -race.
func TestReloadConcurrent(t *testing.T) {
	ctx := context.Background()
	r, libDir := newRoots(t, 0) // stable root "lib", a.wav is 8 bytes
	dir1 := rootDir(t, map[string]string{"x.wav": "AAAAAAAA"})
	dir2 := rootDir(t, map[string]string{"x.wav": "BBBBBBBB"})

	done := make(chan struct{})
	var readers sync.WaitGroup

	// Readers on the stable root: it must resolve throughout.
	for range 6 {
		readers.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				f, err := r.Resolve(ctx, "lib/sub/a.wav")
				if err != nil {
					t.Errorf("stable root failed mid-reload: %v", err)
					return
				}
				f.Close()
			}
		})
	}
	// Readers on the churning root: open racing the reloader's close of that
	// same handle. Any error is fine; a race or use-after-close is not.
	for range 4 {
		readers.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				if f, err := r.Resolve(ctx, "B/x.wav"); err == nil {
					f.ReadAt(make([]byte, 8), 0)
					f.Close()
				}
			}
		})
	}

	states := [][]Root{
		{{Name: "lib", Path: libDir}, {Name: "B", Path: dir1}},
		{{Name: "lib", Path: libDir}, {Name: "B", Path: dir2}}, // re-point (closes the replaced handle)
		{{Name: "lib", Path: libDir}},                          // remove B
	}
	caps := []int64{0, 1 << 30, 64} // all admit the 8-byte stable file
	for i := range 400 {
		if _, err := r.Reload(states[i%len(states)], caps[i%len(caps)]); err != nil {
			t.Errorf("reload %d failed: %v", i, err)
			break
		}
	}
	close(done)
	readers.Wait()
}

// TestResolveDoesNotWaitOnAReload pins that a request never queues behind a
// reload, whose opens a dead network mount can hold: one that finds its
// root's folder back while a reload runs mounts it and serves.
func TestResolveDoesNotWaitOnAReload(t *testing.T) {
	ctx := context.Background()
	later := filepath.Join(t.TempDir(), "later")
	r, err := OpenRoots([]Root{{Name: "B", Path: later}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := os.MkdirAll(later, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(later, "x.wav"), []byte("RIFFdata"), 0o644); err != nil {
		t.Fatal(err)
	}

	r.reloadMu.Lock() // a reload in progress
	defer r.reloadMu.Unlock()
	done := make(chan error, 1)
	go func() {
		f, err := r.Resolve(ctx, "B/x.wav")
		if err == nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("resolve during a reload = %v, want it served", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Resolve waited on the reload in progress")
	}
}

// TestResolveHerdOnAFolderThatAppears pins a burst of requests on a root whose
// folder just appeared: they share one mount, and none of them is told the
// root is unavailable while it is there. Run under -race.
func TestResolveHerdOnAFolderThatAppears(t *testing.T) {
	for range 20 {
		later := filepath.Join(t.TempDir(), "later")
		r, err := OpenRoots([]Root{{Name: "B", Path: later}}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(later, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(later, "x.wav"), []byte("RIFFdata"), 0o644); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				<-start
				for range 15 {
					f, err := r.Resolve(context.Background(), "B/x.wav")
					if err != nil {
						t.Errorf("resolve on a folder that exists = %v", err)
						return
					}
					f.Close()
				}
			})
		}
		close(start)
		wg.Wait()
		r.Close()
	}
}

// TestEmptyFolderIsUnmounted pins the mount point whose drive has not
// arrived: its folder opens but holds nothing, so the root is not mounted,
// a request on it answers root-unavailable rather than not-found, and the
// first request after anything lands in it mounts it.
func TestEmptyFolderIsUnmounted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	r, err := OpenRoots([]Root{{Name: "B", Path: dir}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if st := statusOf(t, r, "B"); st.Mounted || !errors.Is(st.Err, ErrRootEmpty) {
		t.Fatalf("B = %+v, want unmounted with ErrRootEmpty", st)
	}
	if _, err := r.Resolve(ctx, "B/x.wav"); waxerr.CodeOf(err) != waxerr.CodeRootUnavailable {
		t.Fatalf("resolve in an empty folder = %v, want root-unavailable", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.wav"), []byte("RIFFdata"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := r.Resolve(ctx, "B/x.wav")
	if err != nil {
		t.Fatalf("resolve once the folder has content = %v", err)
	}
	f.Close()
}

// TestWatchReportsEveryChange pins that a change is reported whoever commits
// it: a request's mount and its demotion of a vanished root, the sweep's new
// reason for a root that stays down, and the failure of a reload's new root.
func TestWatchReportsEveryChange(t *testing.T) {
	skipIfHeldDirsResistRemoval(t)
	ctx := context.Background()
	base := t.TempDir()
	later := filepath.Join(base, "later")
	r, err := OpenRoots([]Root{{Name: "B", Path: later}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var mu sync.Mutex
	var seen []string
	r.Watch(func(st RootStatus) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, fmt.Sprintf("%s mounted=%v", st.Name, st.Mounted))
	})

	if err := os.MkdirAll(later, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(later, "x.wav"), []byte("RIFFdata"), 0o644); err != nil {
		t.Fatal(err)
	}
	if f, err := r.Resolve(ctx, "B/x.wav"); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	if err := os.RemoveAll(later); err != nil {
		t.Fatal(err)
	}
	r.Resolve(ctx, "B/x.wav")
	if err := os.WriteFile(later, []byte("x"), 0o644); err != nil { // a new reason
		t.Fatal(err)
	}
	r.Remount()
	if _, err := r.Reload([]Root{{Name: "B", Path: later}, {Name: "C", Path: filepath.Join(base, "c")}}, 0); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"B mounted=true", "B mounted=false", "B mounted=false", "C mounted=false"}
	if !slices.Equal(seen, want) {
		t.Fatalf("Watch saw %q, want %q", seen, want)
	}
}

// TestWatchReportsInOrder pins one root's reports to its commits' order: a
// change found while the report of the one before is still being delivered
// waits for it, so the log never ends on a state the root has left.
func TestWatchReportsInOrder(t *testing.T) {
	skipIfHeldDirsResistRemoval(t)
	ctx := context.Background()
	later := filepath.Join(t.TempDir(), "later")
	r, err := OpenRoots([]Root{{Name: "B", Path: later}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var seen []bool
	first := true
	r.Watch(func(st RootStatus) {
		mu.Lock()
		hold := first
		first = false
		mu.Unlock()
		if hold {
			close(entered)
			<-release
		}
		mu.Lock()
		seen = append(seen, st.Mounted)
		mu.Unlock()
	})

	if err := os.MkdirAll(later, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(later, "x.wav"), []byte("RIFFdata"), 0o644); err != nil {
		t.Fatal(err)
	}
	mounted := make(chan struct{})
	go func() {
		defer close(mounted)
		if f, err := r.Resolve(ctx, "B/x.wav"); err == nil {
			f.Close()
		}
	}()
	<-entered // the mount's report is being delivered
	if err := os.RemoveAll(later); err != nil {
		t.Fatal(err)
	}
	demoted := make(chan struct{})
	go func() {
		defer close(demoted)
		r.Remount()
	}()
	time.Sleep(50 * time.Millisecond) // let the demotion reach its commit
	close(release)
	<-mounted
	<-demoted
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(seen, []bool{true, false}) {
		t.Fatalf("reports, mounted each = %v, want [true false]", seen)
	}
}

// TestEmptyFolderIgnoresPlaceholders pins what counts as content: a bare mount
// point a desktop has browsed (a .DS_Store) or a fresh filesystem's lost+found
// is still empty, and one real entry is not.
func TestEmptyFolderIgnoresPlaceholders(t *testing.T) {
	dir := rootDir(t, map[string]string{".DS_Store": "x", "lost+found/.keep": "", "System Volume Information/x": ""})
	r, err := OpenRoots([]Root{{Name: "B", Path: dir}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if st := statusOf(t, r, "B"); st.Mounted || !errors.Is(st.Err, ErrRootEmpty) {
		t.Fatalf("B with placeholders only = %+v, want unmounted as empty", st)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.flac"), []byte("fLaC"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := r.Remount(); len(got) != 1 || !got[0].Mounted {
		t.Fatalf("Remount with one real entry = %+v, want B mounted", got)
	}
}

// TestRootUnavailableNamesItsRoot pins the typed cause inside a
// root-unavailable error: whoever wraps the error (the pid lookup, a CLI
// hint) can still tell which root is down, and the text and the underlying
// cause read as before.
func TestRootUnavailableNamesItsRoot(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	r, err := OpenRoots([]Root{{Name: "lib", Path: gone}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, err = r.Resolve(context.Background(), "lib/a.wav")
	if waxerr.CodeOf(err) != waxerr.CodeRootUnavailable {
		t.Fatalf("resolve on an unmounted root = %v, want root-unavailable", err)
	}
	wrapped := fmt.Errorf("pid lookup: %w", err)
	down, ok := errors.AsType[*RootDownError](wrapped)
	if !ok || down.Root != "lib" {
		t.Fatalf("no RootDownError naming lib inside %v", wrapped)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want the missing folder still visible to errors.Is", err)
	}
	if !strings.Contains(err.Error(), `root "lib" is unavailable: `) || strings.Contains(err.Error(), gone) {
		t.Errorf("err text = %q, want the root named and its path withheld", err)
	}
}
