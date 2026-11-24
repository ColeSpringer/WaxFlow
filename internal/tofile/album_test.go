package tofile

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/waxerr"
)

func tagValues(tags []container.Tag, key string) []string {
	var out []string
	for _, t := range tags {
		if t.Key == key {
			out = append(out, t.Value)
		}
	}
	return out
}

// TestReadAlbumCarriesWhatDescribesAPiece: the mapper's tags fold over the
// container's, ReplayGain and the timeline stay behind, and the front cover
// rides along.
func TestReadAlbumCarriesWhatDescribesAPiece(t *testing.T) {
	art := []byte("the cover")
	m := &fakeMapper{info: &meta.Info{
		Tags: map[string][]string{
			"ALBUM": {"The Album"}, "TITLE": {"The Rip"}, "REPLAYGAIN_TRACK_GAIN": {"-3.00 dB"},
		},
		Chapters: []container.Chapter{{Start: 0, Title: "One"}, {Start: time.Second, Title: "Two"}},
		Synced:   []meta.SyncedLyrics{{Lines: []meta.SyncedLine{{Text: "la"}}}},
		Pictures: []meta.Picture{{MIME: "image/png", Front: true, Data: art}},
	}}
	var notes []Note
	a := ReadAlbum(context.Background(), m, nil, "flac",
		map[string][]string{"ARTIST": {"The Band"}, "ALBUM": {"stale"}, "REPLAYGAIN_ALBUM_GAIN": {"-4.00 dB"}},
		"flac", "", func(n Note) { notes = append(notes, n) })
	if len(notes) != 0 {
		t.Errorf("notes = %+v, want none", notes)
	}
	for key, want := range map[string]string{"ALBUM": "The Album", "ARTIST": "The Band", "TITLE": "The Rip"} {
		if got := tagValues(a.Tags, key); !slices.Equal(got, []string{want}) {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for _, tag := range a.Tags {
		if strings.HasPrefix(tag.Key, "REPLAYGAIN_") {
			t.Errorf("the album carries %s", tag.Key)
		}
	}
	if _, ok := a.Info.Tags["REPLAYGAIN_ALBUM_GAIN"]; ok {
		t.Error("the album's Info carries the container's ReplayGain")
	}
	if len(a.Info.Chapters) != 0 || len(a.Info.Synced) != 0 {
		t.Errorf("the album keeps %d chapters and %d synced lyrics", len(a.Info.Chapters), len(a.Info.Synced))
	}
	if a.Art == nil || string(a.Art.Data) != string(art) {
		t.Errorf("art = %+v, want the front cover", a.Art)
	}
}

// TestReadAlbumNotesAFailedRead: a read that fails is a note, and the
// container's tags still fold.
func TestReadAlbumNotesAFailedRead(t *testing.T) {
	readErr := errors.New("unreadable")
	var notes []Note
	a := ReadAlbum(context.Background(), &fakeMapper{readErr: readErr}, nil, "flac",
		map[string][]string{"ALBUM": {"The Album"}}, "flac", "", func(n Note) { notes = append(notes, n) })
	if len(notes) != 1 || notes[0].Kind != NoteMetadataUnread || !errors.Is(notes[0].Err, readErr) {
		t.Errorf("notes = %+v, want the one unread note", notes)
	}
	if got := tagValues(a.Tags, "ALBUM"); !slices.Equal(got, []string{"The Album"}) {
		t.Errorf("ALBUM = %q, want the container's", got)
	}
}

// TestReadAlbumReportsNoCancel: a canceled read is not damage, so it reports
// nothing.
func TestReadAlbumReportsNoCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := &fakeMapper{readErr: waxerr.Wrap(waxerr.CodeCanceled, "meta: read canceled", ctx.Err())}
	var notes []Note
	a := ReadAlbum(ctx, m, nil, "flac", map[string][]string{"ALBUM": {"The Album"}}, "flac", "",
		func(n Note) { notes = append(notes, n) })
	if len(notes) != 0 {
		t.Errorf("notes = %+v, want none for a cancel", notes)
	}
	if a.Info != nil || a.Tags != nil {
		t.Errorf("album = %+v, want none after a cancel", a)
	}
}

// TestPieceTags: the piece's number, total and title replace the album's,
// an empty title keeps the album's, and the album is left as it was.
func TestPieceTags(t *testing.T) {
	a := Album{Tags: []container.Tag{
		{Key: "ALBUM", Value: "The Album"}, {Key: "title", Value: "The Rip"},
		{Key: "TRACKNUMBER", Value: "9"}, {Key: "TRACKTOTAL", Value: "9"},
	}}
	got := a.PieceTags(2, 12, "Two")
	want := []container.Tag{
		{Key: "ALBUM", Value: "The Album"},
		{Key: "TRACKNUMBER", Value: "2"}, {Key: "TRACKTOTAL", Value: "12"}, {Key: "TITLE", Value: "Two"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("PieceTags = %+v, want %+v", got, want)
	}
	if got := tagValues(a.PieceTags(0, 12, ""), "title"); !slices.Equal(got, []string{"The Rip"}) {
		t.Errorf("an untitled piece's title = %q, want the album's", got)
	}
	if a.Tags[1].Value != "The Rip" || a.Tags[2].Value != "9" {
		t.Errorf("PieceTags changed the album's tags: %+v", a.Tags)
	}
	if got := (Album{}).PieceTags(1, 1, ""); len(got) != 2 {
		t.Errorf("an empty album's piece tags = %+v, want the number and the total", got)
	}
}

// TestPostPassSkipsWhatTheMuxerEmbedded: the post-pass runs for an output
// whose muxer is not the final tag writer, and never without an album or a
// mapper.
func TestPostPassSkipsWhatTheMuxerEmbedded(t *testing.T) {
	album := map[string][]string{"ALBUM": {"The Album"}}
	for _, tc := range []struct {
		name          string
		info          *meta.Info
		format        string
		wantApplyCall int
	}{
		{"flac takes the post-pass", &meta.Info{Tags: album}, "flac", 1},
		{"wavpack embeds its own", &meta.Info{Tags: album}, "wavpack", 0},
		{"no album, no post-pass", nil, "flac", 0},
	} {
		m := &fakeMapper{info: tc.info}
		a := ReadAlbum(context.Background(), m, nil, "flac", nil, tc.format, "", nil)
		if err := a.PostPass(context.Background(), "piece.flac", nil, func(err error) { t.Errorf("%s: %v", tc.name, err) }); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if m.applies != tc.wantApplyCall {
			t.Errorf("%s: %d post-passes, want %d", tc.name, m.applies, tc.wantApplyCall)
		}
	}
	a := ReadAlbum(context.Background(), nil, nil, "flac", album, "flac", "", nil)
	if err := a.PostPass(context.Background(), "piece.flac", nil, func(err error) { t.Errorf("a nil mapper: %v", err) }); err != nil {
		t.Errorf("a nil mapper: %v", err)
	}
}

// TestPostPassReturnsTheSplitsCancel: a failed post-pass under a live context
// goes to its caller's func and the piece stands. A cancel, in the post-pass
// or with none to run, is returned as the split's, so the piece is not
// published.
func TestPostPassReturnsTheSplitsCancel(t *testing.T) {
	album := &meta.Info{Tags: map[string][]string{"ALBUM": {"The Album"}}}
	boom := errors.New("tag library failed")
	m := &fakeMapper{info: album, applyErr: boom}
	a := ReadAlbum(context.Background(), m, nil, "flac", nil, "flac", "", nil)
	var failed []error
	if err := a.PostPass(context.Background(), "piece.flac", nil, func(err error) { failed = append(failed, err) }); err != nil {
		t.Errorf("a failed post-pass under a live context: %v, want nil", err)
	}
	if len(failed) != 1 || failed[0] != boom {
		t.Errorf("failures reported = %v, want the mapper's", failed)
	}

	for _, format := range []string{"flac", "wavpack"} {
		ctx, cancel := context.WithCancel(context.Background())
		m := &fakeMapper{info: album, applyErr: boom, onApply: cancel}
		a := ReadAlbum(context.Background(), m, nil, "flac", nil, format, "", nil)
		if format == "wavpack" {
			cancel()
		}
		err := a.PostPass(ctx, "piece."+format, nil, func(err error) { t.Errorf("%s: a canceled post-pass reported %v", format, err) })
		if waxerr.CodeOf(err) != waxerr.CodeCanceled || !strings.HasPrefix(waxerr.Message(err), "split canceled") {
			t.Errorf("%s: canceled post-pass = %v, want %q", format, err, "split canceled")
		}
	}
}

// TestPieceTagsMakeRoomForThePiece: an album whose tags sit just under the
// embedding cap tags every piece, a long title included. A piece is handed the
// album's tags then its own wherever its writer's block holds them; where it
// cannot, the album's are left out in order until the set fits. A tag left out
// is reported once for the split where that is final, and silently where the
// post-pass rewrites the piece with the full set.
func TestPieceTagsMakeRoomForThePiece(t *testing.T) {
	album := map[string][]string{"ALBUM": {"The Album"}, "COMMENT": {strings.Repeat("c", 49030)}}
	long := strings.Repeat("t", 120)
	all := []string{"ALBUM", "COMMENT", "TRACKNUMBER", "TRACKTOTAL"}
	trimmed := []string{"ALBUM", "TRACKNUMBER", "TRACKTOTAL"}
	titled := func(keys []string) []string { return append(slices.Clone(keys), "TITLE") }
	for _, tc := range []struct {
		format, container, title string
		mapper                   bool
		first, second            []string // the keys each piece is handed
		noted                    bool
	}{
		{"wavpack", "", "", true, trimmed, trimmed, true},
		{"alac", waxflow.ContainerProgressive, "", true, all, all, false},
		{"flac", waxflow.ContainerOgg, "", true, all, all, false},
		{"flac", "", "", true, all, all, false},
		// The first piece's title leaves its block no room for the COMMENT.
		{"wavpack", "", long, true, titled(trimmed), trimmed, true},
		{"flac", waxflow.ContainerOgg, long, true, titled(trimmed), all, true},
		{"flac", "", long, true, titled(trimmed), all, false},
		{"opus", "", long, true, titled(trimmed), all, false},
		{"vorbis", "", long, true, titled(trimmed), all, false},
		{"mp3", "", long, true, titled(all), all, false},
		{"flac", "", long, false, titled(trimmed), all, true},
	} {
		name := fmt.Sprintf("%s/%s, a %d-byte title, mapper %t", tc.format, tc.container, len(tc.title), tc.mapper)
		var mapper meta.Mapper
		var containerTags map[string][]string
		if tc.mapper {
			mapper = &fakeMapper{info: &meta.Info{Tags: album}}
		} else {
			containerTags = album
		}
		var notes []Note
		a := ReadAlbum(context.Background(), mapper, nil, "flac", containerTags, tc.format, tc.container,
			func(n Note) { notes = append(notes, n) })
		for i, want := range [][]string{tc.first, tc.second} {
			title := ""
			if i == 0 {
				title = tc.title
			}
			tags := a.PieceTags(i+1, 2, title)
			if err := waxflow.TagsFit(tc.format, tc.container, tags); err != nil {
				t.Errorf("%s: piece %d's tags do not fit its writer: %v", name, i+1, err)
			}
			if keys := tagKeys(tags); !slices.Equal(keys, want) {
				t.Errorf("%s: piece %d's tags are %q, want %q", name, i+1, keys, want)
			}
		}
		wantNotes := 0
		if tc.noted {
			wantNotes = 1
		}
		if len(notes) != wantNotes || (wantNotes == 1 && (notes[0].Kind != NoteTagTooLarge || notes[0].Key != "COMMENT")) {
			t.Errorf("%s: notes = %+v, want %d for COMMENT", name, notes, wantNotes)
		}
	}
}

func tagKeys(tags []container.Tag) []string {
	var out []string
	for _, t := range tags {
		out = append(out, t.Key)
	}
	return out
}

// TestPieceTagsKeepATagTheWriterHolds: an album tag too large for a capped
// block is kept on every piece whose writer holds it (an MP4's ilst has no
// cap), and left out with one note where the writer is final and cannot.
func TestPieceTagsKeepATagTheWriterHolds(t *testing.T) {
	album := map[string][]string{"ALBUM": {"The Album"}, "COMMENT": {strings.Repeat("c", 60000)}}
	for _, tc := range []struct {
		format, container string
		keys              []string
		noted             bool
	}{
		{"alac", waxflow.ContainerProgressive, []string{"ALBUM", "COMMENT", "TRACKNUMBER", "TRACKTOTAL"}, false},
		{"aac", "", []string{"ALBUM", "COMMENT", "TRACKNUMBER", "TRACKTOTAL"}, false},
		{"wavpack", "", []string{"ALBUM", "TRACKNUMBER", "TRACKTOTAL"}, true},
	} {
		var notes []Note
		a := ReadAlbum(context.Background(), &fakeMapper{info: &meta.Info{Tags: album}}, nil, "m4a", nil,
			tc.format, tc.container, func(n Note) { notes = append(notes, n) })
		for i := range 2 {
			if keys := tagKeys(a.PieceTags(i+1, 2, "")); !slices.Equal(keys, tc.keys) {
				t.Errorf("%s: piece %d's tags are %q, want %q", tc.format, i+1, keys, tc.keys)
			}
		}
		if noted := len(notes) == 1 && notes[0].Kind == NoteTagTooLarge && notes[0].Key == "COMMENT"; noted != tc.noted ||
			!tc.noted && len(notes) != 0 {
			t.Errorf("%s: notes = %+v, want COMMENT noted %t", tc.format, notes, tc.noted)
		}
	}
}

// TestCheckPieceTagsRefusesATitleTooLargeToEmbed: a piece's own tags are a
// caller's, so one its writer's block cannot hold is refused rather than
// dropped. An MP4's ilst holds any.
func TestCheckPieceTagsRefusesATitleTooLargeToEmbed(t *testing.T) {
	if err := CheckPieceTags("flac", "", 1, 12, strings.Repeat("t", 48000)); err != nil {
		t.Errorf("a title that fits: %v", err)
	}
	for _, form := range [][2]string{{"flac", ""}, {"wavpack", ""}, {"flac", waxflow.ContainerOgg}} {
		err := CheckPieceTags(form[0], form[1], 1, 12, strings.Repeat("t", 60000))
		if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest || err.Error() != "a 60000-byte title is too large to embed in a piece's tags" {
			t.Errorf("%s/%s, a title that cannot fit: %v", form[0], form[1], err)
		}
	}
	for _, form := range [][2]string{{"alac", ""}, {"alac", waxflow.ContainerProgressive}, {"aac", ""}} {
		if err := CheckPieceTags(form[0], form[1], 1, 12, strings.Repeat("t", 60000)); err != nil {
			t.Errorf("%s/%s, a title the ilst holds: %v", form[0], form[1], err)
		}
	}
}
