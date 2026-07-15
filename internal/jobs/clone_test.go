package jobs

import (
	"testing"
	"time"
)

// TestJobCloneIsDeep guards the store's central safety property: every job
// handed out is a copy, so a caller reading one cannot see (or scribble on)
// the state a worker is mutating under the lock.
//
// It is hand-written rather than reflective on purpose. What it has to catch
// is a reference field added to Job or Request and forgotten in clone, and
// clone is a struct copy plus one line per such field, so a reflective test
// would be a second implementation of the very thing under test. It has to
// catch a real one, though: Request.Srcs is a slice, so the struct copy that
// used to be a deep copy of Request no longer is.
//
// Two traps are worth naming, because the first two versions of this test fell
// into them and passed while proving nothing:
//
// Mutate *through* the clone's pointers, never reassign the clone's own
// fields. `c.Started = nil` cannot reach the original whether the pointer is
// shared or not, so it passes over a clone that shares every pointer it has.
//
// And hold the expected values in variables nothing else aliases. Pointing
// Started at a local and then comparing against that same local means a shared
// pointer mutates both sides of the comparison, which is equality by
// aliasing rather than by copying.
func TestJobCloneIsDeep(t *testing.T) {
	started := time.Now().UTC()
	finished := started.Add(time.Minute)
	// Copies, taken before anything is mutated: comparing against started and
	// finished themselves is the aliasing trap above, since orig points at them.
	wantStarted, wantFinished := started, finished

	orig := &Job{
		SchemaVersion: 1,
		ID:            "id",
		Type:          TypeTimeline,
		State:         StateDone,
		Request: Request{
			Type: TypeTimeline,
			Srcs: []string{"a.flac", "b.flac"},
		},
		Created:  started,
		Started:  &started,
		Finished: &finished,
		Error:    &ErrInfo{Code: "internal", Message: "m"},
		Output:   &Output{File: "out.flac"},
		Analysis: &Analysis{Silence: &SilenceSummary{Spans: 3}},
		Timeline: &Timeline{Tl: "digest", Members: 2},
		Progress: &Progress{Phase: "measure"},
		Warnings: []string{"w"},
	}
	c := orig.clone()

	epoch := time.Unix(0, 0).UTC()
	c.Request.Srcs[0] = "scribbled"
	c.Warnings[0] = "scribbled"
	*c.Started = epoch
	*c.Finished = epoch
	c.Error.Message = "scribbled"
	c.Output.File = "scribbled"
	c.Analysis.Silence.Spans = 99
	c.Timeline.Tl = "scribbled"
	c.Progress.Phase = "scribbled"

	// Independent checks, not a switch: a switch reports only its first
	// matching case, so one shared field would mask every other one and the
	// next person would fix them one run at a time.
	if orig.Request.Srcs[0] != "a.flac" {
		t.Error("clone shares Request.Srcs' backing array with the stored job")
	}
	if orig.Warnings[0] != "w" {
		t.Error("clone shares Warnings' backing array with the stored job")
	}
	if !orig.Started.Equal(wantStarted) {
		t.Error("clone shares the Started pointer")
	}
	if !orig.Finished.Equal(wantFinished) {
		t.Error("clone shares the Finished pointer")
	}
	if orig.Error.Message != "m" {
		t.Error("clone shares the Error pointer")
	}
	if orig.Output.File != "out.flac" {
		t.Error("clone shares the Output pointer")
	}
	if orig.Analysis.Silence.Spans != 3 {
		t.Error("clone shares the Analysis.Silence pointer")
	}
	if orig.Timeline.Tl != "digest" {
		t.Error("clone shares the Timeline pointer")
	}
	if orig.Progress.Phase != "measure" {
		t.Error("clone shares the Progress pointer")
	}
}
