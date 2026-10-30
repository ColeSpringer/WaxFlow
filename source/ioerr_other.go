//go:build !linux && !plan9 && !windows

package source

import "syscall"

// platformIOFaults is empty: this platform's faults are the common ones.
var platformIOFaults []syscall.Errno
