package jobs

// The input-damage warning cell. A frame-walked source finds its damage
// where the read reaches it, so the probe at creation sees a clean head and
// only a read that covers the whole file has the finding: the write, or an
// analysis. It rides the job's Warnings list beside the clipping note,
// prefixed so a client can tell input damage from the metadata warnings
// folded at creation.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/internal/testutil"
)

func TestTranscodeJobWarnsOnInputDamage(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sine-untagged.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "damaged.mp3"), testutil.ZeroMiddle(raw, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	res := openRoots(t, root)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res})

	const ref = "lib/damaged.mp3"
	j, err := r.Create(transcodeReq(ref, pinID(t, res, ref)))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Tolerated damage is a note, not a failure: the job still completes.
	done := waitJob(t, r, j.ID, StateDone)
	note := warnWith(done, "input damage: ")
	if note == "" {
		t.Fatalf("warnings = %v, want the input damage the read found", done.Warnings)
	}
	if !strings.Contains(note, "unparsable bytes skipped") {
		t.Errorf("warning %q does not name the skipped bytes", note)
	}
}

// TestAnalyzeJobWarnsOnInputDamage: an analyze job reads the whole source
// too, so it carries the same finding a transcode of the file would. The
// engine opens and closes the source inside Analyze, which is why the list
// has to ride the result rather than be read off the media afterwards.
func TestAnalyzeJobWarnsOnInputDamage(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sine-untagged.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "damaged.mp3"), testutil.ZeroMiddle(raw, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	res := openRoots(t, root)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res})

	const ref = "lib/damaged.mp3"
	j, err := r.Create(analyzeReq(ref, pinID(t, res, ref)))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	done := waitJob(t, r, j.ID, StateDone)
	note := warnWith(done, "input damage: ")
	if note == "" {
		t.Fatalf("warnings = %v, want the input damage the read found", done.Warnings)
	}
	if !strings.Contains(note, "unparsable bytes skipped") {
		t.Errorf("warning %q does not name the skipped bytes", note)
	}
}

// TestTwoPassJobWarnsOnInputDamageOnce: a loudness:analyze transcode reads
// the source twice, and the first pass attaches what it found so an encode
// that fails afterwards cannot lose it; the encode's own read then finds the
// same damage, and the job carries it once.
func TestTwoPassJobWarnsOnInputDamageOnce(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sine-untagged.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "damaged.mp3"), testutil.ZeroMiddle(raw, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	res := openRoots(t, root)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res})

	const ref = "lib/damaged.mp3"
	req := transcodeReq(ref, pinID(t, res, ref))
	req.Loudness = "analyze"
	j, err := r.Create(req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	done := waitJob(t, r, j.ID, StateDone)
	var found int
	for _, w := range done.Warnings {
		if strings.HasPrefix(w, "input damage: ") {
			found++
		}
	}
	if found != 1 {
		t.Errorf("warnings = %v, want the damage once", done.Warnings)
	}
}
