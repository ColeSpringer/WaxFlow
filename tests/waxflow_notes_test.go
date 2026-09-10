package waxflow_test

// The kind split, end to end: a probe reports what a file is damaged about
// under warnings, and what this build did with a well-formed file under
// notes. Strict escalates the first and never the second, which is why a
// demuxer carries the kind rather than leaving a caller to guess it from the
// wording. The per-container cells live beside each demuxer; this is the
// surface a client actually reads.

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// TestProbeSplitsNotesFromWarnings uses a QuickTime file with no recognizable
// ftyp box, which is legal (a .mov need not carry one) and used to be
// reported as damage. It must reach Info.Notes, leave Info.Warnings empty, and
// survive strict mode.
func TestProbeSplitsNotesFromWarnings(t *testing.T) {
	raw, err := os.ReadFile(repoPath("testdata", "chapters.m4b"))
	if err != nil {
		t.Fatal(err)
	}
	mangled := append([]byte(nil), raw...)
	copy(mangled[4:8], "xxxx")

	e := waxflow.New()
	clean, err := e.Probe(container.BytesSource(raw), "m4b", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(clean.Notes) != 0 || len(clean.Warnings) != 0 {
		t.Fatalf("the intact fixture already reports %v / %v; this cell cannot attribute anything",
			clean.Warnings, clean.Notes)
	}

	for _, strict := range []bool{false, true} {
		info, err := e.Probe(container.BytesSource(mangled), "m4b", &waxflow.ProbeOptions{Strict: strict})
		if err != nil {
			t.Fatalf("strict=%v refused a QuickTime file with no ftyp: %v", strict, err)
		}
		if !slices.ContainsFunc(info.Notes, func(s string) bool { return strings.Contains(s, "ftyp") }) {
			t.Errorf("strict=%v: notes = %v, want one naming the missing ftyp", strict, info.Notes)
		}
		if len(info.Warnings) != 0 {
			t.Errorf("strict=%v: warnings = %v, want none (a .mov without ftyp is not damaged)", strict, info.Warnings)
		}
	}
}

// TestStrictStillRefusesDamage is the other half, and the reason the split is
// worth carrying: strict has to keep doing its job. A FLAC cut short deviates
// from its own format, and no reclassification touches it.
func TestStrictStillRefusesDamage(t *testing.T) {
	raw, err := os.ReadFile(repoPath("testdata", "noise-s16.flac"))
	if err != nil {
		t.Fatal(err)
	}
	truncated := raw[:len(raw)/3]
	e := waxflow.New()
	if _, err := e.Probe(container.BytesSource(truncated), "flac", &waxflow.ProbeOptions{Strict: true}); err == nil {
		t.Fatal("strict mode accepted a truncated FLAC")
	} else if got := waxerr.CodeOf(err); got != waxerr.CodeMalformedInput {
		t.Errorf("code = %q, want %q (%v)", got, waxerr.CodeMalformedInput, err)
	}
}
