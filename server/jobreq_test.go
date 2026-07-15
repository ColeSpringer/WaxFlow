package server

import (
	"reflect"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/internal/jobs"
)

// jsonTag returns a struct field's json name, ignoring options.
func jsonTag(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return ""
	}
	if i := strings.IndexByte(tag, ','); i >= 0 {
		tag = tag[:i]
	}
	return tag
}

func fieldsByJSONTag(t reflect.Type) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	for i := range t.NumField() {
		f := t.Field(i)
		if tag := jsonTag(f); tag != "" {
			out[tag] = f
		}
	}
	return out
}

// TestJobRequestCoverage pins jobRequest (the wire body) against jobs.Request
// (the domain type) so the two cannot drift, matching on json tag because the
// Go names deliberately differ (Channels vs Ch).
//
// It is a coverage test with an exemption list rather than a bijection, and
// that distinction is the point. The two types are not field-for-field
// duplicates: jobs.Request carries SourceID and jobRequest must not, because
// its absence from the wire type is exactly what stops a client forging the
// identity pin that ADR-0003 rests on. A bijection test would fail on day one,
// and the obvious way to make it pass is to add SourceID to jobRequest, which
// silently converts that guarantee into a client-settable field. So the
// exemption is explicit and carries its reason.
func TestJobRequestCoverage(t *testing.T) {
	// Domain fields with no wire counterpart, and why.
	exempt := map[string]string{
		"sourceId": "server-computed from the resolved source; a client-settable " +
			"identity pin would defeat the source-changed guarantee",
	}

	wire := fieldsByJSONTag(reflect.TypeFor[jobRequest]())
	domain := fieldsByJSONTag(reflect.TypeFor[jobs.Request]())

	for tag, df := range domain {
		wf, ok := wire[tag]
		if !ok {
			if _, allowed := exempt[tag]; !allowed {
				t.Errorf("jobs.Request.%s (json %q) has no jobRequest field and is not exempt; "+
					"add it to the wire type or exempt it with a reason", df.Name, tag)
			}
			continue
		}
		if _, allowed := exempt[tag]; allowed {
			t.Errorf("json %q is exempt but present on jobRequest; the exemption is now a lie", tag)
		}
		// The wire type is plain (string) where the domain type may be named
		// (jobs.Type), so require convertibility and an identical kind rather
		// than identical types: that still catches an int/string drift.
		if wf.Type.Kind() != df.Type.Kind() || !wf.Type.ConvertibleTo(df.Type) {
			t.Errorf("jobRequest.%s is %s but jobs.Request.%s is %s (json %q)",
				wf.Name, wf.Type, df.Name, df.Type, tag)
		}
	}
	for tag, wf := range wire {
		if _, ok := domain[tag]; !ok {
			t.Errorf("jobRequest.%s (json %q) has no jobs.Request field: it would be silently dropped",
				wf.Name, tag)
		}
	}

	// requestFrom must copy every mapped field; a zero in the projection of a
	// fully populated body is a forgotten assignment.
	got := reflect.ValueOf(*requestFrom(populatedJobRequest()))
	for i := range got.NumField() {
		f := got.Type().Field(i)
		if _, allowed := exempt[jsonTag(f)]; allowed {
			continue // filled by the caller from the resolved source, not the body
		}
		if got.Field(i).IsZero() {
			t.Errorf("requestFrom leaves %s zero for a fully populated body", f.Name)
		}
	}
}

// populatedJobRequest is a wire body with every field set. It is a function,
// not a literal repeated per test, because the exhaustiveness guard below must
// check the very value TestJobRequestCoverage uses: a second copy would let the
// guard pass while the literal it claims to guard went stale.
func populatedJobRequest() jobRequest {
	return jobRequest{
		Type: "transcode", Src: "x", Format: "x", Container: "x",
		Rate: 1, Ch: 1, Bits: 1, Bitrate: 1, Gain: "x", Loudness: "x", FLACLevel: 1,
		Silence: true, SilenceThresholdDB: -60, SilenceMinSeconds: 0.25,
	}
}

// TestJobRequestPopulatedIsExhaustive guards the guard: populatedJobRequest
// only proves anything if it sets every wire field, so a field added to
// jobRequest without being added there would be silently untested.
func TestJobRequestPopulatedIsExhaustive(t *testing.T) {
	v := reflect.ValueOf(populatedJobRequest())
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Errorf("the populated jobRequest literal leaves %s zero, so TestJobRequestCoverage "+
				"does not actually check its mapping", v.Type().Field(i).Name)
		}
	}
}
