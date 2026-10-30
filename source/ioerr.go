//go:build !plan9 && !windows

package source

import (
	"errors"
	"slices"
	"syscall"
)

// ioFaults are the errnos of a device or network failure under a healthy
// root: the ones every Unix names, then the platform's own.
var ioFaults = append([]syscall.Errno{
	syscall.EIO, syscall.ESTALE, syscall.ETIMEDOUT, syscall.EHOSTUNREACH,
	syscall.ENETDOWN, syscall.ENETUNREACH, syscall.ECONNRESET,
	syscall.ECONNABORTED, syscall.ENOTCONN, syscall.ENXIO, syscall.ENODEV,
}, platformIOFaults...)

// isIOFault reports a device or network failure under a healthy root.
func isIOFault(err error) bool {
	errno, ok := errors.AsType[syscall.Errno](err)
	return ok && slices.Contains(ioFaults, errno)
}

// isOutOfHandles reports the daemon or the system out of file descriptors.
func isOutOfHandles(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE)
}
