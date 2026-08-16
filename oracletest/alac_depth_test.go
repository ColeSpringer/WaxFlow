package oracletest

// The ALAC depth cell. An MP4 sample entry pins its bits-per-sample field at
// 16 by convention whatever the stream holds, so the real depth lives only in
// the ALAC magic cookie. waxlabel read the sample entry until v1.4.2, which
// made every ALAC file report 16-bit; a cross-implementation round trip is the
// only place that shows up, since our own demuxer reads the cookie already.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	waxlabel "github.com/colespringer/waxlabel"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
)

// TestALACDepthWaxlabelRoundTrip transcodes integer sources at each ALAC depth
// and requires the tag library to read that depth back, in both MP4 forms. The
// progressive and fragmented muxers lay the movie box out differently, and the
// cookie has to be found in both. The output goes to a file because the
// progressive form back-patches its header.
func TestALACDepthWaxlabelRoundTrip(t *testing.T) {
	for _, depth := range []int{16, 20, 24, 32} {
		for _, form := range []string{"progressive", "fragmented"} {
			t.Run(fmt.Sprintf("%d-bit/%s", depth, form), func(t *testing.T) {
				f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2),
					Type: audio.Int, BitDepth: depth}
				wav, _ := synthWAV(t, f, 4410)

				path := filepath.Join(t.TempDir(), "out.m4a")
				file, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := waxflow.New().Transcode(context.Background(), container.BytesSource(wav), "", file,
					waxflow.TranscodeOptions{Format: "alac", Container: form}); err != nil {
					t.Fatalf("Transcode to alac/%s: %v", form, err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}

				doc, err := waxlabel.Parse(t.Context(), container.BytesSource(raw))
				if err != nil {
					t.Fatalf("waxlabel.Parse: %v", err)
				}
				tracks := doc.Properties().Tracks
				if len(tracks) != 1 {
					t.Fatalf("waxlabel found %d tracks, want 1", len(tracks))
				}
				if got := tracks[0].BitsPerSample; got != depth {
					t.Errorf("waxlabel read %d bits per sample from a %d-bit ALAC file; "+
						"the depth must come from the magic cookie, not the sample entry", got, depth)
				}
			})
		}
	}
}
