package jobs

// The lost-audio cell. A FLAC that lost a frame keeps its length, with
// silence where the frame was, and one cut short keeps the total its encoder
// wrote. Both leave a count a job's result carries as a field, since a
// client told only "input damage" cannot tell a dropped tag from a missing
// minute, and names once in its Warnings.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/internal/testutil"
)

// lossFixtures writes two edits of the FLAC fixture into dir, holed.flac with
// its second frame damaged and cut.flac holding its first three frames, and
// returns what the hole took and how many samples the whole file holds.
func lossFixtures(t *testing.T, dir string) (hole, total int64) {
	t.Helper()
	raw, frames := testutil.FLACFixture(t)
	for name, edit := range map[string][]byte{
		"holed.flac": testutil.DamageFLACFrames(t, raw, frames, 1),
		"cut.flac":   raw[:frames[2].End],
	} {
		if err := os.WriteFile(filepath.Join(dir, name), edit, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	last := frames[len(frames)-1]
	return frames[1].Dur, last.Pos + last.Dur
}

func TestAnalyzeJobCountsLostAndTruncatedSamples(t *testing.T) {
	root := t.TempDir()
	hole, total := lossFixtures(t, root)
	res := openRoots(t, root)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res})

	j, err := r.Create(analyzeReq("lib/holed.flac", pinID(t, res, "lib/holed.flac")))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	done := waitJob(t, r, j.ID, StateDone)
	if a := done.Analysis; a.LostSamples != hole || a.TruncatedSamples != 0 {
		t.Errorf("a holed source: %d lost and %d truncated of %d, want %d and 0",
			a.LostSamples, a.TruncatedSamples, a.Samples, hole)
	}
	if note := warnWith(done, fmt.Sprintf("input damage: %d samples", hole)); !strings.HasSuffix(note, "are silence where damage took the audio") {
		t.Errorf("warnings = %v, want one counting the silence", done.Warnings)
	}

	j, err = r.Create(analyzeReq("lib/cut.flac", pinID(t, res, "lib/cut.flac")))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	done = waitJob(t, r, j.ID, StateDone)
	a := done.Analysis
	if want := total - a.Samples; a.TruncatedSamples != want || want <= 0 || a.LostSamples != 0 {
		t.Errorf("a source cut short: %d lost and %d truncated of %d, want 0 and %d",
			a.LostSamples, a.TruncatedSamples, a.Samples, want)
	}
	if note := warnWith(done, "input damage: the audio ends "); !strings.HasSuffix(note, " short of the length its source declares") {
		t.Errorf("warnings = %v, want one counting what the source declares past its end", done.Warnings)
	}
}

func TestTranscodeJobCountsLostAndTruncatedSamples(t *testing.T) {
	root := t.TempDir()
	hole, total := lossFixtures(t, root)
	res := openRoots(t, root)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res})

	j, err := r.Create(transcodeReq("lib/holed.flac", pinID(t, res, "lib/holed.flac")))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	done := waitJob(t, r, j.ID, StateDone)
	if out := soleOutput(t, done); out.LostSamples != hole || out.TruncatedSamples != 0 {
		t.Errorf("a holed source: output has %d lost and %d truncated of %d, want %d and 0",
			out.LostSamples, out.TruncatedSamples, out.Samples, hole)
	}
	if note := warnWith(done, fmt.Sprintf("input damage: %d samples", hole)); note == "" {
		t.Errorf("warnings = %v, want one counting the silence", done.Warnings)
	}

	j, err = r.Create(transcodeReq("lib/cut.flac", pinID(t, res, "lib/cut.flac")))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	done = waitJob(t, r, j.ID, StateDone)
	out := soleOutput(t, done)
	if want := total - out.Samples; out.TruncatedSamples != want || want <= 0 || out.LostSamples != 0 {
		t.Errorf("a source cut short: output has %d lost and %d truncated of %d, want 0 and %d",
			out.LostSamples, out.TruncatedSamples, out.Samples, want)
	}
}

// TestTwoPassJobCountsLostSamplesOnce: a loudness:analyze transcode reads
// the source twice, and both results carry the count: the measurement at the
// source's rate, the output at its own. The warning is the output's, once.
func TestTwoPassJobCountsLostSamplesOnce(t *testing.T) {
	root := t.TempDir()
	hole, _ := lossFixtures(t, root)
	res := openRoots(t, root)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res})
	req := transcodeReq("lib/holed.flac", pinID(t, res, "lib/holed.flac"))
	req.Loudness = "analyze"
	j, err := r.Create(req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	done := waitJob(t, r, j.ID, StateDone)
	if got := done.Analysis.LostSamples; got != hole {
		t.Errorf("the measurement counts %d lost samples, want %d", got, hole)
	}
	if got := soleOutput(t, done).LostSamples; got != hole {
		t.Errorf("the output counts %d lost samples, want %d", got, hole)
	}
	var notes int
	for _, w := range done.Warnings {
		if strings.Contains(w, "are silence where damage took the audio") {
			notes++
		}
	}
	if notes != 1 {
		t.Errorf("warnings = %v, want the silence counted once", done.Warnings)
	}
}
