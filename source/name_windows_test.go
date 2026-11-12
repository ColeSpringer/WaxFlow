package source

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/waxerr"
)

// TestResolveRefusesANameWindowsCannotHold: a byte that is no part of a
// character or a lone surrogate becomes U+FFFD on the way to UTF-16, so the
// open would land on another file. Here that file exists.
func TestResolveRefusesANameWindowsCannotHold(t *testing.T) {
	r, dir := newRoots(t, 0)
	if err := os.WriteFile(filepath.Join(dir, "sub", "caf�.wav"), []byte("RIFFother"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := r.Resolve(context.Background(), "lib/sub/caf\xe9.wav")
	if err == nil {
		defer f.Close()
		t.Fatalf("resolved %q for a name no Windows file has", readAll(t, f))
	}
	wantCode(t, err, waxerr.CodeNotFound)

	// A lone surrogate is a name Windows holds, spelled as three bytes.
	if err := os.WriteFile(filepath.Join(dir, "sub", "caf\xed\xa0\x80.wav"), []byte("RIFFlone"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err = r.Resolve(context.Background(), "lib/sub/caf\xed\xa0\x80.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := readAll(t, f); got != "RIFFlone" {
		t.Errorf("ReadAt = %q, want the lone-surrogate file's bytes", got)
	}
}

// TestOpenRootsRefusesAPathWindowsCannotHold: the same substitution would
// mount another folder.
func TestOpenRootsRefusesAPathWindowsCannotHold(t *testing.T) {
	_, err := OpenRoots([]Root{{Name: "lib", Path: filepath.Join(t.TempDir(), "caf\xe9")}}, 0)
	wantCode(t, err, waxerr.CodeInvalidRequest)
}
