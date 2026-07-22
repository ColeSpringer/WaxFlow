// The client package's job wire mirrors, pinned reflectively against the
// server's own types. The client decodes leniently (plain json.Decode), so
// a field added to the server's job document without a mirror, or a mirror
// with a mistyped tag, would fail nothing on its own: the value would
// silently vanish client-side. These round trips make that a test failure,
// including for fields that do not exist yet, because the filler reaches
// every field by reflection rather than by name.
package server_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/colespringer/waxflow/client"
	"github.com/colespringer/waxflow/internal/jobs"
	"github.com/colespringer/waxflow/server"
)

// fillNonZero sets every field reachable from v to a distinct non-zero
// value. Distinct, so two fields with swapped tags cannot cancel out;
// non-zero, so nothing hides behind omitempty; reflective, so a field
// added to the server types after this test was written is populated
// (and therefore checked) without anyone editing this file.
func fillNonZero(t *testing.T, v reflect.Value, n *int) {
	t.Helper()
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillNonZero(t, v.Elem(), n)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			*n++
			v.Set(reflect.ValueOf(time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC).
				Add(time.Duration(*n) * time.Second)))
			return
		}
		for i := range v.NumField() {
			fillNonZero(t, v.Field(i), n)
		}
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillNonZero(t, s.Index(0), n)
		v.Set(s)
	case reflect.String:
		*n++
		v.SetString(fmt.Sprintf("v%d", *n))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		*n++
		v.SetInt(int64(*n))
	case reflect.Float32, reflect.Float64:
		*n++
		v.SetFloat(float64(*n) + 0.5)
	case reflect.Bool:
		v.SetBool(true)
	default:
		t.Fatalf("fillNonZero: unhandled kind %s (%s); teach the filler, or the mirror check quietly stops being exhaustive", v.Kind(), v.Type())
	}
}

// roundTripEqual marshals the server value, decodes that into the client
// mirror, re-marshals the mirror, and compares the two documents as
// decoded JSON (field order differs between the struct definitions, so
// the bytes cannot be compared directly). A dropped, renamed, or
// re-shaped field surfaces as the diff.
func roundTripEqual(t *testing.T, name string, srv, mirror any) {
	t.Helper()
	wire, err := json.Marshal(srv)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wire, mirror); err != nil {
		t.Fatalf("%s: the client mirror does not decode the server document: %v", name, err)
	}
	back, err := json.Marshal(mirror)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(wire, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(back, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("%s: the client mirror dropped or reshaped part of the document\nserver: %s\nclient: %s", name, wire, back)
	}
}

// TestClientJobMirrorsCoverTheWire pins the client's job types against
// the server's, both fully populated (every field must survive; omitempty
// cannot hide one) and zero (presence semantics: an absent section must
// stay absent, and a null measurement must stay null through the mirror
// rather than decaying to 0). The populated listing envelope carries a
// filled Job, so one round trip covers Job, JobRequest, JobOutput,
// JobAnalysis, SilenceSummary, JobTimeline, JobProgress, JobError, and
// JobsList at once.
func TestClientJobMirrorsCoverTheWire(t *testing.T) {
	var j jobs.Job
	n := 0
	fillNonZero(t, reflect.ValueOf(&j).Elem(), &n)
	roundTripEqual(t, "populated listing",
		server.JobsList{SchemaVersion: 1, Jobs: []*jobs.Job{&j}}, &client.JobsList{})

	// The zero job checks omitempty parity: a mirror field missing its
	// omitempty would emit a zero the server omits. Request.SourceID is
	// pinned non-empty because it is the one deliberate divergence: the
	// server always emits it, while the client omits it when empty so a
	// zero JobRequest stays a valid create body (the field 400s on POST
	// /jobs; see the client's JobRequest doc).
	roundTripEqual(t, "zero job",
		&jobs.Job{Request: jobs.Request{SourceID: "x"}}, &client.Job{})

	// The zero analysis is where the null semantics live: the four
	// measurement pointers marshal as null for digital silence, and a
	// value-typed mirror field would silently read them as 0.
	roundTripEqual(t, "zero analysis", &jobs.Analysis{}, &client.JobAnalysis{})
}
