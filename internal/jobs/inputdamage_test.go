package jobs

// The input-damage warning cell. A frame-walked source finds its damage
// where the read reaches it, so the probe at creation sees a clean head and
// only the write, which reads the whole file, has the finding. It rides the
// job's Warnings list beside the clipping note, prefixed so a client can
// tell input damage from the metadata warnings folded at creation.

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
