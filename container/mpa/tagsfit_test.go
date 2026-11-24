package mpa_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mpa"
)

// TestTagsFitAgreesWithTheWrite: at the ID3v2 tag's edge, a set one byte
// under is answered fitting and written, and one byte over is answered with
// the refusal Begin gives. A key the tag does not map takes no room.
func TestTagsFitAgreesWithTheWrite(t *testing.T) {
	track := container.Track{Codec: codec.MP3, Fmt: audio.Format{Rate: 44100, Channels: 2}}
	// The tag header, then the one frame's header, its encoding byte and the
	// text.
	room := 48<<10 - (10 + 10 + 1)
	for _, n := range []int{room, room + 1} {
		fits := n == room
		tags := []container.Tag{
			{Key: "TITLE", Value: strings.Repeat("t", n)},
			{Key: "COMMENT", Value: strings.Repeat("c", 60000)},
		}
		fitErr := mpa.TagsFit(tags)
		beginErr := mpa.NewMuxer(&bytes.Buffer{}, &mpa.MuxerOptions{Tags: tags}).Begin([]container.Track{track})
		if (fitErr == nil) != fits || (beginErr == nil) != fits {
			t.Errorf("%d bytes: TagsFit = %v, Begin = %v, want fits %t", n, fitErr, beginErr, fits)
		}
		if !fits && fitErr != nil && beginErr != nil && fitErr.Error() != beginErr.Error() {
			t.Errorf("%d bytes: TagsFit says %q, Begin %q", n, fitErr, beginErr)
		}
	}
}
