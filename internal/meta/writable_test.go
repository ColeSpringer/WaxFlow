package meta_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
)

// TestWritableText pins the rule every tag writer shares: no NUL, valid
// UTF-8. The cut comes first, so bytes behind the NUL are never repaired.
func TestWritableText(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		changed  bool
	}{
		{"plain", "plain", false},
		{"", "", false},
		{"naïve 日本", "naïve 日本", false},
		{"cut\x00here", "cut", true},
		{"\x00", "", true},
		{"bad\xff\xfevalue", "bad�value", true},
		{"ok\x00\xff", "ok", true},
		{"\xffhead\x00tail", "�head", true},
	} {
		got, changed := meta.WritableText(tc.in)
		if got != tc.want || changed != tc.changed {
			t.Errorf("WritableText(%q) = %q, %v, want %q, %v", tc.in, got, changed, tc.want, tc.changed)
		}
	}
}

// TestWritableRepairsEveryTextOfAnInfo covers each field Writable reaches and
// what it leaves alone: the input, and an Info that needed nothing.
func TestWritableRepairsEveryTextOfAnInfo(t *testing.T) {
	clean := &meta.Info{Tags: map[string][]string{"TITLE": {"t"}}}
	if got, lines := meta.Writable(clean); got != clean || lines != nil {
		t.Errorf("a writable Info came back as another (%p, %v)", got, lines)
	}
	if got, lines := meta.Writable(nil); got != nil || lines != nil {
		t.Errorf("Writable(nil) = %v, %v", got, lines)
	}

	in := &meta.Info{
		Tags: map[string][]string{
			"ARTIST": {"cut\x00here", "whole"},
			"ALBUM":  {"\x00gone"},
			"GENRE":  {""},
		},
		Chapters:     []container.Chapter{{Title: "one\x00two"}},
		Pictures:     []meta.Picture{{Description: "front\xff", Data: []byte{1}}},
		Synced:       []meta.SyncedLyrics{{Language: "eng", Lines: []meta.SyncedLine{{Text: "la\x00la"}}}},
		CommentItems: []meta.TextItem{{Text: "note\x00x", Description: "d\x00d", Language: "deu"}},
	}
	out, lines := meta.Writable(in)
	wantLines := []string{"tag ALBUM", "tag ARTIST", "tag COMMENT", "a chapter title", "a picture description", "synced lyrics"}
	if !slices.Equal(lines, wantLines) {
		t.Errorf("repaired fields = %q, want %q", lines, wantLines)
	}
	if got := out.Tags["ARTIST"]; !slices.Equal(got, []string{"cut", "whole"}) {
		t.Errorf("ARTIST = %q, want [cut whole]", got)
	}
	if _, ok := out.Tags["ALBUM"]; ok {
		t.Errorf("ALBUM = %q, want the key gone with its only value", out.Tags["ALBUM"])
	}
	// An empty value the source stated is a value, not a repair's leftover.
	if got := out.Tags["GENRE"]; !slices.Equal(got, []string{""}) {
		t.Errorf("GENRE = %q, want the empty value kept", got)
	}
	if out.Chapters[0].Title != "one" || out.Pictures[0].Description != "front\uFFFD" ||
		out.Synced[0].Lines[0].Text != "la" {
		t.Errorf("chapter %q, picture %q, lyric %q", out.Chapters[0].Title, out.Pictures[0].Description, out.Synced[0].Lines[0].Text)
	}
	if got := out.CommentItems[0]; got != (meta.TextItem{Text: "note", Description: "d", Language: "deu"}) {
		t.Errorf("comment item = %+v", got)
	}
	// A cached Info is shared and read-only: the repair is a copy.
	if in.Tags["ARTIST"][0] != "cut\x00here" || in.Chapters[0].Title != "one\x00two" ||
		in.Synced[0].Lines[0].Text != "la\x00la" || in.Pictures[0].Description != "front\xff" {
		t.Errorf("Writable edited its input: %+v", in)
	}
}

// TestTagProjectionsEmitWritableText pins the muxers' two doors. The
// streaming path reads its tags through MinimalTags with no repair before
// it, and a NUL in a value there ends a C-string field early or splits an
// ID3 or APEv2 value in two.
func TestTagProjectionsEmitWritableText(t *testing.T) {
	info := &meta.Info{Tags: map[string][]string{
		"TITLE": {"plain"}, "ARTIST": {"cut\x00here", "\x00gone", "bad\xffbyte"}, "ALBUM": {""},
	}}
	want := []container.Tag{
		{Key: "TITLE", Value: "plain"},
		{Key: "ARTIST", Value: "cut"}, {Key: "ARTIST", Value: "bad\uFFFDbyte"},
		{Key: "ALBUM", Value: ""},
	}
	if got := meta.MinimalTags(info); !slices.Equal(got, want) {
		t.Errorf("MinimalTags = %q, want %q", got, want)
	}
	slices.SortStableFunc(want, func(a, b container.Tag) int { return strings.Compare(a.Key, b.Key) })
	if got := meta.FullTags(info); !slices.Equal(got, want) {
		t.Errorf("FullTags = %q, want %q", got, want)
	}
}
