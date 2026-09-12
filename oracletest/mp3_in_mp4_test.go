package oracletest

// MP3 inside an MP4 is a track two readers have to agree about, and the
// disagreement it replaces was ours: the demuxer knew the object type by name
// and refused the file anyway, so a library scanned with waxlabel listed a
// track WaxFlow would not open. Both now report MP3 for the same bytes, for
// the mp4a/esds object type and for QuickTime's own '.mp3' fourcc alike.
//
// The fixtures are container/mp4's, whose provenance comment holds the
// command lines. All three spellings are here because they reach different
// code in both readers: esds at either timescale, and the fourcc.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

func TestWaxlabelAgreesMP3InMP4(t *testing.T) {
	// CodecProfile carries the raw name only when canonicalizing changed it,
	// so esds files leave it empty and the fourcc file keeps its spelling.
	for _, tc := range []struct {
		name    string
		profile string
	}{
		{"mp3.mp4", ""},
		{"mp3-ms.mp4", ""},
		{"mp3.mov", ".mp3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "container", "mp4", "testdata", tc.name))
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
			if track.Codec != "MP3" || track.CodecProfile != tc.profile {
				t.Errorf("waxlabel Codec = %q profile %q, want MP3 profile %q",
					track.Codec, track.CodecProfile, tc.profile)
			}
			// The parameters the two read from different places: waxlabel
			// takes the sample entry, WaxFlow builds the decoder's output
			// format from it. They must describe one stream.
			if track.SampleRate != info.Default().Fmt.Rate || track.Channels != info.Default().Fmt.Channels {
				t.Errorf("waxlabel says %d Hz %dch, waxflow says %d Hz %dch",
					track.SampleRate, track.Channels, info.Default().Fmt.Rate, info.Default().Fmt.Channels)
			}
			// Length is what the timescale moves, which is why mp3-ms.mp4 is
			// here: waxlabel reads the media header, so it answers on the
			// file's own grid (1.020907029s off the 22050 header, a flat
			// 1.021s off the millisecond one), while WaxFlow counts frames
			// and answers the same either way. One millisecond is the coarser
			// grid's own tick, so agreement inside it is agreement.
			ours := time.Duration(info.Default().Samples) * time.Second /
				time.Duration(info.Default().Fmt.Rate)
			if d := track.Duration - ours; d > time.Millisecond || d < -time.Millisecond {
				t.Errorf("waxlabel says %v, waxflow says %v (%d samples at %d Hz)",
					track.Duration, ours, info.Default().Samples, info.Default().Fmt.Rate)
			}
		})
	}
}
