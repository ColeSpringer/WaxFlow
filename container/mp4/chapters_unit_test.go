package mp4

import (
	"math"
	"slices"
	"testing"
	"time"
)

// TestParseChapterEdits: a first entry that is an empty edit is the lead,
// and the edits that play media sum to the span played, both in movie
// ticks. A 32-bit segment at its ceiling was clamped by its writer.
func TestParseChapterEdits(t *testing.T) {
	elst := func(version byte, count uint32, entries ...[]byte) []byte {
		return joinBytes(append([][]byte{{version, 0, 0, 0}, u32(count)}, entries...)...)
	}
	entry := func(seg uint32, media int32) []byte {
		return joinBytes(u32(seg), u32(uint32(media)), u32(0x00010000))
	}
	entry64 := func(seg uint64, media int64) []byte {
		return joinBytes(u64(seg), u64(uint64(media)), u32(0x00010000))
	}
	for _, tc := range []struct {
		name string
		p    []byte
		want chapterEdits
	}{
		{"an empty edit, then the media", elst(0, 2, entry(4000, -1), entry(9000, 0)), chapterEdits{4000, 9000, false}},
		{"the media alone", elst(0, 1, entry(9000, 0)), chapterEdits{0, 9000, false}},
		{"two edits that play", elst(0, 3, entry(4000, -1), entry(3000, 0), entry(2000, 5000)), chapterEdits{4000, 5000, false}},
		{"a later empty edit is no lead", elst(0, 2, entry(3000, 0), entry(4000, -1)), chapterEdits{0, 3000, false}},
		{"no entries", elst(0, 0), chapterEdits{}},
		{"a clamped lead", elst(0, 1, entry(math.MaxUint32, -1)), chapterEdits{math.MaxUint32, 0, true}},
		{"64-bit entries", elst(1, 2, entry64(1<<33, -1), entry64(9000, 0)), chapterEdits{1 << 33, 9000, false}},
		{"a count past the entries", elst(0, 9, entry(4000, -1), entry(9000, 0)), chapterEdits{4000, 9000, false}},
		{"a cut-off entry", elst(0, 2, entry(4000, -1), entry(9000, 0)[:7]), chapterEdits{4000, 0, false}},
		{"an unknown version", elst(2, 1, entry(9000, 0)), chapterEdits{}},
		{"spans that overflow", elst(1, 2, entry64(math.MaxUint64, 0), entry64(5, 0)), chapterEdits{0, math.MaxUint64, false}},
		{"a clamped span", elst(0, 2, entry(4000, -1), entry(math.MaxUint32, 0)), chapterEdits{4000, 0, false}},
		{"a clamped span among others", elst(0, 2, entry(math.MaxUint32, 0), entry(9000, 0)), chapterEdits{}},
		{"64-bit fields hold that value whole", elst(1, 2, entry64(math.MaxUint32, -1), entry64(math.MaxUint32, 0)),
			chapterEdits{math.MaxUint32, math.MaxUint32, false}},
		{"too short for a count", []byte{0, 0, 0, 0, 0}, chapterEdits{}},
	} {
		if got := parseChapterEdits(tc.p); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestTrackEnd: the last chapter ends where the track's media does on the
// movie timeline: with the edits when the media runs past them, at the
// media's own duration when that reaches the track's end, and never at or
// before the chapter's own start.
func TestTrackEnd(t *testing.T) {
	const s, us = time.Second, time.Microsecond
	slack := ticksToDuration(1, 1000) + ticksToDuration(1, 90000)
	for _, tc := range []struct {
		name                                      string
		mediaEnd, lastStart, offset, played, want time.Duration
	}{
		{"no edit list", 9 * s, 5 * s, 0, 0, 9 * s},
		{"media the edit plays whole", 7 * s, 5 * s, 2 * s, 7 * s, 9 * s},
		{"an edit rounded short of the media", 7*s + 400*us, 5 * s, 2 * s, 7 * s, 9*s + 400*us},
		{"media that reaches the track's end", 9 * s, 5 * s, 2 * s, 7 * s, 9 * s},
		{"the track's end on the finer grid", 8*s + 400*us, 5 * s, 2 * s, 6 * s, 8*s + 400*us},
		{"media an edit cuts short", 20 * s, 5 * s, 2 * s, 7 * s, 9 * s},
		{"media an edit cuts short, no lead", 20 * s, 5 * s, 0, 7 * s, 7 * s},
		{"media short of the edit", 4 * s, 3 * s, 2 * s, 7 * s, 6 * s},
		{"media short of the edit, no lead", 4 * s, 3 * s, 0, 7 * s, 4 * s},
		{"a lead of one unit, media to the track's end", 8 * s, 5 * s, ms, 7999 * ms, 8 * s},
		{"a lead of one unit, media the edit plays whole", 7999 * ms, 5 * s, ms, 7999 * ms, 8 * s},
		{"edits that end before the last start", 10 * s, 8 * s, 0, 5 * s, 10 * s},
		{"edits that end at the last start", 10 * s, 7 * s, 2 * s, 5 * s, 12 * s},
		{"media duration at the last start", 4*s + ms, 4*s + ms, 4*s + ms, ms, 8*s + 2*ms},
		{"a sum past the clamp", math.MaxInt64, 5 * s, s, 0, math.MaxInt64},
	} {
		if got := trackEnd(tc.mediaEnd, tc.lastStart, tc.offset, tc.played, slack); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestPlaceholderTail covers both tails a writer gives an open last
// chapter: one media tick, and the whole second older writers used.
func TestPlaceholderTail(t *testing.T) {
	const mts, movieTS, movieDur = 90000, 1000, 9000 // a 9 s movie
	for _, tc := range []struct {
		name  string
		start time.Duration
		delta uint64
		want  bool
	}{
		{"one tick past the duration", 10 * time.Second, 1, true},
		{"one second past the duration", 10 * time.Second, mts, true},
		{"a real end past the duration", 10 * time.Second, 2 * mts, false},
		{"one tick within the duration", 5 * time.Second, 1, false},
		{"exactly at the duration", 9 * time.Second, 1, true},
	} {
		if got := placeholderTail(tc.start, tc.delta, mts, movieTS, movieDur); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	if placeholderTail(10*time.Second, 1, mts, 0, 0) {
		t.Error("an unknown movie duration must not recognize a placeholder")
	}
}

// TestTicksToDuration: a tick count is rounded to the nearest nanosecond
// and clamped, never wrapped.
func TestTicksToDuration(t *testing.T) {
	for _, tc := range []struct {
		units     uint64
		timescale int64
		want      time.Duration
	}{
		{0, 1000, 0},
		{1500, 1000, 1500 * ms},
		{44144, 44100, 1000997732},
		{1, 3, 333333333},
		{2, 3, 666666667},
		{10_004_000, chplStartUnit, 1000400 * time.Microsecond},
		{math.MaxUint64, 1, math.MaxInt64},
		{5, 0, 0},
		{5, -1, 0},
	} {
		if got := ticksToDuration(tc.units, tc.timescale); got != tc.want {
			t.Errorf("%d ticks of %d = %v, want %v", tc.units, tc.timescale, got, tc.want)
		}
	}
	if got := addClamp(math.MaxInt64-1, 5); got != math.MaxInt64 {
		t.Errorf("addClamp past the top = %v, want the largest Duration", got)
	}
}

// TestParseChplShapes covers the list's two layouts and its refusals.
func TestParseChplShapes(t *testing.T) {
	entry := joinBytes(u64(10_000_000), []byte{3}, []byte("Two"))
	for _, tc := range []struct {
		name    string
		payload []byte
		titles  []string
		ok      bool
	}{
		{"version 1, a reserved word", joinBytes([]byte{1, 0, 0, 0}, u32(0), []byte{1}, entry), []string{"Two"}, true},
		{"version 0, no reserved word", joinBytes([]byte{0, 0, 0, 0}, []byte{1}, entry), []string{"Two"}, true},
		{"no entries", joinBytes([]byte{1, 0, 0, 0}, u32(0), []byte{0}), nil, true},
		{"a count past the entries", joinBytes([]byte{1, 0, 0, 0}, u32(0), []byte{2}, entry), nil, false},
		{"a title cut short", joinBytes([]byte{1, 0, 0, 0}, u32(0), []byte{1}, entry[:len(entry)-1]), nil, false},
		{"no room for a count", []byte{1, 0, 0, 0, 0, 0, 0, 0}, nil, false},
		{"too short", []byte{1, 0, 0}, nil, false},
	} {
		chs, ok := parseChpl(tc.payload, &titleReader{page: &windows1252})
		if ok != tc.ok || len(chs) != len(tc.titles) {
			t.Errorf("%s: %d chapters, ok %v; want %d, %v", tc.name, len(chs), ok, len(tc.titles), tc.ok)
			continue
		}
		for i, ch := range chs {
			if ch.Title != tc.titles[i] || ch.Start != time.Second {
				t.Errorf("%s: chapter %d = %+v", tc.name, i, ch)
			}
		}
	}
}

// TestCodePagesCoverTheirHighHalf: both tables decode every byte, and agree
// with the encodings on the letters that tell them apart.
func TestCodePagesCoverTheirHighHalf(t *testing.T) {
	for name, page := range map[string]*codePage{"Mac OS Roman": &macRoman, "Windows-1252": &windows1252} {
		for i, r := range page.high {
			if r == 0 {
				t.Errorf("%s: byte %#x decodes to nothing", name, 0x80+i)
			}
		}
	}
	r := titleReader{page: &macRoman}
	if got := r.read([]byte("\x8e\xd2\xdb")); got != "é“€" || !r.legacy {
		t.Errorf("Mac OS Roman read %q (legacy %v), want é“€", got, r.legacy)
	}
	r = titleReader{page: &windows1252}
	if got := r.read([]byte("\xe9\x93\x80\x81")); got != "é“€�" || !r.legacy {
		t.Errorf("Windows-1252 read %q (legacy %v), want é“€ and a replacement for the undefined byte", got, r.legacy)
	}
	if got := r.read([]byte("plain")); got != "plain" {
		t.Errorf("ASCII read %q", got)
	}
}

// TestChapterTrackTitleBytesAreBounded: a chapter track is read only while
// its titles fit a budget, so a table of large samples cannot make the open
// read and keep without limit. Past it the track is no chapter source, and
// a chpl beside it still is.
func TestChapterTrackTitleBytesAreBounded(t *testing.T) {
	big := textSample(string(make([]byte, 1<<16-1)))
	n := maxChapterTextBytes/len(big) + 2
	track := fxText{}
	for range n {
		track.samples = append(track.samples, big)
		track.deltas = append(track.deltas, 10)
	}
	m := fxMovie{chap: []uint32{2}, text: []fxText{track}}
	if got := openFx(t, m.build()).Chapters(); len(got) != 0 {
		t.Errorf("read %d chapters past the title budget", len(got))
	}
	m.udta = [][]byte{fxChpl(fxChplEntry{0, "One"})}
	if got := openFx(t, m.build()).Chapters(); len(got) != 1 || got[0].Title != "One" {
		t.Errorf("chapters = %+v, want the chpl's", got)
	}
}

// TestTextTableLocate: samples run back to back from each chunk's offset,
// as many as the run covering the chunk says, to the count asked for.
func TestTextTableLocate(t *testing.T) {
	chunks := []int64{100, 500, 900}
	sizes := func(vs ...uint32) []byte {
		var out []byte
		for _, v := range vs {
			out = append(out, u32(v)...)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		tt   textTable
		n    int
		want []textSampleAt
	}{
		{"two runs", textTable{sizes: sizes(10, 20, 30, 40, 50), count: 5, stsc: []stscEntry{{1, 2}, {3, 1}}, chunks: chunks}, 5,
			[]textSampleAt{{100, 10}, {110, 20}, {500, 30}, {530, 40}, {900, 50}}},
		{"fewer asked for", textTable{sizes: sizes(10, 20, 30, 40, 50), count: 5, stsc: []stscEntry{{1, 2}, {3, 1}}, chunks: chunks}, 3,
			[]textSampleAt{{100, 10}, {110, 20}, {500, 30}}},
		{"one size for all", textTable{constSize: 8, count: 4, stsc: []stscEntry{{1, 3}}, chunks: chunks}, 9,
			[]textSampleAt{{100, 8}, {108, 8}, {116, 8}, {500, 8}}},
		{"a size box that holds the samples asked for", textTable{sizes: sizes(10, 20), count: 5, stsc: []stscEntry{{1, 2}}, chunks: chunks}, 2,
			[]textSampleAt{{100, 10}, {110, 20}}},
		{"a size box short of them", textTable{sizes: sizes(10), count: 5, stsc: []stscEntry{{1, 2}}, chunks: chunks}, 5, nil},
		{"a first chunk of zero", textTable{sizes: sizes(10, 20), count: 2, stsc: []stscEntry{{0, 2}}, chunks: chunks}, 2,
			[]textSampleAt{{100, 10}, {110, 20}}},
		{"runs out of order", textTable{sizes: sizes(10, 20), count: 2, stsc: []stscEntry{{2, 1}, {1, 2}}, chunks: chunks}, 2,
			[]textSampleAt{{100, 10}, {110, 20}}},
		{"a first chunk no run covers", textTable{sizes: sizes(10, 20), count: 2, stsc: []stscEntry{{2, 1}}, chunks: chunks}, 2,
			[]textSampleAt{{500, 10}, {900, 20}}},
		{"chunks that run out", textTable{sizes: sizes(10, 20, 30, 40), count: 4, stsc: []stscEntry{{1, 1}}, chunks: chunks}, 4,
			[]textSampleAt{{100, 10}, {500, 20}, {900, 30}}},
		{"no runs", textTable{sizes: sizes(10), count: 1, chunks: chunks}, 1, nil},
		{"no chunks", textTable{sizes: sizes(10), count: 1, stsc: []stscEntry{{1, 1}}}, 1, nil},
		{"none asked for", textTable{sizes: sizes(10), count: 1, stsc: []stscEntry{{1, 1}}, chunks: chunks}, 0, nil},
	} {
		if got := tc.tt.locate(tc.n); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestParseTextTable: the boxes a chapter track is read from, and the ones
// it is not.
func TestParseTextTable(t *testing.T) {
	stts := makeFullBox("stts", 0, 0, u32(1), u32(2), u32(1000))
	stsz := makeFullBox("stsz", 0, 0, u32(0), u32(2), u32(5), u32(5))
	stsc := makeFullBox("stsc", 0, 0, u32(1), u32(0), u32(2), u32(1))
	stco := makeFullBox("stco", 0, 0, u32(1), u32(100))
	co64 := makeFullBox("co64", 0, 0, u32(1), u64(200))
	stz2 := makeFullBox("stz2", 0, 0, []byte{0, 0, 0, 16}, u32(2), u16(5), u16(5))
	for _, tc := range []struct {
		name  string
		boxes [][]byte
		chunk int64 // the first chunk offset read, 0 for no table
	}{
		{"every box", [][]byte{stts, stsc, stsz, stco}, 100},
		{"wide chunk offsets", [][]byte{stts, stsc, stsz, co64}, 200},
		{"stco ahead of co64, wherever it sits", [][]byte{stts, stsc, stsz, co64, stco}, 100},
		{"the first of two", [][]byte{stts, stsc, stsz, stco, makeFullBox("stco", 0, 0, u32(1), u32(300))}, 100},
		{"sized by stz2", [][]byte{stts, stsc, stz2, stco}, 0},
		{"no stts", [][]byte{stsc, stsz, stco}, 0},
		{"no stsc", [][]byte{stts, stsz, stco}, 0},
		{"no chunk offsets", [][]byte{stts, stsc, stsz}, 0},
		{"runs past the box", [][]byte{stts, makeFullBox("stsc", 0, 0, u32(9), u32(1), u32(2), u32(1)), stsz, stco}, 0},
		{"a cut-off box", [][]byte{stts, stsc, stsz, stco, {0, 0, 0, 99, 'f', 'r', 'e', 'e'}}, 0},
	} {
		tt := parseTextTable(joinBytes(tc.boxes...))
		switch {
		case tc.chunk == 0 && tt != nil:
			t.Errorf("%s: read a table, want none", tc.name)
		case tc.chunk != 0 && (tt == nil || len(tt.chunks) != 1 || tt.chunks[0] != tc.chunk || tt.count != 2 || tt.stsc[0].first != 0):
			t.Errorf("%s: table = %+v, want one chunk at %d", tc.name, tt, tc.chunk)
		}
	}
}
