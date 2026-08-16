package jobs

// The clipping-warning cell. A job whose output's level did not fit still
// succeeds, since a clamp is inherent to an integer target and a hot true
// peak is a property of the audio; what it owes the caller is a note saying
// so. It rides the existing Warnings list rather than a new Output field:
// the numbers are already on the engine result, and a new wire field would
// churn the client mirror and the goldens for values nothing else reads.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/internal/testutil"
)

// libWAV builds a library root holding one WAV and returns its ref.
func libWAV(t *testing.T, dir string, chans [][]float32) (ref string) {
	t.Helper()
	testutil.WriteFloatWAV(t, filepath.Join(dir, "hot.wav"), 44100, chans)
	return "lib/hot.wav"
}

// hotLib holds the shared clipping fixture, or a genuinely quiet tone: the
// zero-overs HotFloatChans still rings past full scale at its rail step,
// which would earn the quiet case a true-peak warning instead of silence.
// A second of audio, so a split has room for two pieces.
func hotLib(t *testing.T, dir string, hot bool) (ref string) {
	t.Helper()
	if hot {
		return libWAV(t, dir, testutil.HotFloatChans(t, 44100, 44100, 2))
	}
	return libWAV(t, dir, testutil.ToneChans(44100, 44100))
}

// warnWith returns the job's first warning carrying the prefix, or "".
func warnWith(j *Job, prefix string) string {
	for _, w := range j.Warnings {
		if strings.HasPrefix(w, prefix) {
			return w
		}
	}
	return ""
}

func TestTranscodeJobWarnsOnClipping(t *testing.T) {
	for _, tc := range []struct {
		name string
		hot  bool
		want bool
	}{
		{"over full scale", true, true},
		{"in range", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			ref := hotLib(t, root, tc.hot)
			res := openRoots(t, root)
			r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res})

			j, err := r.Create(transcodeReq(ref, pinID(t, res, ref)))
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			// Clipping is a note, not a failure: the job still completes.
			done := waitJob(t, r, j.ID, StateDone)

			note := warnWith(done, "clipping: ")
			if (note != "") != tc.want {
				t.Fatalf("warnings = %v, want a clipping note: %v", done.Warnings, tc.want)
			}
			if tc.want && !strings.Contains(note, "4 of 88200 output samples exceeded full scale") {
				t.Errorf("warning %q does not carry the count and its total", note)
			}
			// One note per run in either case: a clamp already says the level
			// was over, and the quiet case has nothing to say at all.
			if tp := warnWith(done, "true peak: "); tp != "" {
				t.Errorf("job carries a true-peak warning %q alongside the clamp report", tp)
			}
		})
	}
}

// TestTranscodeJobWarnsOnTruePeak: the case the count cannot see. Every
// stored sample fits, so the quantizer clamps nothing; the output still
// clips on playback, and the job's warning list is where that is said.
func TestTranscodeJobWarnsOnTruePeak(t *testing.T) {
	root := t.TempDir()
	ref := libWAV(t, root, testutil.IntersampleHotChans(44100, 44100))
	res := openRoots(t, root)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res})

	j, err := r.Create(transcodeReq(ref, pinID(t, res, ref)))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	done := waitJob(t, r, j.ID, StateDone)

	note := warnWith(done, "true peak: ")
	if note == "" {
		t.Fatalf("warnings = %v, want a true-peak note", done.Warnings)
	}
	if !strings.Contains(note, "dBTP") {
		t.Errorf("warning %q does not carry the measured level", note)
	}
	if clip := warnWith(done, "clipping: "); clip != "" {
		t.Errorf("an intersample-only source drew a clipping warning %q", clip)
	}
}

// TestSplitJobWarnsOnClipping: the warning belongs on writeMedia, the tail
// every audio-writing job shares, not on runTranscode alone. A split whose
// pieces were clamped used to complete with an empty Warnings list, so a
// caller reading warnings to decide whether a rip was clean was told nothing.
func TestSplitJobWarnsOnClipping(t *testing.T) {
	root := t.TempDir()
	ref := hotLib(t, root, true)
	res := openRoots(t, root)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res})

	j, err := r.Create(Request{
		Type: TypeSplit, Src: ref, SourceID: pinID(t, res, ref),
		Format: "flac", FLACLevel: -1, Cuts: []int64{22050},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	done := waitJob(t, r, j.ID, StateDone)
	if len(done.Outputs) != 2 {
		t.Fatalf("split produced %d outputs, want 2", len(done.Outputs))
	}
	// The overs sit in the first piece, so exactly one piece reports, and
	// the note must say which: two clipped tracks of twelve would otherwise
	// be indistinguishable in the warning list.
	note := warnWith(done, "clipping: ")
	if note == "" {
		t.Fatalf("a split whose first piece clipped carried no warning: %v", done.Warnings)
	}
	if !strings.Contains(note, " in "+done.Outputs[0].File) {
		t.Errorf("warning %q does not name the clipped piece %q", note, done.Outputs[0].File)
	}
}
