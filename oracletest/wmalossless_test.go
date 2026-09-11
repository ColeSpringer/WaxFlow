package oracletest

// The reader-agreement cell for WMA Lossless. Two readers speak for the same
// file: WaxFlow's demuxer, which builds a decodable track out of the
// WAVEFORMATEX, and waxlabel, which reads the same header for its properties.
// They agree or one of them is wrong about the format.
//
// The field worth pinning is the DEPTH, and pinning it needs a header built to
// disagree with itself. WMA Lossless leaves wBitsPerSample as decoration and
// puts the depth a decoder uses in the codec extra bytes; Windows writes the
// same value into both, so on every real file a reader taking either one is
// right and this cell would pass whichever it took. The second half below
// patches the fixed field to 16 on a 24-bit stream, which is the only way to
// tell the two readings apart.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
)

func TestWaxlabelAgreesOnWMALossless(t *testing.T) {
	for _, tc := range []struct {
		name           string
		rate, channels int
		bits           int
	}{
		{"ll-44100-2ch-16", 44100, 2, 16},
		{"ll-44100-2ch-24", 44100, 2, 24},
		{"ll-48000-6ch-24", 48000, 6, 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "codec", "wmalossless", "testdata", "corpus", tc.name+".wma"))
			if err != nil {
				t.Fatal(err)
			}
			info, err := waxflow.New().Probe(container.BytesSource(raw), "", nil)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			tr := info.Default()
			if tr.Codec != "wmalossless" {
				t.Fatalf("WaxFlow reports codec %q, want wmalossless", tr.Codec)
			}
			if tr.Fmt.Rate != tc.rate || tr.Fmt.Channels != tc.channels {
				t.Errorf("WaxFlow reports %d Hz %d ch, want %d Hz %d ch",
					tr.Fmt.Rate, tr.Fmt.Channels, tc.rate, tc.channels)
			}
			if tr.Fmt.Type != audio.Int || tr.Fmt.BitDepth != tc.bits {
				t.Errorf("WaxFlow reports %v, want the int domain at %d bits", tr.Fmt, tc.bits)
			}

			got := waxlabelTrack(t, raw)
			if got.SampleRate != tc.rate {
				t.Errorf("waxlabel SampleRate = %d, WaxFlow %d", got.SampleRate, tc.rate)
			}
			if got.Channels != tc.channels {
				t.Errorf("waxlabel Channels = %d, WaxFlow %d", got.Channels, tc.channels)
			}
			if got.BitsPerSample != tc.bits {
				t.Errorf("waxlabel BitsPerSample = %d, the stream is %d", got.BitsPerSample, tc.bits)
			}
			if got.Codec == "" {
				t.Error("waxlabel names no codec")
			}
		})
	}
}

// TestBothReadersTakeTheDepthFromTheExtraBytes is the half with teeth. On a
// real file wBitsPerSample and the codec extra bytes agree, so a reader taking
// either is right; here they are made to disagree, and only the extra bytes
// are the format's answer.
func TestBothReadersTakeTheDepthFromTheExtraBytes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "codec", "wmalossless", "testdata", "corpus", "ll-44100-2ch-24.wma"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := waxflow.New().Probe(container.BytesSource(raw), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := info.Default().CodecConfig
	if len(cfg) != 36 {
		t.Fatalf("codec config is %d bytes, want 36", len(cfg))
	}
	// Find the WAVEFORMATEX in the Stream Properties Object by the bytes the
	// demuxer handed back, and set wBitsPerSample to 16 on this 24-bit stream.
	at := bytes.Index(raw, cfg)
	if at < 0 {
		t.Fatal("the codec config is not a byte run of the file")
	}
	patched := bytes.Clone(raw)
	patched[at+14], patched[at+15] = 16, 0

	info, err = waxflow.New().Probe(container.BytesSource(patched), "", nil)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got := info.Default().Fmt; got.BitDepth != 24 || got.Type != audio.Int {
		t.Errorf("WaxFlow reports %v, want int24 from the extra bytes", got)
	}
	// waxlabel reads the FIXED field, so it answers 16 here where the stream
	// is 24. That is an upstream divergence rather than a WaxFlow defect and
	// it is latent, since no encoder writes a disagreeing header; it is filed
	// in docs/upstream-requests.md. Pinned at today's answer so the entry
	// cannot go stale: when waxlabel starts reading the extra bytes this cell
	// fails and the request gets retired.
	if got := waxlabelTrack(t, patched).BitsPerSample; got != 16 {
		t.Errorf("waxlabel BitsPerSample = %d, want 16: it read the extra bytes, so "+
			"retire the entry in docs/upstream-requests.md and flip this to 24", got)
	}
}
