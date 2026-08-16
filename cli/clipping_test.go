package cli

// The clipping-note cell: an output whose level did not fit tells the
// operator on stderr, and one that fits says nothing. The absence half is
// the one worth having a test for, since a note that fires on every file is
// worse than no note at all.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/internal/testutil"
)

// hotWAV writes the shared clipping fixture when hot, and a genuinely quiet
// tone otherwise. The quiet half is ToneChans on purpose: even at zero
// overs, HotFloatChans' rail step rings past full scale when reconstructed,
// which would earn a true-peak note and leave the absence case asserting
// nothing.
func hotWAV(t *testing.T, path string, hot bool) {
	chans := testutil.ToneChans(44100, 4410)
	if hot {
		chans = testutil.HotFloatChans(t, 44100, 4410, 2)
	}
	testutil.WriteFloatWAV(t, path, 44100, chans)
}

func TestTranscodeClippingNote(t *testing.T) {
	for _, tc := range []struct {
		name string
		hot  bool
		want bool
	}{
		{"over full scale", true, true},
		{"in range", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "src.wav")
			hotWAV(t, src, tc.hot)

			code, stdout, stderr := run(t, "transcode", src, filepath.Join(dir, "out.flac"))
			if code != 0 {
				t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
			}
			if got := strings.Contains(stderr, "clipping: "); got != tc.want {
				t.Errorf("stderr carries a clipping note: %v, want %v; stderr: %s", got, tc.want, stderr)
			}
			// One note per run in either case: the true-peak note is for
			// outputs the count cannot see, never a second line on a clamp.
			if strings.Contains(stderr, "true peak: ") {
				t.Errorf("stderr carries a true-peak note: %s", stderr)
			}
			if tc.want {
				// Both numbers: the count and the total it is a fraction of.
				if !strings.Contains(stderr, "4 of 8820 output samples exceeded full scale") {
					t.Errorf("stderr does not carry the count and its total: %s", stderr)
				}
				// The note goes to stderr, so a script consuming the "wrote"
				// line on stdout is unaffected by it.
				if strings.Contains(stdout, "clipping") {
					t.Errorf("the clipping note reached stdout: %s", stdout)
				}
			}
		})
	}
}

// TestTranscodeTruePeakNote: a source over only between samples clamps
// nothing, so the count stays silent; the true-peak note is what tells the
// operator the output still clips on playback.
func TestTranscodeTruePeakNote(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.wav")
	testutil.WriteFloatWAV(t, src, 44100, testutil.IntersampleHotChans(44100, 4410))

	code, stdout, stderr := run(t, "transcode", src, filepath.Join(dir, "out.flac"))
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "true peak: ") || !strings.Contains(stderr, "dBTP") {
		t.Errorf("stderr carries no true-peak note: %s", stderr)
	}
	if strings.Contains(stderr, "clipping: ") {
		t.Errorf("an intersample-only source drew a clipping note: %s", stderr)
	}
	if strings.Contains(stdout, "true peak") {
		t.Errorf("the true-peak note reached stdout: %s", stdout)
	}
}

// TestSplitClippingNote: every command that writes audio reports the same
// way. A split that clamped its pieces used to print nothing at all, so an
// operator who trusted the note read its absence as "this rip was fine".
func TestSplitClippingNote(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "album.wav")
	hotWAV(t, src, true)
	outDir := filepath.Join(dir, "pieces")

	code, _, stderr := run(t, "split", "--at", "2205", src, outDir)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	// The overs sit in the first piece (samples 100 and 137 of the source),
	// so exactly one of the two pieces reports.
	if n := strings.Count(stderr, "clipping: "); n != 1 {
		t.Errorf("split printed %d clipping notes, want 1; stderr: %s", n, stderr)
	}
}
