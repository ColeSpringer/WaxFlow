package posixfs

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// WriteFile writes data to path all or nothing: into a uniquely named temp
// beside it, then [Replace]. A failure removes the temp. Nothing is synced.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	st, err := Stage(path, perm, true)
	if err != nil {
		return err
	}
	return writeStaged(st, data)
}

func writeStaged(st *Staged, data []byte) error {
	if _, err := st.File.Write(data); err != nil {
		st.Abort()
		return err
	}
	return st.Commit()
}

const tempMark = ".tmp-"

// IsTemp reports whether name is one a file is written under before it is
// published: a ".tmp" suffix, or ".tmp-" and hex digits to the end. No
// published name matches, so a sweep may remove what this reports.
func IsTemp(name string) bool {
	if strings.HasSuffix(name, ".tmp") {
		return true
	}
	i := strings.LastIndex(name, tempMark)
	if i < 0 {
		return false
	}
	tail := name[i+len(tempMark):]
	return tail != "" && strings.Trim(tail, "0123456789abcdef") == ""
}

// SweepTemps removes the regular files in dir that [IsTemp] matches, and
// never a directory. It is for a store's open, when no write is in flight.
func SweepTemps(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Type().IsRegular() && IsTemp(e.Name()) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// Staged is a file on its way to being published. See [Stage].
type Staged struct {
	// File is open for reading and writing.
	File *os.File
	// Path is where the file is: the temp beside the target until a
	// successful Commit, and the target from then on. Read it after Commit
	// returns.
	Path string

	target  string // where Commit publishes Path
	replace bool   // whether Commit may replace a file at target

	mu   sync.Mutex // held across Commit and across Abort
	done bool
}

// Stage opens a uniquely named temp beside path, which Commit publishes
// there, so path only ever holds a complete file. With replace true Commit
// moves the temp over path: the published file takes perm, not the old
// target's mode, and a symlink at path is replaced, not written through.
// With replace false an existing path is refused here, with an error
// wrapping [os.ErrExist], and Commit never replaces one. perm is ignored on
// Windows, where reading the staged file back by name goes through [Open],
// or the handle blocks Commit and Abort.
func Stage(path string, perm os.FileMode, replace bool) (*Staged, error) {
	if !replace {
		if err := CheckAbsent(path); err != nil {
			return nil, err
		}
	}
	st := &Staged{
		Path:    fmt.Sprintf("%s%s%016x", path, tempMark, rand.Uint64()),
		target:  path,
		replace: replace,
	}
	f, err := create(st.Path, perm)
	if err != nil {
		return nil, err
	}
	st.File = f
	return st, nil
}

// CheckAbsent returns nil when nothing is at path, the error Stage refuses
// an existing path with when something is, or the failure to look.
func CheckAbsent(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return errExists(path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// errExists is the refusal of a target that exists, at Stage or at a Commit
// that must not replace it, so both read alike and name no temp.
func errExists(target string) error {
	return &os.PathError{Op: "stage", Path: target, Err: os.ErrExist}
}

// Commit closes File, if the caller has not, and publishes the temp at its
// target: over it with replace, else only where nothing is, failing with an
// error wrapping [os.ErrExist] on a target that appeared since Stage. A
// failure removes the temp. The first of Commit and Abort wins, from any
// goroutine: a Commit after either fails, and returns only once the
// winner's outcome is on disk.
func (st *Staged) Commit() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.done {
		return os.ErrClosed
	}
	st.done = true
	if err := st.File.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		remove(st.Path)
		return err
	}
	publish := publishNew
	if st.replace {
		publish = Replace
	}
	if err := publish(st.Path, st.target); err != nil {
		remove(st.Path)
		return err
	}
	st.Path = st.target
	return nil
}

// Abort closes File and removes what was written. It tolerates a File the
// caller already closed. After a Commit or an Abort it does nothing, and
// returns only once that one's outcome is on disk.
func (st *Staged) Abort() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.done {
		return
	}
	st.done = true
	st.File.Close()
	remove(st.Path)
}
