package source

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

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
	// Deliberately not in the original order, to pin ordering too.
	desired := []Root{{Name: "C", Path: dirC}, {Name: "lib", Path: libDir}, {Name: "B", Path: dirB}}

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
}

// TestReloadRollback pins atomicity: a reload whose desired set has an
// unopenable path returns an error and leaves the live set and cap
// untouched, including a valid root that opened before the failing one.
func TestReloadRollback(t *testing.T) {
	ctx := context.Background()
	r, libDir := newRoots(t, 0)
	dirC := rootDir(t, map[string]string{"track.wav": "RIFFdata"})
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	// C is valid and opens first; B fails. The cleanup must close C, and
	// nothing is swapped. The smaller cap must not stick either.
	desired := []Root{{Name: "lib", Path: libDir}, {Name: "C", Path: dirC}, {Name: "B", Path: missing}}
	if _, err := r.Reload(desired, 4); err == nil {
		t.Fatal("reload with an unopenable path succeeded, want an error")
	}

	// The prior root still resolves at the OLD (default) cap: neither the
	// root set nor maxBytes moved.
	if f, err := r.Resolve(ctx, "lib/sub/a.wav"); err != nil {
		t.Fatalf("rollback disturbed the live set: %v", err)
	} else {
		f.Close()
	}
	// C was never added.
	if _, err := r.Resolve(ctx, "C/track.wav"); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Fatalf("C resolve after rollback = %v, want not-found (nothing swapped)", err)
	}
	// A subsequent valid reload still works (no wedged lock or state).
	if _, err := r.Reload([]Root{{Name: "lib", Path: libDir}, {Name: "C", Path: dirC}}, 0); err != nil {
		t.Fatalf("reload after a rolled-back one = %v", err)
	}
	if f, err := r.Resolve(ctx, "C/track.wav"); err != nil {
		t.Fatalf("C resolve after recovery reload = %v", err)
	} else {
		f.Close()
	}
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
		readers.Add(1)
		go func() {
			defer readers.Done()
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
		}()
	}
	// Readers on the churning root: open racing the reloader's close of that
	// same handle. Any error is fine; a race or use-after-close is not.
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
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
		}()
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
