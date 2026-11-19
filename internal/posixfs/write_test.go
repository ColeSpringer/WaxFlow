package posixfs

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// tempsIn lists the names under dir that IsTemp matches.
func tempsIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var temps []string
	for _, e := range entries {
		if IsTemp(e.Name()) {
			temps = append(temps, e.Name())
		}
	}
	return temps
}

func mustRead(t *testing.T, path, want string) {
	t.Helper()
	got, err := ReadFile(path)
	if err != nil || string(got) != want {
		t.Errorf("%s holds %q (%v), want %q", filepath.Base(path), got, err, want)
	}
}

func TestIsTempMatchesEveryTempAndNoPublishedName(t *testing.T) {
	t.Parallel()
	const (
		ulid   = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
		idxKey = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
		digest = "su59a_C8eJDxfNfUJF919WWypFDpldfuSypsPjdmqXY"
	)
	for _, tc := range []struct {
		name string
		want bool
	}{
		// What WriteFile and Stage write.
		{"meta.json.tmp-0123456789abcdef", true},
		{"seg-4.m4s.tmp-00000000000000ff", true},
		// The fixed names older builds wrote.
		{"job.json.tmp", true},
		{digest + ".json.tmp", true},
		{idxKey + ".idx.tmp", true},
		{ulid + ".json.tmp", true},
		{"meta.json.tmp-12", true},
		{"media.m3u8.tmp-7", true},
		// The streamed bodies, and the CLI's staged output.
		{ulid + ".tmp", true},
		{"out.flac.tmp-311", true},
		{"album.flac.tmp-40213", true},
		// The tagging library's, left beside a job's output by a post-pass
		// that died.
		{".waxlabel-1234567.tmp", true},

		// Every name a store publishes.
		{"meta.json", false},
		{"out.flac", false},
		{"out.m4a", false},
		{"out.3.flac", false},
		{"init.mp4", false},
		{"seg-0.m4s", false},
		{"media.m3u8", false},
		{idxKey + ".idx", false},
		{ulid, false},
		{ulid + ".json", false},
		{"job.json", false},
		{"silence.json", false},
		{digest + ".json", false},

		// Near misses: the suffix is hex digits and nothing else.
		{"", false},
		{"tmp", false},
		{"out.tmp-", false},
		{"out.tmp-12.flac", false},
		{"out.tmp-xyz", false},
		{"out.tmp-AB", false},
		{"out.tmpl", false},
		{"out.tmp.flac", false},
	} {
		if got := IsTemp(tc.name); got != tc.want {
			t.Errorf("IsTemp(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWriteFileLeavesNoTempOnFailure(t *testing.T) {
	t.Parallel()
	t.Run("the replace fails", func(t *testing.T) {
		dir := t.TempDir()
		// A file cannot replace a directory, least of all a full one.
		target := filepath.Join(dir, "out")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		kept := filepath.Join(target, "kept")
		if err := os.WriteFile(kept, []byte("kept"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := WriteFile(target, []byte("payload"), 0o644); err == nil {
			t.Fatal("WriteFile over a non-empty directory = nil, want an error")
		}
		if temps := tempsIn(t, dir); len(temps) != 0 {
			t.Errorf("the failed write left %v behind", temps)
		}
		mustRead(t, kept, "kept")
	})
	t.Run("the write fails", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "out")
		st, err := Stage(target, 0o644, true)
		if err != nil {
			t.Fatal(err)
		}
		// A closed file refuses the write, as a full disk would.
		st.File.Close()
		if err := writeStaged(st, []byte("payload")); err == nil {
			t.Fatal("a failed write = nil, want an error")
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("the failed write left %d files behind", len(entries))
		}
	})
	t.Run("the directory is missing", func(t *testing.T) {
		dir := t.TempDir()
		if err := WriteFile(filepath.Join(dir, "gone", "out"), []byte("payload"), 0o644); err == nil {
			t.Fatal("WriteFile into a missing directory = nil, want an error")
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("the failed write left %d files behind", len(entries))
		}
	})
}

// Two writers and a reader share one path. A fixed temp name would let the
// writers tear each other's temp; a write in place would show the reader a
// prefix.
func TestWriteFileIsAllOrNothing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "blob")
	payloads := [2][]byte{
		bytes.Repeat([]byte("a"), 1<<20),
		bytes.Repeat([]byte("b"), 1<<20+4096),
	}
	if err := WriteFile(path, payloads[0], 0o644); err != nil {
		t.Fatal(err)
	}

	// The writers keep going until the reader has read often enough to
	// have met them, and stop early only if it gave up.
	const minWrites, minReads = 40, 20
	var reads atomic.Int64
	stop := make(chan struct{})
	gaveUp := make(chan struct{})
	var reader sync.WaitGroup
	reader.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := ReadFile(path)
			if err != nil {
				t.Errorf("ReadFile during a write: %v", err)
				close(gaveUp)
				return
			}
			if !bytes.Equal(got, payloads[0]) && !bytes.Equal(got, payloads[1]) {
				t.Errorf("the reader saw %d bytes that are neither payload", len(got))
				close(gaveUp)
				return
			}
			reads.Add(1)
		}
	})
	var writers sync.WaitGroup
	for w := range 2 {
		writers.Go(func() {
			for i := 0; i < minWrites || reads.Load() < minReads; i++ {
				select {
				case <-gaveUp:
					return
				default:
				}
				if err := WriteFile(path, payloads[(w+i)%2], 0o644); err != nil {
					t.Errorf("WriteFile: %v", err)
					return
				}
			}
		})
	}
	writers.Wait()
	close(stop)
	reader.Wait()
	if temps := tempsIn(t, filepath.Dir(path)); len(temps) != 0 {
		t.Errorf("the writes left %v behind", temps)
	}
}

// A reader holding the target open does not block the write, and keeps the
// bytes it opened. On Windows only the POSIX rename lands this.
func TestWriteFileReplacesUnderAReader(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "seg-1.m4s")
	if err := WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	serving, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer serving.Close()
	if err := WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatalf("WriteFile over a target a reader holds: %v", err)
	}
	buf := make([]byte, 3)
	if _, err := serving.ReadAt(buf, 0); err != nil || string(buf) != "old" {
		t.Errorf("the held handle reads %q (%v), want %q", buf, err, "old")
	}
	mustRead(t, path, "new")
}

func TestWriteFileSetsTheMode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows files carry no mode")
	}
	path := filepath.Join(t.TempDir(), "job.json")
	for _, perm := range []os.FileMode{0o600, 0o700} {
		if err := WriteFile(path, []byte("{}"), perm); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != perm {
			t.Errorf("mode = %o, want %o", fi.Mode().Perm(), perm)
		}
	}
}

func TestStageAbortRemoves(t *testing.T) {
	t.Parallel()
	t.Run("staged beside the target", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "out.flac")
		if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		st, err := Stage(target, 0o644, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.File.Write([]byte("half")); err != nil {
			t.Fatal(err)
		}
		st.Abort()
		if temps := tempsIn(t, dir); len(temps) != 0 {
			t.Errorf("Abort left %v behind", temps)
		}
		mustRead(t, target, "old")
		if _, err := st.File.Write([]byte("x")); err == nil {
			t.Error("the file is still open after Abort")
		}
		if err := st.Commit(); err == nil {
			t.Error("Commit after Abort = nil, want an error")
		}
		mustRead(t, target, "old")
	})
	t.Run("created in place", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "out.flac")
		st, err := Stage(target, 0o644, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.File.Write([]byte("half")); err != nil {
			t.Fatal(err)
		}
		st.Abort()
		if _, err := os.Stat(target); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Abort left the half-written file: %v", err)
		}
		if err := st.Commit(); err == nil {
			t.Error("Commit after Abort = nil, want an error")
		}
	})
	t.Run("the file is already closed", func(t *testing.T) {
		dir := t.TempDir()
		st, err := Stage(filepath.Join(dir, "out.flac"), 0o644, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.File.Close(); err != nil {
			t.Fatal(err)
		}
		st.Abort()
		st.Abort()
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("Abort left %d files behind", len(entries))
		}
	})
}

func TestStageCommitReplaces(t *testing.T) {
	t.Parallel()
	t.Run("staged beside the target", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "out.flac")
		if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		st, err := Stage(target, 0o644, true)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Abort()
		name := filepath.Base(st.Path)
		if filepath.Dir(st.Path) != dir || !regexp.MustCompile(`^out\.flac\.tmp-[0-9a-f]{16}$`).MatchString(name) {
			t.Errorf("staged at %q, want out.flac.tmp-<16 hex> beside the target", st.Path)
		}
		if _, err := st.File.Write([]byte("new")); err != nil {
			t.Fatal(err)
		}
		// The pipeline reads its output back and patches it.
		buf := make([]byte, 3)
		if _, err := st.File.ReadAt(buf, 0); err != nil || string(buf) != "new" {
			t.Errorf("read back %q (%v), want %q", buf, err, "new")
		}
		mustRead(t, target, "old")
		if err := st.Commit(); err != nil {
			t.Fatal(err)
		}
		mustRead(t, target, "new")
		if temps := tempsIn(t, dir); len(temps) != 0 {
			t.Errorf("Commit left %v behind", temps)
		}
		st.Abort()
		mustRead(t, target, "new")
		if st.Path != target {
			t.Errorf("Path after Commit = %q, want the target", st.Path)
		}
	})
	t.Run("the caller closed the file first", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "out.flac")
		st, err := Stage(target, 0o644, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.File.Write([]byte("new")); err != nil {
			t.Fatal(err)
		}
		if err := st.File.Close(); err != nil {
			t.Fatal(err)
		}
		if err := st.Commit(); err != nil {
			t.Fatalf("Commit of a closed file: %v", err)
		}
		mustRead(t, target, "new")
	})
	t.Run("a failed replace removes the temp", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "out")
		if err := os.MkdirAll(filepath.Join(target, "kept"), 0o755); err != nil {
			t.Fatal(err)
		}
		st, err := Stage(target, 0o644, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Commit(); err == nil {
			t.Fatal("Commit over a non-empty directory = nil, want an error")
		}
		if temps := tempsIn(t, dir); len(temps) != 0 {
			t.Errorf("the failed Commit left %v behind", temps)
		}
	})
	t.Run("created in place", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "out.flac")
		st, err := Stage(target, 0o644, false)
		if err != nil {
			t.Fatal(err)
		}
		if st.Path != target {
			t.Errorf("Path = %q, want the target itself", st.Path)
		}
		if _, err := st.File.Write([]byte("new")); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 3)
		if _, err := st.File.ReadAt(buf, 0); err != nil || string(buf) != "new" {
			t.Errorf("read back %q (%v), want %q", buf, err, "new")
		}
		if err := st.Commit(); err != nil {
			t.Fatal(err)
		}
		st.Abort()
		mustRead(t, target, "new")
		if entries, _ := os.ReadDir(dir); len(entries) != 1 {
			t.Errorf("the directory holds %d files, want the one output", len(entries))
		}
	})
	t.Run("in place refuses an existing target", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "out.flac")
		if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		if st, err := Stage(target, 0o644, false); !errors.Is(err, fs.ErrExist) {
			if err == nil {
				st.Abort()
			}
			t.Fatalf("Stage over an existing target = %v, want fs.ErrExist", err)
		}
		mustRead(t, target, "old")
	})
}

// outcome is what dir holds: its names, and the output's bytes.
func outcome(dir, target string) string {
	entries, _ := os.ReadDir(dir)
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	b, _ := os.ReadFile(target)
	return fmt.Sprintf("%v %q", names, b)
}

// Commits and Aborts race, as they do when a signal stops a run that is
// just finishing. The first wins whole, and every call returns only once
// that outcome is on disk: the output alone, or nothing.
func TestStageCommitAndAbortRace(t *testing.T) {
	t.Parallel()
	const calls = 6 // Commits and Aborts by turns
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replace=%v", replace), func(t *testing.T) {
			base := t.TempDir()
			for round := range 200 {
				dir := filepath.Join(base, strconv.Itoa(round))
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(dir, "out.flac")
				st, err := Stage(target, 0o644, replace)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := st.File.Write([]byte("payload")); err != nil {
					t.Fatal(err)
				}
				start := make(chan struct{})
				var committed atomic.Int32
				seen := make([]string, calls)
				var wg sync.WaitGroup
				for g := range calls {
					wg.Go(func() {
						<-start
						if g%2 == 0 {
							if st.Commit() == nil {
								committed.Add(1)
							}
						} else {
							st.Abort()
						}
						seen[g] = outcome(dir, target)
					})
				}
				close(start)
				wg.Wait()

				want := `[] ""`
				switch n := committed.Load(); n {
				case 0:
				case 1:
					want = `[out.flac] "payload"`
				default:
					t.Fatalf("round %d: %d Commits returned nil, want at most one", round, n)
				}
				for g, got := range seen {
					if got != want {
						t.Fatalf("round %d: call %d returned with the directory at %s, want %s", round, g, got, want)
					}
				}
				if got := outcome(dir, target); got != want {
					t.Fatalf("round %d: the directory ends at %s, want %s", round, got, want)
				}
			}
		})
	}
}

// Abort arrives while the pipeline is still writing. The writer's next
// write fails as closed, and nothing is left on disk.
func TestStageAbortDuringWrites(t *testing.T) {
	t.Parallel()
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replace=%v", replace), func(t *testing.T) {
			dir := t.TempDir()
			st, err := Stage(filepath.Join(dir, "out.flac"), 0o644, replace)
			if err != nil {
				t.Fatal(err)
			}
			wrote := make(chan struct{})
			var writeErr error
			var wg sync.WaitGroup
			wg.Go(func() {
				chunk := make([]byte, 4096)
				for i := 0; ; i++ {
					if _, writeErr = st.File.Write(chunk); writeErr != nil {
						return
					}
					if i == 0 {
						close(wrote)
					}
				}
			})
			<-wrote
			st.Abort()
			wg.Wait()
			if !errors.Is(writeErr, os.ErrClosed) {
				t.Errorf("the write after Abort = %v, want os.ErrClosed", writeErr)
			}
			if err := st.Commit(); err == nil {
				t.Error("Commit after Abort = nil, want an error")
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("Abort left %d files behind", len(entries))
			}
		})
	}
}

func TestSweepTempsRemovesTempFilesAndNothingElse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"job.json", "out.flac", "job.json.tmp", "job.json.tmp-0123456789abcdef"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A directory is never a temp, whatever it is called.
	for _, name := range []string{"x.tmp", "x.tmp-1"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	SweepTemps(dir)
	SweepTemps(filepath.Join(dir, "missing"))

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if want := []string{"job.json", "out.flac", "x.tmp", "x.tmp-1"}; !slices.Equal(got, want) {
		t.Errorf("after the sweep the directory holds %v, want %v", got, want)
	}
	mustRead(t, filepath.Join(dir, "job.json"), "job.json")
}
