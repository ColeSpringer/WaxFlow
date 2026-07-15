package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/waxerr"
)

// cueFixture writes a 44.1 kHz rip and a CUE sheet indexing it at the given
// CD-frame starts, into the env's library root.
//
// 44.1 kHz because a CUE sheet describes a CD: a frame is 1/75 s, and
// 44100/75 = 588 exactly, which is the whole reason a boundary converts to a
// sample without rounding. A 48 kHz fixture would divide by 75 exactly too
// (640) and so would hide nothing, but it would also be describing a rip
// that does not exist.
func cueFixture(t *testing.T, env *testEnv, name string, frameStarts []int) int64 {
	t.Helper()
	total := int64(frameStarts[len(frameStarts)-1])*588 + 44100
	if err := os.WriteFile(filepath.Join(env.root, name+".wav"),
		rampWAV(t, 44100, 2, int(total)), 0o644); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "PERFORMER %q\nTITLE %q\nFILE %q WAVE\n", "The Band", "The Album", name+".wav")
	for i, f := range frameStarts {
		fmt.Fprintf(&b, "  TRACK %02d AUDIO\n    TITLE \"Track %d\"\n    INDEX 01 %02d:%02d:%02d\n",
			i+1, i+1, f/75/60, (f/75)%60, f%75)
	}
	if err := os.WriteFile(filepath.Join(env.root, name+".cue"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return total
}

// TestSplitJobFromCueSheet is the daemon's half of the CUE story: a client
// hands the daemon a sheet, not a list of samples it had to derive itself.
//
// That is the point of the surface. The CD-frame arithmetic is subtle in
// exactly one direction (1/75 s is not representable in any nanosecond
// clock, and the obvious conversion is a sample short at every boundary),
// so making each client redo it is making each client rediscover the bug.
// The daemon resolves the sheet into the same cuts a caller could have sent
// by hand, and the job is those cuts.
func TestSplitJobFromCueSheet(t *testing.T) {
	env := jobsEnv(t)
	// Boundaries on frames that are not whole seconds: FF = 37, 12, 61.
	starts := []int{0, 5*75 + 37, 11*75 + 12, 18*75 + 61}
	total := cueFixture(t, env, "rip", starts)

	body, err := json.Marshal(map[string]any{
		"type": "split", "src": "lib/rip.wav", "format": "flac", "cue": "lib/rip.cue",
	})
	if err != nil {
		t.Fatal(err)
	}
	job := awaitJob(t, env, createJob(t, env, string(body)))

	if len(job.Outputs) != len(starts) {
		t.Fatalf("the sheet names %d tracks, the split made %d pieces: %+v",
			len(starts), len(job.Outputs), job.Outputs)
	}
	// The sheet's own arithmetic, spelled out rather than taken from
	// cue.Samples: an expectation computed by the code under test would
	// agree with any conversion at all, including the wrong one.
	for i, out := range job.Outputs {
		end := total
		if i+1 < len(starts) {
			end = int64(starts[i+1]) * 588
		}
		if want := end - int64(starts[i])*588; out.Samples != want {
			t.Errorf("piece %d is %d samples, want %d: the cut is not on the frame the sheet names",
				i, out.Samples, want)
		}
	}

	// The sheet became cut points, and the job is those points: nothing has
	// to read the sheet again to know what this job does, and an edit to it
	// now cannot change what the 201 accepted.
	if len(job.Request.Cuts) != len(starts)-1 {
		t.Fatalf("the job carries %d cuts, want the %d interior boundaries of %d tracks",
			len(job.Request.Cuts), len(starts)-1, len(starts))
	}
	for i, c := range job.Request.Cuts {
		if want := int64(starts[i+1]) * 588; c != want {
			t.Errorf("cut %d is sample %d, want %d", i, c, want)
		}
	}
}

// TestSplitJobCueEqualsCuts is the property that makes the sheet surface
// honest rather than a second implementation: handing the daemon a sheet
// must produce the identical job to handing it the samples that sheet means.
func TestSplitJobCueEqualsCuts(t *testing.T) {
	env := jobsEnv(t)
	starts := []int{0, 5*75 + 37, 11*75 + 12}
	cueFixture(t, env, "same", starts)

	viaCue, err := json.Marshal(map[string]any{
		"type": "split", "src": "lib/same.wav", "format": "flac", "cue": "lib/same.cue",
	})
	if err != nil {
		t.Fatal(err)
	}
	viaCuts, err := json.Marshal(map[string]any{
		"type": "split", "src": "lib/same.wav", "format": "flac",
		"cuts": []int64{int64(starts[1]) * 588, int64(starts[2]) * 588},
	})
	if err != nil {
		t.Fatal(err)
	}
	a := awaitJob(t, env, createJob(t, env, string(viaCue)))
	b := awaitJob(t, env, createJob(t, env, string(viaCuts)))

	if fmt.Sprint(a.Request.Cuts) != fmt.Sprint(b.Request.Cuts) {
		t.Fatalf("cue gave cuts %v, the same boundaries by hand gave %v", a.Request.Cuts, b.Request.Cuts)
	}
	if len(a.Outputs) != len(b.Outputs) {
		t.Fatalf("cue made %d pieces, cuts made %d", len(a.Outputs), len(b.Outputs))
	}
	for i := range a.Outputs {
		if a.Outputs[i].Samples != b.Outputs[i].Samples {
			t.Errorf("piece %d: cue gave %d samples, cuts gave %d",
				i, a.Outputs[i].Samples, b.Outputs[i].Samples)
		}
	}
}

// TestSplitJobCueKeepsANonzeroLeadIn covers the sheet whose TRACK 01 does not
// begin at frame 0: a pregap, or hidden-track-one audio, which on a real CD
// can be a whole song.
//
// The lead-in is audio in the file, and the only cut list that accounts for
// every sample keeps it: the pieces are [0, c0), [c0, c1), ... so the audio
// before TRACK 01 becomes the first piece rather than being folded into
// track 1 (which would make track 1 play a song that is not it) or dropped
// (which would lose it). A sheet whose first start IS 0 has no lead-in and
// yields one fewer cut, which is the case below it.
//
// The two spellings are checked against each other because the sheet surface's
// whole promise is that it produces the job the caller could have sent by
// hand. cue.Cuts is the one funnel that decides this, shared with the CLI, so
// the daemon and the CLI cannot cut one sheet two ways.
func TestSplitJobCueKeepsANonzeroLeadIn(t *testing.T) {
	env := jobsEnv(t)
	// TRACK 01 at 00:05:37, TRACK 02 at 00:11:12: nothing claims [0, 5s).
	starts := []int{5*75 + 37, 11*75 + 12}
	cueFixture(t, env, "pregap", starts)

	body, err := json.Marshal(map[string]any{
		"type": "split", "src": "lib/pregap.wav", "format": "flac", "cue": "lib/pregap.cue",
	})
	if err != nil {
		t.Fatal(err)
	}
	job := awaitJob(t, env, createJob(t, env, string(body)))

	// Both starts are cuts: a nonzero first start opens the first track, and
	// the lead-in before it is the piece that cut closes.
	wantCuts := []int64{int64(starts[0]) * 588, int64(starts[1]) * 588}
	if fmt.Sprint(job.Request.Cuts) != fmt.Sprint(wantCuts) {
		t.Fatalf("the sheet became cuts %v, want %v: a nonzero TRACK 01 start is a cut, "+
			"because the audio before it is a piece", job.Request.Cuts, wantCuts)
	}
	if len(job.Outputs) != len(starts)+1 {
		t.Fatalf("the split made %d pieces, want %d: the lead-in, then one per track",
			len(job.Outputs), len(starts)+1)
	}
	// The first piece is the lead-in itself, and its length is the whole of
	// what the folding bug got wrong: folded into track 1, piece 0 would be
	// starts[1]*588 samples long instead.
	if want := int64(starts[0]) * 588; job.Outputs[0].Samples != want {
		t.Errorf("piece 0 is %d samples, want the %d before TRACK 01", job.Outputs[0].Samples, want)
	}

	// A sheet whose TRACK 01 is at frame 0 has no lead-in to keep, so the
	// implied 0 is dropped and N tracks make N-1 cuts. Without this the case
	// above would pass over a version that simply never dropped anything.
	cueFixture(t, env, "zero", []int{0, 11*75 + 12})
	body, err = json.Marshal(map[string]any{
		"type": "split", "src": "lib/zero.wav", "format": "flac", "cue": "lib/zero.cue",
	})
	if err != nil {
		t.Fatal(err)
	}
	zero := awaitJob(t, env, createJob(t, env, string(body)))
	if want := []int64{int64(11*75+12) * 588}; fmt.Sprint(zero.Request.Cuts) != fmt.Sprint(want) {
		t.Fatalf("a sheet starting at frame 0 became cuts %v, want %v: a cut at 0 would ask "+
			"for an empty piece", zero.Request.Cuts, want)
	}
}

func TestSplitJobCueRejects(t *testing.T) {
	env := jobsEnv(t)
	cueFixture(t, env, "rej", []int{0, 5 * 75})

	// A sheet whose tracks are already separate files: nothing to cut.
	if err := os.WriteFile(filepath.Join(env.root, "multi.cue"), []byte(
		"FILE \"01.wav\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n"+
			"FILE \"02.wav\" WAVE\n  TRACK 02 AUDIO\n    INDEX 01 00:00:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A sheet with one track: no interior boundary, so no cut.
	if err := os.WriteFile(filepath.Join(env.root, "one.cue"), []byte(
		"FILE \"rej.wav\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Not a sheet at all.
	if err := os.WriteFile(filepath.Join(env.root, "junk.cue"), []byte(
		"INDEX 01 99:99:99\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"cue and cuts together", map[string]any{
			"type": "split", "src": "lib/rej.wav", "format": "flac",
			"cue": "lib/rej.cue", "cuts": []int64{1000},
		}, "exclusive"},
		{"cue on a transcode", map[string]any{
			"type": "transcode", "src": "lib/rej.wav", "format": "flac", "cue": "lib/rej.cue",
		}, "cue applies to split"},
		{"multi-file sheet", map[string]any{
			"type": "split", "src": "lib/rej.wav", "format": "flac", "cue": "lib/multi.cue",
		}, "indexes 2 files"},
		{"single-track sheet", map[string]any{
			"type": "split", "src": "lib/rej.wav", "format": "flac", "cue": "lib/one.cue",
		}, "nothing to cut"},
		{"unparseable sheet", map[string]any{
			"type": "split", "src": "lib/rej.wav", "format": "flac", "cue": "lib/junk.cue",
		}, "cue:"},
		{"missing sheet", map[string]any{
			"type": "split", "src": "lib/rej.wav", "format": "flac", "cue": "lib/nope.cue",
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatal(err)
			}
			resp := env.postJSON(t, "/jobs", string(body))
			got := readBody(t, resp)
			if resp.StatusCode == http.StatusCreated {
				t.Fatalf("job accepted, want a refusal mentioning %q", tc.want)
			}
			if tc.want != "" && !strings.Contains(string(got), tc.want) {
				t.Errorf("body = %s, want it to mention %q", got, tc.want)
			}
		})
	}

	// A sheet naming a boundary past the source is the mismatched-rip case,
	// and it must be a 400 at creation rather than a job that dies part way.
	if err := os.WriteFile(filepath.Join(env.root, "long.cue"), []byte(
		"FILE \"rej.wav\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n"+
			"  TRACK 02 AUDIO\n    INDEX 01 99:00:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"type": "split", "src": "lib/rej.wav", "format": "flac", "cue": "lib/long.cue",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantEnvelope(t, env.postJSON(t, "/jobs", string(body)), http.StatusBadRequest, waxerr.CodeInvalidRequest)
}
