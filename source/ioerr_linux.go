package source

import "syscall"

// platformIOFaults are the faults only Linux names: a share whose host is
// down, a removable medium that is out, and a remote I/O error.
var platformIOFaults = []syscall.Errno{syscall.EHOSTDOWN, syscall.ENOMEDIUM, syscall.EREMOTEIO}
