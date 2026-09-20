//go:build unix

package cli

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestProbeRefusesAFIFO pins the one case the extension hint used to carry
// past classification: a named pipe called foo.wav reached the WAV reader,
// whose size came from a stat that says 0 for a FIFO however much is
// queued behind it.
func TestProbeRefusesAFIFO(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pipe.wav")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	code, _, errOut := run(t, "probe", p)
	if code == 0 || !strings.Contains(errOut, "named pipe") {
		t.Errorf("fifo exit = %d, stderr %q; want a refusal naming the pipe", code, errOut)
	}
}
