package mka

import (
	"bytes"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
)

// TestTagsFitAgreesWithTheWrite: the muxer writes no tags, so a set past
// every other writer's cap is answered fitting and Begin takes it.
func TestTagsFitAgreesWithTheWrite(t *testing.T) {
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	enc, err := flac.NewEncoder(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	tags := []container.Tag{{Key: "COMMENT", Value: strings.Repeat("c", 200<<10)}}
	if err := TagsFit(tags); err != nil {
		t.Fatalf("TagsFit = %v, want nil", err)
	}
	m := NewMuxer(&bytes.Buffer{}, &MuxerOptions{Tags: tags})
	if err := m.Begin([]container.Track{{Codec: codec.FLAC, CodecConfig: enc.CodecConfig(), Fmt: f, Samples: 1000}}); err != nil {
		t.Errorf("Begin = %v, want the set TagsFit answered for taken", err)
	}
}
