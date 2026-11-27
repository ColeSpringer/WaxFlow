package flacn

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// TestVorbisCommentBlockLayout pins the block body's wire form (RFC 9639
// section 8.6): little-endian vendor length, the WaxFlow vendor string, a
// little-endian comment count, then length-prefixed KEY=value comments in
// tag order, and nil when there are no tags.
func TestVorbisCommentBlockLayout(t *testing.T) {
	tags := []container.Tag{
		{Key: "TITLE", Value: "Flac Title"},
		{Key: "ARTIST", Value: "Flac Artist"},
	}
	want := binary.LittleEndian.AppendUint32(nil, uint32(len("WaxFlow")))
	want = append(want, "WaxFlow"...)
	want = binary.LittleEndian.AppendUint32(want, 2)
	for _, c := range []string{"TITLE=Flac Title", "ARTIST=Flac Artist"} {
		want = binary.LittleEndian.AppendUint32(want, uint32(len(c)))
		want = append(want, c...)
	}
	got, err := vorbisCommentBlock(tags)
	if err != nil {
		t.Fatalf("vorbisCommentBlock: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("block bytes\n got % x\nwant % x", got, want)
	}
	if got, err := vorbisCommentBlock(nil); err != nil || got != nil {
		t.Errorf("no tags rendered %d bytes (err %v), want nil", len(got), err)
	}
}

// TestVorbisCommentBlockRefusesOversized pins the cap as a refusal: a comment
// that does not fit fails the render rather than being skipped, so a FLAC file
// never comes back missing a tag the caller asked to embed.
func TestVorbisCommentBlockRefusesOversized(t *testing.T) {
	tags := []container.Tag{
		{Key: "TITLE", Value: "kept"},
		{Key: "LYRICS", Value: strings.Repeat("x", maxCommentBytes)},
	}
	got, err := vorbisCommentBlock(tags)
	if err == nil {
		t.Fatalf("rendered %d bytes over an oversized comment, want a refusal", len(got))
	}
	if got != nil {
		t.Errorf("a refused render returned %d bytes, want nil", len(got))
	}
	if !strings.Contains(err.Error(), "LYRICS") {
		t.Errorf("error %q does not name the key", err)
	}
}

// TestVorbisCommentBlockWritesTheDateAsDATE: the canonical RECORDINGDATE is
// the DATE field, the only spelling other readers know, and nothing else is
// respelled. A literal DATE key is the same field: the two write one DATE per
// distinct value, in the order they come, whatever the keys' case.
func TestVorbisCommentBlockWritesTheDateAsDATE(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags []container.Tag
		want []string
	}{
		{"the canonical key", []container.Tag{{Key: "RECORDINGDATE", Value: "2020-05-01"}, {Key: "ORIGINALDATE", Value: "1999"}},
			[]string{"DATE=2020-05-01", "ORIGINALDATE=1999"}},
		{"both keys, one value", []container.Tag{{Key: "RECORDINGDATE", Value: "2020"}, {Key: "TITLE", Value: "t"}, {Key: "DATE", Value: "2020"}},
			[]string{"DATE=2020", "TITLE=t"}},
		{"both keys, two values", []container.Tag{{Key: "date", Value: "2021"}, {Key: "RecordingDate", Value: "2020"}, {Key: "DATE", Value: "2021"}},
			[]string{"DATE=2021", "DATE=2020"}},
	} {
		got, err := vorbisCommentBlock(tc.tags)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		// Behind the vendor string: the count, then each comment.
		got = got[4+len("WaxFlow"):]
		if n := binary.LittleEndian.Uint32(got); int(n) != len(tc.want) {
			t.Errorf("%s: %d comments, want %d", tc.name, n, len(tc.want))
		}
		got = got[4:]
		for _, want := range tc.want {
			n := binary.LittleEndian.Uint32(got)
			if c := string(got[4 : 4+n]); c != want {
				t.Errorf("%s: comment %q, want %q", tc.name, c, want)
			}
			got = got[4+n:]
		}
	}
}
