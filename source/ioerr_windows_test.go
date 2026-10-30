package source

import (
	"io/fs"
	"syscall"
	"testing"

	"github.com/colespringer/waxflow/waxerr"
)

// TestOpenFaultWin32 pins the Win32 codes a failing disk, removed medium,
// dropped share or locked file reports: under a healthy root they are the
// library's fault, like EIO elsewhere, running out of handles is the daemon's,
// and the codes Go reads as missing or forbidden keep those answers.
func TestOpenFaultWin32(t *testing.T) {
	cases := []struct {
		name  string
		errno syscall.Errno
		want  waxerr.Code
	}{
		{"ERROR_NOT_READY", 21, waxerr.CodeSourceUnreadable},
		{"ERROR_CRC", 23, waxerr.CodeSourceUnreadable},
		{"ERROR_SEEK", 25, waxerr.CodeSourceUnreadable},
		{"ERROR_SECTOR_NOT_FOUND", 27, waxerr.CodeSourceUnreadable},
		{"ERROR_READ_FAULT", 30, waxerr.CodeSourceUnreadable},
		{"ERROR_GEN_FAILURE", 31, waxerr.CodeSourceUnreadable},
		{"ERROR_NETWORK_BUSY", 54, waxerr.CodeSourceUnreadable},
		{"ERROR_DEV_NOT_EXIST", 55, waxerr.CodeSourceUnreadable},
		{"ERROR_UNEXP_NET_ERR", 59, waxerr.CodeSourceUnreadable},
		{"ERROR_NETNAME_DELETED", 64, waxerr.CodeSourceUnreadable},
		{"ERROR_SEM_TIMEOUT", 121, waxerr.CodeSourceUnreadable},
		{"ERROR_DEVICE_HARDWARE_ERROR", 483, waxerr.CodeSourceUnreadable},
		{"ERROR_NO_MEDIA_IN_DRIVE", 1112, waxerr.CodeSourceUnreadable},
		{"ERROR_IO_DEVICE", 1117, waxerr.CodeSourceUnreadable},
		{"ERROR_DISK_OPERATION_FAILED", 1127, waxerr.CodeSourceUnreadable},
		{"ERROR_DEVICE_NOT_CONNECTED", 1167, waxerr.CodeSourceUnreadable},
		{"ERROR_NETWORK_UNREACHABLE", 1231, waxerr.CodeSourceUnreadable},
		{"ERROR_FILE_CORRUPT", 1392, waxerr.CodeSourceUnreadable},
		{"ERROR_DISK_CORRUPT", 1393, waxerr.CodeSourceUnreadable},
		{"ERROR_SHARING_VIOLATION", 32, waxerr.CodeSourceUnreadable},
		{"ERROR_LOCK_VIOLATION", 33, waxerr.CodeSourceUnreadable},
		{"ERROR_TOO_MANY_OPEN_FILES", 4, waxerr.CodeOverloaded},
		{"ERROR_FILE_NOT_FOUND", 2, waxerr.CodeNotFound},
		{"ERROR_PATH_NOT_FOUND", 3, waxerr.CodeNotFound},
		{"ERROR_BAD_NETPATH", 53, waxerr.CodeNotFound},
		{"ERROR_ACCESS_DENIED", 5, waxerr.CodeSourceUnreadable},
		{"ERROR_INVALID_NAME", 123, waxerr.CodeInvalidRequest},
	}
	for _, tc := range cases {
		err := &fs.PathError{Op: "openat", Path: "a.wav", Err: tc.errno}
		if got := waxerr.CodeOf(openFault(err)); got != tc.want {
			t.Errorf("%s (%d): code = %s, want %s", tc.name, tc.errno, got, tc.want)
		}
	}
}
