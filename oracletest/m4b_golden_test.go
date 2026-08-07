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

// TestGoldenM4BChapters is the audiobook passthrough pin: an m4b with
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
	if len(info.Chapters) != 3 {
		t.Fatalf("fixture metadata: %d chapters, want 3", len(info.Chapters))
	}
	// The descriptive tags both read-back routes are held to, resolved once so
	// the fixture guard and the assertions cannot disagree about which keys
	// have to exist: a regenerated fixture missing one reports that here
	// instead of panicking on an index deep in the comparison below.
	wantTags := map[string]string{"TITLE": "Chaptered Book"}
	for _, key := range []string{"ARTIST", "ALBUM"} {
		if len(info.Tags[key]) == 0 {
			t.Fatalf("fixture is missing %s: tags %v", key, info.Tags)
		}
		wantTags[key] = info.Tags[key][0]
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
	// Present is not the same as readable, and the difference is how B1
	// shipped: these atoms were asserted by substring for a release and
	// nothing ever read one back. Both routes are exercised, because a
	// regression can reach one and not the other.
	//
	// The tag library reads a fragmented movie as of v1.4.0, so it is held to
	// the same three tags and the same chapter count the container path is
	// below. Values rather than a count: a count passes on partial metadata,
	// which is exactly the regression a new upstream reader could introduce,
	// and the chapters are the half this test exists for.
	back, err := label.New().Read(ctx, container.BytesSource(got), "m4b", meta.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range wantTags {
		if vs := back.Tags[key]; len(vs) != 1 || vs[0] != want {
			t.Errorf("the tag library read back %s = %v, want [%q]", key, vs, want)
		}
	}
	if len(back.Chapters) != 3 {
		t.Errorf("the tag library read back %d chapters, want 3", len(back.Chapters))
	}
	probed, err := waxflow.New().Probe(container.BytesSource(got), "m4b", nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range wantTags {
		if vs := probed.Tags[key]; len(vs) != 1 || vs[0] != want {
			t.Errorf("container read back %s = %v, want [%q]", key, vs, want)
		}
	}
	if len(probed.Chapters) != 3 {
		t.Errorf("container read back %d chapters, want 3", len(probed.Chapters))
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
