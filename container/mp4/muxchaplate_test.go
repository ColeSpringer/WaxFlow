package mp4

import (
	"testing"
	"time"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

// chapterTrakTimes reads the durations a muxed chapter track states: its
// media duration and the sum of its stts deltas in chapter ticks, and its
// tkhd duration and the edit that plays the media in movie ticks.
func chapterTrakTimes(t *testing.T, file []byte) (media, deltas, tkhd, played int64) {
	t.Helper()
	traks := traksForTest(t, findBoxForTest(t, file, "moov"))
	if len(traks) != 2 {
		t.Fatalf("movie has %d tracks, want audio plus chapters", len(traks))
	}
	trak := traks[1]
	mdia := findBoxForTest(t, trak, "mdia")
	_, media = mdhdTime(findBoxForTest(t, mdia, "mdhd"))
	stbl := findBoxForTest(t, findBoxForTest(t, mdia, "minf"), "stbl")
	stts, err := parseStts(findBoxForTest(t, stbl, "stts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range stts {
		deltas += e.count * e.delta
	}
	tkhd = tkhdDuration(t, findBoxForTest(t, trak, "tkhd"))
	tr := &track{editMedia: -1}
	parseElst(tr, findBoxForTest(t, findBoxForTest(t, trak, "edts"), "elst"))
	return media, deltas, tkhd, tr.editSegDur
}

// TestLateFirstChapterRunsTheMediaOn: a chapter track whose first chapter
// starts late is placed by an empty edit, and its media may run on by that
// start so it ends where the last chapter does on the movie timeline.
// ffmpeg takes the media duration for that end; with media only as long as
// the chapters it reads the last chapter short by the first one's start.
// The edit that plays the media keeps the chapters' own span, and the track
// header the movie's.
//
// Run on, the track ends at the last chapter's end plus the first start,
// and ffmpeg counts a track that ends less than a second past the audio
// into the file's duration. A file reported longer than it plays is the
// worse error, so the media runs on only where that end is no later than
// the movie's or two seconds and more past it. Our own reader ends the
// last chapter where it was written to end in either shape.
func TestLateFirstChapterRunsTheMediaOn(t *testing.T) {
	const npkt = 150 * 50 // 150 seconds of 20 ms packets at 48 kHz
	trailer := codec.Trailer{Samples: npkt * 960}
	chapters := []container.Chapter{
		{Start: 5 * time.Second, Title: "Prologue"},
		{Start: 65 * time.Second, Title: "Chapter One"},
		{Start: 125 * time.Second, Title: "Chapter Two"},
	}
	t.Run("to the end of the movie", func(t *testing.T) {
		file := muxChapterFile(t, chapters, npkt, trailer)
		media, deltas, tkhd, played := chapterTrakTimes(t, file)
		if media != 150_000 || deltas != 150_000 {
			t.Errorf("media duration = %d ms, stts sums to %d; want both the movie's 150000", media, deltas)
		}
		if want := int64(145 * 48000); played != want {
			t.Errorf("the edit plays %d movie ticks, want the chapters' own %d", played, want)
		}
		if want := int64(150 * 48000); tkhd != want {
			t.Errorf("tkhd duration = %d movie ticks, want the movie's %d", tkhd, want)
		}
		for _, raw := range [][]byte{file, dropChpl(t, file)} {
			got := openFx(t, raw).Chapters()
			if len(got) != 3 || got[0].Start != 5*time.Second || got[2].Start != 125*time.Second || got[2].End != 150*time.Second {
				t.Errorf("read back %+v, want the chapters as written, the last ending at 150 s", got)
			}
		}
	})
	t.Run("to a stated end", func(t *testing.T) {
		ended := append([]container.Chapter(nil), chapters...)
		ended[2].End = 140 * time.Second
		file := muxChapterFile(t, ended, npkt, trailer)
		if media, deltas, _, played := chapterTrakTimes(t, file); media != 140_000 || deltas != 140_000 || played != 135*48000 {
			t.Errorf("media %d ms, stts %d, edit %d ticks; want 140000, 140000 and %d", media, deltas, played, 135*48000)
		}
		if got := openFx(t, dropChpl(t, file)).Chapters(); len(got) != 3 || got[2].End != 140*time.Second {
			t.Errorf("read back %+v, want the last chapter ending at 140 s", got)
		}
	})
	for _, tc := range []struct {
		lead  time.Duration
		media int64 // the track's media duration, in milliseconds
	}{
		{500 * time.Millisecond, 149_500},
		{1999 * time.Millisecond, 148_001},
		{2 * time.Second, 150_000},
	} {
		t.Run("a first start of "+tc.lead.String(), func(t *testing.T) {
			early := append([]container.Chapter(nil), chapters...)
			early[0].Start = tc.lead
			file := muxChapterFile(t, early, npkt, trailer)
			media, deltas, tkhd, played := chapterTrakTimes(t, file)
			if media != tc.media || deltas != tc.media {
				t.Errorf("media duration = %d ms, stts sums to %d; want both %d", media, deltas, tc.media)
			}
			if want := (150*time.Second - tc.lead).Milliseconds() * 48; played != want || tkhd != 150*48000 {
				t.Errorf("the edit plays %d movie ticks under a tkhd of %d, want %d and %d", played, tkhd, want, 150*48000)
			}
			for _, raw := range [][]byte{file, dropChpl(t, file)} {
				got := openFx(t, raw).Chapters()
				if len(got) != 3 || got[0].Start != tc.lead || got[2].End != 150*time.Second {
					t.Errorf("read back %+v, want a first start of %v and a last end of 150 s", got, tc.lead)
				}
			}
		})
	}
	// What decides is where the track would end run on, the last chapter's
	// end plus the first one's start, against the movie's 150 s: no later,
	// or two seconds and more past it.
	for _, tc := range []struct {
		lead, end time.Duration
		runOn     bool
	}{
		{5 * time.Second, 145 * time.Second, true},
		{5 * time.Second, 145*time.Second + time.Millisecond, false},
		{5 * time.Second, 147*time.Second - time.Millisecond, false},
		{5 * time.Second, 147 * time.Second, true},
		{2500 * time.Millisecond, 148 * time.Second, false},
		{500 * time.Millisecond, 149500 * time.Millisecond, true},
		{500 * time.Millisecond, 149501 * time.Millisecond, false},
	} {
		t.Run("a first start of "+tc.lead.String()+" and a last end of "+tc.end.String(), func(t *testing.T) {
			ended := append([]container.Chapter(nil), chapters...)
			ended[0].Start, ended[2].End = tc.lead, tc.end
			file := muxChapterFile(t, ended, npkt, trailer)
			want := tc.end.Milliseconds()
			if !tc.runOn {
				want -= tc.lead.Milliseconds()
			}
			if media, deltas, _, _ := chapterTrakTimes(t, file); media != want || deltas != want {
				t.Errorf("media duration = %d ms, stts sums to %d; want both %d", media, deltas, want)
			}
			for _, raw := range [][]byte{file, dropChpl(t, file)} {
				got := openFx(t, raw).Chapters()
				if len(got) != 3 || got[0].Start != tc.lead || got[2].End != tc.end {
					t.Errorf("read back %+v, want a first start of %v and a last end of %v", got, tc.lead, tc.end)
				}
			}
		})
	}
	// A last chapter that starts where the movie ends has no end to carry:
	// its one tick stays one tick, and it reads back open.
	t.Run("an open tail is left alone", func(t *testing.T) {
		tail := append(append([]container.Chapter(nil), chapters[:2]...), container.Chapter{Start: 150 * time.Second, Title: "Tail"})
		file := muxChapterFile(t, tail, npkt, trailer)
		if media, deltas, _, _ := chapterTrakTimes(t, file); media != 145_001 || deltas != 145_001 {
			t.Errorf("media %d ms, stts %d; want the chapters' own 145001", media, deltas)
		}
		got := openFx(t, dropChpl(t, file)).Chapters()
		if len(got) != 3 || got[2].Start != 150*time.Second || got[2].End != 0 {
			t.Errorf("read back %+v, want an open last chapter at 150 s", got)
		}
	})
}
