package posixfs

import (
	"io"
	"os"
)

// Create creates path exclusively for read-write (O_RDWR|O_CREATE|O_EXCL).
// On Windows the handle shares delete, so the file can be renamed or
// unlinked while it is open.
func Create(path string) (*os.File, error) { return create(path, 0o644) }

// ReadFile reads the named file whole through [Open], so on Windows the
// brief hold cannot block a concurrent [Replace] of the same path.
func ReadFile(path string) ([]byte, error) {
	f, err := Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
