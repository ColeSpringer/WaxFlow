package testutil

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// errTB records what a helper reports in place of failing the test.
type errTB struct {
	testing.TB
	errs []string
}

func (e *errTB) Helper()             {}
func (e *errTB) Logf(string, ...any) {}
func (e *errTB) Errorf(format string, args ...any) {
	e.errs = append(e.errs, fmt.Sprintf(format, args...))
}

func TestGoldenTextNamesTheLinesThatMoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "table.txt")
	GoldenText(t, path, []byte("a | 1\nb | 2\nc | 3\n"), true, "")

	same := &errTB{TB: t}
	GoldenText(same, path, []byte("a | 1\nb | 2\nc | 3\n"), false, "advice")
	if len(same.errs) != 0 {
		t.Fatalf("an unchanged table reported %q", same.errs)
	}

	moved := &errTB{TB: t}
	GoldenText(moved, path, []byte("a | 1\nb | 9\nd | 4\n"), false, "say what moved it")
	if len(moved.errs) != 1 {
		t.Fatalf("a changed table reported %d errors, want 1: %q", len(moved.errs), moved.errs)
	}
	msg := moved.errs[0]
	for _, want := range []string{"- b | 2\n", "- c | 3\n", "+ b | 9\n", "+ d | 4\n", "say what moved it"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the report lacks %q:\n%s", want, msg)
		}
	}
	// The line that held is not listed, and the Version rule is Golden's.
	for _, not := range []string{"a | 1", "Version"} {
		if strings.Contains(msg, not) {
			t.Errorf("the report names %q:\n%s", not, msg)
		}
	}

	// A lost final newline is not a moved line, and is not reported as one.
	ending := &errTB{TB: t}
	GoldenText(ending, path, []byte("a | 1\nb | 2\nc | 3"), false, "advice")
	if len(ending.errs) != 1 || strings.Contains(ending.errs[0], "c | 3") || !strings.Contains(ending.errs[0], "line ending") {
		t.Errorf("a table short of its final newline reported %q, want one error naming the line endings and no line", ending.errs)
	}

	order := &errTB{TB: t}
	GoldenText(order, path, []byte("b | 2\na | 1\nc | 3\n"), false, "advice")
	if len(order.errs) != 1 || !strings.Contains(order.errs[0], "order") {
		t.Errorf("the same lines reordered reported %q, want one error naming the order", order.errs)
	}
}
