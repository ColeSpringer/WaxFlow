//go:build unix

package source

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"

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
