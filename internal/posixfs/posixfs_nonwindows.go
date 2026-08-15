//go:build !windows

package posixfs

import "os"

// Replace atomically replaces target with tmpName. See the package doc for
// what the Windows build adds.
func Replace(tmpName, target string) error { return os.Rename(tmpName, target) }

// Create creates path exclusively for read-write (O_RDWR|O_CREATE|O_EXCL).
func Create(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
}

// Open opens path for reading.
func Open(path string) (*os.File, error) { return os.Open(path) }
