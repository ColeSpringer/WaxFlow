package source

import (
	"errors"
	"os"
	"path/filepath"
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
	f, err := r.Resolve("lib/sub/a.wav")
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
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("sub", "a.wav"), filepath.Join(dir, "inside")); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		ref  string
		code waxerr.Code
	}{
		{"", waxerr.CodeInvalidRequest},
		{"lib", waxerr.CodeInvalidRequest},
		{"lib/", waxerr.CodeInvalidRequest},
		{"nope/a.wav", waxerr.CodeNotFound},
		{"lib/missing.wav", waxerr.CodeNotFound},
		{"lib/sub", waxerr.CodeUnsupportedSource}, // directory
		{"lib/../a.wav", waxerr.CodeInvalidRequest},
		{"lib/escape", waxerr.CodeInvalidRequest}, // symlink out of the root
		{"upload:abc", waxerr.CodeUnsupportedSource},
		{"pid:01ARZ3NDEKTSV4RRFFQ69G5FAV", waxerr.CodeUnsupportedSource},
		{"weird:thing", waxerr.CodeUnsupportedSource},
	}
	for _, tc := range cases {
		f, err := r.Resolve(tc.ref)
		if err == nil {
			f.Close()
			t.Errorf("Resolve(%q) succeeded, want %s", tc.ref, tc.code)
			continue
		}
		if got := waxerr.CodeOf(err); got != tc.code {
			t.Errorf("Resolve(%q) code = %s, want %s (%v)", tc.ref, got, tc.code, err)
		}
	}

	// A colon after the first slash is a filename, not a scheme.
	if err := os.WriteFile(filepath.Join(dir, "sub", "b:c.wav"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := r.Resolve("lib/sub/b:c.wav")
	if err != nil {
		t.Fatalf("colon-in-path ref failed: %v", err)
	}
	f.Close()

	// Within-root symlinks stay allowed (in-place libraries use them).
	f, err = r.Resolve("lib/inside")
	if err != nil {
		t.Fatalf("within-root symlink failed: %v", err)
	}
	f.Close()
}

func TestResolveSizeCap(t *testing.T) {
	r, _ := newRoots(t, 4)
	_, err := r.Resolve("lib/sub/a.wav") // 8 bytes > 4-byte cap
	if got := waxerr.CodeOf(err); got != waxerr.CodePayloadTooLarge {
		t.Fatalf("code = %s (%v), want payload-too-large", got, err)
	}
}

func TestOpenRootsValidation(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"", "a/b", "a:b"} {
		if _, err := OpenRoots([]Root{{Name: name, Path: dir}}, 0); err == nil {
			t.Errorf("root name %q accepted", name)
		}
	}
	if _, err := OpenRoots([]Root{{Name: "a", Path: dir}, {Name: "a", Path: dir}}, 0); err == nil {
		t.Error("duplicate root name accepted")
	}
	if _, err := OpenRoots([]Root{{Name: "a", Path: filepath.Join(dir, "missing")}}, 0); err == nil {
		t.Error("missing root path accepted")
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
