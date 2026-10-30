//go:build unix && !solaris && !aix

package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/colespringer/waxflow/waxerr"
)

func TestResolveRejectsFIFO(t *testing.T) {
	r, dir := newRoots(t, 0)
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	// This must return promptly (O_NONBLOCK), not hang waiting for a
	// writer, and then fail the regular-file check.
	_, err := r.Resolve(context.Background(), "lib/pipe")
	if got := waxerr.CodeOf(err); got != waxerr.CodeUnsupportedSource {
		t.Fatalf("FIFO resolve code = %s (%v), want unsupported-source", got, err)
	}
}

// hungFolder makes path a FIFO, whose open(2) without O_NONBLOCK waits for a
// writer as a dead hard mount waits for its server, and releases every open
// stuck on it when the test ends.
func hungFolder(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
	})
}

// within runs fn and fails the test when it takes longer than d.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s is held by a folder that never answers", what)
	}
}

// TestOpenRootsIsNotHeldByAHungFolder pins start-up against a folder whose
// open never returns: the daemon starts with that root unmounted, and a
// request on it joins the stuck check, which has run out its bound, so it
// answers root-unavailable at once instead of starting a second stuck open.
func TestOpenRootsIsNotHeldByAHungFolder(t *testing.T) {
	setAttemptWait(t, 300*time.Millisecond)
	lib := rootDir(t, map[string]string{"a.wav": "RIFFdata"})
	hung := filepath.Join(t.TempDir(), "hung")
	hungFolder(t, hung)
	var r *Roots
	within(t, 5*time.Second, "OpenRoots", func() {
		var err error
		if r, err = OpenRoots([]Root{{Name: "lib", Path: lib}, {Name: "H", Path: hung}}, 0); err != nil {
			t.Error(err)
		}
	})
	if r == nil {
		return
	}
	defer r.Close()
	if st := statusOf(t, r, "H"); st.Mounted || !errors.Is(st.Err, errNotAnswering) {
		t.Fatalf("H = %+v, want unmounted as not answering once its check ran out", st)
	}
	within(t, 100*time.Millisecond, "a request on the hung root", func() {
		if _, err := r.Resolve(context.Background(), "H/x.wav"); waxerr.CodeOf(err) != waxerr.CodeRootUnavailable {
			t.Errorf("resolve on the hung root = %v, want root-unavailable", err)
		}
	})
	if f, err := r.Resolve(context.Background(), "lib/a.wav"); err != nil {
		t.Fatalf("lib beside the hung root = %v", err)
	} else {
		f.Close()
	}
}

// TestReloadIsNotHeldByAHungFolder pins that a folder whose open never returns
// costs a reload the attempt bound, not the rest of time, so the next reload
// runs too.
func TestReloadIsNotHeldByAHungFolder(t *testing.T) {
	setAttemptWait(t, 50*time.Millisecond)
	r, libDir := newRoots(t, 0)
	hung := filepath.Join(t.TempDir(), "hung")
	hungFolder(t, hung)
	desired := []Root{{Name: "lib", Path: libDir}, {Name: "H", Path: hung}}
	for i := range 2 {
		within(t, 5*time.Second, "Reload", func() {
			res, err := r.Reload(desired, 0)
			if err != nil || !slices.Equal(res.Unmounted, []string{"H"}) {
				t.Errorf("reload %d = %+v, %v; want H unmounted", i, res, err)
			}
		})
	}
}

// TestRemountIsNotHeldByAHungFolder pins the sweep against one folder whose
// open never returns: the other roots are still checked, and the pass ends.
func TestRemountIsNotHeldByAHungFolder(t *testing.T) {
	setAttemptWait(t, 50*time.Millisecond)
	base := t.TempDir()
	hung, later := filepath.Join(base, "hung"), filepath.Join(base, "later")
	r, err := OpenRoots([]Root{{Name: "H", Path: hung}, {Name: "B", Path: later}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	hungFolder(t, hung)
	if err := os.MkdirAll(later, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(later, "x.wav"), []byte("RIFFdata"), 0o644); err != nil {
		t.Fatal(err)
	}
	within(t, 5*time.Second, "Remount", func() { r.Remount() })
	if st := statusOf(t, r, "B"); !st.Mounted {
		t.Fatalf("B = %+v, want mounted beside the hung root", st)
	}
}

// TestResolveHonoursItsContext pins a request's own deadline: a request that
// waits on a check of a folder that is not answering gives up when its
// context does, not when the check's bound does.
func TestResolveHonoursItsContext(t *testing.T) {
	setAttemptWait(t, 5*time.Second)
	hung := filepath.Join(t.TempDir(), "hung")
	r, err := OpenRoots([]Root{{Name: "H", Path: hung}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	hungFolder(t, hung)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = r.Resolve(ctx, "H/x.wav")
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("Resolve waited %v past its context", waited)
	}
	if waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("resolve past its context = %v, want canceled", err)
	}
}
