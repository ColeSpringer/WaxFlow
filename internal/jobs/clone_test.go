package jobs

import (
	"reflect"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
)

// ptrTo returns a pointer to v, a fresh one per call. The fixture's pointers
// must alias nothing else, per the aliasing trap named below.
func ptrTo[T any](v T) *T { return &v }

// assertRefFieldsSet fails for each reference field of v left nil, naming it.
//
// This and assertNoSharedPointers are the only reflection here, and neither is
// the second implementation the doc above refuses. This one makes no claim
// about clone at all: it says the fixture is complete, which is the premise
// every hand-written check below rests on and the one thing a person keeps
// forgetting (all four of Analysis' float64 pointers sat nil here while clone
// shared them).
func assertRefFieldsSet(t *testing.T, name string, v any) {
	t.Helper()
	rv := reflect.ValueOf(v)
	for i := range rv.NumField() {
		switch f := rv.Field(i); f.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map:
			if f.IsNil() {
				t.Errorf("%s.%s is nil in this fixture: a reference field the fixture leaves unset "+
					"is one this test cannot prove clone copies", name, rv.Type().Field(i).Name)
			}
		}
	}
}

// assertNoSharedPointers fails for each pointer field the clone holds in
// common with the original.
//
// It states clone's invariant rather than re-implementing its body, which is
// what keeps it out of the trap: clone copies pointees, this asks only whether
// two addresses differ. It earns its place by catching what the hand-written
// checks structurally cannot, a pointer field added to Analysis and forgotten
// in clone, since it is told nothing about which fields exist. The fixture
// guard above is what keeps it honest, since a nil field shares no address.
func assertNoSharedPointers(t *testing.T, name string, orig, clone any) {
	t.Helper()
	ov, cv := reflect.ValueOf(orig), reflect.ValueOf(clone)
	for i := range ov.NumField() {
		if ov.Field(i).Kind() != reflect.Pointer {
			continue
		}
		if p := ov.Field(i).Pointer(); p != 0 && p == cv.Field(i).Pointer() {
			t.Errorf("clone shares the %s.%s pointer with the stored job",
				name, ov.Type().Field(i).Name)
		}
	}
}

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
		SchemaVersion: SchemaVersion,
		ID:            "id",
		Type:          TypeTimeline,
		State:         StateDone,
		Request: Request{
			Type:         TypeTimeline,
			Srcs:         []string{"a.flac", "b.flac"},
			SourceIDs:    []string{"1-1", "2-2"},
			Cuts:         []int64{100, 200},
			MemberTitles: []string{"One", "Two"},
			Spans:        []MemberSpan{{From: 10, To: 20}, {}},
		},
		Created:  started,
		Started:  &started,
		Finished: &finished,
		Error:    &ErrInfo{Code: "internal", Message: "m"},
		Outputs:  []Output{{File: "out.0.flac"}, {File: "out.1.flac"}},
		Analysis: &Analysis{
			// Every one of these, not just the summary: they are pointers to
			// carry the negative infinity of digital silence, and a fixture
			// that leaves them nil cannot catch clone sharing them, which is
			// exactly what it did not catch.
			IntegratedLUFS: ptrTo(-14.5),
			TruePeakDB:     ptrTo(-1.5),
			SamplePeakDB:   ptrTo(-2.25),
			AppliedGainDB:  ptrTo(3.75),
			Silence:        &SilenceSummary{Spans: 3},
		},
		Timeline: &Timeline{
			Tl: "digest", Members: 2,
			// Boundaries is a slice, so the struct copy clone makes of Timeline
			// is not a deep copy of it: a fixture that leaves it nil cannot
			// catch clone sharing its backing array.
			Boundaries: []waxflow.MemberBoundary{{OffsetSamples: 5, DurationSamples: 100}},
		},
		Progress: &Progress{Phase: "measure"},
		Warnings: []string{"w"},
	}
	// The fixture must stay complete for any of the above to mean anything, and
	// completeness is the one part of this a person cannot be trusted to keep:
	// a pointer added to Analysis and left nil here would restore the blind
	// spot silently.
	assertRefFieldsSet(t, "Job", *orig)
	assertRefFieldsSet(t, "Job.Request", orig.Request)
	assertRefFieldsSet(t, "Job.Analysis", *orig.Analysis)
	assertRefFieldsSet(t, "Job.Timeline", *orig.Timeline)

	c := orig.clone()
	assertNoSharedPointers(t, "Job.Analysis", *orig.Analysis, *c.Analysis)

	epoch := time.Unix(0, 0).UTC()
	c.Request.Srcs[0] = "scribbled"
	c.Request.SourceIDs[0] = "scribbled"
	c.Request.Cuts[0] = -1
	c.Request.MemberTitles[0] = "scribbled"
	c.Request.Spans[0].From = -1
	c.Warnings[0] = "scribbled"
	*c.Started = epoch
	*c.Finished = epoch
	c.Error.Message = "scribbled"
	c.Outputs[0].File = "scribbled"
	*c.Analysis.IntegratedLUFS = 99
	*c.Analysis.TruePeakDB = 99
	*c.Analysis.SamplePeakDB = 99
	*c.Analysis.AppliedGainDB = 99
	c.Analysis.Silence.Spans = 99
	c.Timeline.Tl = "scribbled"
	c.Timeline.Boundaries[0].OffsetSamples = 99
	c.Progress.Phase = "scribbled"

	// Independent checks, not a switch: a switch reports only its first
	// matching case, so one shared field would mask every other one and the
	// next person would fix them one run at a time.
	if orig.Request.Srcs[0] != "a.flac" {
		t.Error("clone shares Request.Srcs' backing array with the stored job")
	}
	if orig.Request.SourceIDs[0] != "1-1" {
		t.Error("clone shares Request.SourceIDs' backing array with the stored job")
	}
	if orig.Request.Cuts[0] != 100 {
		t.Error("clone shares Request.Cuts' backing array with the stored job")
	}
	if orig.Request.MemberTitles[0] != "One" {
		t.Error("clone shares Request.MemberTitles' backing array with the stored job")
	}
	if orig.Request.Spans[0].From != 10 {
		t.Error("clone shares Request.Spans' backing array with the stored job")
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
	if orig.Outputs[0].File != "out.0.flac" {
		t.Error("clone shares Outputs' backing array with the stored job")
	}
	if *orig.Analysis.IntegratedLUFS != -14.5 {
		t.Error("clone shares the Analysis.IntegratedLUFS pointer")
	}
	if *orig.Analysis.TruePeakDB != -1.5 {
		t.Error("clone shares the Analysis.TruePeakDB pointer")
	}
	if *orig.Analysis.SamplePeakDB != -2.25 {
		t.Error("clone shares the Analysis.SamplePeakDB pointer")
	}
	if *orig.Analysis.AppliedGainDB != 3.75 {
		t.Error("clone shares the Analysis.AppliedGainDB pointer")
	}
	if orig.Analysis.Silence.Spans != 3 {
		t.Error("clone shares the Analysis.Silence pointer")
	}
	if orig.Timeline.Tl != "digest" {
		t.Error("clone shares the Timeline pointer")
	}
	if orig.Timeline.Boundaries[0].OffsetSamples != 5 {
		t.Error("clone shares Timeline.Boundaries' backing array with the stored job")
	}
	if orig.Progress.Phase != "measure" {
		t.Error("clone shares the Progress pointer")
	}
}
