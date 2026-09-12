package adts_test

import (
	"bytes"
	"flag"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/adts"
	"github.com/colespringer/waxflow/internal/testutil"
)

var update = flag.Bool("update", false, "rewrite golden files (make goldens)")

// TestGoldenMuxOutputs pins the muxer's byte-exact output, which is what the
// ADR-0004 MuxerVersion stands for: every ADTS header field, the per-frame
// lengths and the payloads behind them. Regenerate with `make goldens` and
// review the diff. The AAC encoder is already pinned cross-architecture by
// codec/aac's own golden, so these files are the same on every architecture.
func TestGoldenMuxOutputs(t *testing.T) {
	// LC: the ordinary header, with the core rate and channel config the
	// sample entry would otherwise carry.
	t.Run("golden-lc-44k-stereo.aac", func(t *testing.T) {
		f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2),
			Type: audio.Float, BitDepth: 32}
		const frames = 8820 // ~0.2 s
		raw, _ := encodeADTSAt(t, f, museSrc(frames, 2, f.Rate), 128000)
		testutil.Golden(t, filepath.Join("testdata", "golden-lc-44k-stereo.aac"), raw, *update)
	})
	// HE-AAC: implicit signalling, so the header states the LC core layer at
	// the core rate while the payload carries the SBR extension untouched.
	t.Run("golden-heaac-implicit.aac", func(t *testing.T) {
		f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2),
			Type: audio.Float, BitDepth: 32}
		const frames = 9600 // ~0.2 s
		testutil.Golden(t, filepath.Join("testdata", "golden-heaac-implicit.aac"),
			encodeADTSHE(t, f, museSrc(frames, 2, f.Rate), 64000), *update)
	})
}

// encodeADTSHE is encodeADTS through the HE-AAC encoder, so the golden holds
// real SBR payloads rather than stubs.
func encodeADTSHE(t *testing.T, f audio.Format, src [][]float32, bitrate int) []byte {
	t.Helper()
	enc, err := aac.NewHEEncoder(f, &aac.EncoderOptions{Bitrate: bitrate})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	mux := adts.NewMuxer(&out)
	track := container.Track{Codec: codec.HEAAC, CodecConfig: enc.CodecConfig(),
		Fmt: f, Samples: int64(len(src[0]))}
	if err := mux.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	write := func(p codec.Packet) error {
		return mux.WritePacket(container.Packet{Track: 0, Packet: p})
	}
	n := len(src[0])
	for off := 0; off < n; off += 1024 {
		end := min(off+1024, n)
		buf := audio.Get(f, end-off)
		buf.N = end - off
		for c := 0; c < f.Channels; c++ {
			copy(buf.ChanF(c), src[c][off:end])
		}
		if err := enc.Encode(buf, write); err != nil {
			t.Fatal(err)
		}
		audio.Put(buf)
	}
	tr, err := enc.Finish(write)
	if err != nil {
		t.Fatal(err)
	}
	if err := mux.End(tr); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
