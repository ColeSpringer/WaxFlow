package container

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/waxerr"
)

// TestFileSourceRefusesWhatIsNotAFile pins the classification refusal. A
// directory and a FIFO both stat to a size that is not a byte count any
// reader can use, and before this the sniffer read a head that was not the
// file's: a FIFO named foo.wav reached the WAV reader on its extension
// hint alone.
func TestFileSourceRefusesWhatIsNotAFile(t *testing.T) {
	dir := t.TempDir()
	d, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := FileSource(d); waxerr.CodeOf(err) != waxerr.CodeUnsupportedSource {
		t.Errorf("FileSource(directory) = %v, want CodeUnsupportedSource", err)
	} else {
		t.Logf("directory: %v", err)
	}

	// A regular file still passes, including an empty one: emptiness is
	// the format layer's call, not this one's.
	p := filepath.Join(dir, "empty.bin")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	src, err := FileSource(f)
	if err != nil {
		t.Fatalf("FileSource(empty regular file): %v", err)
	}
	if src.Size() != 0 {
		t.Errorf("Size = %d, want 0", src.Size())
	}
}
