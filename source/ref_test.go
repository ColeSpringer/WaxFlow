package source

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/waxerr"
)

type refDoc struct {
	Src  Ref   `json:"src,omitempty"`
	Srcs []Ref `json:"srcs,omitempty"`
}

func TestRefSurvivesJSON(t *testing.T) {
	in := refDoc{Src: "lib/caf\xe9.flac", Srcs: []Ref{"lib/a.flac", "lib/100%\xff.flac"}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"src":"pct:lib/caf%E9.flac","srcs":["lib/a.flac","pct:lib/100%25%FF.flac"]}`
	if string(b) != want {
		t.Fatalf("marshaled %s, want %s", b, want)
	}
	var out refDoc
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Src != in.Src || len(out.Srcs) != 2 || out.Srcs[0] != in.Srcs[0] || out.Srcs[1] != in.Srcs[1] {
		t.Fatalf("round trip gave %q, want %q", out, in)
	}
}

// TestRefMarshalsAUTF8ReferenceAsAString: stored documents, timeline digests
// and HLS descriptors hash this JSON, so a plain reference must not move.
func TestRefMarshalsAUTF8ReferenceAsAString(t *testing.T) {
	for _, ref := range []string{"", "lib/a.flac", "lib/café <&>  100%.flac", "pid:01JABC", "upload:x"} {
		typed, err := json.Marshal(struct {
			Src Ref `json:"src,omitempty"`
		}{Ref(ref)})
		if err != nil {
			t.Fatal(err)
		}
		plain, err := json.Marshal(struct {
			Src string `json:"src,omitempty"`
		}{ref})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(typed, plain) {
			t.Errorf("Ref(%q) marshals %s, a string %s", ref, typed, plain)
		}
	}
}

func TestRefUnmarshalRefusesAMalformedEscape(t *testing.T) {
	var out refDoc
	err := json.Unmarshal([]byte(`{"src":"pct:lib/100%.flac"}`), &out)
	if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Fatalf("unmarshal = %v, want invalid-request", err)
	}
}

func TestParseRef(t *testing.T) {
	got, err := ParseRef("pct:lib%2Fcaf%E9.flac")
	if err != nil || got != "lib/caf\xe9.flac" {
		t.Fatalf("ParseRef = %q, %v", got, err)
	}
	if got.Text() != "pct:lib/caf%E9.flac" {
		t.Fatalf("Text = %q", got.Text())
	}
	if _, err := ParseRef("pct:%"); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Fatalf("ParseRef of a malformed escape = %v, want invalid-request", err)
	}
}

// TestRefLogsItsTextForm: a JSON log handler writes U+FFFD for a stray byte,
// and the line would no longer say which file.
func TestRefLogsItsTextForm(t *testing.T) {
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "src", Ref("lib/caf\xe9.flac"))
	if !strings.Contains(buf.String(), `"src":"pct:lib/caf%E9.flac"`) {
		t.Fatalf("logged %s", buf.String())
	}
}

// TestRefUnmarshalRefusesWhatJSONReadsLossily: a decoder turns a stray byte,
// and half a surrogate pair written \udXXX, into U+FFFD. Python's
// json.dumps writes a surrogateescape'd name the second way.
func TestRefUnmarshalRefusesWhatJSONReadsLossily(t *testing.T) {
	for _, body := range []string{
		"{\"src\":\"lib/caf\xe9.flac\"}",
		`{"src":"lib/caf\udce9.flac"}`,
		`{"srcs":["lib/a.flac","lib/caf\ud83c.flac"]}`,
	} {
		var out refDoc
		if err := json.Unmarshal([]byte(body), &out); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
			t.Errorf("%q decoded to %#v, %v; want invalid-request", body, out, err)
		}
	}
}

// TestRefUnmarshalLeavesOtherFieldsAlone: only a reference names a file, so
// only a reference is refused.
func TestRefUnmarshalLeavesOtherFieldsAlone(t *testing.T) {
	var out struct {
		Src   Ref    `json:"src"`
		Title string `json:"title"`
	}
	err := json.Unmarshal([]byte("{\"src\":\"lib/a.flac\",\"title\":\"caf\xe9 \\udce9\"}"), &out)
	if err != nil || out.Src != "lib/a.flac" || out.Title != "caf\ufffd \ufffd" {
		t.Fatalf("decoded %#v, %v", out, err)
	}
	// A pair, an escaped backslash and null are not lone surrogates.
	var doc refDoc
	if err := json.Unmarshal([]byte(`{"src":"lib/\ud83c\udfb5 \\ud800.flac","srcs":null}`), &doc); err != nil || doc.Src != "lib/\U0001F3B5 \\ud800.flac" {
		t.Fatalf("decoded %#v, %v", doc, err)
	}
	if err := json.Unmarshal([]byte(`{"src":null}`), &doc); err != nil || doc.Src == "" {
		t.Fatalf("null overwrote the reference: %#v, %v", doc, err)
	}
	if err := json.Unmarshal([]byte(`{"src":5}`), &doc); err == nil {
		t.Fatal("a number decoded as a reference")
	}
}

// TestRefQueryValue: a query string holds a reference's bytes as they are,
// except one that starts with the marker, which every reader would decode.
func TestRefQueryValue(t *testing.T) {
	for _, ref := range []string{"lib/caf\xe9.flac", "lib/a.flac", "pct:abc", "pct:lib/caf\xe9"} {
		got, err := ParseRef(Ref(ref).QueryValue())
		if err != nil || string(got) != ref {
			t.Errorf("ParseRef(Ref(%q).QueryValue()) = %q, %v", ref, got, err)
		}
	}
	if got := Ref("lib/caf\xe9.flac").QueryValue(); got != "lib/caf\xe9.flac" {
		t.Errorf("QueryValue escaped a reference that does not start with the marker: %q", got)
	}
}

// TestRootStatusText: a root's status goes out as JSON, and both its path
// and the error that quotes the path have to survive that.
func TestRootStatusText(t *testing.T) {
	st := RootStatus{Name: "lib", Path: "/m/caf\xe9",
		Err: &fs.PathError{Op: "open", Path: "/m/caf\xe9", Err: fs.ErrNotExist}}
	if got := st.PathText(); got != "pct:/m/caf%E9" {
		t.Errorf("PathText = %q", got)
	}
	if got := st.ErrText(); got != "open pct:/m/caf%E9: file does not exist" {
		t.Errorf("ErrText = %q", got)
	}
	// An error that quotes some other name keeps its bytes legible.
	st.Err = errors.New("stat /other/caf\xe9: boom")
	if got := st.ErrText(); got != `stat /other/caf\xe9: boom` {
		t.Errorf("ErrText = %q", got)
	}
	// An error that quotes a longer path under the root writes that whole
	// path in one form, which reads back as the path's own bytes.
	st.Err = &fs.PathError{Op: "open", Path: "/m/caf\xe9/100%\xff", Err: fs.ErrNotExist}
	if got := st.ErrText(); got != "open pct:/m/caf%E9/100%25%FF: file does not exist" {
		t.Errorf("ErrText = %q", got)
	}
	st.Err = fmt.Errorf("checking the root: %w", &fs.PathError{Op: "stat", Path: "/m/caf\xe9/x\xff", Err: fs.ErrPermission})
	if got := st.ErrText(); got != "checking the root: stat pct:/m/caf%E9/x%FF: permission denied" {
		t.Errorf("ErrText = %q", got)
	}
	plain := RootStatus{Path: "/music", Err: &fs.PathError{Op: "open", Path: "/music", Err: fs.ErrNotExist}}
	if plain.PathText() != "/music" || plain.ErrText() != plain.Err.Error() {
		t.Errorf("a UTF-8 path changed: %q, %q", plain.PathText(), plain.ErrText())
	}
	if got := (RootStatus{Path: "/music"}).ErrText(); got != "" {
		t.Errorf("a mounted root's ErrText = %q", got)
	}
}
