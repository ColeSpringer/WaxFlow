package mp4

import (
	"slices"
	"testing"
	"time"

	"github.com/colespringer/waxflow/container"
)

const ms = time.Millisecond

// twoChapters is a chapter track of two one-second titles, named 2.
func twoChapters(first, second string) fxText {
	return fxText{samples: [][]byte{textSample(first), textSample(second)}, deltas: []uint32{1000, 1000}}
}

func wantChapterList(t *testing.T, got, want []container.Chapter) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("chapters = %+v, want %+v", got, want)
	}
}

// TestSubtitleTrackIsNotChapters: a text track is chapters only when another
// track's tref chap entry names it. One nothing names is subtitles, and so
// is one that only its own list names.
func TestSubtitleTrackIsNotChapters(t *testing.T) {
	subs := fxText{handler: "sbtl", samples: [][]byte{textSample(""), textSample("Hello subtitle")}, deltas: []uint32{200, 700}}
	t.Run("unnamed", func(t *testing.T) {
		if got := openFx(t, fxMovie{text: []fxText{subs}}.build()).Chapters(); len(got) != 0 {
			t.Errorf("a subtitle track read as %d chapters: %+v", len(got), got)
		}
	})
	t.Run("named by itself alone", func(t *testing.T) {
		own := subs
		own.chap = []uint32{2}
		if got := openFx(t, fxMovie{text: []fxText{own}}.build()).Chapters(); len(got) != 0 {
			t.Errorf("a track only its own list names read as %d chapters: %+v", len(got), got)
		}
	})
	t.Run("beside a chpl", func(t *testing.T) {
		raw := fxMovie{text: []fxText{twoChapters("One", "Two")},
			udta: [][]byte{fxChpl(fxChplEntry{0, "ChplOne"}, fxChplEntry{10_000_000, "ChplTwo"})}}.build()
		wantChapterList(t, openFx(t, raw).Chapters(), []container.Chapter{
			{Start: 0, End: time.Second, Title: "ChplOne"}, {Start: time.Second, Title: "ChplTwo"}})
	})
	// The control: the same track, named, is the chapter track, and a track
	// other than the sound track may be the one that names it.
	t.Run("named by the sound track", func(t *testing.T) {
		raw := fxMovie{chap: []uint32{2}, text: []fxText{twoChapters("One", "Two")}}.build()
		wantChapterList(t, openFx(t, raw).Chapters(), []container.Chapter{
			{Start: 0, End: time.Second, Title: "One"}, {Start: time.Second, End: 2 * time.Second, Title: "Two"}})
	})
	t.Run("named by another text track", func(t *testing.T) {
		other := fxText{id: 3, chap: []uint32{2}, samples: [][]byte{textSample("x")}, deltas: []uint32{2000}}
		raw := fxMovie{text: []fxText{twoChapters("One", "Two"), other}}.build()
		wantChapterList(t, openFx(t, raw).Chapters(), []container.Chapter{
			{Start: 0, End: time.Second, Title: "One"}, {Start: time.Second, End: 2 * time.Second, Title: "Two"}})
	})
}

// TestLateFirstChapterEndsWithItsEdits: a writer that places a late first
// chapter with an empty edit may run the media on by that start, so the
// media ends where the movie does. The last chapter then ends with the edit
// that plays the media, not a start later.
func TestLateFirstChapterEndsWithItsEdits(t *testing.T) {
	// 90 kHz media over a 1 kHz movie: chapters at 0.5 s and 1.2 s of a two
	// second movie, the last delta run on by the half second.
	late := fxText{timescale: 90000, elst: chapElst(500, 1500),
		samples: [][]byte{textSample("Late"), textSample("Second")},
		deltas:  []uint32{63000, 72000 + 45000}}
	want := []container.Chapter{
		{Start: 500 * ms, End: 1200 * ms, Title: "Late"},
		{Start: 1200 * ms, End: 2000 * ms, Title: "Second"},
	}
	raw := fxMovie{movieTimescale: 1000, chap: []uint32{2}, text: []fxText{late}}.build()
	wantChapterList(t, openFx(t, raw).Chapters(), want)

	// A track whose media is only as long as its chapters ends there too.
	late.deltas = []uint32{63000, 72000}
	raw = fxMovie{movieTimescale: 1000, chap: []uint32{2}, text: []fxText{late}}.build()
	wantChapterList(t, openFx(t, raw).Chapters(), want)
}

// TestOpenLastChapterReadsOpen: a last chapter that starts at the movie's
// end was given one tick by its writer, which is no end.
func TestOpenLastChapterReadsOpen(t *testing.T) {
	tail := fxText{samples: [][]byte{textSample("One"), textSample("Tail")}, deltas: []uint32{2000, 1}}
	raw := fxMovie{chap: []uint32{2}, text: []fxText{tail}}.build()
	wantChapterList(t, openFx(t, raw).Chapters(), []container.Chapter{
		{Start: 0, End: 2 * time.Second, Title: "One"}, {Start: 2 * time.Second, Title: "Tail"}})
}

// TestChapterTitleDecoding pins how a title's bytes become text, in a
// chapter track and in a chpl: UTF-16 behind either byte-order mark, an
// 8-bit title cut at its first NUL, bytes that are not UTF-8 through a code
// page, and spaces kept.
func TestChapterTitleDecoding(t *testing.T) {
	for _, tc := range []struct {
		name, raw, track, chpl string
	}{
		{"utf-16 little endian", "\xff\xfeC\x00a\x00f\x00\xe9\x00", "Café", "Café"},
		{"utf-16 big endian", "\xfe\xff\x00C\x00a\x00f\x00\xe9", "Café", "Café"},
		{"utf-16 to its first NUL unit", "\xfe\xff\x00O\x00n\x00e\x00\x00\x00x", "One", "One"},
		{"cut at the first NUL", "One\x00junk", "One", "One"},
		{"spaces kept", " One ", " One ", " One "},
		// 0x8E is e-acute in Mac OS Roman, which a chapter track on its own
		// is read as; 0xE9 is e-acute in Windows-1252, a Nero list's page.
		{"legacy 8-bit", "Caf\x8e", "Café", "CafŽ"},
		{"windows-1252", "Caf\xe9", "CafÈ", "Café"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			track := fxMovie{chap: []uint32{2}, text: []fxText{twoChapters(tc.raw, "Two")}}.build()
			if got := openFx(t, track).Chapters(); len(got) != 2 || got[0].Title != tc.track {
				t.Errorf("chapter track title = %+q, want %+q", got, tc.track)
			}
			chpl := fxMovie{udta: [][]byte{fxChpl(fxChplEntry{0, tc.raw}, fxChplEntry{10_000_000, "Two"})}}.build()
			if got := openFx(t, chpl).Chapters(); len(got) != 2 || got[0].Title != tc.chpl {
				t.Errorf("chpl title = %+q, want %+q", got, tc.chpl)
			}
		})
	}
	// A track beside a list of 8-bit titles holds the same bytes as the
	// list, so both are read through the list's page and agree.
	t.Run("a track beside a legacy chpl", func(t *testing.T) {
		raw := fxMovie{chap: []uint32{2}, text: []fxText{twoChapters("Caf\xe9", "Two")},
			udta: [][]byte{fxChpl(fxChplEntry{0, "Caf\xe9"}, fxChplEntry{10_000_000, "Two"})}}.build()
		wantChapterList(t, openFx(t, raw).Chapters(), []container.Chapter{
			{Start: 0, End: time.Second, Title: "Café"}, {Start: time.Second, End: 2 * time.Second, Title: "Two"}})
	})
}

// TestFirstChplHoldingChaptersIsRead: of several chpl atoms the first that
// holds chapters is the list, and one cut short inside an entry holds none.
func TestFirstChplHoldingChaptersIsRead(t *testing.T) {
	first := fxChpl(fxChplEntry{0, "One"}, fxChplEntry{10_000_000, "Two"})
	second := fxChpl(fxChplEntry{0, "X"}, fxChplEntry{5_000_000, "Y"}, fxChplEntry{15_000_000, "Z"})
	want := []container.Chapter{{Start: 0, End: time.Second, Title: "One"}, {Start: time.Second, Title: "Two"}}

	wantChapterList(t, openFx(t, fxMovie{udta: [][]byte{joinBytes(first, second)}}.build()).Chapters(), want)

	// A list that declares three entries and holds two and a half is none,
	// and the next one is read.
	cut := fxChpl(fxChplEntry{0, "A"}, fxChplEntry{5_000_000, "B"}, fxChplEntry{15_000_000, "Cut short"})
	cut = cut[:len(cut)-4]
	cut[3] = byte(len(cut)) // the box's own size, which fits one byte here
	wantChapterList(t, openFx(t, fxMovie{udta: [][]byte{joinBytes(cut, first)}}.build()).Chapters(), want)

	// An empty list is none either.
	wantChapterList(t, openFx(t, fxMovie{udta: [][]byte{joinBytes(fxChpl(), first)}}.build()).Chapters(), want)
}

// TestChplTitlesFillAnAgreeingTrack: where the two stores agree, the chpl
// gives the titles and starts and the track the last end. A track title
// that reads empty agrees with the chpl's.
func TestChplTitlesFillAnAgreeingTrack(t *testing.T) {
	chpl := fxChpl(fxChplEntry{0, "One"}, fxChplEntry{10_004_000, "Two"})
	t.Run("empty track titles", func(t *testing.T) {
		raw := fxMovie{chap: []uint32{2}, text: []fxText{twoChapters("", "")}, udta: [][]byte{chpl}}.build()
		wantChapterList(t, openFx(t, raw).Chapters(), []container.Chapter{
			{Start: 0, End: 1000400 * time.Microsecond, Title: "One"},
			{Start: 1000400 * time.Microsecond, End: 2 * time.Second, Title: "Two"}})
	})
	// Starts half a second apart are not the same chapters: the track wins.
	t.Run("a disagreeing track wins", func(t *testing.T) {
		far := fxText{samples: [][]byte{textSample("One"), textSample("Two")}, deltas: []uint32{400, 1600}}
		raw := fxMovie{chap: []uint32{2}, text: []fxText{far}, udta: [][]byte{chpl}}.build()
		wantChapterList(t, openFx(t, raw).Chapters(), []container.Chapter{
			{Start: 0, End: 400 * ms, Title: "One"}, {Start: 400 * ms, End: 2 * time.Second, Title: "Two"}})
	})
	t.Run("a differently titled track wins", func(t *testing.T) {
		raw := fxMovie{chap: []uint32{2}, text: []fxText{twoChapters("Uno", "Dos")}, udta: [][]byte{chpl}}.build()
		wantChapterList(t, openFx(t, raw).Chapters(), []container.Chapter{
			{Start: 0, End: time.Second, Title: "Uno"}, {Start: time.Second, End: 2 * time.Second, Title: "Dos"}})
	})
}

// TestChapterTrackNeedsNoSampleDescription: a chapter track is titles and
// times, so one whose stsd is missing or holds no entry still reads.
func TestChapterTrackNeedsNoSampleDescription(t *testing.T) {
	want := []container.Chapter{
		{Start: 0, End: time.Second, Title: "One"}, {Start: time.Second, End: 2 * time.Second, Title: "Two"}}
	for name, stsd := range map[string][]byte{
		"no stsd":            {},
		"an stsd of nothing": makeFullBox("stsd", 0, 0, u32(0)),
		"a cut-off entry":    makeFullBox("stsd", 0, 0, u32(1), []byte{0, 0, 0, 40, 't', 'e', 'x', 't'}),
	} {
		t.Run(name, func(t *testing.T) {
			track := twoChapters("One", "Two")
			track.stsd = stsd
			wantChapterList(t, openFx(t, fxMovie{chap: []uint32{2}, text: []fxText{track}}.build()).Chapters(), want)
		})
	}
}

// TestChapterPastEOFKeepsItsTime: a chapter whose sample the file no longer
// holds, as a download cut short leaves it, is a chapter with no title. The
// list is not cut there, and beside a chpl the track still gives the last
// chapter's end.
func TestChapterPastEOFKeepsItsTime(t *testing.T) {
	track := twoChapters("One", "Two")
	track.pastEOF = true
	wantChapterList(t, openFx(t, fxMovie{chap: []uint32{2}, text: []fxText{track}}.build()).Chapters(), []container.Chapter{
		{Start: 0, End: time.Second}, {Start: time.Second, End: 2 * time.Second}})

	raw := fxMovie{chap: []uint32{2}, text: []fxText{track},
		udta: [][]byte{fxChpl(fxChplEntry{0, "One"}, fxChplEntry{10_000_000, "Two"})}}.build()
	wantChapterList(t, openFx(t, raw).Chapters(), []container.Chapter{
		{Start: 0, End: time.Second, Title: "One"}, {Start: time.Second, End: 2 * time.Second, Title: "Two"}})
}

// TestChapterTrackTablesAreReadLeniently: a chapter track's sample-to-chunk
// runs are taken as written, with a first chunk of 0 or out of order, where
// a sound track's table is refused for either. Only stsz sizes a chapter
// track: one sized by stz2 has no chapters.
func TestChapterTrackTablesAreReadLeniently(t *testing.T) {
	want := []container.Chapter{
		{Start: 0, End: time.Second, Title: "One"}, {Start: time.Second, End: 2 * time.Second, Title: "Two"}}
	for name, stsc := range map[string][]byte{
		"a first chunk of zero": makeFullBox("stsc", 0, 0, u32(1), u32(0), u32(2), u32(1)),
		"runs out of order":     makeFullBox("stsc", 0, 0, u32(2), u32(1), u32(2), u32(1), u32(1), u32(2), u32(1)),
	} {
		t.Run(name, func(t *testing.T) {
			track := twoChapters("One", "Two")
			track.stsc = stsc
			wantChapterList(t, openFx(t, fxMovie{chap: []uint32{2}, text: []fxText{track}}.build()).Chapters(), want)
		})
	}
	t.Run("sized by stz2", func(t *testing.T) {
		track := twoChapters("One", "Two")
		track.stz2 = true
		if got := openFx(t, fxMovie{chap: []uint32{2}, text: []fxText{track}}.build()).Chapters(); len(got) != 0 {
			t.Errorf("a chapter track sized by stz2 read %+v, want none", got)
		}
	})
}

// TestChapterTableIsTheFirstOne: a text track with a second stbl, minf or
// mdia is still read through its first. The second here times the same two
// titles three seconds apart.
func TestChapterTableIsTheFirstOne(t *testing.T) {
	want := []container.Chapter{
		{Start: 0, End: time.Second, Title: "One"}, {Start: time.Second, End: 2 * time.Second, Title: "Two"}}
	for _, at := range []string{"stbl", "minf", "mdia"} {
		t.Run("a second "+at, func(t *testing.T) {
			track := twoChapters("One", "Two")
			track.again, track.againAt = []uint32{3000, 3000}, at
			wantChapterList(t, openFx(t, fxMovie{chap: []uint32{2}, text: []fxText{track}}.build()).Chapters(), want)
		})
	}
	// A first stbl that gives no table is still the one read: the second
	// does not stand in for it.
	t.Run("a first stbl with no table", func(t *testing.T) {
		track := twoChapters("One", "Two")
		track.stz2, track.again, track.againAt = true, []uint32{3000, 3000}, "stbl"
		raw := fxMovie{chap: []uint32{2}, text: []fxText{track}}.build()
		if got := openFx(t, raw).Chapters(); len(got) != 0 {
			t.Errorf("chapters = %+v, want none: the first stbl is sized by stz2", got)
		}
	})
}
