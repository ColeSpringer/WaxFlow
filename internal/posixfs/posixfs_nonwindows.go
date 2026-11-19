//go:build !windows

package posixfs

import "os"

// Replace atomically replaces target with tmpName. See the package doc for
// what the Windows build adds.
func Replace(tmpName, target string) error { return os.Rename(tmpName, target) }

func create(path string, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
}

func remove(path string) { os.Remove(path) }

// Open opens path for reading.
func Open(path string) (*os.File, error) { return os.Open(path) }
