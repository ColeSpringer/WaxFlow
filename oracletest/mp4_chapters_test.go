package oracletest

// The MP4 chapter agreement cells. Two readers speak for an MP4's chapters:
// container/mp4, which serves the probe and any embedder with no mapper, and
// the tag library behind cli/label, whose answer wins on the mapper-wired
// route. A file one of them reads differently is a transcode that carries
// one set of chapters while the probe reports another.

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestMP4SubtitlesAreNotChapters: a text track nothing names in a tref chap
// entry is subtitles, to both readers and to ffprobe.
func TestMP4SubtitlesAreNotChapters(t *testing.T) {
	t.Run("built", func(t *testing.T) {
		subs := mp4Text{handler: "sbtl", samples: [][]byte{mp4Title(""), mp4Title("Hello subtitle")}, deltas: []uint32{200, 1800}}
		if got := mp4ChaptersAgree(t, mp4Movie{text: []mp4Text{subs}}.build()); len(got) != 0 {
			t.Errorf("a subtitle track read as chapters: %+v", got)
		}
	})
	t.Run("mov_text", func(t *testing.T) {
		testutil.FFmpegEncoder(t, "mov_text")
		dir := t.TempDir()
		srt := filepath.Join(dir, "s.srt")
		if err := os.WriteFile(srt, []byte("1\n00:00:00,200 --> 00:00:00,900\nHello subtitle\n\n2\n00:00:01,100 --> 00:00:01,800\nSecond line\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "subs.mp4")
		cmd := exec.Command(testutil.FFmpeg(t), "-v", "error", "-y",
			"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=8000:duration=2", "-i", srt,
			"-map", "0", "-map", "1", "-c:a", "aac", "-b:a", "24k", "-c:s", "mov_text", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v\n%s", err, out)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := mp4ChaptersAgree(t, raw); len(got) != 0 {
			t.Errorf("a mov_text track read as chapters: %+v", got)
		}
		if got := testutil.FFprobeChapters(t, path); len(got) != 0 {
			t.Errorf("ffprobe reads chapters from the fixture: %+v", got)
		}
	})
}

// TestMP4LateFirstChapterAgreesAcrossReaders: a chapter list whose first
// chapter starts after zero, written by the tag library's own editor, which
// places it with an empty edit and runs the media on by that start.
func TestMP4LateFirstChapterAgreesAcrossReaders(t *testing.T) {
	want := []container.Chapter{
		{Start: 500 * time.Millisecond, End: 1200 * time.Millisecond, Title: "Late"},
		{Start: 1200 * time.Millisecond, End: 2 * time.Second, Title: "Second"},
	}
	t.Run("written by the tag library", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "late.m4a")
		if err := os.WriteFile(path, mp4Movie{movieTimescale: 1000}.build(), 0o644); err != nil {
			t.Fatal(err)
		}
		open := []container.Chapter{{Start: want[0].Start, Title: "Late"}, {Start: want[1].Start, Title: "Second"}}
		if err := label.New().Apply(t.Context(), path, &meta.Info{Chapters: open}, nil); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := mp4ChaptersAgree(t, raw); !slices.Equal(got, want) {
			t.Errorf("chapters = %+v, want %+v", got, want)
		}
	})
	// The same layout with no chpl beside it, so the track alone is read.
	t.Run("the track alone", func(t *testing.T) {
		late := mp4Text{timescale: 90000, elst: mp4LateElst(500, 1500),
			samples: [][]byte{mp4Title("Late"), mp4Title("Second")}, deltas: []uint32{63000, 72000 + 45000}}
		raw := mp4Movie{movieTimescale: 1000, chap: []uint32{2}, text: []mp4Text{late}}.build()
		if got := mp4ChaptersAgree(t, raw); !slices.Equal(got, want) {
			t.Errorf("chapters = %+v, want %+v", got, want)
		}
	})
}

// TestFFprobeReadsALateChapterTrack: the progressive muxer's chapter track,
// with a first chapter after zero, read by a third party. Two of ffprobe's
// numbers pull against each other there. It takes a chapter track's media
// duration for the last chapter's end, which wants the media run on by the
// first start; and it counts a track that ends less than a second past the
// audio into the file's duration, which a run-on can lengthen. So the file's
// duration is the audio's in every shape, and the last chapter ends where
// it was written to wherever the muxer runs the media on: where the track
// then ends no later than the movie, or two seconds and more past it. Our
// own readers end it there in every shape.
func TestFFprobeReadsALateChapterTrack(t *testing.T) {
	testutil.FFprobe(t)
	const movie = 6 * time.Second
	wav := rampWAV(t, 8000, 1, 48000)
	for _, tc := range []struct {
		lead, end time.Duration // the first start, and the last end (the movie's when open)
		open      bool
		runOn     bool
	}{
		// An open last chapter runs to the movie's end, so the track would
		// end past it by the first start.
		{500 * time.Millisecond, movie, true, false},
		{1500 * time.Millisecond, movie, true, false},
		{2500 * time.Millisecond, movie, true, true},
		// A stated end moves where the track would end.
		{2500 * time.Millisecond, 5 * time.Second, false, false}, // 1.5 s past the movie
		{2500 * time.Millisecond, 5900 * time.Millisecond, false, true},
		{500 * time.Millisecond, 4 * time.Second, false, true}, // short of the movie's end
		{2500 * time.Millisecond, 3600 * time.Millisecond, false, false},
	} {
		t.Run(tc.lead.String()+" to "+tc.end.String(), func(t *testing.T) {
			second := tc.lead + time.Second
			want := []container.Chapter{
				{Start: tc.lead, End: second, Title: "Late"},
				{Start: second, End: tc.end, Title: "Second"},
			}
			handed := []container.Chapter{{Start: tc.lead, Title: "Late"}, {Start: second, End: tc.end, Title: "Second"}}
			if tc.open {
				handed[1].End = 0
			}
			out := filepath.Join(t.TempDir(), "out.m4a")
			f, err := os.Create(out)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := waxflow.New().Transcode(context.Background(), container.BytesSource(wav), "wav", f, waxflow.TranscodeOptions{
				Format: "alac", Container: "progressive",
			}, waxflow.WithChapters(handed)); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			// Both of our readers first, with the chpl beside the track and
			// without.
			raw, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if got := mp4ChaptersAgree(t, raw); !slices.Equal(got, want) {
				t.Errorf("chapters = %+v, want %+v", got, want)
			}
			stripChpl(t, out)
			if raw, err = os.ReadFile(out); err != nil {
				t.Fatal(err)
			}
			if got := mp4ChaptersAgree(t, raw); !slices.Equal(got, want) {
				t.Errorf("chapters from the track alone = %+v, want %+v", got, want)
			}

			if dur := testutil.FFprobeFormatDuration(t, out); math.Abs(dur-movie.Seconds()) > 0.002 {
				t.Errorf("ffprobe reports a file of %.3f s, want the audio's %.3f", dur, movie.Seconds())
			}
			got := testutil.FFprobeChapters(t, out)
			if len(got) != 2 {
				t.Fatalf("ffprobe read %d chapters from the text track, want 2", len(got))
			}
			for i, ch := range got {
				if ch.Start != want[i].Start || ch.Title != want[i].Title {
					t.Errorf("ffprobe chapter %d = %+v, want %+v", i, ch, want[i])
				}
			}
			// Without the run-on ffprobe ends the last chapter where the
			// media does, short by the first start or wherever it then falls
			// back to: that end is what the muxer trades for the duration,
			// and is not held to anything here.
			if got[0].End != want[0].End || tc.runOn && got[1].End != tc.end {
				t.Errorf("ffprobe ends the chapters at %v and %v, want %v and %v", got[0].End, got[1].End, want[0].End, tc.end)
			}
		})
	}
}

// TestMP4ChapterTitleEncodingsAgreeAcrossReaders: titles that are not plain
// UTF-8, in a chapter track, in a chpl, and in a track beside a chpl.
func TestMP4ChapterTitleEncodingsAgreeAcrossReaders(t *testing.T) {
	for _, tc := range []struct {
		name, raw, track, chpl string
	}{
		{"utf-16 little endian", "\xff\xfeC\x00a\x00f\x00\xe9\x00", "Café", "Café"},
		{"utf-16 big endian", "\xfe\xff\x00C\x00a\x00f\x00\xe9", "Café", "Café"},
		{"cut at the first NUL", "One\x00junk", "One", "One"},
		{"spaces kept", " One ", " One ", " One "},
		{"mac os roman", "Caf\x8e", "Café", "CafŽ"},
		{"windows-1252", "Caf\xe9", "CafÈ", "Café"},
		{"empty", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := mp4Text{samples: [][]byte{mp4Title(tc.raw), mp4Title("Two")}, deltas: []uint32{1000, 1000}}
			chpl := mp4Chpl(0, tc.raw, 10_000_000, "Two")
			for _, shape := range []struct {
				name  string
				movie mp4Movie
				want  string
			}{
				{"track", mp4Movie{chap: []uint32{2}, text: []mp4Text{text}}, tc.track},
				{"chpl", mp4Movie{udta: [][]byte{chpl}}, tc.chpl},
				{"both", mp4Movie{chap: []uint32{2}, text: []mp4Text{text}, udta: [][]byte{chpl}}, tc.chpl},
			} {
				got := mp4ChaptersAgree(t, shape.movie.build())
				if len(got) != 2 || got[0].Title != shape.want {
					t.Errorf("%s: chapters = %+q, want the first titled %+q", shape.name, got, shape.want)
				}
			}
		})
	}
}

// TestMP4ChapterStoresAgreeAcrossReaders: the shapes where the two stores
// have to be weighed against each other.
func TestMP4ChapterStoresAgreeAcrossReaders(t *testing.T) {
	two := mp4Text{samples: [][]byte{mp4Title("One"), mp4Title("Two")}, deltas: []uint32{1000, 1000}}
	for _, tc := range []struct {
		name  string
		movie mp4Movie
		want  []string
	}{
		{"several chpl atoms", mp4Movie{udta: [][]byte{mp4Join(
			mp4Chpl(0, "One", 10_000_000, "Two"), mp4Chpl(0, "X", 5_000_000, "Y", 15_000_000, "Z"))}}, []string{"One", "Two"}},
		{"an empty chpl ahead of one that holds chapters", mp4Movie{udta: [][]byte{mp4Join(
			mp4Chpl(), mp4Chpl(0, "One", 10_000_000, "Two"))}}, []string{"One", "Two"}},
		{"empty track titles beside a chpl", mp4Movie{chap: []uint32{2},
			text: []mp4Text{{samples: [][]byte{mp4Title(""), mp4Title("")}, deltas: []uint32{1000, 1000}}},
			udta: [][]byte{mp4Chpl(0, "One", 10_000_000, "Two")}}, []string{"One", "Two"}},
		{"an unnamed text track beside a chpl", mp4Movie{text: []mp4Text{two},
			udta: [][]byte{mp4Chpl(0, "ChplOne", 10_000_000, "ChplTwo")}}, []string{"ChplOne", "ChplTwo"}},
		{"a track that disagrees with the chpl", mp4Movie{chap: []uint32{2}, text: []mp4Text{two},
			udta: [][]byte{mp4Chpl(0, "Uno", 10_000_000, "Dos")}}, []string{"One", "Two"}},
		{"a late first chapter off the track's grid", mp4Movie{chap: []uint32{2},
			text: []mp4Text{{elst: mp4LateElst(4001, 11999), samples: [][]byte{mp4Title("One"), mp4Title("Two")}, deltas: []uint32{700, 800}}}},
			[]string{"One", "Two"}},
		{"samples too short for a title", mp4Movie{chap: []uint32{2},
			text: []mp4Text{{samples: [][]byte{{0}, mp4Title("Two"), {0, 9, 'a'}}, deltas: []uint32{700, 800, 500}}}},
			[]string{"", "Two", "a"}},
		{"deltas of zero", mp4Movie{chap: []uint32{2},
			text: []mp4Text{{samples: [][]byte{mp4Title("One"), mp4Title("Two"), mp4Title("Three")}, deltas: []uint32{0, 800, 0}}}},
			[]string{"One", "Two", "Three"}},
		{"an open tail at the movie's end", mp4Movie{chap: []uint32{2},
			text: []mp4Text{{samples: [][]byte{mp4Title("One"), mp4Title("Tail")}, deltas: []uint32{2000, 1}}}},
			[]string{"One", "Tail"}},
		// A chapter track is titles and times: its sample description has no
		// say, and a title the file no longer holds is still a chapter.
		{"a chapter track with no stsd", mp4Movie{chap: []uint32{2},
			text: []mp4Text{{stsd: []byte{}, samples: two.samples, deltas: two.deltas}}}, []string{"One", "Two"}},
		{"a chapter track with an stsd of nothing", mp4Movie{chap: []uint32{2},
			text: []mp4Text{{stsd: mp4FullBox("stsd", mp4U32(0)), samples: two.samples, deltas: two.deltas}}}, []string{"One", "Two"}},
		{"chapter samples past the end of the file", mp4Movie{chap: []uint32{2},
			text: []mp4Text{{pastEOF: true, samples: two.samples, deltas: two.deltas}}}, []string{"", ""}},
		// A second table does not restate the first, at whichever level it
		// sits, and does not stand in for a first that gives none.
		{"a second stbl", mp4Movie{chap: []uint32{2}, text: []mp4Text{{samples: two.samples, deltas: two.deltas,
			again: []uint32{3000, 3000}, againAt: "stbl"}}}, []string{"One", "Two"}},
		{"a second minf", mp4Movie{chap: []uint32{2}, text: []mp4Text{{samples: two.samples, deltas: two.deltas,
			again: []uint32{3000, 3000}, againAt: "minf"}}}, []string{"One", "Two"}},
		{"a second mdia", mp4Movie{chap: []uint32{2}, text: []mp4Text{{samples: two.samples, deltas: two.deltas,
			again: []uint32{3000, 3000}, againAt: "mdia"}}}, []string{"One", "Two"}},
		{"a second stbl behind one sized by stz2", mp4Movie{chap: []uint32{2}, text: []mp4Text{{samples: two.samples, deltas: two.deltas,
			stz2: true, again: []uint32{3000, 3000}, againAt: "stbl"}}}, nil},
		// The chapter track's runs need not be in order or start at chunk 1,
		// and only stsz sizes it: the library reads no stz2.
		{"a first chunk of zero", mp4Movie{chap: []uint32{2}, text: []mp4Text{{samples: two.samples, deltas: two.deltas,
			stsc: mp4FullBox("stsc", mp4U32(1), mp4U32(0), mp4U32(2), mp4U32(1))}}}, []string{"One", "Two"}},
		{"sample-to-chunk runs out of order", mp4Movie{chap: []uint32{2}, text: []mp4Text{{samples: two.samples, deltas: two.deltas,
			stsc: mp4FullBox("stsc", mp4U32(2), mp4U32(1), mp4U32(2), mp4U32(1), mp4U32(1), mp4U32(2), mp4U32(1))}}}, []string{"One", "Two"}},
		{"a chapter track sized by stz2", mp4Movie{chap: []uint32{2},
			text: []mp4Text{{stz2: true, samples: two.samples, deltas: two.deltas}}}, nil},
		{"chapter samples past the end of the file, beside a chpl", mp4Movie{chap: []uint32{2},
			text: []mp4Text{{pastEOF: true, samples: two.samples, deltas: two.deltas}},
			udta: [][]byte{mp4Chpl(0, "One", 10_000_000, "Two")}}, []string{"One", "Two"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mp4ChaptersAgree(t, tc.movie.build())
			var titles []string
			for _, ch := range got {
				titles = append(titles, ch.Title)
			}
			if !slices.Equal(titles, tc.want) {
				t.Errorf("titles = %q, want %q", titles, tc.want)
			}
		})
	}
}
