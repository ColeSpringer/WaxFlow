//go:build unix

package container

import "syscall"

// OpenNonblock is the open flag that keeps a FIFO from blocking the caller
// until a writer arrives, so CheckRegular gets to refuse it instead.
// Harmless on regular files.
const OpenNonblock = syscall.O_NONBLOCK
