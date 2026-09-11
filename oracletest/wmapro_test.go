package oracletest

// The reader-agreement cell for WMA Pro. Two readers speak for the same file:
// WaxFlow's demuxer, which builds a decodable track out of the WAVEFORMATEX,
// and waxlabel, which reads the same header for its properties. They agree or
// one of them is wrong about the format.
//
// Unlike the lossless cell there is no depth to pin against a disagreeing
// header: a Pro track is float on WaxFlow's side whatever the stream's coded
// depth, so the two readers answer different questions there and the codec
// depth is checked against the cell's known value instead.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
)

func TestWaxlabelAgreesOnWMAPro(t *testing.T) {
	for _, tc := range []struct {
		name           string
		rate, channels int
		bits           int
	}{
		{"pro-44100-2ch-16-128k", 44100, 2, 16},
		{"pro-48000-6ch-24-384k", 48000, 6, 24},
		{"pro-96000-2ch-24-384k", 96000, 2, 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "codec", "wmapro", "testdata", "corpus", tc.name+".wma"))
			if err != nil {
				t.Fatal(err)
			}
			info, err := waxflow.New().Probe(container.BytesSource(raw), "", nil)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			tr := info.Default()
			if tr.Codec != "wmapro" {
				t.Fatalf("WaxFlow reports codec %q, want wmapro", tr.Codec)
			}
			if tr.Fmt.Rate != tc.rate || tr.Fmt.Channels != tc.channels {
				t.Errorf("WaxFlow reports %d Hz %d ch, want %d Hz %d ch",
					tr.Fmt.Rate, tr.Fmt.Channels, tc.rate, tc.channels)
			}
			if tr.Fmt.Type != audio.Float || tr.Fmt.BitDepth != 32 {
				t.Errorf("WaxFlow reports %v, want float32", tr.Fmt)
			}

			got := waxlabelTrack(t, raw)
			if got.SampleRate != tc.rate {
				t.Errorf("waxlabel SampleRate = %d, WaxFlow %d", got.SampleRate, tc.rate)
			}
			if got.Channels != tc.channels {
				t.Errorf("waxlabel Channels = %d, WaxFlow %d", got.Channels, tc.channels)
			}
			if got.BitsPerSample != tc.bits {
				t.Errorf("waxlabel BitsPerSample = %d, the stream is coded at %d", got.BitsPerSample, tc.bits)
			}
			if got.Codec == "" {
				t.Error("waxlabel names no codec")
			}
		})
	}
}
