package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/server"
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
	return sheetFixture(t, env, name, false, frameStarts)
}

// mixedModeFixture is cueFixture with a data track first, the shape of a
// single-file image of a mixed-mode disc (a PlayStation game, a CD32 title):
// TRACK 01 is MODE1/2352 at frame 0, the audio tracks follow at the given
// CD-frame starts, numbered from 2, and the file holds the data track's
// bytes before them.
func mixedModeFixture(t *testing.T, env *testEnv, name string, audioStarts []int) int64 {
	t.Helper()
	return sheetFixture(t, env, name, true, audioStarts)
}

// sheetFixture writes the rip and its sheet, and returns the rip's length.
func sheetFixture(t *testing.T, env *testEnv, name string, dataFirst bool, frameStarts []int) int64 {
	t.Helper()
	total := int64(frameStarts[len(frameStarts)-1])*588 + 44100
	if err := os.WriteFile(filepath.Join(env.root, name+".wav"),
		rampWAV(t, 44100, 2, int(total)), 0o644); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "PERFORMER %q\nTITLE %q\nFILE %q WAVE\n", "The Band", "The Album", name+".wav")
	first := 1
	if dataFirst {
		b.WriteString("  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n")
		first = 2
	}
	for i, f := range frameStarts {
		fmt.Fprintf(&b, "  TRACK %02d AUDIO\n    TITLE \"Track %d\"\n    INDEX 01 %02d:%02d:%02d\n",
			i+first, i+first, f/75/60, (f/75)%60, f%75)
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
	// A timestamp the sheet cannot mean: refused at its line, since a
	// skipped line would be a silently wrong cut.
	if err := os.WriteFile(filepath.Join(env.root, "junk.cue"), []byte(
		"FILE \"rej.wav\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 99:99:99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Not a sheet at all: nothing in it is a command, so it indexes nothing.
	if err := os.WriteFile(filepath.Join(env.root, "nonsense.cue"), []byte(
		"this is not a cue sheet\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	bad := http.StatusBadRequest
	for _, tc := range []struct {
		name   string
		body   map[string]any
		status int
		code   waxerr.Code
		want   []string
	}{
		{"cue and cuts together", map[string]any{
			"type": "split", "src": "lib/rej.wav", "format": "flac",
			"cue": "lib/rej.cue", "cuts": []int64{1000},
		}, bad, waxerr.CodeInvalidRequest, []string{"exclusive"}},
		{"cue on a transcode", map[string]any{
			"type": "transcode", "src": "lib/rej.wav", "format": "flac", "cue": "lib/rej.cue",
		}, bad, waxerr.CodeInvalidRequest, []string{"cue applies to split"}},
		// skip is resolved from the sheet exactly as cuts is, so a client
		// cannot send it beside one.
		{"skip with a cue", map[string]any{
			"type": "split", "src": "lib/rej.wav", "format": "flac",
			"cue": "lib/rej.cue", "skip": []int{0},
		}, bad, waxerr.CodeInvalidRequest, []string{"exclusive"}},
		{"multi-file sheet", map[string]any{
			"type": "split", "src": "lib/rej.wav", "format": "flac", "cue": "lib/multi.cue",
		}, bad, waxerr.CodeInvalidRequest, []string{"indexes 2 files"}},
		{"single-track sheet", map[string]any{
			"type": "split", "src": "lib/rej.wav", "format": "flac", "cue": "lib/one.cue",
		}, bad, waxerr.CodeInvalidRequest, []string{"nothing to cut"}},
		{"unreadable timestamp", map[string]any{
			"type": "split", "src": "lib/rej.wav", "format": "flac", "cue": "lib/junk.cue",
		}, bad, waxerr.CodeInvalidRequest, []string{"line 3", "a minute holds 60"}},
		{"not a sheet", map[string]any{
			"type": "split", "src": "lib/rej.wav", "format": "flac", "cue": "lib/nonsense.cue",
		}, bad, waxerr.CodeInvalidRequest, []string{"indexes no files"}},
		{"missing sheet", map[string]any{
			"type": "split", "src": "lib/rej.wav", "format": "flac", "cue": "lib/nope.cue",
		}, http.StatusNotFound, waxerr.CodeNotFound, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatal(err)
			}
			resp := env.postJSON(t, "/jobs", string(body))
			got := readBody(t, resp)
			var envelope server.ErrorBody
			if resp.StatusCode != tc.status || json.Unmarshal(got, &envelope) != nil || envelope.Code != tc.code {
				t.Fatalf("status %d, body %s; want %d and code %s", resp.StatusCode, got, tc.status, tc.code)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(got), want) {
					t.Errorf("body = %s, want it to mention %q", got, want)
				}
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

// TestSplitJobCueImpliedFile: a sheet with no FILE line indexes src, which
// is what a sidecar beside its one rip means.
func TestSplitJobCueImpliedFile(t *testing.T) {
	env := jobsEnv(t)
	cueFixture(t, env, "rej", []int{0, 5 * 75})
	if err := os.WriteFile(filepath.Join(env.root, "nofile.cue"), []byte(
		"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n  TRACK 02 AUDIO\n    INDEX 01 00:05:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"type": "split", "src": "lib/rej.wav", "format": "flac", "cue": "lib/nofile.cue",
	})
	if err != nil {
		t.Fatal(err)
	}
	job := awaitJob(t, env, createJob(t, env, string(body)))
	// 00:05:00 is 375 frames, 588 samples each at 44100.
	if want := []int64{220500}; fmt.Sprint(job.Request.Cuts) != fmt.Sprint(want) {
		t.Errorf("the sheet became cuts %v, want %v", job.Request.Cuts, want)
	}
	if len(job.Outputs) != 2 {
		t.Errorf("the split made %d pieces, want 2", len(job.Outputs))
	}
}

// TestSplitJobCueSkipsADataTrack: a mixed-mode disc's data track is not
// audio, and a split that cut it would hand back a piece of noise as track
// 1. The sheet's boundaries still partition the whole file, so the data
// track becomes a piece the job names as skipped, and the outputs are the
// audio pieces alone. Handing the daemon the cuts and the skip by hand is
// the identical job, which is the sheet surface's whole promise.
func TestSplitJobCueSkipsADataTrack(t *testing.T) {
	env := jobsEnv(t)
	// The data track runs to the first audio INDEX 01 at 00:05:37.
	starts := []int{5*75 + 37, 11*75 + 12}
	total := mixedModeFixture(t, env, "mixed", starts)

	body, err := json.Marshal(map[string]any{
		"type": "split", "src": "lib/mixed.wav", "format": "flac", "cue": "lib/mixed.cue",
	})
	if err != nil {
		t.Fatal(err)
	}
	job := awaitJob(t, env, createJob(t, env, string(body)))

	wantCuts := []int64{int64(starts[0]) * 588, int64(starts[1]) * 588}
	if fmt.Sprint(job.Request.Cuts) != fmt.Sprint(wantCuts) {
		t.Fatalf("the sheet became cuts %v, want %v: the data track's end is a cut like any other",
			job.Request.Cuts, wantCuts)
	}
	if fmt.Sprint(job.Request.Skip) != "[0]" {
		t.Fatalf("the job skips %v, want [0]: the data track is the first piece", job.Request.Skip)
	}
	if len(job.Outputs) != len(starts) {
		t.Fatalf("the split made %d pieces, want the %d audio tracks: %+v", len(job.Outputs), len(starts), job.Outputs)
	}
	for i, out := range job.Outputs {
		end := total
		if i+1 < len(starts) {
			end = int64(starts[i+1]) * 588
		}
		if want := end - int64(starts[i])*588; out.Samples != want {
			t.Errorf("piece %d is %d samples, want %d", i, out.Samples, want)
		}
		// Named by output index, the one /result and a warning both use.
		if want := fmt.Sprintf("out.%d.flac", i); out.File != want {
			t.Errorf("output %d is %q, want %q", i, out.File, want)
		}
	}

	byHand, err := json.Marshal(map[string]any{
		"type": "split", "src": "lib/mixed.wav", "format": "flac",
		"cuts": wantCuts, "skip": []int{0},
	})
	if err != nil {
		t.Fatal(err)
	}
	same := awaitJob(t, env, createJob(t, env, string(byHand)))
	if len(same.Outputs) != len(job.Outputs) {
		t.Fatalf("cuts and skip by hand made %d pieces, the sheet made %d", len(same.Outputs), len(job.Outputs))
	}
	for i := range job.Outputs {
		if same.Outputs[i].Samples != job.Outputs[i].Samples {
			t.Errorf("piece %d: by hand %d samples, by sheet %d", i, same.Outputs[i].Samples, job.Outputs[i].Samples)
		}
	}
}

// TestSplitJobCueDataFileBesideTheRip: EAC and XLD give a disc's data track
// a FILE of its own, with no INDEX, beside the one WAVE holding every audio
// track. That is still one audio file with tracks to cut, not a rip already
// split per track.
func TestSplitJobCueDataFileBesideTheRip(t *testing.T) {
	env := jobsEnv(t)
	starts := []int{0, 5*75 + 37}
	total := cueFixture(t, env, "beside", starts)
	if err := os.WriteFile(filepath.Join(env.root, "beside.cue"), []byte(
		"FILE \"beside.iso\" BINARY\n  TRACK 01 MODE1/2352\n"+
			"FILE \"beside.wav\" WAVE\n  TRACK 02 AUDIO\n    INDEX 01 00:00:00\n"+
			"  TRACK 03 AUDIO\n    INDEX 01 00:05:37\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"type": "split", "src": "lib/beside.wav", "format": "flac", "cue": "lib/beside.cue",
	})
	if err != nil {
		t.Fatal(err)
	}
	job := awaitJob(t, env, createJob(t, env, string(body)))
	if len(job.Request.Skip) != 0 {
		t.Errorf("the job skips %v; the data track occupies none of this file", job.Request.Skip)
	}
	if len(job.Outputs) != 2 || job.Outputs[0].Samples != int64(starts[1])*588 ||
		job.Outputs[1].Samples != total-int64(starts[1])*588 {
		t.Errorf("outputs = %+v, want the two audio tracks", job.Outputs)
	}
}

// TestSplitJobCueDataTrackBehindALyingHeader: a header that under-declares
// cannot turn a data track into noise. A concatenated MP3 declares half of
// what it holds, and the demuxer files the surplus frames as trailing
// padding rather than audio, so a data track addressed inside that surplus
// is past the audio: the split folds it away, and what it writes adds up
// to the declared count and not a sample more. That, and not a re-measure,
// is what keeps the daemon's answer the CLI's.
func TestSplitJobCueDataTrackBehindALyingHeader(t *testing.T) {
	env := jobsEnv(t)
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "sine-cbr128.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	doubled := append(append([]byte(nil), raw...), raw...)
	if err := os.WriteFile(filepath.Join(env.root, "lie.mp3"), doubled, 0o644); err != nil {
		t.Fatal(err)
	}
	declared := probeSamples(t, env, "lib/lie.mp3")
	e := waxflow.New()
	med, err := e.OpenStream(container.BytesSource(doubled), "mp3")
	if err != nil {
		t.Fatal(err)
	}
	if w, ok := med.(format.Walker); ok {
		if err := w.Walk(); err != nil {
			t.Fatal(err)
		}
	}
	walked := med.Info().Default()
	med.Close()
	rate := walked.Fmt.Rate
	if walked.Padding <= walked.Delay || rate%75 != 0 {
		t.Fatalf("fixture holds %d samples with %d of padding at %d Hz; this cell needs a surplus behind the declared count at a CD rate",
			walked.Samples, walked.Padding, rate)
	}
	perFrame := int64(rate / 75)
	second := declared / 2 / perFrame
	// Addressed inside the surplus: past the declared count, short of the
	// frames the file really holds.
	inside := (declared+walked.Padding/2)/perFrame + 1
	msf := func(f int64) string { return fmt.Sprintf("%02d:%02d:%02d", f/75/60, (f/75)%60, f%75) }
	if err := os.WriteFile(filepath.Join(env.root, "lie.cue"), []byte(
		"FILE \"lie.mp3\" MP3\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n"+
			"  TRACK 02 AUDIO\n    INDEX 01 "+msf(second)+"\n"+
			"  TRACK 03 MODE1/2352\n    INDEX 01 "+msf(inside)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"type": "split", "src": "lib/lie.mp3", "format": "flac", "cue": "lib/lie.cue",
	})
	if err != nil {
		t.Fatal(err)
	}
	job := awaitJob(t, env, createJob(t, env, string(body)))
	if len(job.Request.Skip) != 0 || len(job.Outputs) != 2 {
		t.Fatalf("skip %v, %d outputs; want the data track folded away and 2 audio pieces: %+v",
			job.Request.Skip, len(job.Outputs), job.Outputs)
	}
	var sum int64
	for _, out := range job.Outputs {
		sum += out.Samples
	}
	if sum != declared {
		t.Errorf("the pieces hold %d samples, want the declared %d: nothing behind the declaration is audio", sum, declared)
	}
}
