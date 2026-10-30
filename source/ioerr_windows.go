package source

import (
	"errors"
	"slices"
	"syscall"
)

// win32IOFaults are the Win32 codes a failing disk, a removed medium, a
// dropped network share or a file another program holds locked reports;
// Windows has no EIO to read them by.
var win32IOFaults = []syscall.Errno{
	21,   // ERROR_NOT_READY
	23,   // ERROR_CRC
	25,   // ERROR_SEEK
	27,   // ERROR_SECTOR_NOT_FOUND
	30,   // ERROR_READ_FAULT
	31,   // ERROR_GEN_FAILURE
	32,   // ERROR_SHARING_VIOLATION
	33,   // ERROR_LOCK_VIOLATION
	54,   // ERROR_NETWORK_BUSY
	55,   // ERROR_DEV_NOT_EXIST
	59,   // ERROR_UNEXP_NET_ERR
	64,   // ERROR_NETNAME_DELETED
	121,  // ERROR_SEM_TIMEOUT
	483,  // ERROR_DEVICE_HARDWARE_ERROR
	1112, // ERROR_NO_MEDIA_IN_DRIVE
	1117, // ERROR_IO_DEVICE
	1127, // ERROR_DISK_OPERATION_FAILED
	1167, // ERROR_DEVICE_NOT_CONNECTED
	1231, // ERROR_NETWORK_UNREACHABLE
	1392, // ERROR_FILE_CORRUPT
	1393, // ERROR_DISK_CORRUPT
}

// isIOFault reports a device or network failure under a healthy root.
func isIOFault(err error) bool {
	errno, ok := errors.AsType[syscall.Errno](err)
	return ok && slices.Contains(win32IOFaults, errno)
}

// isOutOfHandles reports the daemon out of handles (ERROR_TOO_MANY_OPEN_FILES).
func isOutOfHandles(err error) bool {
	return errors.Is(err, syscall.Errno(4))
}
