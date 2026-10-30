package source

import (
	"io/fs"
	"syscall"
	"testing"

	"github.com/colespringer/waxflow/waxerr"
)

// TestOpenFaultLinux pins the faults only Linux names: a share whose host is
// down, a removable medium that is out, and a remote I/O error.
func TestOpenFaultLinux(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EHOSTDOWN, syscall.ENOMEDIUM, syscall.EREMOTEIO} {
		err := &fs.PathError{Op: "openat", Path: "a.wav", Err: errno}
		if got := waxerr.CodeOf(openFault(err)); got != waxerr.CodeSourceUnreadable {
			t.Errorf("%v: code = %s, want source-unreadable", errno, got)
		}
	}
}
