//go:build unix

package container

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/colespringer/waxflow/waxerr"
)

// TestFileSourceRefusesAFIFO is the other half of the classification
// refusal. Unix rather than linux, matching the build tag on the code it
// covers: OpenNonblock is what lets the open return at all with no writer
// on the other end, and that is a unix rule, not a Linux one.
func TestFileSourceRefusesAFIFO(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pipe.wav")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	f, err := os.OpenFile(p, os.O_RDONLY|OpenNonblock, 0)
	if err != nil {
		t.Fatalf("open fifo: %v", err)
	}
	defer f.Close()
	_, err = FileSource(f)
	if waxerr.CodeOf(err) != waxerr.CodeUnsupportedSource {
		t.Fatalf("FileSource(fifo) = %v, want CodeUnsupportedSource", err)
	}
	t.Logf("fifo: %v", err)
}
