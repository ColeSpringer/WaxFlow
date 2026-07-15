package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/internal/jobs"
	"github.com/colespringer/waxflow/source"
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
		"sourceIds": "the same pin, per merge member. Exempt for exactly the reason sourceId " +
			"is, and it is the reason that carries across rather than the field: a client that " +
			"could name its members' identities could name the ones it wished were true, so a " +
			"merge would concatenate whatever it was handed and call it unchanged",
	}

	// Wire fields with no domain counterpart, and why. This direction needs
	// its own list because it is the opposite failure: the map above is
	// "the domain has a field the wire cannot set", this one is "the wire
	// takes a field the domain never sees", which is normally a value
	// silently dropped and occasionally, as here, a value resolved into a
	// different one.
	wireOnly := map[string]string{
		"cue": "resolved into cuts at creation, so it is consumed rather than dropped. " +
			"The sheet deliberately does not reach the job: a job is its cut points, and " +
			"carrying the reference would let an edit to the sheet between creation and " +
			"execution change what the 201 accepted",
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
		if _, ok := domain[tag]; ok {
			if _, only := wireOnly[tag]; only {
				t.Errorf("json %q is exempt as wire-only but jobs.Request has it; the exemption is now a lie", tag)
			}
			continue
		}
		if _, allowed := wireOnly[tag]; !allowed {
			t.Errorf("jobRequest.%s (json %q) has no jobs.Request field: it would be silently dropped; "+
				"add it to the domain type or exempt it with a reason", wf.Name, tag)
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
	// Deliberately not a request any type would be accepted with: it carries
	// every field at once, which no job may. What it checks is the mapping,
	// and a body missing a field checks the mapping of that field not at all.
	return jobRequest{
		Type: "transcode", Src: "x", Srcs: []string{"x"}, Cuts: []int64{1}, Cue: "x",
		Format: "x", Container: "x",
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

// countingResolver hands out real files and reports how many of the ones it
// has already handed out are still open.
//
// It asks the only question a closed file answers differently: a read. There
// is no Close hook to count on (source.File wraps an *os.File and closes it
// directly), and counting the process's descriptors would be both unportable
// and a race to sample. A read is neither.
type countingResolver struct {
	next source.Resolver
	out  []*source.File
	// peak is the most previously-resolved members found still open at any one
	// resolve: the caller's own high-water mark, sampled at a point the caller
	// necessarily passes through rather than from a racing goroutine.
	peak int
}

func (c *countingResolver) Resolve(ctx context.Context, ref string) (*source.File, error) {
	if n := c.live(); n > c.peak {
		c.peak = n
	}
	f, err := c.next.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	c.out = append(c.out, f)
	return f, nil
}

func (c *countingResolver) live() int {
	n := 0
	var b [1]byte
	for _, f := range c.out {
		if _, err := f.ReadAt(b[:], 0); !errors.Is(err, os.ErrClosed) {
			n++
		}
	}
	return n
}

// TestValidateMergeHoldsOneMemberAtATime pins that accepting a merge costs one
// descriptor at a time rather than one per member.
//
// This is the path resolveMember's own comment argues against at length: a
// thousand-member queue is a thousand descriptors against a default limit of
// 1024, and eagerly holding them is what Concat exists not to do. Creation is
// where it would hurt most, because the measure pass is the slow part, so the
// handles would be held for the whole of it. runMerge already opens one member
// at a time and proves that is all a merge needs.
//
// The assertion is on the resolver rather than on the descriptor count because
// the invariant is about what this function holds, not about what the process
// happens to have open.
func TestValidateMergeHoldsOneMemberAtATime(t *testing.T) {
	root := t.TempDir()
	wav, err := os.ReadFile(filepath.Join("..", "testdata", "sine-s16.wav"))
	if err != nil {
		t.Fatal(err)
	}
	// Each member is a distinct size, which is what makes it a distinct
	// identity: an identity is size plus mtime, so same-sized copies written in
	// one go collide, and the memo behind trackFor would then answer three of
	// the four for free. The padding rides past the RIFF chunk size, so the
	// audio is unchanged and only the identity moves.
	var refs []string
	for i := range 4 {
		name := fmt.Sprintf("m%d.wav", i)
		if err := os.WriteFile(filepath.Join(root, name), append(slices.Clone(wav), make([]byte, i)...), 0o644); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, "lib/"+name)
	}
	roots, err := source.OpenRoots([]source.Root{{Name: "lib", Path: root}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()

	counting := &countingResolver{next: roots}
	s, err := New(Config{
		Addr:     "127.0.0.1:0",
		APIKeys:  []string{"k"},
		Resolver: counting,
		CacheDir: t.TempDir(),
		Version:  "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	body := jobRequest{Type: "merge", Srcs: refs, Format: "flac"}
	req, err := s.validateJobRequest(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if counting.peak != 0 {
		t.Errorf("validating a %d-member merge held %d earlier members open while resolving the next; "+
			"it must hold one at a time, or a thousand-member queue is a thousand descriptors",
			len(refs), counting.peak)
	}
	// The handles are scaffolding, and dropping them must not have dropped what
	// they were opened for: the identity pins are the merge's source-changed
	// guarantee, and they must still be every member's, in order.
	if len(req.SourceIDs) != len(refs) {
		t.Fatalf("the merge pinned %d identities, want %d", len(req.SourceIDs), len(refs))
	}
	seen := map[string]bool{}
	for i, id := range req.SourceIDs {
		if id == "" {
			t.Fatalf("member %d pinned no identity", i)
		}
		seen[id] = true
	}
	if len(seen) != len(refs) {
		t.Errorf("the %d members pinned %d distinct identities; the order or the mapping is wrong",
			len(refs), len(seen))
	}
	if !slices.Equal(req.Srcs, refs) {
		t.Errorf("the merge's members are %v, want %v in order", req.Srcs, refs)
	}
}
