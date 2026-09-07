package oracletest

// The ALAC depth cell. The real depth of an ALAC stream lives in the magic
// cookie, and waxlabel read the sample entry's bits-per-sample field instead
// until v1.4.2, which made every ALAC file report 16-bit (the field was a
// conventional 16 whatever the stream held). The entry now repeats the
// cookie's depth, so a plain round trip could no longer tell the two apart;
// the test rewrites the field to a value no ALAC stream has before parsing,
// and the cookie has to win.

import (
	"bytes"
	"context"
	"encoding/binary"
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
// and requires the tag library to read that depth back, in both MP4 forms,
// with the sample entry's own depth field made to disagree. The progressive
// and fragmented muxers lay the movie box out differently, and the cookie has
// to be found in both. The output goes to a file because the progressive form
// back-patches its header.
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
				disagreeOnDepth(t, raw)

				doc, err := waxlabel.Parse(t.Context(), container.BytesSource(raw))
				if err != nil {
					t.Fatalf("waxlabel.Parse: %v", err)
				}
				tracks := doc.Properties().Tracks
				if len(tracks) != 1 {
					t.Fatalf("waxlabel found %d tracks, want 1", len(tracks))
				}
				if got := tracks[0].BitsPerSample; got != depth {
					t.Errorf("waxlabel read %d bits per sample from a %d-bit ALAC file whose sample entry says %d; "+
						"the depth must come from the magic cookie, not the sample entry", got, depth, wrongDepth)
				}
			})
		}
	}
}

// wrongDepth is what disagreeOnDepth writes into the sample entry: a depth
// ALAC cannot hold, so a reader that took the field would report it.
const wrongDepth = 12

// disagreeOnDepth rewrites the 'alac' sample entry's samplesize in place.
// The entry is located by its fixed head (the fourcc, six reserved bytes,
// data_reference_index 1); samplesize sits 18 bytes into the entry body.
func disagreeOnDepth(t *testing.T, raw []byte) {
	t.Helper()
	head := []byte("alac\x00\x00\x00\x00\x00\x00\x00\x01")
	i := bytes.Index(raw, head)
	if i < 0 || i+4+20 > len(raw) {
		t.Fatal("no alac sample entry in the output")
	}
	binary.BigEndian.PutUint16(raw[i+4+18:], wrongDepth)
}
