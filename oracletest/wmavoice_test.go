package oracletest

// The reader-agreement cell for WMA Voice. Two readers speak for the same file:
// WaxFlow's demuxer, which builds a decodable track out of the WAVEFORMATEX,
// and waxlabel, which reads the same header for its properties. They agree or
// one of them is wrong about the format.
//
// The depth is the interesting one here and it is checked against the cell's
// known value rather than against WaxFlow's track: this codec emits float
// whatever the stream says, so the two readers answer different questions
// there. The channel count is the other: WaxFlow takes it from the format
// rather than from nChannels, because nothing in this bitstream can carry a
// second channel, and this is what says the header agrees with that.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
)

func TestWaxlabelAgreesOnWMAVoice(t *testing.T) {
	for _, tc := range []struct {
		name string
		rate int
	}{
		{"voice-8000-4k", 8000},
		{"voice-11025-10k", 11025},
		{"voice-16000-16k", 16000},
		{"voice-22050-20k", 22050},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "codec", "wmavoice", "testdata", "corpus", tc.name+".wma"))
			if err != nil {
				t.Fatal(err)
			}
			info, err := waxflow.New().Probe(container.BytesSource(raw), "", nil)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			tr := info.Default()
			if tr.Codec != "wmavoice" {
				t.Fatalf("WaxFlow reports codec %q, want wmavoice", tr.Codec)
			}
			if tr.Fmt.Rate != tc.rate || tr.Fmt.Channels != 1 {
				t.Errorf("WaxFlow reports %d Hz %d ch, want %d Hz mono", tr.Fmt.Rate, tr.Fmt.Channels, tc.rate)
			}
			if tr.Fmt.Type != audio.Float || tr.Fmt.BitDepth != 32 {
				t.Errorf("WaxFlow reports %v, want float32", tr.Fmt)
			}

			got := waxlabelTrack(t, raw)
			if got.SampleRate != tc.rate {
				t.Errorf("waxlabel SampleRate = %d, WaxFlow %d", got.SampleRate, tc.rate)
			}
			if got.Channels != 1 {
				t.Errorf("waxlabel Channels = %d, WaxFlow 1", got.Channels)
			}
			// Every format this codec has is 16-bit, so a reader that found
			// anything else read the wrong field.
			if got.BitsPerSample != 16 {
				t.Errorf("waxlabel BitsPerSample = %d, the format is 16 everywhere", got.BitsPerSample)
			}
			if got.Codec == "" {
				t.Error("waxlabel names no codec")
			}
		})
	}
}
