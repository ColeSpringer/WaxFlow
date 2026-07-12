package waxflow_test

import (
	"path/filepath"
	"runtime"
)

// repoPath joins parts onto the module root, resolved from this file's own
// on-disk location. These black-box tests live under tests/, so the working
// directory `go test` gives them is tests/, not the module root; committed
// fixtures like testdata/ and the per-container testdata dirs are addressed
// relative to the root, so they must be anchored here rather than to the CWD.
func repoPath(parts ...string) string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("repoPath: runtime.Caller failed")
	}
	root := filepath.Dir(filepath.Dir(file)) // <root>/tests -> <root>
	return filepath.Join(append([]string{root}, parts...)...)
}
