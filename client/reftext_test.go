package client

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow/internal/reftext"
)

// A reference a caller hands this package is the reference's own bytes, and
// a file name need not be UTF-8. json.Marshal would write U+FFFD for each
// stray byte, so the request would name another file.

func TestJobRequestCarriesAnyReference(t *testing.T) {
	in := JobRequest{Type: "split", Src: "lib/caf\xe9.flac", Cue: "lib/100% \xff.cue",
		Srcs: []string{"lib/a.flac", "lib/\xed\xa0\x80.wav"}, Format: "flac"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"src":"pct:lib/caf%E9.flac"`,
		`"cue":"pct:lib/100%25 %FF.cue"`,
		`"srcs":["lib/a.flac","pct:lib/%ED%A0%80.wav"]`,
		`"format":"flac"`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the body lacks %s:\n%s", want, b)
		}
	}
	// The daemon echoes the request on the job document.
	var out JobRequest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Src != in.Src || out.Cue != in.Cue || !slices.Equal(out.Srcs, in.Srcs) || out.Format != in.Format {
		t.Fatalf("the echo decoded to %#v, want %#v", out, in)
	}
	// A pointer marshals the same way, and the caller's value is not rewritten.
	if p, err := json.Marshal(&in); err != nil || string(p) != string(b) {
		t.Errorf("a pointer marshals %s (%v), a value %s", p, err, b)
	}
	if in.Src != "lib/caf\xe9.flac" || in.Srcs[1] != "lib/\xed\xa0\x80.wav" {
		t.Errorf("marshaling rewrote the caller's request: %#v", in)
	}
}

func TestTimelineSrcCarriesAnyReference(t *testing.T) {
	b, err := json.Marshal(TimelineRequest{Srcs: []TimelineSrc{{Src: "lib/caf\xe9.flac", From: 5}, {Src: "lib/a.flac"}}})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"srcs":[{"src":"pct:lib/caf%E9.flac","from":5},{"src":"lib/a.flac"}]}`
	if string(b) != want {
		t.Fatalf("the body is %s, want %s", b, want)
	}
}

func TestSignSendsAnyReference(t *testing.T) {
	var got string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		io.WriteString(w, `{"schemaVersion":1,"url":"/stream?x","exp":1}`)
	}))
	defer ts.Close()
	c, err := New(ts.URL, "k")
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]string{"src": "lib/caf\xe9.flac", "format": "wav"}
	if _, err := c.Sign(context.Background(), SignRequest{Params: params}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"src":"pct:lib/caf%E9.flac"`) || !strings.Contains(got, `"format":"wav"`) {
		t.Errorf("the body is %s", got)
	}
	if params["src"] != "lib/caf\xe9.flac" {
		t.Errorf("Sign rewrote the caller's params: %+q", params)
	}
}

// TestRequestsSurviveTheirOwnJSON: a caller that stores a request and
// reads it back must get the reference it stored, and must not send it
// escaped twice.
func TestRequestsSurviveTheirOwnJSON(t *testing.T) {
	tl := TimelineRequest{Srcs: []TimelineSrc{{Src: "lib/caf\xe9.flac", To: 9}}}
	sign := SignRequest{Path: "/stream", Params: map[string]string{"src": "lib/caf\xe9.flac", "format": "wav"}}
	for name, v := range map[string]struct{ in, out any }{
		"timeline": {tl, &TimelineRequest{}},
		"sign":     {sign, &SignRequest{}},
	} {
		first, err := json.Marshal(v.in)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(first, v.out); err != nil {
			t.Fatal(err)
		}
		again, err := json.Marshal(v.out)
		if err != nil || string(again) != string(first) {
			t.Errorf("%s: read back and written again it is %s (%v), first %s", name, again, err, first)
		}
	}
	var back TimelineRequest
	b, _ := json.Marshal(tl)
	if err := json.Unmarshal(b, &back); err != nil || back.Srcs[0] != tl.Srcs[0] {
		t.Errorf("the timeline read back as %#v (%v), want %#v", back, err, tl)
	}
	var signed SignRequest
	b, _ = json.Marshal(sign)
	if err := json.Unmarshal(b, &signed); err != nil || signed.Params["src"] != sign.Params["src"] {
		t.Errorf("the sign request read back as %#v (%v), want %#v", signed, err, sign)
	}
	if err := json.Unmarshal([]byte(`{"srcs":[{"src":"pct:lib/100%.flac"}]}`), &back); err == nil {
		t.Error("a malformed escape decoded")
	}
}

// TestQueriesKeepAReferenceThatStartsWithTheMarker: the daemon decodes pct:
// in a query, so a reference that starts with it is escaped on the way out.
// Any other goes as its own bytes, which a query string holds.
func TestQueriesKeepAReferenceThatStartsWithTheMarker(t *testing.T) {
	var got []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Query().Get("src"))
		io.WriteString(w, `{}`)
	}))
	defer ts.Close()
	c, err := New(ts.URL, "k")
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"pct:abc", "lib/caf\xe9.flac"} {
		got = nil
		if _, err := c.Probe(context.Background(), src); err != nil {
			t.Fatal(err)
		}
		params := url.Values{"src": {src}, "format": {"wav"}}
		resp, err := c.Stream(context.Background(), params)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		minted, err := MintURL("secret", "/stream", params, time.Unix(2000000000, 0))
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(minted)
		if err != nil {
			t.Fatal(err)
		}
		for _, sent := range append(got, u.Query().Get("src")) {
			if back, err := reftext.Decode(sent); err != nil || back != src {
				t.Errorf("src %q went out as %q, which reads back as %q (%v)", src, sent, back, err)
			}
		}
		if params.Get("src") != src {
			t.Errorf("the caller's params were rewritten: %q", params.Get("src"))
		}
	}
}

// TestEveryReferenceFieldHasAMarshaler guards the wrappers in reftext.go.
// This package's fields are plain strings, so a reference field added to a
// type with no MarshalJSON of its own would go out through json.Marshal,
// which writes U+FFFD for a stray byte and reports nothing. Any field whose
// JSON name is one a reference travels under must sit on a type that
// marshals itself both ways, or be listed here with why it need not.
func TestEveryReferenceFieldHasAMarshaler(t *testing.T) {
	referenceNames := map[string]bool{"src": true, "srcs": true, "cue": true, "ref": true}
	exempt := map[string]string{
		"UploadResponse.Ref": "the daemon writes upload:<id>, which is ASCII",
	}
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	methods := map[string]map[string]bool{} // type -> its JSON methods
	fields := map[string][]string{}         // type -> its reference-named fields
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil || (d.Name.Name != "MarshalJSON" && d.Name.Name != "UnmarshalJSON") {
					continue
				}
				recv := d.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				name := recv.(*ast.Ident).Name
				if methods[name] == nil {
					methods[name] = map[string]bool{}
				}
				methods[name][d.Name.Name] = true
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					for _, f := range st.Fields.List {
						if f.Tag == nil || len(f.Names) == 0 {
							continue
						}
						tag, _ := strconv.Unquote(f.Tag.Value)
						name, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ",")
						// A string, or a list of them: a struct in that place
						// answers for its own fields.
						elem := f.Type
						if arr, ok := elem.(*ast.ArrayType); ok {
							elem = arr.Elt
						}
						if id, ok := elem.(*ast.Ident); ok && id.Name == "string" && referenceNames[name] {
							fields[ts.Name.Name] = append(fields[ts.Name.Name], f.Names[0].Name)
						}
					}
				}
			}
		}
	}
	if len(fields) < 2 {
		t.Fatalf("found reference fields on %d types; the scan is not seeing the package", len(fields))
	}
	for typ, names := range fields {
		for _, field := range names {
			if exempt[typ+"."+field] != "" {
				continue
			}
			if !methods[typ]["MarshalJSON"] || !methods[typ]["UnmarshalJSON"] {
				t.Errorf("%s.%s is named like a reference, and %s has no MarshalJSON and UnmarshalJSON to write it in text form", typ, field, typ)
			}
		}
	}
}
