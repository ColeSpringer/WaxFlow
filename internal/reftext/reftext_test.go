package reftext

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/colespringer/waxflow/waxerr"
)

func TestEncodeLeavesUTF8Alone(t *testing.T) {
	for _, ref := range []string{
		"",
		"lib/a.flac",
		"lib/café.flac",
		"lib/100% Hits/a b+c.flac",
		"lib/a%20b.flac",
		"upload:01JABC",
		"pid:01JABC",
	} {
		if got := Encode(ref); got != ref {
			t.Errorf("Encode(%q) = %q, want it unchanged", ref, got)
		}
	}
}

func TestEncodeEscapesWhatJSONCannotHold(t *testing.T) {
	for _, tc := range []struct{ ref, want string }{
		{"lib/caf\xe9.flac", "pct:lib/caf%E9.flac"},
		// A literal % is escaped once the form is marked, or it would read
		// as an escape.
		{"lib/100%\xff.flac", "pct:lib/100%25%FF.flac"},
		// Valid UTF-8 beside the stray byte stays readable.
		{"lib/é\xe9 x.flac", "pct:lib/é%E9 x.flac"},
		// A lone surrogate, as Go spells a Windows name that holds one.
		{"lib/\xed\xa0\x80.wav", "pct:lib/%ED%A0%80.wav"},
		// A truncated multi-byte sequence.
		{"lib/\xe2\x82", "pct:lib/%E2%82"},
		// A reference that itself starts with the marker keeps its bytes.
		{"pct:lib/a.flac", "pct:pct%3Alib/a.flac"},
	} {
		if got := Encode(tc.ref); got != tc.want {
			t.Errorf("Encode(%q) = %q, want %q", tc.ref, got, tc.want)
		}
	}
}

func TestDecode(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"", ""},
		{"lib/a.flac", "lib/a.flac"},
		// Unmarked text is literal: a query string hands over raw bytes, and
		// a name may hold a % of its own.
		{"lib/caf\xe9.flac", "lib/caf\xe9.flac"},
		{"lib/100%25.flac", "lib/100%25.flac"},
		{"pid:01JABC", "pid:01JABC"},
		{"pct:lib/caf%E9.flac", "lib/caf\xe9.flac"},
		{"pct:lib/caf%e9.flac", "lib/caf\xe9.flac"},
		{"pct:lib%2Fa%20b.flac", "lib/a b.flac"},
		{"pct:lib/100%25.flac", "lib/100%.flac"},
		{"pct:lib/a+b.flac", "lib/a+b.flac"},
		{"pct:lib/a.flac", "lib/a.flac"},
		{"pct:pct%3Alib/a.flac", "pct:lib/a.flac"},
	} {
		got, err := Decode(tc.text)
		if err != nil || got != tc.want {
			t.Errorf("Decode(%q) = %q, %v; want %q", tc.text, got, err, tc.want)
		}
	}
}

func TestDecodeRefusesAMalformedEscape(t *testing.T) {
	for _, text := range []string{
		"pct:lib/100%",
		"pct:lib/100%2",
		"pct:lib/%zz.flac",
		"pct:lib/%4g.flac",
		"pct:lib/100% Hits/a.flac",
		// A marker with nothing behind it is a mistake, not an absent reference.
		"pct:",
	} {
		got, err := Decode(text)
		if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
			t.Errorf("Decode(%q) = %q, %v; want invalid-request", text, got, err)
		}
	}
}

// TestDecodeErrorStaysShort: the message reaches a client's envelope, so it
// must not carry a megabyte reference back.
func TestDecodeErrorStaysShort(t *testing.T) {
	_, err := Decode("pct:" + strings.Repeat("a", 1<<20) + "%")
	if err == nil || len(err.Error()) > 200 {
		t.Fatalf("Decode error is %d bytes, want a short one", len(err.Error()))
	}
}

func FuzzRoundTrip(f *testing.F) {
	for _, s := range []string{"", "lib/a.flac", "lib/caf\xe9.flac", "pct:lib/caf%E9.flac", "pct:%", "100%\xff", "\xed\xa0\x80"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		text := Encode(s)
		if !utf8.ValidString(text) {
			t.Fatalf("Encode(%q) = %q, which is not valid UTF-8", s, text)
		}
		back, err := Decode(text)
		if err != nil || back != s {
			t.Fatalf("Decode(Encode(%q)) = %q, %v", s, back, err)
		}
		// The point of the form: JSON carries it whole.
		b, err := json.Marshal(text)
		if err != nil {
			t.Fatal(err)
		}
		var viaJSON string
		if err := json.Unmarshal(b, &viaJSON); err != nil || viaJSON != text {
			t.Fatalf("JSON turned %q into %q (%v)", text, viaJSON, err)
		}
		// Any text Decode accepts names bytes Encode can carry back.
		if d, err := Decode(s); err == nil {
			if again, err := Decode(Encode(d)); err != nil || again != d {
				t.Fatalf("Decode(%q) = %q does not survive Encode: %q, %v", s, d, again, err)
			}
		}
	})
}

// TestEncodeQuery: a query string holds any bytes, so only a reference that
// starts with the marker needs escaping there, or a reader would decode it.
func TestEncodeQuery(t *testing.T) {
	for _, tc := range []struct{ ref, want string }{
		{"", ""},
		{"lib/a.flac", "lib/a.flac"},
		{"lib/caf\xe9.flac", "lib/caf\xe9.flac"},
		{"lib/100%25.flac", "lib/100%25.flac"},
		{"pct:lib/a.flac", "pct:pct%3Alib/a.flac"},
		{"pct:lib/caf\xe9.flac", "pct:pct%3Alib/caf%E9.flac"},
	} {
		got := EncodeQuery(tc.ref)
		if got != tc.want {
			t.Errorf("EncodeQuery(%q) = %q, want %q", tc.ref, got, tc.want)
		}
		if back, err := Decode(got); err != nil || back != tc.ref {
			t.Errorf("Decode(EncodeQuery(%q)) = %q, %v", tc.ref, back, err)
		}
	}
}

// TestCheckJSON: a JSON decoder reads a stray byte, and an unpaired
// surrogate escape, as U+FFFD without an error.
func TestCheckJSON(t *testing.T) {
	for _, ok := range []string{
		`"lib/a.flac"`,
		`"lib/caf\u00e9 \ud83c\udfb5 \\ud800 \" \n.flac"`,
		`"lib/\ufffd.flac"`,
		"\"lib/caf\u00e9.flac\"",
		`{"src":"lib/a.flac","n":[1,2]}`,
		`null`,
	} {
		if err := CheckJSON([]byte(ok)); err != nil {
			t.Errorf("CheckJSON(%s) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"\"lib/caf\xe9.flac\"",
		`"lib/caf\udce9.flac"`,        // a low surrogate alone, as Python's surrogateescape writes a byte
		`"lib/caf\ud83c.flac"`,        // a high surrogate alone
		`"lib/caf\ud83c\u00e9.flac"`,  // a high surrogate before something else
		`"lib/caf\ud83c\ud83c\udfb5"`, // a high surrogate before a whole pair
		`"lib/caf\uDCE9"`,
		`{"roots":[{"path":"/m/caf\udce9"}]}`,
	} {
		if err := CheckJSON([]byte(bad)); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
			t.Errorf("CheckJSON(%s) = %v, want invalid-request", bad, err)
		}
	}
}

// FuzzCheckJSON: in whatever it passes, a decoder puts U+FFFD only where
// the text spelled one.
func FuzzCheckJSON(f *testing.F) {
	for _, s := range []string{`"a"`, `"\udce9"`, `"\ud83c\udfb5"`, "\"\xe9\"", `"\\ud800"`, `"\ud800\\udc00"`, `"\ufffd"`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		var got string
		if json.Unmarshal([]byte(s), &got) != nil || CheckJSON([]byte(s)) != nil {
			return
		}
		spelled := strings.Contains(s, "\ufffd") || strings.Contains(strings.ToLower(s), `\ufffd`)
		if strings.Contains(got, "\ufffd") && !spelled {
			t.Fatalf("%s decoded to %q: a U+FFFD the text did not spell", s, got)
		}
	})
}

func TestEscapeStray(t *testing.T) {
	for _, tc := range []struct{ s, want string }{
		{"", ""},
		{"open /m/caf\u00e9: no such file", "open /m/caf\u00e9: no such file"},
		{"open /m/caf\xe9.wav: no such file", `open /m/caf\xe9.wav: no such file`},
		{"a\xed\xa0\x80b", `a\xed\xa0\x80b`},
	} {
		got := EscapeStray(tc.s)
		if got != tc.want || !utf8.ValidString(got) {
			t.Errorf("EscapeStray(%q) = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// legible is a text type that marshals itself, as source.Ref does.
type legible string

func (l legible) MarshalText() ([]byte, error) { return []byte("x"), nil }

// TestEscapeStrays: every plain string a document holds is made legible in
// one pass, so a field added later needs no escaping of its own. A type that
// marshals itself is left alone: its bytes are its value.
func TestEscapeStrays(t *testing.T) {
	type inner struct {
		Message string
		Since   time.Time
	}
	type doc struct {
		Name     string
		Notes    []string
		Err      *inner
		Items    []inner
		ByKey    map[string]string
		RefByKey map[string]legible
		Ref      legible
		Refs     []legible
		Any      any
		n        string
		Count    int
		Pointers []*inner
	}
	when := time.Unix(1700000000, 0)
	d := doc{
		Name:     "caf\xe9",
		Notes:    []string{"ok", "bad \xff"},
		Err:      &inner{Message: "open /m/caf\xe9: gone", Since: when},
		Items:    []inner{{Message: "a\xe9"}},
		ByKey:    map[string]string{"k": "v\xe9"},
		RefByKey: map[string]legible{"k": "lib/\xe9"},
		Ref:      "lib/caf\xe9.flac",
		Refs:     []legible{"lib/\xff"},
		Any:      "left\xe9",
		n:        "unexported\xe9",
		Pointers: []*inner{nil, {Message: "p\xe9"}},
	}
	EscapeStrays(&d)
	want := doc{
		Name:     `caf\xe9`,
		Notes:    []string{"ok", `bad \xff`},
		Err:      &inner{Message: `open /m/caf\xe9: gone`, Since: when},
		Items:    []inner{{Message: `a\xe9`}},
		ByKey:    map[string]string{"k": `v\xe9`},
		RefByKey: map[string]legible{"k": "lib/\xe9"},
		Ref:      "lib/caf\xe9.flac",
		Refs:     []legible{"lib/\xff"},
		Any:      "left\xe9",
		n:        "unexported\xe9",
		Pointers: []*inner{nil, {Message: `p\xe9`}},
	}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("EscapeStrays gave\n%#v\nwant\n%#v", d, want)
	}
	// A document with nothing to escape is not written to.
	shared := []string{"a", "b"}
	clean := doc{Notes: shared}
	EscapeStrays(&clean)
	if &clean.Notes[0] != &shared[0] {
		t.Error("a clean slice was replaced")
	}
	EscapeStrays(nil)
	EscapeStrays(d) // not a pointer: nothing to write to, and no panic
}
