package testutil

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Golden compares a writer's output against a committed file, or rewrites it
// when update is set (the `-update` flag behind `make goldens`). A missing
// golden is fatal rather than an implicit write, so a typo in the path reads
// as a missing file instead of a new one nobody reviewed.
//
// The mismatch message names the version constant as well as the regeneration
// command, because those bytes are what an ADR-0004 cache key stands for: a
// deliberate change has to bump the writer's Version in the same commit, and
// this failure is the only place that gets said out loud.
func Golden(t testing.TB, path string, got []byte, update bool) {
	t.Helper()
	want, ok := goldenWant(t, path, got, update)
	if ok && !bytes.Equal(got, want) {
		t.Errorf("output differs from %s (%d vs %d bytes)%s\n"+
			"if the change is intentional: bump this writer's Version constant (ADR-0004), "+
			"then `make goldens` and review the diff",
			path, len(got), len(want), firstDiff(got, want))
	}
}

// GoldenText is Golden for a table of lines. A mismatch lists the lines that
// left and the lines that came, then advice: what a moved line means for this
// file, which is not Golden's Version rule.
func GoldenText(t testing.TB, path string, got []byte, update bool, advice string) {
	t.Helper()
	want, ok := goldenWant(t, path, got, update)
	if ok && !bytes.Equal(got, want) {
		t.Errorf("output differs from %s:\n%s%s", path, movedLines(got, want), advice)
	}
}

// goldenWant returns the committed bytes to compare got against. It reports
// false when update rewrote them, and so there is nothing to compare.
func goldenWant(t testing.TB, path string, got []byte, update bool) ([]byte, bool) {
	t.Helper()
	if update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote golden %s (%d bytes)", path, len(got))
		return nil, false
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (run `make goldens`): %v", path, err)
	}
	return want, true
}

// movedLines lists want's lines got lacks under "- " and got's lines want
// lacks under "+ ", capped at maxMovedLines. Lines are compared without their
// endings, so a lost final newline is named as that and not as a line that
// left and came back.
func movedLines(got, want []byte) string {
	const maxMovedLines = 40
	lines := func(b []byte) []string {
		var out []string
		for l := range strings.Lines(string(b)) {
			out = append(out, strings.TrimRight(l, "\r\n"))
		}
		return out
	}
	g, w := lines(got), lines(want)
	only := func(a, b []string, mark string) []string {
		in := map[string]bool{}
		for _, l := range b {
			in[l] = true
		}
		var out []string
		for _, l := range a {
			if !in[l] {
				out = append(out, mark+l+"\n")
			}
		}
		return out
	}
	moved := append(only(w, g, "- "), only(g, w, "+ ")...)
	switch {
	case len(moved) == 0 && slices.Equal(g, w):
		return "the same lines; only line endings differ (the final newline, or CRLF)\n"
	case len(moved) == 0:
		return "the same lines, in another order or number\n"
	}
	if more := len(moved) - maxMovedLines; more > 0 {
		moved = append(moved[:maxMovedLines], fmt.Sprintf("... and %d more\n", more))
	}
	return strings.Join(moved, "")
}
