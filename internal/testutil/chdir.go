package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// Chdir changes into dir for the rest of the test and restores by path.
// t.Chdir restores through a directory handle, which fails on Windows when
// the test starts on a network share.
func Chdir(t testing.TB, dir string) {
	t.Helper()
	back, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	// PWD keeps Getwd on this spelling as t.Chdir does; t.Setenv also
	// refuses a parallel test, which a change of directory would race.
	t.Setenv("PWD", abs)
	if err := os.Chdir(abs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(back); err != nil {
			t.Errorf("restoring the working directory: %v", err)
		}
	})
}

// startDir is where the test binary started, which go test makes the
// package's directory.
var startDir = func() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return wd
}()

// PkgFile joins elem under the package's directory, so a fixture path
// resolves the same after Chdir.
func PkgFile(elem ...string) string {
	return filepath.Join(append([]string{startDir}, elem...)...)
}
