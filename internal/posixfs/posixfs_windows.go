//go:build windows

package posixfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// errnoSharingViolation is ERROR_SHARING_VIOLATION. syscall exports
// ERROR_ACCESS_DENIED but not this one.
const errnoSharingViolation = syscall.Errno(32)

// The delay doubles each round: five attempts wait 5+10+20+40 = 75ms. Long
// enough for a scanner's hold on a just-written file, short enough that a
// genuine permission failure (also ERROR_ACCESS_DENIED) still reports
// within the budget.
const (
	attempts     = 5
	initialDelay = 5 * time.Millisecond
)

// Replace replaces target with tmpName. Same-directory replaces (every
// current caller) go through os.Root.Rename for FILE_RENAME_POSIX_SEMANTICS,
// which takes a target whose holders share delete and moves a source this
// process holds open through [Create]; Go itself falls back to the classic
// rename on filesystems without it. The backoff covers what POSIX semantics
// cannot: a holder without delete sharing, which blocks any rename until it
// closes. A handle held for the duration still surfaces after every attempt.
func Replace(tmpName, target string) error {
	rename := func() error { return os.Rename(tmpName, target) }
	if dir := filepath.Dir(target); filepath.Dir(tmpName) == dir {
		if root, err := os.OpenRoot(dir); err == nil {
			defer root.Close()
			rename = func() error { return root.Rename(filepath.Base(tmpName), filepath.Base(target)) }
		}
	}
	var err error
	delay := initialDelay
	for attempt := range attempts {
		if err = rename(); err == nil {
			return nil
		}
		if !retryable(err) {
			return err
		}
		if attempt < attempts-1 {
			time.Sleep(delay)
			delay *= 2
		}
	}
	return err
}

// retryable reports whether a failed rename is worth another attempt.
// Anything but a transient handle must surface at once, not after the full
// backoff.
func retryable(err error) bool {
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED) || errors.Is(err, errnoSharingViolation)
}

// Create creates path exclusively for read-write (O_RDWR|O_CREATE|O_EXCL
// semantics) with delete sharing, so the file can be renamed or unlinked
// while this handle is open.
func Create(path string) (*os.File, error) {
	return sharedFile(path, syscall.GENERIC_READ|syscall.GENERIC_WRITE, syscall.CREATE_NEW)
}

// Open opens path for reading with delete sharing, so a [Replace] or a
// removal racing this handle proceeds instead of failing until it closes.
func Open(path string) (*os.File, error) {
	return sharedFile(path, syscall.GENERIC_READ, syscall.OPEN_EXISTING)
}

func sharedFile(path string, access, createmode uint32) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(longPath(path))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := syscall.CreateFile(p, access,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, createmode, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// longPath does for the raw CreateFile above what os.OpenFile's fixLongPath
// does internally: past ~248 chars a plain path fails ERROR_PATH_NOT_FOUND
// on hosts without long-path support, and the \\?\ form (absolute and clean
// by construction via Abs) lifts the limit.
func longPath(path string) string {
	if len(path) < 248 || strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) {
		return path
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if strings.HasPrefix(abs, `\\`) {
		return `\\?\UNC\` + abs[2:]
	}
	return `\\?\` + abs
}
