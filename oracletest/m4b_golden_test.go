package oracletest

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
)

// TestGoldenM4BChapters is the M16 audiobook passthrough pin: an m4b with
// chapters and tags transcodes to AAC fMP4 with the chapters (Nero chpl),
// the tags (ilst), and full gapless signaling (iTunSMPB plus the exact
// edit list, the seekable job path) preserved, byte for byte against the
// committed golden. Regenerate with `make goldens` and review the diff.
func TestGoldenM4BChapters(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "chapters.m4b"))
	if err != nil {
		t.Fatal(err)
	}
	src := container.BytesSource(raw)
	ctx := context.Background()

	info, err := label.New().Read(ctx, src, "m4b", meta.ReadOptions{Pictures: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Chapters) != 3 || len(info.Tags["TITLE"]) == 0 {
		t.Fatalf("fixture metadata: %d chapters, tags %v", len(info.Chapters), info.Tags)
	}

	out := filepath.Join(t.TempDir(), "out.m4b")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	e := waxflow.New()
	res, err := e.Transcode(ctx, src, "m4b", f, waxflow.TranscodeOptions{
		Format:   "aac",
		Tags:     meta.FullTags(info),
		Chapters: info.Chapters,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	// The chapters, the descriptive tags, and the gapless atom must all
	// be present regardless of the golden compare (these assertions
	// survive an intentional regeneration).
	for _, want := range []string{"chpl", "Intro", "Middle", "Coda", "\xa9nam", "Chaptered Book", "iTunSMPB"} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("output lacks %q", want)
		}
	}
	// iTunSMPB carries the exact numbers: the encoder delay, the padding
	// to a whole frame count, and the trimmed length End patched in.
	smpb := got[bytes.Index(got, []byte("iTunSMPB")):]
	payload := smpb[bytes.Index(smpb, []byte(" 00000000 ")):]
	delay := parseHexField(t, payload[10:18])
	padding := parseHexField(t, payload[19:27])
	length := parseHexField(t, payload[28:44])
	if delay != int64(aac.EncoderDelay) {
		t.Errorf("iTunSMPB delay = %d, want %d", delay, aac.EncoderDelay)
	}
	if length != res.Samples {
		t.Errorf("iTunSMPB length = %d, want %d", length, res.Samples)
	}
	if total := delay + padding + length; total%1024 != 0 {
		t.Errorf("iTunSMPB fields sum to %d, not whole AAC frames", total)
	}

	golden := filepath.Join("..", "testdata", "golden", "m4b-chapters.m4b")
	if *updateGoldens {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("missing golden %s (run `make goldens`): %v", golden, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s (%d vs %d bytes); if intentional, `make goldens` and review", golden, len(got), len(want))
	}
}

func parseHexField(t *testing.T, b []byte) int64 {
	t.Helper()
	v, err := strconv.ParseInt(string(b), 16, 64)
	if err != nil {
		t.Fatalf("hex field %q: %v", b, err)
	}
	return v
}
