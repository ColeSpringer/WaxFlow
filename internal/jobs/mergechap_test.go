package jobs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestMergeTitle pins the A18 chapter-title precedence purely: the request field
// wins, then the member's TITLE tag, then a generated "Chapter N" (1-based). An
// empty request entry does not force a blank title; the precedence cannot spell
// "deliberately blank", so it falls through.
func TestMergeTitle(t *testing.T) {
	req := []string{"ReqA", "", "ReqC", ""}
	tag := []string{"TagA", "TagB", "", ""}
	for _, tc := range []struct {
		i    int
		want string
	}{
		{0, "ReqA"},      // the request field wins over the tag
		{1, "TagB"},      // an empty request entry falls through to the tag
		{2, "ReqC"},      // the request field wins with no tag present
		{3, "Chapter 4"}, // neither present: the generated fallback, 1-based
	} {
		if got := mergeTitle(tc.i, req, tag); got != tc.want {
			t.Errorf("mergeTitle(%d) = %q, want %q", tc.i, got, tc.want)
		}
	}
	// An index past both slices (a shorter title list than the member count)
	// falls through cleanly rather than panicking.
	if got := mergeTitle(5, nil, nil); got != "Chapter 6" {
		t.Errorf("mergeTitle with no title slices = %q, want %q", got, "Chapter 6")
	}
}

// writeMergeWAV renders frames of stereo 48 kHz sine into dir/name and returns
// its lib ref. Distinct frame counts give members of distinct lengths, so a
// chapter placed at the wrong seam is a mismatch rather than a coincidence.
func writeMergeWAV(t *testing.T, dir, name string, frames int) string {
	t.Helper()
	f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	buf := testutil.Sine(f, frames, 440, 0.5)
	defer audio.Put(buf)
	enc, err := pcm.NewEncoder(pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, f)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	mux := riff.NewMuxer(&out, nil)
	track := container.Track{Codec: codec.PCM, CodecConfig: enc.CodecConfig(), Fmt: f, Samples: int64(frames), Default: true}
	if err := mux.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error { return mux.WritePacket(container.Packet{Track: 0, Packet: p}) }
	if err := enc.Encode(buf, emit); err != nil {
		t.Fatal(err)
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := mux.End(trailer); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return "lib/" + name
}

// countingTitleMapper reports a TITLE tag per member in call order and counts
// its reads. The measure pass opens members strictly in order, one at a time,
// so the ith Read is member i; the count is what a gating test asserts is zero
// for a non-mp4 merge.
type countingTitleMapper struct {
	titles []string
	reads  int
}

func (m *countingTitleMapper) Read(context.Context, container.Source, string, meta.ReadOptions) (*meta.Info, error) {
	i := m.reads
	m.reads++
	if i < len(m.titles) && m.titles[i] != "" {
		return &meta.Info{Tags: map[string][]string{"TITLE": {m.titles[i]}}}, nil
	}
	return &meta.Info{}, nil
}

func (m *countingTitleMapper) Apply(context.Context, string, *meta.Info, []container.Tag) error {
	return nil
}

// mergeMembers writes members of the given lengths into a fresh root and opens
// a runner over them, with the given metadata mapper (nil for none).
func mergeMembers(t *testing.T, lens []int, m meta.Mapper) (*Runner, []string) {
	t.Helper()
	root := t.TempDir()
	refs := make([]string, len(lens))
	for i, n := range lens {
		refs[i] = writeMergeWAV(t, root, fmt.Sprintf("m%d.wav", i), n)
	}
	res := openRoots(t, root)
	return openRunner(t, Config{Dir: t.TempDir(), Resolver: res, MeasureTrack: measureTrack(), Meta: m}), refs
}

// mergeOutputChapters runs a merge to completion and reads the chapters the
// output file actually carries, off the written bytes rather than off the job:
// a field set on an options struct proves nothing about what a client downloads.
func mergeOutputChapters(t *testing.T, r *Runner, req Request) []container.Chapter {
	t.Helper()
	j, err := r.Create(req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	done := waitJob(t, r, j.ID, StateDone)
	out := soleOutput(t, done)
	raw, err := os.ReadFile(r.OutputPath(done, 0))
	if err != nil {
		t.Fatalf("reading the output: %v", err)
	}
	info, err := format.Probe(bytes.NewReader(raw), out.Container, nil)
	if err != nil {
		t.Fatalf("probing the output: %v", err)
	}
	return info.Chapters
}

// TestMergeStampsChapters is A18's headline: an mp4-family merge writes a
// QuickTime chapter text track, one chapter per member at its boundary on the
// concatenated timeline, titled by the A18 precedence. Members are whole-/
// half-second so their boundary offsets land on whole chapter ticks
// (chapterTimescale is 1 kHz) and round-trip through the muxer exactly.
func TestMergeStampsChapters(t *testing.T) {
	lens := []int{48000, 96000, 72000} // 1 s, 2 s, 1.5 s at 48 kHz
	wantStarts := []time.Duration{0, time.Second, 3 * time.Second}

	t.Run("request titles at member boundaries", func(t *testing.T) {
		r, refs := mergeMembers(t, lens, nil)
		titles := []string{"Intro", "The Long Middle", "Coda"}
		got := mergeOutputChapters(t, r, Request{
			Type: TypeMerge, Srcs: refs, MemberTitles: titles,
			Format: "alac", Container: mp4ProgressiveContainer,
		})
		if len(got) != len(refs) {
			t.Fatalf("output carries %d chapters, want one per member (%d)", len(got), len(refs))
		}
		for i := range got {
			if got[i].Title != titles[i] {
				t.Errorf("chapter %d title = %q, want the request's %q", i, got[i].Title, titles[i])
			}
			if got[i].Start != wantStarts[i] {
				t.Errorf("chapter %d starts at %v, want %v (the member's boundary)", i, got[i].Start, wantStarts[i])
			}
		}
	})

	t.Run("member TITLE tags when no request title", func(t *testing.T) {
		r, refs := mergeMembers(t, lens, &countingTitleMapper{titles: []string{"TagA", "TagB", "TagC"}})
		got := mergeOutputChapters(t, r, Request{
			Type: TypeMerge, Srcs: refs, Format: "alac", Container: mp4ProgressiveContainer,
		})
		want := []string{"TagA", "TagB", "TagC"}
		if len(got) != len(want) {
			t.Fatalf("output carries %d chapters, want %d", len(got), len(want))
		}
		for i := range got {
			if got[i].Title != want[i] {
				t.Errorf("chapter %d title = %q, want the member's TITLE tag %q", i, got[i].Title, want[i])
			}
		}
	})

	t.Run("a request title skips the member's tag read", func(t *testing.T) {
		// mergeTitle prefers a non-empty request title, so a fully-titled merge
		// must parse no member tags: the reads would only be discarded. The
		// counting mapper's titles are never even consulted, so 0 reads is both
		// "no work done" and "no wrong title could sneak in".
		m := &countingTitleMapper{titles: []string{"TagA", "TagB", "TagC"}}
		r, refs := mergeMembers(t, lens, m)
		titles := []string{"ReqA", "ReqB", "ReqC"}
		got := mergeOutputChapters(t, r, Request{
			Type: TypeMerge, Srcs: refs, MemberTitles: titles,
			Format: "alac", Container: mp4ProgressiveContainer,
		})
		for i := range got {
			if got[i].Title != titles[i] {
				t.Errorf("chapter %d title = %q, want the request's %q", i, got[i].Title, titles[i])
			}
		}
		if m.reads != 0 {
			t.Errorf("a fully-titled merge parsed member tags %d times; each read's result is discarded", m.reads)
		}
	})

	t.Run("generated Chapter N when no title anywhere", func(t *testing.T) {
		r, refs := mergeMembers(t, lens, nil)
		got := mergeOutputChapters(t, r, Request{
			Type: TypeMerge, Srcs: refs, Format: "alac", Container: mp4ProgressiveContainer,
		})
		want := []string{"Chapter 1", "Chapter 2", "Chapter 3"}
		if len(got) != len(want) {
			t.Fatalf("output carries %d chapters, want %d", len(got), len(want))
		}
		for i := range got {
			if got[i].Title != want[i] {
				t.Errorf("chapter %d title = %q, want the generated %q", i, got[i].Title, want[i])
			}
		}
	})
}

// TestMergeGatesChaptersOnMP4 pins that only an mp4-progressive merge reads
// member titles and stamps chapters: a merge to another format pays no
// per-member metadata read and writes no chapter track, so a non-mp4 merge is
// not slowed by work its output cannot carry.
func TestMergeGatesChaptersOnMP4(t *testing.T) {
	m := &countingTitleMapper{titles: []string{"A", "B"}}
	r, refs := mergeMembers(t, []int{48000, 48000}, m)
	got := mergeOutputChapters(t, r, Request{
		Type: TypeMerge, Srcs: refs, MemberTitles: []string{"A", "B"}, Format: "flac",
	})
	if len(got) != 0 {
		t.Errorf("a flac merge wrote %d chapters; the QuickTime chapter track is mp4-only", len(got))
	}
	if m.reads != 0 {
		t.Errorf("a non-mp4 merge read member metadata %d times; it must read none", m.reads)
	}
}
