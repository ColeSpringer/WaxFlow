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
