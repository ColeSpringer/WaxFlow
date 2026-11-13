package source

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/waxerr"
)

func newRoots(t *testing.T, maxBytes int64) (*Roots, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "a.wav"), []byte("RIFFdata"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRoots([]Root{{Name: "lib", Path: dir}}, maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, dir
}

func TestResolve(t *testing.T) {
	r, dir := newRoots(t, 0)
	f, err := r.Resolve(context.Background(), "lib/sub/a.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	fi, err := os.Stat(filepath.Join(dir, "sub", "a.wav"))
	if err != nil {
		t.Fatal(err)
	}
	if f.ID.Size != fi.Size() || f.ID.MtimeNS != fi.ModTime().UnixNano() {
		t.Errorf("identity %+v does not match stat (%d, %d)", f.ID, fi.Size(), fi.ModTime().UnixNano())
	}
	if f.Ext != "wav" {
		t.Errorf("ext = %q, want wav", f.Ext)
	}
	if f.Size() != int64(len("RIFFdata")) {
		t.Errorf("Size() = %d", f.Size())
	}
	head := make([]byte, 4)
	if _, err := f.ReadAt(head, 0); err != nil || string(head) != "RIFF" {
		t.Errorf("ReadAt got %q, %v", head, err)
	}
}

func TestResolveErrors(t *testing.T) {
	r, dir := newRoots(t, 0)

	cases := []struct {
		ref  string
		code waxerr.Code
	}{
		{"", waxerr.CodeInvalidRequest},
		{"lib", waxerr.CodeInvalidRequest},
		{"lib/", waxerr.CodeInvalidRequest},
		{"/lib/a.wav", waxerr.CodeInvalidRequest},
		{"nope/a.wav", waxerr.CodeNotFound},
		{"lib/missing.wav", waxerr.CodeNotFound},
		{"lib/sub", waxerr.CodeUnsupportedSource}, // directory
		{"lib/../a.wav", waxerr.CodeInvalidRequest},
		{"upload:abc", waxerr.CodeUnsupportedSource},
		{"pid:01ARZ3NDEKTSV4RRFFQ69G5FAV", waxerr.CodeUnsupportedSource},
		{"weird:thing", waxerr.CodeUnsupportedSource},
	}
	for _, tc := range cases {
		f, err := r.Resolve(context.Background(), tc.ref)
		if err == nil {
			f.Close()
			t.Errorf("Resolve(%q) succeeded, want %s", tc.ref, tc.code)
			continue
		}
		if got := waxerr.CodeOf(err); got != tc.code {
			t.Errorf("Resolve(%q) code = %s, want %s (%v)", tc.ref, got, tc.code, err)
		}
	}

	// A colon after the first slash is a filename, not a scheme. Windows has
	// no such filename (a colon there opens an alternate data stream, and
	// os.Root refuses the path outright), so there the claim is checked as
	// far as it goes: the ref reached the filesystem, which is what the
	// openat PathError under the code proves. A ref turned away while it was
	// still being parsed as a scheme would carry no such error.
	if runtime.GOOS == "windows" {
		f, err := r.Resolve(context.Background(), "lib/sub/b:c.wav")
		var pathErr *fs.PathError
		switch {
		case err == nil:
			f.Close()
			t.Error("Resolve of a colon-bearing name succeeded")
		case !errors.As(err, &pathErr):
			t.Errorf("colon-in-path ref = %v, want the filesystem's own refusal", err)
		}
	} else {
		if err := os.WriteFile(filepath.Join(dir, "sub", "b:c.wav"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := r.Resolve(context.Background(), "lib/sub/b:c.wav")
		if err != nil {
			t.Fatalf("colon-in-path ref failed: %v", err)
		}
		f.Close()
	}
}

// TestResolveSymlinks pins os.Root's confinement as Resolve answers it: a
// symlink out of the root is refused like any escape, and one that stays
// inside serves (in-place libraries use them).
func TestResolveSymlinks(t *testing.T) {
	ctx := context.Background()
	r, dir := newRoots(t, 0)
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "escape")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("creating a symlink needs a privilege this Windows account lacks: %v", err)
		}
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("sub", "a.wav"), filepath.Join(dir, "inside")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(ctx, "lib/escape"); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Errorf("symlink out of the root = %v, want invalid-request", err)
	}
	f, err := r.Resolve(ctx, "lib/inside")
	if err != nil {
		t.Fatalf("within-root symlink failed: %v", err)
	}
	f.Close()
}

func TestResolveSizeCap(t *testing.T) {
	r, _ := newRoots(t, 4)
	_, err := r.Resolve(context.Background(), "lib/sub/a.wav") // 8 bytes > 4-byte cap
	if got := waxerr.CodeOf(err); got != waxerr.CodePayloadTooLarge {
		t.Fatalf("code = %s (%v), want payload-too-large", got, err)
	}
}

func TestOpenRootsValidation(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"", "a/b", "a:b"} {
		if _, err := OpenRoots([]Root{{Name: name, Path: dir}}, 0); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
			t.Errorf("root name %q = %v, want invalid-request", name, err)
		}
	}
	if _, err := OpenRoots([]Root{{Name: "a", Path: dir}, {Name: "a", Path: dir}}, 0); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Errorf("duplicate root name = %v, want invalid-request", err)
	}
	if _, err := OpenRoots([]Root{{Name: "a", Path: ""}}, 0); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Errorf("empty root path = %v, want invalid-request", err)
	}
}

// TestOpenRootsMountsWhatItCan pins the tolerant open: a root whose path does
// not open is kept unmounted with its error, and the others serve.
func TestOpenRootsMountsWhatItCan(t *testing.T) {
	lib := rootDir(t, map[string]string{"a.wav": "RIFFdata"})
	missing := filepath.Join(t.TempDir(), "missing")
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRoots([]Root{{Name: "lib", Path: lib}, {Name: "gone", Path: missing}, {Name: "file", Path: file}}, 0)
	if err != nil {
		t.Fatalf("OpenRoots with unopenable paths = %v, want success", err)
	}
	defer r.Close()

	st := r.Status()
	if len(st) != 3 || st[0].Name != "lib" || st[1].Name != "gone" || st[2].Name != "file" {
		t.Fatalf("Status() = %+v, want lib, gone, file in configuration order", st)
	}
	if !st[0].Mounted || st[0].Err != nil || st[0].Path != lib {
		t.Errorf("lib = %+v, want mounted at %s", st[0], lib)
	}
	if st[1].Mounted || st[1].Path != missing || !errors.Is(st[1].Err, fs.ErrNotExist) {
		t.Errorf("gone = %+v, want unmounted with a not-exist error", st[1])
	}
	if st[2].Mounted || st[2].Err == nil || errors.Is(st[2].Err, fs.ErrNotExist) {
		t.Errorf("file = %+v, want unmounted with an error other than not-exist", st[2])
	}
	for _, s := range st {
		if s.Since.IsZero() {
			t.Errorf("%s has a zero Since", s.Name)
		}
	}
	f, err := r.Resolve(context.Background(), "lib/a.wav")
	if err != nil {
		t.Fatalf("lib resolve beside unmounted roots = %v", err)
	}
	f.Close()
}

// TestResolveUnmountedIsRootUnavailable pins the answer for a configured root
// that will not open: root-unavailable, naming the root but not its path (a
// signed playback URL hands this message to a client), with the cause kept.
func TestResolveUnmountedIsRootUnavailable(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRoots([]Root{{Name: "gone", Path: filepath.Join(base, "missing")}, {Name: "file", Path: file}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	for _, tc := range []struct {
		name     string
		notExist bool
	}{{"gone", true}, {"file", false}} {
		_, err := r.Resolve(context.Background(), tc.name+"/a.wav")
		if got := waxerr.CodeOf(err); got != waxerr.CodeRootUnavailable {
			t.Errorf("%s: code = %s (%v), want root-unavailable", tc.name, got, err)
			continue
		}
		if !strings.Contains(err.Error(), strconv.Quote(tc.name)) || strings.Contains(err.Error(), base) {
			t.Errorf("%s: message %q should name the root and not its path", tc.name, err)
		}
		if errors.Is(err, fs.ErrNotExist) != tc.notExist {
			t.Errorf("%s: errors.Is(err, fs.ErrNotExist) = %v, want %v", tc.name, !tc.notExist, tc.notExist)
		}
	}
}

// TestOpenFault pins how an open error on a healthy root is coded; each
// platform's I/O faults have their own test beside its classifier.
func TestOpenFault(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want waxerr.Code
	}{
		{"not exist", &fs.PathError{Op: "openat", Path: "a.wav", Err: fs.ErrNotExist}, waxerr.CodeNotFound},
		{"permission", &fs.PathError{Op: "openat", Path: "a.wav", Err: fs.ErrPermission}, waxerr.CodeSourceUnreadable},
		{"escape", &fs.PathError{Op: "openat", Path: "../a.wav", Err: errors.New("path escapes from parent")}, waxerr.CodeInvalidRequest},
	}
	for _, tc := range cases {
		if got := waxerr.CodeOf(openFault(tc.err)); got != tc.want {
			t.Errorf("%s: code = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestIdentityRoundTrip(t *testing.T) {
	id := Identity{Size: 12345, MtimeNS: -7} // pre-1970 mtimes exist in the wild
	got, err := ParseIdentity(id.String())
	if err != nil || got != id {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	for _, s := range []string{"", "12", "a-b", "-1-5", "3-x"} {
		if _, err := ParseIdentity(s); err == nil {
			t.Errorf("ParseIdentity(%q) accepted", s)
		}
	}
	if !errors.Is(func() error { _, err := ParseIdentity("zz"); return err }(), waxerr.ErrInvalidRequest) {
		t.Error("parse errors must carry invalid-request")
	}
}

func TestNamesOrder(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRoots([]Root{{Name: "b", Path: dir}, {Name: "a", Path: dir}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := strings.Join(r.Names(), ","); got != "b,a" {
		t.Fatalf("Names() = %s, want configuration order b,a", got)
	}
}

// TestLocalFaultClassifies: a file that is missing is not-found, and one
// that is there and cannot be opened is not. OpenLocal called every open
// failure not-found, so an upload the daemon could not read answered 404 as
// if it had been deleted.
func TestLocalFaultClassifies(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code waxerr.Code
		msg  string
	}{
		{"missing", fs.ErrNotExist, waxerr.CodeNotFound, "source: no such file"},
		{"permission", fs.ErrPermission, waxerr.CodeSourceUnreadable, "source: permission denied"},
		{"anything else", errors.New("an error nothing names"), waxerr.CodeSourceUnreadable, "source: opening the file"},
	} {
		in := &fs.PathError{Op: "open", Path: "spool/01J", Err: tc.err}
		got := localFault(in)
		if waxerr.CodeOf(got) != tc.code {
			t.Errorf("%s: code = %s, want %s", tc.name, waxerr.CodeOf(got), tc.code)
		}
		if !strings.HasPrefix(got.Error(), tc.msg+": ") {
			t.Errorf("%s: message = %q, want it to start %q", tc.name, got, tc.msg)
		}
		if !errors.Is(got, tc.err) {
			t.Errorf("%s: the cause is not kept: %v", tc.name, got)
		}
	}
}

// TestOpenLocalMissing: a path that names nothing is not-found, whether its
// last element is missing or an earlier one is a file.
func TestOpenLocalMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.flac"), []byte("fLaC"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(dir, "absent"),
		filepath.Join(dir, "absent", "x.flac"),
		filepath.Join(dir, "a.flac", "x.flac"),
	} {
		_, err := OpenLocal("pid:1", path, path)
		if waxerr.CodeOf(err) != waxerr.CodeNotFound {
			t.Errorf("OpenLocal(%s) = %v, want not-found", path, err)
		}
	}
}
