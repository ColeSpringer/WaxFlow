package oracletest

// G.711 and the two ADPCM families are the second half of the reader gap
// waxlabel 1.8.0 opened: it describes these tracks in WAV, AIFF-C and MOV,
// and WaxFlow refused every one of them, so a library scanned with one tool
// listed tracks the other would not open.
//
// The agreement here is exact, like the PCM one beside it: there is no codec
// to disagree over at the header level, only a law or a block geometry that
// both readers take from the same fields.
//
// The interesting cell is sine-ima4.aifc, and it is the reason waxlabel 1.8.0
// exists: AIFF-C's numSampleFrames counts PACKETS for ima4, so a reader taking
// it as frames reports a 125-sample file where there are 8000. Both readers
// have to make the same conversion for the lengths to meet.
//
// The fixtures are the repository's own (testdata/ and container/mp4/testdata/),
// whose provenance comments hold the command lines.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

func TestWaxlabelAgreesG711AndADPCM(t *testing.T) {
	for _, tc := range []struct {
		path    string
		codec   codec.ID
		label   string
		profile string
	}{
		{"testdata/sine-alaw.wav", codec.ALaw, "A-law", ""},
		{"testdata/sine-ulaw.wav", codec.MuLaw, "mu-law", ""},
		{"testdata/sine-ima.wav", codec.IMAADPCM, "IMA ADPCM", ""},
		{"testdata/sine-ima-stereo.wav", codec.IMAADPCM, "IMA ADPCM", ""},
		{"testdata/sine-msadpcm.wav", codec.MSADPCM, "ADPCM", ""},
		{"testdata/sine-msadpcm-stereo.wav", codec.MSADPCM, "ADPCM", ""},
		{"testdata/sine-alaw.aifc", codec.ALaw, "A-law", "alaw"},
		{"testdata/sine-ulaw.aifc", codec.MuLaw, "mu-law", "ulaw"},
		{"testdata/sine-ima4.aifc", codec.IMAADPCM, "IMA ADPCM", "ima4"},
		{"testdata/sine-ima4-stereo.aifc", codec.IMAADPCM, "IMA ADPCM", "ima4"},
		{"container/mp4/testdata/g711-alaw.mov", codec.ALaw, "A-law", "alaw"},
		{"container/mp4/testdata/g711-ulaw.mov", codec.MuLaw, "mu-law", "ulaw"},
		{"container/mp4/testdata/ima4.mov", codec.IMAADPCM, "IMA ADPCM", "ima4"},
	} {
		t.Run(filepath.Base(tc.path), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(tc.path)))
			if err != nil {
				t.Fatal(err)
			}
			info, err := waxflow.New().Probe(container.BytesSource(raw), "", nil)
			if err != nil {
				t.Fatalf("waxflow.Probe: %v", err)
			}
			ours := info.Default()
			if ours.Codec != tc.codec {
				t.Errorf("waxflow codec = %q, want %q", ours.Codec, tc.codec)
			}
			track := waxlabelTrack(t, raw)
			if track.Codec != tc.label {
				t.Errorf("waxlabel Codec = %q, want %q", track.Codec, tc.label)
			}
			if tc.profile != "" && track.CodecProfile != tc.profile {
				t.Errorf("waxlabel CodecProfile = %q, want %q", track.CodecProfile, tc.profile)
			}
			if track.SampleRate != ours.Fmt.Rate || track.Channels != ours.Fmt.Channels {
				t.Errorf("waxlabel says %d Hz %dch, waxflow says %d Hz %dch",
					track.SampleRate, track.Channels, ours.Fmt.Rate, ours.Fmt.Channels)
			}
			// What the file STORES a sample in, which for these codecs is not
			// what they decode to: 8 bits for a companding law, 4 for both
			// ADPCM families, against the 16 both produce. SourceBitDepth is
			// the field that carries it, and it is what waxlabel reads.
			if track.BitsPerSample != ours.SourceBitDepth {
				t.Errorf("waxlabel says %d bits, waxflow's source depth is %d",
					track.BitsPerSample, ours.SourceBitDepth)
			}
			// Length: the two readers reach it by different routes (a media
			// header or a COMM field for one, the payload's own geometry for
			// the other), so an agreement here is a real cross-check rather
			// than one number read twice.
			want := time.Duration(ours.Samples) * time.Second / time.Duration(ours.Fmt.Rate)
			if track.Duration != want {
				t.Errorf("waxlabel says %v, waxflow says %v (%d samples at %d Hz)",
					track.Duration, want, ours.Samples, ours.Fmt.Rate)
			}
		})
	}
}
