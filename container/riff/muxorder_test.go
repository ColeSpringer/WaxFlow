package riff

import (
	"bytes"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// TestMuxRefusesAPermutedOrder pins that a copy cannot carry a decode-side
// channel order: the packets are in the source file's order and the header
// would claim the mask's, so a track whose config carries one is refused by
// name, and the caller decodes instead.
func TestMuxRefusesAPermutedOrder(t *testing.T) {
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16, Order: []uint8{1, 0}}
	blob, err := cfg.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	track := container.Track{Codec: codec.PCM, CodecConfig: blob,
		Fmt: cfg.PCMFormat(44100, 2, audio.DefaultLayout(2)), Samples: 4, Default: true}
	err = NewMuxer(&bytes.Buffer{}, nil).Begin([]container.Track{track})
	if waxerr.CodeOf(err) != waxerr.CodeUnsupportedFormat || !strings.Contains(err.Error(), "permuted") {
		t.Fatalf("Begin = %v, want an unsupported-format refusal naming the permuted order", err)
	}
}
