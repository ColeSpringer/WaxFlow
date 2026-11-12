//go:build !windows

package source

// nameable reports whether the system can hold name as a file name's bytes.
// Only Windows, whose names are UTF-16, cannot hold some.
func nameable(string) bool { return true }
