package source

import (
	"errors"
	"syscall"
)

// isIOFault reports a device failure under a healthy root. Plan 9 has no
// ESTALE.
func isIOFault(err error) bool {
	return errors.Is(err, syscall.EIO)
}

// isOutOfHandles reports the daemon out of file descriptors.
func isOutOfHandles(err error) bool {
	return errors.Is(err, syscall.EMFILE)
}
