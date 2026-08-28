package waxflow_test

// Oversized tags are refused, not dropped. Every container that embeds tags
// caps the block it writes them into, and each cap used to be enforced by
// skipping the item that did not fit: the file came back missing a tag the
// caller asked to embed, and the transcode reported success. The loss reached
// whoever read the file back instead of whoever could still fix it.
//
// These run through Transcode rather than the muxers because that is where the
// silence was: the muxer's own tests can pin a refusal that the engine then
// swallows.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/waxerr"
)

// tagCapRows are the outputs whose muxer embeds tags in a capped block: the
// block each one names in its refusal, and a key that output actually writes.
// mp3 needs its own key because the ID3v2 writer maps a descriptive core and
// has no LYRICS frame, so an oversized LYRICS there is an unmapped key rather
// than the cap under test.
var tagCapRows = []struct{ format, block, key string }{
	{"wavpack", "APEv2 tag block", "LYRICS"},
	{"ape", "APEv2 tag block", "LYRICS"},
	{"flac", "VORBIS_COMMENT block", "LYRICS"},
	{"opus", "comment header", "LYRICS"},
	{"vorbis", "comment header", "LYRICS"},
	{"mp3", "ID3v2 tag", "ARTIST"},
}

// TestOversizedTagIsRefused checks a tag too large for the output's block fails
// the transcode with a message naming the key, rather than producing a file
// that quietly lacks it.
func TestOversizedTagIsRefused(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "sine-s16.wav"))
	if err != nil {
		t.Fatal(err)
	}
	// Well past every cap here, and the shape that found this: a lyric sheet.
	big := strings.Repeat("a line of the sheet\n", 4000)
	for _, row := range tagCapRows {
		t.Run(row.format, func(t *testing.T) {
			// A real file: the ape muxer needs a seekable destination, and the
			// refusal has to arrive before that matters.
			out, err := os.Create(filepath.Join(t.TempDir(), "out"))
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			_, err = waxflow.New().Transcode(context.Background(), container.BytesSource(raw), "wav", out,
				waxflow.TranscodeOptions{Format: row.format, Tags: []container.Tag{
					{Key: "TITLE", Value: "Kept"},
					{Key: row.key, Value: big},
				}})
			if err == nil {
				t.Fatal("the transcode succeeded over a tag the output cannot hold")
			}
			// The caller has to learn which tag to trim and how far.
			for _, want := range []string{row.key, row.block} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
			if code := waxerr.CodeOf(err); code != waxerr.CodeUnsupportedFormat {
				t.Errorf("refusal carries code %q, want %q", code, waxerr.CodeUnsupportedFormat)
			}
			// Nothing was written at all. The tag is known before any header
			// is, so every one of these refuses without touching the
			// destination: no audio encoded, and for the Ogg rows not even the
			// identification page. A byte here means the check moved after
			// something was emitted, which is the regression this pins, and
			// comparing against a full encode would not catch it -- a refusal
			// from End leaves a file smaller than a complete one too.
			st, err := out.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if st.Size() != 0 {
				t.Errorf("the refused output is %d bytes, want nothing written", st.Size())
			}
		})
	}
}

// TestOrdinaryTagsStillEmbed is the other half: the refusal must be the cap
// biting, not tagging breaking. A descriptive set well under every cap still
// lands, so a test that only asserted the refusal could not pass by accident.
func TestOrdinaryTagsStillEmbed(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "sine-s16.wav"))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range tagCapRows {
		t.Run(row.format, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out")
			out, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = waxflow.New().Transcode(context.Background(), container.BytesSource(raw), "wav", out,
				waxflow.TranscodeOptions{Format: row.format, Tags: []container.Tag{
					{Key: "TITLE", Value: "A Title"},
					{Key: "ARTIST", Value: "WaxFlow"},
				}})
			out.Close()
			if err != nil {
				t.Fatalf("transcode with ordinary tags: %v", err)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), "A Title") {
				t.Error("the title never reached the file")
			}
		})
	}
}

// TestForwardedSourceTagsStillTranscode is the regression the refusal
// introduced. The callers that carry a source's tags onto an output (the jobs
// runner, the CLI's transcode and split, the server's stream path) hand the
// muxer whatever the source had. Once an oversized tag became a refusal rather
// than a skip, any file carrying one stopped transcoding at all -- to every
// embedding format, for a tag nobody asked for. meta.EmbeddableTags is the
// projection that absorbs it, and this walks the whole path: build a source
// with a tag no capped block can hold (MP4 bounds nothing, so it can hold it),
// read its tags back, project them, and transcode.
func TestForwardedSourceTagsStillTranscode(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "sine-s16.wav"))
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("a line of the sheet\n", 4000)
	src := filepath.Join(t.TempDir(), "src.m4a")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	_, err = waxflow.New().Transcode(context.Background(), container.BytesSource(raw), "wav", f,
		waxflow.TranscodeOptions{Format: "aac", Tags: []container.Tag{
			{Key: "TITLE", Value: "Sung"},
			{Key: "LYRICS", Value: big},
		}})
	f.Close()
	if err != nil {
		t.Fatalf("MP4 refused a tag it bounds nothing on: %v", err)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	d, err := mp4.NewDemuxer(container.BytesSource(b), nil)
	if err != nil {
		t.Fatal(err)
	}
	tagger, ok := any(d).(interface{ Tags() map[string][]string })
	if !ok {
		t.Fatal("the MP4 demuxer reads no tags, so this test proves nothing")
	}
	forwarded, dropped := meta.EmbeddableTags(meta.FullTags(meta.WithContainerTags(nil, tagger.Tags())))
	if len(dropped) != 1 || dropped[0] != "LYRICS" {
		t.Fatalf("the projection dropped %v, want the oversized LYRICS alone", dropped)
	}
	for _, row := range tagCapRows {
		t.Run(row.format, func(t *testing.T) {
			out, err := os.Create(filepath.Join(t.TempDir(), "out"))
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			if _, err := waxflow.New().Transcode(context.Background(), container.BytesSource(raw), "wav", out,
				waxflow.TranscodeOptions{Format: row.format, Tags: forwarded}); err != nil {
				t.Errorf("a source carrying an oversized tag no longer transcodes: %v", err)
			}
		})
	}
}
