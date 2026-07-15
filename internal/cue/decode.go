package cue

import (
	"strings"
	"unicode/utf8"
)

// utf8BOM is the byte-order mark a Windows editor prepends when it saves
// UTF-8. It is not a character, and leaving it in place would make the
// first command of the sheet unrecognizable.
const utf8BOM = "\xef\xbb\xbf"

// cp1252High maps the bytes CP1252 assigns and latin-1 does not, 0x80 to
// 0x9F, onto their Unicode code points. The five holes are the byte values
// CP1252 leaves undefined; they map to their own value, which is what
// every decoder in practice does with them.
//
// This table is the whole reason the fallback is CP1252 rather than
// latin-1, and it is worth being concrete about the difference: latin-1
// reads this range as C1 control characters, so a Windows-authored sheet's
// curly quotes, dashes, and ellipses would each become an unprintable
// control code. These 32 entries are exactly the bytes a real sheet
// carries and a naive rune(b) destroys.
var cp1252High = [32]rune{
	0x20AC, 0x0081, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021,
	0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0x008D, 0x017D, 0x008F,
	0x0090, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
	0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0x009D, 0x017E, 0x0178,
}

// decode turns a sheet's bytes into text, best effort.
//
// Sheets in the wild are UTF-8, UTF-8 with a BOM, or an 8-bit Windows
// codepage, and they carry no declaration of which one. So: strip a BOM,
// take the bytes as UTF-8 when they are valid UTF-8, and fall back to
// CP1252 when they are not.
//
// Valid-UTF-8-wins is what makes the fallback safe rather than a guess.
// The two encodings only ever disagree about bytes >= 0x80, and a
// multi-byte UTF-8 sequence is not valid CP1252 text in any meaningful
// sense, so the check is close to a decision procedure for the two
// encodings this can actually tell apart.
//
// It is still best effort: a Shift-JIS sheet's titles come out wrong,
// because telling Shift-JIS from CP1252 needs a real encoding table and
// this has 32 entries. Tag-level repair is WaxLabel's job either way.
//
// The table is hand-written rather than golang.org/x/text/encoding because
// this package sits in the root module, which server imports and which
// therefore has to stay stdlib-only (ADR-0002, enforced by depcheck). That
// is a consequence of where this package lives and not a judgment that the
// dependency would be wrong: were CUE parsing ever CLI-only, it would
// belong in cli/, where x/text costs the root module's import graph
// nothing.
func decode(b []byte) string {
	s := strings.TrimPrefix(string(b), utf8BOM)
	if utf8.ValidString(s) {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c < 0x80:
			sb.WriteByte(c)
		case c < 0xA0:
			sb.WriteRune(cp1252High[c-0x80])
		default:
			// 0xA0 and up are the range CP1252 shares with latin-1, where
			// the byte is its own code point.
			sb.WriteRune(rune(c))
		}
	}
	return sb.String()
}
