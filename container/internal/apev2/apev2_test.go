package apev2

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

type item struct {
	key   string
	value string
	flags uint32
}

// build assembles an APEv2 tag: items, a footer, and optionally the identical
// header that precedes them.
func build(withHeader bool, items ...item) []byte {
	var body []byte
	for _, it := range items {
		var hdr [8]byte
		binary.LittleEndian.PutUint32(hdr[0:], uint32(len(it.value)))
		binary.LittleEndian.PutUint32(hdr[4:], it.flags)
		body = append(body, hdr[:]...)
		body = append(body, it.key...)
		body = append(body, 0)
		body = append(body, it.value...)
	}
	preamble := func(isHeader bool) []byte {
		b := make([]byte, FooterLen)
		copy(b, "APETAGEX")
		binary.LittleEndian.PutUint32(b[8:], 2000)
		binary.LittleEndian.PutUint32(b[12:], uint32(len(body)+FooterLen))
		binary.LittleEndian.PutUint32(b[16:], uint32(len(items)))
		var flags uint32
		if withHeader {
			flags |= flagHasHeader
		}
		if isHeader {
			flags |= flagIsHeader
		}
		binary.LittleEndian.PutUint32(b[20:], flags)
		return b
	}
	var out []byte
	if withHeader {
		out = append(out, preamble(true)...)
	}
	out = append(out, body...)
	return append(out, preamble(false)...)
}

func TestSizeAndParse(t *testing.T) {
	for _, withHeader := range []bool{false, true} {
		tag := build(withHeader,
			item{key: "Title", value: "A Title"},
			item{key: "Album Artist", value: "Someone"},
			item{key: "Track", value: "3"},
			item{key: "Year", value: "1997"},
			item{key: "Artist", value: "One\x00Two"},
			item{key: "REPLAYGAIN_TRACK_GAIN", value: "-4.20 dB"},
			item{key: "Cover Art (Front)", value: "binary", flags: 2},
		)
		got, gotHeader := Size(tag)
		if got != int64(len(tag)) || gotHeader != withHeader {
			t.Fatalf("Size = %d header=%v, want the whole %d-byte tag with header=%v",
				got, gotHeader, len(tag), withHeader)
		}
		if StartsTag(tag) != withHeader {
			t.Errorf("StartsTag = %v, want %v", StartsTag(tag), withHeader)
		}
		tags := Parse(tag)
		for key, want := range map[string][]string{
			"TITLE":                 {"A Title"},
			"ALBUMARTIST":           {"Someone"},
			"TRACKNUMBER":           {"3"},
			"RECORDINGDATE":         {"1997"},
			"ARTIST":                {"One", "Two"},
			"REPLAYGAIN_TRACK_GAIN": {"-4.20 dB"},
		} {
			got := tags[key]
			if len(got) != len(want) {
				t.Fatalf("%s = %v, want %v", key, got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("%s[%d] = %q, want %q", key, i, got[i], want[i])
				}
			}
		}
		if _, ok := tags["COVER ART (FRONT)"]; ok {
			t.Error("a binary item was read as text")
		}
	}
}

// TestSizeRejects covers what must not be mistaken for a tag footer: a header
// (the tag starts there rather than ends), a truncated buffer, and a size
// field past the cap.
func TestSizeRejects(t *testing.T) {
	tag := build(true, item{key: "Title", value: "x"})
	if got, _ := Size(tag[:FooterLen]); got != 0 {
		t.Errorf("Size of a header = %d, want 0", got)
	}
	if got, _ := Size(tag[:10]); got != 0 {
		t.Errorf("Size of a short buffer = %d, want 0", got)
	}
	if got, _ := Size(bytes.Repeat([]byte{'z'}, 64)); got != 0 {
		t.Errorf("Size of junk = %d, want 0", got)
	}
	huge := build(false, item{key: "Title", value: "x"})
	binary.LittleEndian.PutUint32(huge[len(huge)-FooterLen+12:], 1<<30)
	if got, _ := Size(huge); got != 0 {
		t.Errorf("Size past the cap = %d, want 0", got)
	}
	if StartsTag(tag[FooterLen:]) {
		t.Error("StartsTag accepted bytes that are not the tag's header")
	}
}

// TestParseSurvivesDamage checks the item walk stops rather than reading past
// its buffer when the counts and sizes lie.
func TestParseSurvivesDamage(t *testing.T) {
	tag := build(false, item{key: "Title", value: "A Title"}, item{key: "Artist", value: "Someone"})
	t.Run("count too high", func(t *testing.T) {
		b := append([]byte(nil), tag...)
		binary.LittleEndian.PutUint32(b[len(b)-FooterLen+16:], 1<<20)
		if got := Parse(b)["TITLE"]; len(got) != 1 {
			t.Errorf("TITLE = %v, want the one real item", got)
		}
	})
	t.Run("value size too big", func(t *testing.T) {
		b := append([]byte(nil), tag...)
		binary.LittleEndian.PutUint32(b[0:], 1<<20)
		Parse(b) // must not panic; nothing usable is expected back
	})
	t.Run("no key terminator", func(t *testing.T) {
		b := append([]byte(nil), tag...)
		for i := 8; i < len(b); i++ {
			if b[i] == 0 {
				b[i] = 'x'
			}
		}
		Parse(b)
	})
	t.Run("truncated", func(t *testing.T) {
		for n := 0; n < len(tag); n += 3 {
			Parse(tag[:n])
		}
	})
}

// TestCanonicalRejectsUnwritableKeys checks that a key no muxer could write
// back is dropped rather than mangled.
func TestCanonicalRejectsUnwritableKeys(t *testing.T) {
	for _, key := range []string{"", "with=equals", "with\x01control", strings.Repeat("x", 300)} {
		if got := canonical(key); got != "" {
			t.Errorf("canonical(%q) = %q, want it dropped", key, got)
		}
	}
	if got := canonical("title"); got != "TITLE" {
		t.Errorf("canonical(title) = %q, want TITLE", got)
	}
	// container.ValidTagKey stops at 0x7d, so a key a muxer would skip is
	// dropped here rather than surfaced as a tag nothing can write back.
	if got := canonical("tilde~"); got != "" {
		t.Errorf("canonical(tilde~) = %q, want it dropped", got)
	}
}
