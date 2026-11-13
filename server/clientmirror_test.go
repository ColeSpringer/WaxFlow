// The client package's wire mirrors, pinned reflectively against the
// server's own types. The client decodes leniently (plain json.Decode), so
// a field added to a server document without a mirror, or a mirror with a
// mistyped tag, would fail nothing on its own: the value would silently
// vanish client-side. These round trips make that a test failure,
// including for fields that do not exist yet, because the filler reaches
// every field by reflection rather than by name.
package server_test

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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
		if v.Type() == reflect.TypeFor[time.Time]() {
			*n++
			v.Set(reflect.ValueOf(time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC).
				Add(time.Duration(*n) * time.Second)))
			return
		}
		for _, field := range v.Fields() {
			fillNonZero(t, field, n)
		}
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillNonZero(t, s.Index(0), n)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		fillNonZero(t, key, n)
		val := reflect.New(v.Type().Elem()).Elem()
		fillNonZero(t, val, n)
		m.SetMapIndex(key, val)
		v.Set(m)
	case reflect.String:
		*n++
		v.SetString(fmt.Sprintf("v%d", *n))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		*n++
		v.SetInt(int64(*n))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		*n++
		v.SetUint(uint64(*n))
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

// wireMirrors pairs each document the daemon writes or reads with the
// client type that mirrors it. A row covers every struct its server type
// reaches, so the listing envelope answers for Job and all a job holds.
var wireMirrors = []struct {
	name   string
	server any // the server's type, as its zero value
	client any // the client's mirror, as its zero value
	// zero is the empty document, where the server's zero value is not one
	// the two sides agree on. nil means the zero value.
	zero any
}{
	{name: "version", server: server.VersionInfo{}, client: client.VersionInfo{}},
	// The probe body is where fields most recently moved: warnings is damage
	// only, notes is what this build did with a well-formed file.
	{name: "probe", server: server.ProbeInfo{}, client: client.ProbeInfo{}},
	{name: "caps", server: server.Caps{}, client: client.Caps{}},
	{name: "upload", server: server.UploadResponse{}, client: client.UploadResponse{}},
	{name: "timeline request", server: server.TimelineRequest{}, client: client.TimelineRequest{}},
	{name: "timeline response", server: server.TimelineResponse{}, client: client.TimelineResponse{}},
	{name: "sign request", server: server.SignRequest{}, client: client.SignRequest{}},
	{name: "sign response", server: server.SignResponse{}, client: client.SignResponse{}},
	{name: "cache stats", server: server.CacheStatsResponse{}, client: client.CacheStatsResponse{}},
	{name: "cache gc", server: server.CacheGCResponse{}, client: client.CacheGCResponse{}},
	{name: "roots reload", server: server.RootsReloadResponse{}, client: client.RootsReloadResponse{}},
	{name: "roots", server: server.RootsResponse{}, client: client.RootsResponse{}},
	{name: "root", server: server.RootStatus{}, client: client.RootStatus{}},
	{name: "jobs listing", server: server.JobsList{}, client: client.JobsList{}},
	// Request.SourceID is the one deliberate divergence: the server always
	// writes it, and the client omits it when empty so a zero JobRequest
	// stays a valid create body (the field 400s on POST /jobs).
	{name: "job", server: jobs.Job{}, client: client.Job{},
		zero: jobs.Job{Request: jobs.Request{SourceID: "x"}}},
	// The zero analysis is where the null semantics live: the measurement
	// pointers marshal as null for digital silence, and a value-typed mirror
	// field would read them as 0.
	{name: "analysis", server: jobs.Analysis{}, client: client.JobAnalysis{}},
	{name: "silence map", server: jobs.SilenceMap{}, client: client.SilenceMap{}},
}

// TestClientMirrorsCoverTheWire pins every client mirror against the
// server's type, both fully populated (every field must survive; omitempty
// cannot hide one) and zero (presence semantics: an absent section stays
// absent, and a null measurement stays null instead of decaying to 0).
func TestClientMirrorsCoverTheWire(t *testing.T) {
	for _, row := range wireMirrors {
		mirror := func() any { return reflect.New(reflect.TypeOf(row.client)).Interface() }
		full := reflect.New(reflect.TypeOf(row.server))
		n := 0
		fillNonZero(t, full.Elem(), &n)
		roundTripEqual(t, "populated "+row.name, full.Interface(), mirror())

		zero := row.zero
		if zero == nil {
			zero = row.server
		}
		roundTripEqual(t, "zero "+row.name, zero, mirror())
	}
}

// reachStructs adds every struct type t reaches to seen, by its qualified
// name (server.Caps).
func reachStructs(t reflect.Type, seen map[string]bool) {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		reachStructs(t.Elem(), seen)
	case reflect.Map:
		reachStructs(t.Key(), seen)
		reachStructs(t.Elem(), seen)
	case reflect.Struct:
		if seen[t.String()] || t == reflect.TypeFor[time.Time]() {
			return
		}
		seen[t.String()] = true
		for field := range t.Fields() {
			reachStructs(field.Type, seen)
		}
	}
}

// jsonStructs lists the structs declared in dir's product files that carry
// a json tag, exported or not, as <package>.<name>.
func jsonStructs(t *testing.T, dir string) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			if slices.ContainsFunc(st.Fields.List, func(f *ast.Field) bool {
				return f.Tag != nil && strings.Contains(f.Tag.Value, `json:"`)
			}) {
				out = append(out, file.Name.Name+"."+ts.Name.Name)
			}
			return true
		})
	}
	return out
}

// TestEveryWireTypeHasAMirror guards the table above: a document type added
// to the server or to the jobs package with no row there would be checked
// by nothing. Every struct that carries a json tag is a row's type, is
// reached from one, or is listed here with why it needs no mirror.
func TestEveryWireTypeHasAMirror(t *testing.T) {
	exempt := map[string]string{
		"server.ErrorBody":    "the error envelope, which the client's decodeEnvelope reads into a waxerr code",
		"server.jobRequest":   "the POST /jobs body: client.JobRequest serves both directions, and TestClientJobRequestCarriesEveryCreateField pins this one",
		"server.probeRequest": "the POST /probe body: the client probes by GET",
	}
	covered := map[string]bool{}
	for _, row := range wireMirrors {
		reachStructs(reflect.TypeOf(row.server), covered)
	}
	declared := append(jsonStructs(t, "."), jsonStructs(t, "../internal/jobs")...)
	if len(declared) < 30 {
		t.Fatalf("found %d structs with a json tag; the scan is not seeing the packages", len(declared))
	}
	for _, name := range declared {
		switch {
		case covered[name] && exempt[name] != "":
			t.Errorf("%s has a mirror row and an exemption; drop the exemption", name)
		case !covered[name] && exempt[name] == "":
			t.Errorf("%s is written to the wire and no row of wireMirrors reaches it: "+
				"give the client a mirror and a row, or exempt it here with the reason", name)
		}
	}
	for name := range exempt {
		if !slices.Contains(declared, name) {
			t.Errorf("the exemption for %s names a type that is not declared any more", name)
		}
	}
}

// TestClientJobRequestCarriesEveryCreateField covers the one body the table
// cannot hold, since its type is unexported: every field the daemon reads
// from a POST /jobs body is one client.JobRequest can send. The job
// document's echo pins the rest, and a create-only field (cue) is in no
// echo.
func TestClientJobRequestCarriesEveryCreateField(t *testing.T) {
	sends := map[string]bool{}
	for field := range reflect.TypeFor[client.JobRequest]().Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		sends[name] = true
	}
	for _, tag := range server.JobRequestJSONTags() {
		if !sends[tag] {
			t.Errorf("POST /jobs reads %q and client.JobRequest has no field that sends it", tag)
		}
	}
}
