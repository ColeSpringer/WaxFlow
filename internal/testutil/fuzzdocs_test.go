package testutil

import (
	"fmt"
	"go/build"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// fuzzTarget is how scripts/fuzz.sh finds a target: gofmt pins the spelling.
var fuzzTarget = regexp.MustCompile(`(?m)^func Fuzz[A-Za-z0-9_]*`)

// unrunTarget is a test file holding a fuzz target scripts/fuzz.sh never
// runs, and why.
type unrunTarget struct {
	path, why string
}

// countFuzzTargets counts the Fuzz functions scripts/fuzz.sh runs: those in
// the test files `go list ./...` names for the root module on the linux
// runners the fuzz jobs use. A target that listing leaves out is returned
// apart, since nothing would ever fuzz it.
func countFuzzTargets(t *testing.T, root string) (count int, unrun []unrunTarget) {
	t.Helper()
	runner := build.Default
	runner.GOOS, runner.GOARCH, runner.BuildTags = "linux", "amd64", nil
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		n := len(fuzzTarget.FindAll(src, -1))
		if n == 0 {
			return nil
		}
		// A file belongs to the nearest go.mod above it, and go list skips
		// a directory named testdata or starting with an underscore.
		skipped := false
		for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				if dir != root {
					unrun = append(unrun, unrunTarget{path, "it is in a nested module, and the script lists the root module"})
					return nil
				}
				break
			}
			if name := filepath.Base(dir); name == "testdata" || strings.HasPrefix(name, "_") {
				skipped = true
			}
		}
		if skipped {
			unrun = append(unrun, unrunTarget{path, "go list skips its directory"})
			return nil
		}
		match, err := runner.MatchFile(filepath.Dir(path), filepath.Base(path))
		if err != nil {
			return err
		}
		if !match {
			unrun = append(unrun, unrunTarget{path, "a build constraint keeps it out of a linux build with no tags"})
			return nil
		}
		count += n
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return count, unrun
}

// fuzzDocs reads the workflows and the maintenance notes for what they say
// about the fuzz targets, collecting every disagreement with count.
type fuzzDocs struct {
	problems []string
}

func (d *fuzzDocs) failf(format string, args ...any) {
	d.problems = append(d.problems, fmt.Sprintf(format, args...))
}

// number returns the number pattern captures in text, which has to state it
// exactly once.
func (d *fuzzDocs) number(file, text, pattern string) int {
	m := regexp.MustCompile(pattern).FindAllStringSubmatch(text, -1)
	if len(m) != 1 {
		d.failf("%s matches %q %d times, want once: the sentence this test reads was reworded", file, pattern, len(m))
		return 0
	}
	n, _ := strconv.Atoi(m[0][1])
	return n
}

var workflowJobKey = regexp.MustCompile(`^  [A-Za-z0-9_-]+:`)

// job returns the text of one job in a workflow file: its key at two spaces
// of indent, up to the next key at that indent.
func (d *fuzzDocs) job(file, text, name string) string {
	lines := strings.Split(text, "\n")
	start := slices.Index(lines, "  "+name+":")
	if start < 0 {
		d.failf("%s has no job %s", file, name)
		return ""
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if workflowJobKey.MatchString(lines[i]) {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// shards returns a fuzz job's divisor N, having checked that its matrix
// lists the shards 1 to N once each. A divisor raised without its matrix
// entry leaves every Nth target unrun, and nothing else would say so.
func (d *fuzzDocs) shards(file, job string) int {
	n := d.number(file, job, `FUZZ_SHARD: \$\{\{ matrix\.shard \}\}/(\d+)`)
	m := regexp.MustCompile(`(?m)^\s+shard: \[([0-9, ]+)\]$`).FindAllStringSubmatch(job, -1)
	if len(m) != 1 {
		d.failf("%s: found %d shard matrices in the job, want one", file, len(m))
		return n
	}
	var listed []int
	for f := range strings.SplitSeq(m[0][1], ",") {
		k, _ := strconv.Atoi(strings.TrimSpace(f))
		listed = append(listed, k)
	}
	want := make([]int, n)
	for i := range want {
		want[i] = i + 1
	}
	if !slices.Equal(listed, want) {
		d.failf("%s: the matrix runs shards %v of FUZZ_SHARD=k/%d, want 1 to %d once each", file, listed, n, n)
	}
	return n
}

const (
	nightlyFile = ".github/workflows/nightly.yml"
	ciFile      = ".github/workflows/ci.yml"
	maintFile   = "MAINTENANCE.md"
)

// fuzzDocProblems lists where the three files disagree with count targets.
func fuzzDocProblems(count int, nightly, ci, maint string) []string {
	d := &fuzzDocs{}
	if got := d.number(nightlyFile, nightly, `The list is (\d+) targets`); got != count {
		d.failf("%s says the list is %d targets; there are %d", nightlyFile, got, count)
	}
	if got := d.number(maintFile, maint, `the (\d+) targets are about`); got != count {
		d.failf("%s says %d targets; there are %d", maintFile, got, count)
	}

	// The nightly: a shard's targets stay under the threshold its comment
	// names, and that many targets fit in the job's timeout.
	soak := d.job(nightlyFile, nightly, "fuzz-soak")
	n := max(d.shards(nightlyFile, soak), 1)
	threshold := d.number(nightlyFile, nightly, `once a shard holds (\d+) targets`)
	minutes := d.number(nightlyFile, soak, `scripts/fuzz\.sh (\d+)m`)
	limit := d.number(nightlyFile, soak, `timeout-minutes: (\d+)`)
	if perShard := (count + n - 1) / n; perShard >= threshold {
		d.failf("%s: a shard holds %d of the %d targets, and the job says to add a shard at %d", nightlyFile, perShard, count, threshold)
	}
	if threshold*minutes >= limit {
		d.failf("%s: %d targets at %d minutes each is %d minutes, not under the job's %d-minute timeout",
			nightlyFile, threshold, minutes, threshold*minutes, limit)
	}

	// The per-push smoke: the fuzzing alone has to fit its timeout.
	short := d.job(ciFile, ci, "fuzz-short")
	n = max(d.shards(ciFile, short), 1)
	seconds := d.number(ciFile, short, `scripts/fuzz\.sh (\d+)s`)
	limit = d.number(ciFile, short, `timeout-minutes: (\d+)`)
	if perShard := (count + n - 1) / n; perShard*seconds >= limit*60 {
		d.failf("%s: a shard fuzzes %d targets for %d s each, which does not fit its %d-minute timeout", ciFile, perShard, seconds, limit)
	}
	return d.problems
}

// readFuzzDocs returns the root module's target count and the three files.
func readFuzzDocs(t *testing.T) (count int, nightly, ci, maint string) {
	t.Helper()
	const root = "../.."
	count, unrun := countFuzzTargets(t, root)
	if count < 12 {
		t.Fatalf("found %d fuzz targets; the walk is not seeing the tree", count)
	}
	for _, u := range unrun {
		t.Errorf("%s holds a fuzz target scripts/fuzz.sh never runs: %s", u.path, u.why)
	}
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	return count, read(nightlyFile), read(ciFile), read(maintFile)
}

// TestFuzzCountInDocs holds what is written about the fuzz targets to the
// targets: the two sentences that state how many there are, and the shard
// arithmetic the two workflows run them under. A target added to a full
// shard fails here, where a comment used to ask someone to notice.
func TestFuzzCountInDocs(t *testing.T) {
	count, nightly, ci, maint := readFuzzDocs(t)
	for _, problem := range fuzzDocProblems(count, nightly, ci, maint) {
		t.Error(problem)
	}
}

// TestFuzzDocChecksBite breaks the real files one way at a time and expects
// each break to be named, so a check above that stopped matching its
// sentence would fail here and not pass in silence.
func TestFuzzDocChecksBite(t *testing.T) {
	count, nightly, ci, maint := readFuzzDocs(t)
	swap := func(text, old, new string) string {
		t.Helper()
		if strings.Count(text, old) != 1 {
			t.Fatalf("%q occurs %d times, want once", old, strings.Count(text, old))
		}
		return strings.Replace(text, old, new, 1)
	}
	for _, tc := range []struct {
		name                      string
		count                     int
		nightly, ci, maint, wants string
	}{
		{"a target added", count + 1, nightly, ci, maint, "there are"},
		{"a nightly divisor raised without its matrix entry", count,
			swap(nightly, "matrix.shard }}/6", "matrix.shard }}/7"), ci, maint, "want 1 to 7 once each"},
		{"a nightly matrix naming a shard twice", count,
			swap(nightly, "shard: [1, 2, 3, 4, 5, 6]", "shard: [1, 2, 3, 4, 5, 5]"), ci, maint, "once each"},
		{"a per-push divisor raised without its matrix entry", count,
			nightly, swap(ci, "matrix.shard }}/4", "matrix.shard }}/5"), maint, "want 1 to 5 once each"},
		{"a nightly shard filled to its threshold", count,
			swap(nightly, "once a shard holds 24 targets", "once a shard holds 11 targets"), ci, maint, "add a shard at 11"},
		{"a threshold that overruns the timeout", count,
			swap(nightly, "once a shard holds 24 targets", "once a shard holds 30 targets"), ci, maint, "not under the job's"},
		{"a per-push run that overruns its timeout", count,
			nightly, swap(ci, "scripts/fuzz.sh 45s", "scripts/fuzz.sh 450s"), maint, "does not fit"},
		{"a count sentence reworded", count,
			swap(nightly, "The list is", "The list holds"), ci, maint, "reworded"},
	} {
		problems := fuzzDocProblems(tc.count, tc.nightly, tc.ci, tc.maint)
		if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tc.wants) }) {
			t.Errorf("%s: problems = %q, want one saying %q", tc.name, problems, tc.wants)
		}
	}
}

// TestFuzzTargetsOutsideTheListingAreNamed: the count is what scripts/fuzz.sh
// runs, which is what `go list ./...` names on the linux runners. A target
// that listing leaves out is reported, not counted.
func TestFuzzTargetsOutsideTheListingAreNamed(t *testing.T) {
	root := t.TempDir()
	const target = "package x\n\nimport \"testing\"\n\nfunc FuzzX(f *testing.F) {}\n"
	for path, body := range map[string]string{
		"go.mod":                     "module example.test/m\n",
		"a/a_test.go":                target, // the one the script runs
		"a/testdata/b_test.go":       target,
		"a/_old/c_test.go":           target,
		"a/d_windows_test.go":        target,
		"a/e_test.go":                "//go:build sometag\n\n" + target,
		"nested/go.mod":              "module example.test/n\n",
		"nested/f_test.go":           target,
		"a/g_test.go":                "package x\n", // no target
		"a/.hidden/h_test.go":        target,        // not Go's to list, nor ours to walk
		"a/node_modules/p/i_test.go": target,
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	count, unrun := countFuzzTargets(t, root)
	if count != 1 {
		t.Errorf("counted %d targets, want the 1 the script runs", count)
	}
	var got []string
	for _, u := range unrun {
		rel, _ := filepath.Rel(root, u.path)
		got = append(got, filepath.ToSlash(rel))
	}
	slices.Sort(got)
	want := []string{"a/_old/c_test.go", "a/d_windows_test.go", "a/e_test.go", "a/testdata/b_test.go", "nested/f_test.go"}
	if !slices.Equal(got, want) {
		t.Errorf("targets reported as never run = %v, want %v", got, want)
	}
}
