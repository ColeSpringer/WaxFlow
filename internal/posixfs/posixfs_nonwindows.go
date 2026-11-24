//go:build !windows

package posixfs

import (
	"errors"
	"io/fs"
	"os"
)

// Replace atomically replaces target with tmpName. See the package doc for
// what the Windows build adds.
func Replace(tmpName, target string) error { return os.Rename(tmpName, target) }

// link is os.Link, a variable so a test can play a filesystem without hard
// links.
var link = os.Link

// publishNew moves tmpName to target unless something is there. A hard link
// fails EEXIST on an existing target, atomically. Where the filesystem
// refuses hard links it checks the target is absent, then renames, which a
// file created between the two would lose to.
func publishNew(tmpName, target string) error {
	err := link(tmpName, target)
	if err == nil {
		// A kill or a failed remove leaves the temp as a second link to the
		// published file: harmless, and IsTemp names it.
		remove(tmpName)
		return nil
	}
	if errors.Is(err, fs.ErrExist) {
		return errExists(target)
	}
	// Where links do work, the rename reports the same failure.
	if _, err := os.Lstat(target); err == nil {
		return errExists(target)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Rename(tmpName, target)
}

func create(path string, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
}

func remove(path string) { os.Remove(path) }

// Open opens path for reading.
func Open(path string) (*os.File, error) { return os.Open(path) }
