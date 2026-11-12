package source

import (
	"encoding/json"
	"log/slog"

	"github.com/colespringer/waxflow/internal/reftext"
)

// Ref is a source reference where it is written as text, a JSON field above
// all. Its value is the reference's own bytes, the string a Resolver takes.
// Its text form is that string when it is valid UTF-8, and otherwise "pct:"
// and the reference with each stray byte, and each '%', written %XX: JSON
// would turn those bytes into U+FFFD and name another file.
type Ref string

// ParseRef reads a reference's text form. Text without the pct: marker is
// the reference; a marked one with a malformed escape, or with nothing
// after the marker, is refused with waxerr.CodeInvalidRequest.
func ParseRef(text string) (Ref, error) {
	ref, err := reftext.Decode(text)
	return Ref(ref), err
}

// Text returns the reference's text form, which is always valid UTF-8.
func (r Ref) Text() string { return reftext.Encode(string(r)) }

// QueryValue returns the reference as a query parameter carries it: its own
// bytes, which a query string holds, unless it starts with the pct: marker
// and a reader would decode it.
func (r Ref) QueryValue() string { return reftext.EncodeQuery(string(r)) }

// MarshalText implements encoding.TextMarshaler with the text form.
func (r Ref) MarshalText() ([]byte, error) { return []byte(r.Text()), nil }

// UnmarshalText implements encoding.TextUnmarshaler, as ParseRef.
func (r *Ref) UnmarshalText(text []byte) error {
	ref, err := ParseRef(string(text))
	if err != nil {
		return err
	}
	*r = ref
	return nil
}

// UnmarshalJSON reads a JSON string as ParseRef reads its text, after
// CheckJSON: a decoder would otherwise turn a stray byte or half a surrogate
// pair into U+FFFD, and the reference into another file's. Only the
// reference is held to that; the fields beside it decode as they always did.
func (r *Ref) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	if err := CheckJSON(b); err != nil {
		return err
	}
	var text string
	if err := json.Unmarshal(b, &text); err != nil {
		return err
	}
	return r.UnmarshalText([]byte(text))
}

// CheckJSON refuses JSON text, one string or a whole document, that a
// decoder would read lossily: a byte that is not UTF-8, or a \u escape of
// half a surrogate pair, both of which encoding/json reads as U+FFFD.
func CheckJSON(b []byte) error { return reftext.CheckJSON(b) }

// LogValue implements slog.LogValuer with the text form.
func (r Ref) LogValue() slog.Value { return slog.StringValue(r.Text()) }
