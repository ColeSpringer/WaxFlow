package oracletest

// The chapter-track oracle cell: our own reader agreeing with our own
// writer proves only that the two share a bug, so the text chapter track
// the progressive muxer synthesizes is put to a third party that never
// saw our box builders.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestFFprobeReadsProgressiveChapterTrack is the third-party proof that the
// text chapter track is a real track and not a shape only we can read. It
// transcodes the chaptered m4b fixture to a PROGRESSIVE mp4 (Container
// "progressive"; the default fragmented form writes its moov before any
// chapter sample exists and carries no text track), strips the Nero chpl
// beside it so ffprobe has nothing else to read, and asks ffprobe for the
// chapters. Everything it reports comes from the text track alone.
func TestFFprobeReadsProgressiveChapterTrack(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "chapters.m4b"))
	if err != nil {
		t.Fatal(err)
	}
	src := container.BytesSource(raw)
	ctx := context.Background()

	info, err := label.New().Read(ctx, src, "m4b", meta.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Chapters) != 3 {
		t.Fatalf("fixture carries %d chapters, want 3", len(info.Chapters))
	}

	out := filepath.Join(t.TempDir(), "out.m4a")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waxflow.New().Transcode(ctx, src, "m4b", f, waxflow.TranscodeOptions{
		Format:    "aac",
		Container: "progressive",
		Chapters:  info.Chapters,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	stripChpl(t, out)

	got := testutil.FFprobeChapters(t, out)
	if len(got) != len(info.Chapters) {
		t.Fatalf("ffprobe read %d chapters from the text track, want %d", len(got), len(info.Chapters))
	}
	for i, ch := range got {
		if want := info.Chapters[i].Title; ch.Title != want {
			t.Errorf("chapter %d title = %q, want %q", i, ch.Title, want)
		}
		// A start ffprobe can time at all means the stts chained; the exact
		// value is the round trip's business, pinned in container/mp4.
		if ch.Start < 0 || ch.End < 0 {
			t.Errorf("chapter %d has no timing: %+v", i, ch)
		}
	}
}

// stripChpl renames the Nero chapter list in place to a box nothing reads,
// leaving the text track as the file's only chapter form. Renaming rather than
// excising keeps every enclosing box size correct.
func stripChpl(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	i := indexOnce(t, raw, "chpl")
	if _, err := f.WriteAt([]byte("free"), int64(i)); err != nil {
		t.Fatal(err)
	}
}

// indexOnce returns the offset of the only occurrence of want, failing when
// there is not exactly one (a second would make the rename ambiguous).
func indexOnce(t *testing.T, raw []byte, want string) int {
	t.Helper()
	first, n := -1, 0
	for i := 0; i+len(want) <= len(raw); i++ {
		if string(raw[i:i+len(want)]) == want {
			if first < 0 {
				first = i
			}
			n++
		}
	}
	if n != 1 {
		t.Fatalf("found %d occurrences of %q, want exactly 1", n, want)
	}
	return first
}
