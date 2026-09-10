package oracletest

// MP3 inside an MP4 is a track two readers have to agree about, and the
// disagreement it replaces was ours: the demuxer knew the object type by name
// and refused the file anyway, so a library scanned with waxlabel listed a
// track WaxFlow would not open. Both now report MP3 for the same bytes.
//
// The fixtures are container/mp4's, whose provenance comment holds the
// command lines. Both spellings are here because they reach different code in
// both readers: the mp4a/esds object type and QuickTime's own '.mp3' fourcc.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

func TestWaxlabelAgreesMP3InMP4(t *testing.T) {
	for _, name := range []string{"mp3.mp4", "mp3.mov"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "container", "mp4", "testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			info, err := waxflow.New().Probe(container.BytesSource(raw), "", nil)
			if err != nil {
				t.Fatalf("waxflow.Probe: %v", err)
			}
			if got := info.Default().Codec; got != codec.MP3 {
				t.Errorf("waxflow codec = %q, want %q", got, codec.MP3)
			}
			track := waxlabelTrack(t, raw)
			// waxlabel v1.7.0 canonicalizes the mp4a object type to "MP3"
			// and leaves QuickTime's own fourcc uncanonicalized, which is
			// the open ask in docs/upstream-requests.md. Both readings are
			// accepted so the cell reports the fix landing rather than
			// failing on it; anything else is a real disagreement.
			switch track.Codec {
			case "MP3":
			case ".mp3":
				t.Logf("waxlabel reports the raw fourcc %q (profile %q); see docs/upstream-requests.md",
					track.Codec, track.CodecProfile)
			default:
				t.Errorf("waxlabel Codec = %q (profile %q), want MP3", track.Codec, track.CodecProfile)
			}
			// The parameters the two read from different places: waxlabel
			// takes the sample entry, WaxFlow builds the decoder's output
			// format from it. They must describe one stream.
			if track.SampleRate != info.Default().Fmt.Rate || track.Channels != info.Default().Fmt.Channels {
				t.Errorf("waxlabel says %d Hz %dch, waxflow says %d Hz %dch",
					track.SampleRate, track.Channels, info.Default().Fmt.Rate, info.Default().Fmt.Channels)
			}
		})
	}
}
