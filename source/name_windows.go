package source

import "syscall"

// nameable reports whether name survives the trip to UTF-16 and back, which
// is whether Windows can hold it as a file name's bytes. A byte that is no
// part of a character or of a lone surrogate's three becomes U+FFFD on the
// way, and the open would land on another file.
func nameable(name string) bool {
	u, err := syscall.UTF16FromString(name)
	if err != nil {
		return true // a NUL, which the open refuses for itself
	}
	return syscall.UTF16ToString(u) == name
}
