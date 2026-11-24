package flacn_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
)

// TestTagsFitAgreesWithTheWrite: at the VORBIS_COMMENT block's edge, a set
// one byte under is answered fitting and written, and one byte over is
// answered with the refusal Begin gives.
func TestTagsFitAgreesWithTheWrite(t *testing.T) {
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	enc, err := flac.NewEncoder(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	track := container.Track{Codec: codec.FLAC, CodecConfig: enc.CodecConfig(), Fmt: f, Samples: 1000}
	// The body: the vendor's length and string, the count, then the one
	// comment's length and its KEY=value.
	room := 48<<10 - (4 + len("WaxFlow") + 4 + 4 + len("COMMENT="))
	for _, n := range []int{room, room + 1} {
		fits := n == room
		tags := []container.Tag{{Key: "COMMENT", Value: strings.Repeat("c", n)}}
		fitErr := flacn.TagsFit(tags)
		beginErr := flacn.NewMuxer(&bytes.Buffer{}, &flacn.MuxerOptions{Tags: tags}).Begin([]container.Track{track})
		if (fitErr == nil) != fits || (beginErr == nil) != fits {
			t.Errorf("%d bytes: TagsFit = %v, Begin = %v, want fits %t", n, fitErr, beginErr, fits)
		}
		if !fits && fitErr != nil && beginErr != nil && fitErr.Error() != beginErr.Error() {
			t.Errorf("%d bytes: TagsFit says %q, Begin %q", n, fitErr, beginErr)
		}
	}
}
