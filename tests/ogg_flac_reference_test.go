package waxflow_test

// The reference producer's Ogg-FLAC, which is the only one that writes a
// nonzero STREAMINFO total: ffmpeg writes 0 and so did this tree until the
// muxer learned to stamp its projection. The declared-total branches are
// pinned on patched fixtures in container/ogg (no binary needed); this is the
// cross-check that the shape those pins describe is the shape a real encoder
// writes.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestReferenceOggFLACFixtureDecodes is the committed fixture against a reader
// that has no stake in what either this tree or the reference thinks its
// headers say. It needs no flac binary, so it runs everywhere ffmpeg does.
//
// The fixture is container/ogg/testdata/ref-flac.oga; its recipe is in
// container/ogg/mapflacverify_test.go, beside the cell that checks its granule.
func TestReferenceOggFLACFixtureDecodes(t *testing.T) {
	testutil.FFmpeg(t)
	path := filepath.Join("..", "container", "ogg", "testdata", "ref-flac.oga")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := format.Probe(container.BytesSource(raw), "oga", nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := info.Default()
	if !tr.SamplesExact {
		t.Errorf("a declared total the granule confirms must be exact: %+v", tr)
	}
	if got, want := len(testutil.FFmpegDecodeS32(t, path))/tr.Fmt.Channels, int(tr.Samples); got != want {
		t.Errorf("ffmpeg decodes %d frames, the probe reports %d", got, want)
	}
}

// TestReferenceOggFLACDeclaresItsTotal: `flac --ogg` writes the length into
// STREAMINFO, the final page granule agrees with it, and an open reports it
// exact with nothing to complain about. It runs against a fresh encode rather
// than the committed fixture, so the pin follows the installed reference.
func TestReferenceOggFLACDeclaresItsTotal(t *testing.T) {
	flacBin := testutil.FlacTool(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "ref-flac.oga")
	in := repoPath("testdata", "sine-s16.wav")
	cmd := exec.Command(flacBin, "--ogg", "-s", "-f", "-o", out, in)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("flac --ogg: %v\n%s", err, b)
	}
	testutil.FlacTest(t, out)

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// The field this whole branch exists for: a nonzero total in the BOS
	// page's STREAMINFO. Without it the fixture proves nothing the committed
	// ffmpeg ones do not already.
	if total := oggFLACStreamInfo(t, raw).Samples; total == 0 {
		t.Fatal("the reference wrote STREAMINFO total 0; this cell needs a declared total")
	}
	info, err := format.Probe(container.BytesSource(raw), "oga", nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := info.Default()
	if !tr.SamplesExact {
		t.Errorf("a declared total the granule confirms must be exact: %+v", tr)
	}
	if len(info.Warnings) != 0 || len(info.Notes) != 0 {
		t.Errorf("the reference's own file disagrees with itself: %v %v", info.Warnings, info.Notes)
	}
	if want := int64(oggFLACStreamInfo(t, raw).Samples); tr.Samples != want {
		t.Errorf("probe reports %d samples, STREAMINFO declares %d", tr.Samples, want)
	}
	// And against ffmpeg, which reads the same file with no reference to what
	// either of us thinks the header says.
	if testutil.HaveFFmpeg(t) {
		if got, want := len(testutil.FFmpegDecodeS32(t, out))/int(tr.Fmt.Channels), int(tr.Samples); got != want {
			t.Errorf("ffmpeg decodes %d frames, the probe reports %d", got, want)
		}
	}
}
