package oracletest

// The waxlabel read-back cells for stream-form tags: the muxer-side
// structural assertions live with each container package; this is the
// cross-implementation half, moved here at the v1.0 extraction so the
// main module carries no waxlabel dependency. It runs the tags through
// the REAL path (engine transcode with TranscodeOptions.Tags) rather
// than hand-built muxer calls, so it also pins the engine plumbing.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	waxlabel "github.com/colespringer/waxlabel"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
)

// TestStreamTagsWaxlabelRoundTrip transcodes to each stream-taggable
// live format with embedded tags and reads them back through waxlabel:
// Ogg OpusTags, FLAC VORBIS_COMMENT, MP3 ID3v2, and MP4 ilst. Both
// muxer forms run: streaming (a plain writer, the live-stream shape)
// and seekable (a file, where flacn back-patches STREAMINFO with a
// trailing SEEKTABLE and mpa back-patches the exact Xing/TOC), since
// the two lay the metadata out differently.
func TestStreamTagsWaxlabelRoundTrip(t *testing.T) {
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	wav, _ := synthWAV(t, f, 44100)
	tags := []container.Tag{
		{Key: "TITLE", Value: "Stream Title"},
		{Key: "ARTIST", Value: "Stream Artist"},
	}

	transcode := func(t *testing.T, format string, seekable bool) []byte {
		t.Helper()
		opts := waxflow.TranscodeOptions{Format: format, Tags: tags}
		if !seekable {
			var out bytes.Buffer
			if _, err := waxflow.New().Transcode(context.Background(), container.BytesSource(wav), "", &out, opts); err != nil {
				t.Fatalf("Transcode to %s: %v", format, err)
			}
			return out.Bytes()
		}
		path := filepath.Join(t.TempDir(), "out."+format)
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := waxflow.New().Transcode(context.Background(), container.BytesSource(wav), "", file, opts); err != nil {
			t.Fatalf("Transcode to %s (seekable): %v", format, err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	for _, format := range []string{"opus", "flac", "mp3", "aac", "alac"} {
		for _, seekable := range []bool{false, true} {
			name := format + "/streaming"
			if seekable {
				name = format + "/seekable"
			}
			t.Run(name, func(t *testing.T) {
				raw := transcode(t, format, seekable)
				doc, err := waxlabel.Parse(t.Context(), container.BytesSource(raw))
				if err != nil {
					if format == "aac" || format == "alac" {
						// waxlabel cannot parse fragmented MP4 (the
						// recorded finding; the mux is fragmented in
						// both forms); the m4b golden and the jobs e2e
						// cover the MP4 ilst path instead.
						t.Skipf("waxlabel cannot parse the fragmented %s output: %v", format, err)
					}
					t.Fatalf("waxlabel.Parse: %v", err)
				}
				fields := doc.Fields()
				if fields.Title != "Stream Title" {
					t.Errorf("waxlabel TITLE %q, want %q", fields.Title, "Stream Title")
				}
				if len(fields.Artists) != 1 || fields.Artists[0] != "Stream Artist" {
					t.Errorf("waxlabel ARTIST %v, want [Stream Artist]", fields.Artists)
				}
			})
		}
	}
}
