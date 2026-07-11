package flacn

import (
	"bytes"
	"encoding/binary"
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
	if got := vorbisCommentBlock(tags); !bytes.Equal(got, want) {
		t.Errorf("block bytes\n got % x\nwant % x", got, want)
	}
	if got := vorbisCommentBlock(nil); got != nil {
		t.Errorf("no tags rendered %d bytes, want nil", len(got))
	}
}
