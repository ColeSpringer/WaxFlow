package label_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/waxerr"
)

// writeFragmentedMP4 builds the shape both cells need: our own fragmented
// (CMAF) MP4, which is the aac row's default container. The tag library reads
// it and refuses to rewrite it, so it is the one source that exercises the
// note and the refusal at once. Built through the engine because a CLI file
// output is the flat form.
func writeFragmentedMP4(t *testing.T, path string, tags []container.Tag) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sine-s16.wav"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := waxflow.New().Transcode(t.Context(), container.BytesSource(raw), "wav", out,
		waxflow.TranscodeOptions{Format: "aac", Tags: tags}); err != nil {
		t.Fatal(err)
	}
}

// TestFragmentedIsANoteNotAWarning pins the sourceLint classification at its
// source, which is the switch behind both surfaces that show a mapper warning
// to a user: logMetaRead (transcode and split) and a job's client-visible
// Warnings. Probe's human line and the /probe envelope are not among them,
// though they look like they should be: both iterate format.Info.Warnings off
// the probe itself, and ProbeMetadata carries no warnings at all.
//
// A fragmented source reads its tags exactly, so it must not spend a warning
// saying so.
func TestFragmentedIsANoteNotAWarning(t *testing.T) {
	frag := filepath.Join(t.TempDir(), "frag.m4a")
	writeFragmentedMP4(t, frag, []container.Tag{{Key: "TITLE", Value: "A Title"}})

	raw, err := os.ReadFile(frag)
	if err != nil {
		t.Fatal(err)
	}
	info, err := label.New().Read(t.Context(), container.BytesSource(raw), "m4a", meta.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The premise: the mapper really did read this file, so the note is
	// describing a source it parsed rather than one it gave up on.
	if vs := info.Tags["TITLE"]; len(vs) != 1 || vs[0] != "A Title" {
		t.Fatalf("the mapper read TITLE = %v off a fragmented source, want [%q]", vs, "A Title")
	}
	if !slices.ContainsFunc(info.Notes, func(n string) bool { return strings.Contains(n, "fragmented") }) {
		t.Errorf("no fragmented note: %v", info.Notes)
	}
	if len(info.Warnings) != 0 {
		t.Errorf("a readable fragmented source warned: %v", info.Warnings)
	}
}

// TestFragmentedApplyIsUnsupportedNotInternal pins the code on the write
// refusal. The tag library moved that refusal from parse to plan time, which
// routed it through the blanket Prepare mapping and relabelled a file it
// simply cannot rewrite an internal error (exit 1) instead of an unsupported
// one (exit 5).
//
// The edit has to be real. The refusal sits behind the unchanged-file fast
// path, so applying a tag set the file already carries returns a no-op plan
// and no error, and a cell built from a tagged fixture would pass on the
// unfixed mapping. Hence the untagged fixture.
//
// All three production Apply sites route MP4 outputs to the muxer instead
// (cli/transcode.go, cli/split.go, internal/jobs/runner.go), so this pins the
// seam's contract rather than a live route.
func TestFragmentedApplyIsUnsupportedNotInternal(t *testing.T) {
	frag := filepath.Join(t.TempDir(), "frag.m4a")
	writeFragmentedMP4(t, frag, nil)

	err := label.New().Apply(t.Context(), frag,
		&meta.Info{Tags: map[string][]string{"TITLE": {"A Title"}}}, nil)
	if err == nil {
		t.Fatal("Apply accepted a fragmented MP4; the fixture is not exercising the refusal")
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeUnsupportedFormat {
		t.Errorf("code = %q, want %q (%v)", got, waxerr.CodeUnsupportedFormat, err)
	}
}

// TestCanceledApplyIsCanceled keeps the format mapping from swallowing the one
// thing it must not: an abandoned operation is not a statement about the
// output, which would otherwise report exit 5 "unsupported" for a file nothing
// is wrong with.
//
// Scope, precisely: ParseFile checks the context first thing, so a ctx canceled
// up front always stops there and this covers that branch alone. The matching
// check on plan.Execute is for a cancel that lands mid-write, which has no
// deterministic seam to drive from a test; it is symmetric code, not code this
// pins. No fixture is built for the same reason: ParseFile returns before it
// opens anything, so any path answers identically.
func TestCanceledApplyIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := label.New().Apply(ctx, filepath.Join("..", "..", "testdata", "sine-s16.flac"),
		&meta.Info{Tags: map[string][]string{"TITLE": {"A Title"}}}, nil)
	if err == nil {
		t.Fatal("a canceled Apply succeeded, so this cell covers nothing")
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeCanceled {
		t.Errorf("code = %q, want %q (%v)", got, waxerr.CodeCanceled, err)
	}
}

// TestBadTagValueIsNotBlamedOnTheOutput pins the other half of the Prepare
// split. A value carrying invalid UTF-8 comes off the source, and FLAC holds
// metadata perfectly well, so answering "unsupported format" points the user
// at the wrong file entirely.
func TestBadTagValueIsNotBlamedOnTheOutput(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sine-s16.wav"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "out.flac")
	writeFLAC(t, raw, path)

	err = label.New().Apply(t.Context(), path,
		&meta.Info{Tags: map[string][]string{"COMMENT": {"bad\xff\xfevalue"}}}, nil)
	if err == nil {
		t.Fatal("Apply accepted invalid UTF-8; this cell covers nothing")
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeInvalidRequest {
		t.Errorf("code = %q, want %q (%v)", got, waxerr.CodeInvalidRequest, err)
	}
}

// writeFLAC builds a taggable output: an ordinary flat file the mapper
// rewrites without complaint, so a refusal is about the metadata.
func writeFLAC(t *testing.T, wav []byte, path string) {
	t.Helper()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := waxflow.New().Transcode(t.Context(), container.BytesSource(wav), "wav", out,
		waxflow.TranscodeOptions{Format: "flac"}); err != nil {
		t.Fatal(err)
	}
}
