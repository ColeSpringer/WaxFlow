//go:build !windows

package posixfs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestStageNewWithoutHardLinks: where the filesystem refuses a hard link, a
// new file still publishes, and still refuses a target that appeared. Not
// parallel: it swaps link.
func TestStageNewWithoutHardLinks(t *testing.T) {
	link = func(oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EPERM}
	}
	defer func() { link = os.Link }()

	dir := t.TempDir()
	target := filepath.Join(dir, "out.flac")
	st, err := Stage(target, 0o644, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.File.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := st.Commit(); err != nil {
		t.Fatalf("Commit without hard links: %v", err)
	}
	mustRead(t, target, "new")
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the directory holds %d files, want the one output", len(entries))
	}

	other := filepath.Join(dir, "other.flac")
	st, err = Stage(other, 0o644, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("theirs"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantExistsRefusal(t, st.Commit(), other)
	mustRead(t, other, "theirs")
	if temps := tempsIn(t, dir); len(temps) != 0 {
		t.Errorf("the refused Commit left %v behind", temps)
	}
}
