package posixfs

import "io"

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
