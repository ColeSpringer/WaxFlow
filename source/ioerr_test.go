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
