//go:build unix

package source

import "syscall"

// openNonblock keeps Root.OpenFile from blocking on a FIFO before the
// regular-file check can reject it. Harmless on regular files.
const openNonblock = syscall.O_NONBLOCK
