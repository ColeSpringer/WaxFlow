package ogg

import (
	"bytes"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/codec/vorbis"
	"github.com/colespringer/waxflow/container"
)

// TestTagsFitAgreesWithTheWrite: at the comment header's edge, for each codec
// the muxer carries, a set one byte under is answered fitting and written,
// and one byte over is answered with the refusal Begin gives.
func TestTagsFitAgreesWithTheWrite(t *testing.T) {
	f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	flacEnc, err := flac.NewEncoder(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	ff := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	vorbisEnc, err := vorbis.NewEncoder(ff, &vorbis.EncoderOptions{Quality: 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		track container.Track
		magic string // what the header carries ahead of the vendor
		fit   func([]container.Tag) error
	}{
		{"opus", container.Track{Codec: codec.Opus, CodecConfig: muxOpusHead(tagTestPreSkip)}, "OpusTags", OpusTagsFit},
		{"vorbis", container.Track{Codec: codec.Vorbis, CodecConfig: vorbisEnc.CodecConfig(), Fmt: ff}, "", VorbisTagsFit},
		{"flac", container.Track{Codec: codec.FLAC, CodecConfig: flacEnc.CodecConfig(), Fmt: f, Samples: 1000}, "", FLACTagsFit},
	} {
		// The header: the magic, vendor length and string, the count, then
		// the one comment's length and its KEY=value.
		room := maxTagsPageBytes - (len(tc.magic) + 4 + len(defaultVendor) + 4 + 4 + len("COMMENT="))
		for _, n := range []int{room, room + 1} {
			fits := n == room
			tags := []container.Tag{{Key: "COMMENT", Value: strings.Repeat("c", n)}}
			fitErr := tc.fit(tags)
			beginErr := NewMuxer(&bytes.Buffer{}, &MuxerOptions{Tags: tags}).Begin([]container.Track{tc.track})
			if (fitErr == nil) != fits || (beginErr == nil) != fits {
				t.Errorf("%s, %d bytes: TagsFit = %v, Begin = %v, want fits %t", tc.name, n, fitErr, beginErr, fits)
			}
			if !fits && fitErr != nil && beginErr != nil && fitErr.Error() != beginErr.Error() {
				t.Errorf("%s, %d bytes: TagsFit says %q, Begin %q", tc.name, n, fitErr, beginErr)
			}
		}
	}
}
