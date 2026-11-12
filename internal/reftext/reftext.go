// Package reftext is the text form of a source reference. A reference is a
// byte string, since a file name is, and JSON holds only UTF-8: an encoder
// writes U+FFFD for each stray byte, and the name that arrives is another
// file's. The text form is the reference itself when it is valid UTF-8, and
// otherwise "pct:" followed by the reference with those bytes written %XX.
package reftext

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/colespringer/waxflow/waxerr"
)

// Prefix marks text whose %XX sequences are bytes.
const Prefix = "pct:"

const upperHex = "0123456789ABCDEF"

// Encode returns ref in text form: unchanged when it is valid UTF-8, else
// marked, with every byte outside a valid sequence and every '%' escaped.
// A ref that starts with the marker is escaped too, so Decode returns it.
func Encode(ref string) string {
	marked := strings.HasPrefix(ref, Prefix)
	if !marked && utf8.ValidString(ref) {
		return ref
	}
	var b strings.Builder
	b.Grow(len(Prefix) + len(ref) + 8)
	b.WriteString(Prefix)
	for i := 0; i < len(ref); {
		r, size := utf8.DecodeRuneInString(ref[i:])
		stray := r == utf8.RuneError && size == 1
		if stray || ref[i] == '%' || (marked && i == len(Prefix)-1) {
			b.WriteByte('%')
			b.WriteByte(upperHex[ref[i]>>4])
			b.WriteByte(upperHex[ref[i]&0x0f])
			i++
			continue
		}
		b.WriteString(ref[i : i+size])
		i += size
	}
	return b.String()
}

// EncodeQuery is Encode for a carrier that holds any bytes, a query string:
// ref itself, unless it starts with the marker, which a reader would decode.
func EncodeQuery(ref string) string {
	if strings.HasPrefix(ref, Prefix) {
		return Encode(ref)
	}
	return ref
}

// Decode reads text form back into a reference. Unmarked text is the
// reference. In marked text %XX is one byte and every other byte is itself
// ('+' is not a space). A '%' without two hex digits after it, and a marker
// with nothing after it, are refused with waxerr.CodeInvalidRequest.
func Decode(text string) (string, error) {
	rest, ok := strings.CutPrefix(text, Prefix)
	if !ok {
		return text, nil
	}
	if rest == "" {
		return "", waxerr.New(waxerr.CodeInvalidRequest, "source: a pct: reference names nothing after the marker")
	}
	if !strings.Contains(rest, "%") {
		return rest, nil
	}
	b := make([]byte, 0, len(rest))
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if c != '%' {
			b = append(b, c)
			continue
		}
		if i+2 >= len(rest) || !isHex(rest[i+1]) || !isHex(rest[i+2]) {
			return "", waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
				"source: a pct: reference has a %% at byte %d without two hex digits after it (a literal %% is %%25)",
				len(Prefix)+i))
		}
		b = append(b, unhex(rest[i+1])<<4|unhex(rest[i+2]))
		i += 2
	}
	return string(b), nil
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c >= 'a':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

// writtenPct is how a refusal says what to send instead.
const writtenPct = "a name whose bytes are not UTF-8 is written pct:, each such byte as %XX"

// CheckJSON refuses JSON text, one string or a whole document, that a
// decoder would read lossily: a byte that is not UTF-8, or a \u escape of
// half a surrogate pair. encoding/json reads both as U+FFFD with no error,
// and a name read that way is another file's.
func CheckJSON(b []byte) error {
	if !utf8.Valid(b) {
		return waxerr.New(waxerr.CodeInvalidRequest,
			"source: JSON text holds a byte that is not UTF-8; "+writtenPct)
	}
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' {
			continue
		}
		i++ // the escaped character, which a second backslash may be
		u, ok := escapedUnit(b, i)
		if !ok {
			continue
		}
		i += 4
		paired := false
		if 0xD800 <= u && u <= 0xDBFF && i+2 < len(b) && b[i+1] == '\\' {
			if lo, ok := escapedUnit(b, i+2); ok && 0xDC00 <= lo && lo <= 0xDFFF {
				paired = true
				i += 6
			}
		}
		if 0xD800 <= u && u <= 0xDFFF && !paired {
			return waxerr.New(waxerr.CodeInvalidRequest,
				"source: JSON text holds an unpaired surrogate escape, which stands for no character; "+writtenPct)
		}
	}
	return nil
}

// escapedUnit reads the uXXXX at b[i:], the tail of a \u escape.
func escapedUnit(b []byte, i int) (rune, bool) {
	if i+4 >= len(b) || b[i] != 'u' {
		return 0, false
	}
	var u rune
	for _, c := range b[i+1 : i+5] {
		if !isHex(c) {
			return 0, false
		}
		u = u<<4 | rune(unhex(c))
	}
	return u, true
}

// EscapeStray returns s with each byte outside a valid UTF-8 sequence
// written \xNN, as %q writes it, so JSON holds a message that quotes a name
// that is not UTF-8. A valid s is returned as it is.
func EscapeStray(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	const lowerHex = "0123456789abcdef"
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteString(`\x`)
			b.WriteByte(lowerHex[s[i]>>4])
			b.WriteByte(lowerHex[s[i]&0x0f])
			i++
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}

var (
	textMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
	jsonMarshaler = reflect.TypeFor[json.Marshaler]()
)

// EscapeStrays applies EscapeStray to every plain string v points at: in
// its fields, and through the slices, maps and pointers below them. It is
// for a document about to be written as JSON, so a field added to one is
// covered without a call of its own. A type that marshals itself (a
// reference, a time) is left alone, as are unexported fields.
func EscapeStrays(v any) {
	if p := reflect.ValueOf(v); p.Kind() == reflect.Pointer && !p.IsNil() {
		escapeStrays(p.Elem())
	}
}

// marshalsItself reports whether t writes its own JSON, so that its bytes
// are its value and not text to escape.
func marshalsItself(t reflect.Type) bool {
	p := reflect.PointerTo(t)
	return t.Implements(textMarshaler) || t.Implements(jsonMarshaler) ||
		p.Implements(textMarshaler) || p.Implements(jsonMarshaler)
}

func escapeStrays(v reflect.Value) {
	if marshalsItself(v.Type()) {
		return
	}
	switch v.Kind() {
	case reflect.String:
		if s := v.String(); v.CanSet() && !utf8.ValidString(s) {
			v.SetString(EscapeStray(s))
		}
	case reflect.Pointer:
		if !v.IsNil() {
			escapeStrays(v.Elem())
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				escapeStrays(v.Field(i))
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			escapeStrays(v.Index(i))
		}
	case reflect.Map:
		// A map's values cannot be set in place, and only a string value has
		// anything to escape without one.
		if elem := v.Type().Elem(); elem.Kind() != reflect.String || marshalsItself(elem) {
			return
		}
		for _, k := range v.MapKeys() {
			if s := v.MapIndex(k).String(); !utf8.ValidString(s) {
				v.SetMapIndex(k, reflect.ValueOf(EscapeStray(s)).Convert(v.Type().Elem()))
			}
		}
	}
}
