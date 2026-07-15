package format

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestProbeReportsChapters pins the capability gate: a container that
// parses chapters must surface them on Info, with no metadata mapper
// involved.
//
// That last clause is the whole point. Chapters used to reach a caller
// only through the injected tag mapper, so a daemon embedded by anyone who
// did not wire one saw none, for a file whose chapters our own demuxer had
// already parsed while reading the header. Probe pays nothing to ask: the
// demuxer answers from a field.
func TestProbeReportsChapters(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "chapters.m4b"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := Probe(bytes.NewReader(raw), "m4b", nil)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(info.Chapters) == 0 {
		t.Fatal("Probe reported no chapters for the chaptered m4b fixture; the Chapterer gate is not wired")
	}
	// The fixture's own three markers, so this fails on a mis-plumbed
	// slice rather than merely on an empty one.
	want := []string{"Intro", "Middle", "Coda"}
	if len(info.Chapters) != len(want) {
		t.Fatalf("chapters = %d, want %d", len(info.Chapters), len(want))
	}
	for i, ch := range info.Chapters {
		if ch.Title != want[i] {
			t.Errorf("chapter %d title = %q, want %q", i, ch.Title, want[i])
		}
	}
	// Chapters ascend, which is what makes them usable as cut points.
	for i := 1; i < len(info.Chapters); i++ {
		if info.Chapters[i].Start < info.Chapters[i-1].Start {
			t.Errorf("chapter %d starts at %v, before chapter %d at %v",
				i, info.Chapters[i].Start, i-1, info.Chapters[i-1].Start)
		}
	}
}

// TestProbeChaptersAbsentForPlainContainer is the other half of an honest
// capability gate: a container with no chapter form reports none rather
// than an empty non-nil slice or a fabricated marker.
func TestProbeChaptersAbsentForPlainContainer(t *testing.T) {
	info, err := Probe(bytes.NewReader(buildFile(t, "wav", 1000)), "wav", nil)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.Chapters != nil {
		t.Errorf("a WAV reported chapters %v; it has no chapter form", info.Chapters)
	}
}
