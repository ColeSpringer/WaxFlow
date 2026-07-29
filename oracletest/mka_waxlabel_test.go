package oracletest

// The Matroska rewrite cell. One tool rewrites these files in place, the CLI's
// .mka tag post-pass through cli/label into waxlabel, whose Matroska writer
// rebuilds the layout, SeekHead and Cues. It lives in oracletest for the same
// reason streamtags_test.go does: depcheck keeps the public tree stdlib-only.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	waxlabel "github.com/colespringer/waxlabel"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mka"
	"github.com/colespringer/waxflow/internal/meta"
)

// TestMKAWaxlabelRewrite transcodes to .mka, tags it through the real CLI
// post-pass, and requires our reader to still find a sound file. Both muxer
// forms run; only the seekable one has an index for the rewrite to disturb.
func TestMKAWaxlabelRewrite(t *testing.T) {
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	// A dozen clusters, so a shallow seek has something to skip.
	const frames = 44100 * 50
	wav, _ := synthWAV(t, f, frames)

	for _, format := range []string{"flac", "opus"} {
		for _, seekable := range []bool{false, true} {
			name := format + "/streaming"
			if seekable {
				name = format + "/seekable"
			}
			t.Run(name, func(t *testing.T) {
				raw := transcodeMKA(t, wav, format, seekable)
				before := readMKA(t, raw)

				path := filepath.Join(t.TempDir(), "tagged.mka")
				if err := os.WriteFile(path, raw, 0o644); err != nil {
					t.Fatal(err)
				}
				extra := []container.Tag{
					{Key: "TITLE", Value: "Matroska Title"},
					{Key: "ARTIST", Value: "Matroska Artist"},
				}
				if err := label.New().Apply(context.Background(), path, &meta.Info{}, extra); err != nil {
					t.Fatalf("the CLI tag post-pass: %v", err)
				}
				tagged, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Equal(tagged, raw) {
					t.Fatal("the tag post-pass wrote nothing; nothing below is being tested")
				}

				// waxlabel reads its own tags back.
				doc, err := waxlabel.Parse(t.Context(), container.BytesSource(tagged))
				if err != nil {
					t.Fatalf("waxlabel.Parse of the tagged file: %v", err)
				}
				if got := doc.Fields().Title; got != "Matroska Title" {
					t.Errorf("waxlabel TITLE %q, want %q", got, "Matroska Title")
				}

				// And our reader still finds a sound file behind them.
				after := readMKA(t, tagged)
				if after.samples != before.samples {
					t.Errorf("track length changed across the rewrite: %d, was %d", after.samples, before.samples)
				}
				if after.packets != before.packets {
					t.Errorf("packet count changed across the rewrite: %d, was %d", after.packets, before.packets)
				}
				// A cheap shallow seek is the external proof that the SeekHead
				// still resolves and the Cues still name clusters. FLAC only: a
				// CodecDelay track walks the whole stream eagerly at open, so no
				// seek on it can be bounded.
				if seekable && format == "flac" && after.shallowRead*2 >= after.deepRead {
					t.Errorf("a shallow seek read %d bytes against a deep seek's %d; the rewrite's index no longer resolves",
						after.shallowRead, after.deepRead)
				}
			})
		}
	}
}

// mkaShape is what the reader can say about a file: its declared length,
// packet count, and what a shallow and a deep seek cost.
type mkaShape struct {
	samples     int64
	packets     int
	shallowRead int64
	deepRead    int64
}

// countingSource totals the bytes read, so an index that stops working shows
// up as a seek costing a whole-file walk.
type countingSource struct {
	src  container.Source
	read int64
}

func (c *countingSource) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.src.ReadAt(p, off)
	c.read += int64(n)
	return n, err
}

func (c *countingSource) Size() int64 { return c.src.Size() }

// readMKA opens raw with the production demuxer, drains it, and seeks.
func readMKA(t *testing.T, raw []byte) mkaShape {
	t.Helper()
	d, err := mka.NewDemuxer(container.BytesSource(raw), &mka.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	out := mkaShape{samples: d.Tracks()[0].Samples}
	for {
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			break
		}
		out.packets++
	}
	if out.packets == 0 {
		t.Fatal("the file yielded no packets")
	}
	if w := d.Warnings(); len(w) != 0 {
		t.Errorf("warnings: %v", w)
	}
	out.shallowRead = seekCost(t, raw, out.samples/10)
	out.deepRead = seekCost(t, raw, out.samples*9/10)
	return out
}

// seekCost seeks once on a fresh demuxer and returns the bytes served. The
// landing is checked too: at or before the target, however the index behaves.
func seekCost(t *testing.T, raw []byte, target int64) int64 {
	t.Helper()
	c := &countingSource{src: container.BytesSource(raw)}
	d, err := mka.NewDemuxer(c, nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	landed, err := d.SeekSample(0, target)
	if err != nil {
		t.Fatalf("SeekSample to %d: %v", target, err)
	}
	if landed > target {
		t.Errorf("seek to %d landed past it, at %d", target, landed)
	}
	return c.read
}

// transcodeMKA runs the WAV to Matroska on a plain writer or a file.
func transcodeMKA(t *testing.T, wav []byte, format string, seekable bool) []byte {
	t.Helper()
	opts := waxflow.TranscodeOptions{Format: format, Container: "mka"}
	if !seekable {
		var out bytes.Buffer
		if _, err := waxflow.New().Transcode(context.Background(), container.BytesSource(wav), "", &out, opts); err != nil {
			t.Fatalf("Transcode to %s/mka: %v", format, err)
		}
		return out.Bytes()
	}
	path := filepath.Join(t.TempDir(), "out.mka")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waxflow.New().Transcode(context.Background(), container.BytesSource(wav), "", file, opts); err != nil {
		t.Fatalf("Transcode to %s/mka (seekable): %v", format, err)
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
