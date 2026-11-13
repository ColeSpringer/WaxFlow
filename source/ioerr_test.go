//go:build !plan9 && !windows

package source

import (
	"io/fs"
	"syscall"
	"testing"

	"github.com/colespringer/waxflow/waxerr"
)

// TestOpenFaultIO pins that a device or network fault under a healthy root is
// the library's, not the request's, and running out of file descriptors the
// daemon's, so neither is a 400.
func TestOpenFaultIO(t *testing.T) {
	cases := []struct {
		errno syscall.Errno
		want  waxerr.Code
	}{
		{syscall.EIO, waxerr.CodeSourceUnreadable},
		{syscall.ESTALE, waxerr.CodeSourceUnreadable},
		{syscall.ETIMEDOUT, waxerr.CodeSourceUnreadable},
		{syscall.EHOSTUNREACH, waxerr.CodeSourceUnreadable},
		{syscall.ENETDOWN, waxerr.CodeSourceUnreadable},
		{syscall.ENETUNREACH, waxerr.CodeSourceUnreadable},
		{syscall.ECONNRESET, waxerr.CodeSourceUnreadable},
		{syscall.ECONNABORTED, waxerr.CodeSourceUnreadable},
		{syscall.ENOTCONN, waxerr.CodeSourceUnreadable},
		{syscall.ENXIO, waxerr.CodeSourceUnreadable},
		{syscall.ENODEV, waxerr.CodeSourceUnreadable},
		{syscall.EMFILE, waxerr.CodeOverloaded},
		{syscall.ENFILE, waxerr.CodeOverloaded},
	}
	for _, tc := range cases {
		err := &fs.PathError{Op: "openat", Path: "a.wav", Err: tc.errno}
		if got := waxerr.CodeOf(openFault(err)); got != tc.want {
			t.Errorf("%v: code = %s, want %s", tc.errno, got, tc.want)
		}
	}
}

// TestLocalFaultIO: a trusted local path fails as a root's file does. A
// device fault is the file's, running out of descriptors is the daemon's
// and worth a retry, and an errno nothing here names is still not a
// missing file.
func TestLocalFaultIO(t *testing.T) {
	for _, tc := range []struct {
		errno syscall.Errno
		want  waxerr.Code
	}{
		{syscall.EIO, waxerr.CodeSourceUnreadable},
		{syscall.ESTALE, waxerr.CodeSourceUnreadable},
		{syscall.EMFILE, waxerr.CodeOverloaded},
		{syscall.ENFILE, waxerr.CodeOverloaded},
		// A path that runs through a file names nothing, as a missing one does.
		{syscall.ENOTDIR, waxerr.CodeNotFound},
		{syscall.ENAMETOOLONG, waxerr.CodeSourceUnreadable},
		{syscall.ELOOP, waxerr.CodeSourceUnreadable},
	} {
		err := &fs.PathError{Op: "open", Path: "spool/01J", Err: tc.errno}
		if got := waxerr.CodeOf(localFault(err)); got != tc.want {
			t.Errorf("%v: code = %s, want %s", tc.errno, got, tc.want)
		}
	}
}
