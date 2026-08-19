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

// TestBuildParseRoundTrip pins the writer against the reader in this package,
// so the two halves of the format live and fail together rather than only
// through a demuxer that happens to call both.
func TestBuildParseRoundTrip(t *testing.T) {
	tags := []Tag{
		{Key: "TITLE", Value: "Round Trip"},
		{Key: "ARTIST", Value: "WaxFlow"},
		{Key: "TRACKNUMBER", Value: "3"},
		{Key: "RECORDINGDATE", Value: "2026"},
		{Key: "with=equals", Value: "dropped"}, // no reader could ask for it
		{Key: "EMPTY", Value: ""},              // nothing to say
	}
	blob := Build(tags)
	if blob == nil {
		t.Fatal("Build produced nothing for four writable tags")
	}
	// The extent the reading side computes off the footer has to be the whole
	// block, header included, or a demuxer peels the wrong number of bytes.
	total, hasHeader := Size(blob[len(blob)-FooterLen:])
	if !hasHeader {
		t.Error("the footer does not announce the header Build writes")
	}
	if total != int64(len(blob)) {
		t.Errorf("the footer declares %d bytes, Build wrote %d", total, len(blob))
	}
	if !StartsTag(blob) {
		t.Error("the block does not begin with the header its footer promised")
	}

	got := Parse(blob)
	for _, want := range tags[:4] {
		if v := got[want.Key]; len(v) != 1 || v[0] != want.Value {
			t.Errorf("tag %s read back as %v, want %q", want.Key, v, want.Value)
		}
	}
	if len(got) != 4 {
		t.Errorf("read back %d tags, want the 4 writable ones: %v", len(got), got)
	}
}

// TestBuildSpellsAPEv2Keys pins the write direction of the alias table. The
// canonical vocabulary is Vorbis comment's; APEv2's differs by more than case
// for four fields, and writing the canonical name puts each of them where no
// reader looks -- ffmpeg's APEv2 converter lists exactly these four, and the
// reference tools and foobar2000 spell them the same way. Build used to
// compute the canonical name only to test it and then write the caller's raw
// key, so a .wv carried TRACKNUMBER, RECORDINGDATE and ALBUMARTIST where a
// wavpack-written one carries Track, Year and Album Artist.
func TestBuildSpellsAPEv2Keys(t *testing.T) {
	want := map[string]string{
		"TRACKNUMBER":   "Track",
		"DISCNUMBER":    "Disc",
		"RECORDINGDATE": "Year",
		"ALBUMARTIST":   "Album Artist",
		"TITLE":         "TITLE", // no APEv2 spelling of its own; case is not a difference
	}
	for canon, spelled := range want {
		blob := Build([]Tag{{Key: canon, Value: "v"}})
		if !bytes.Contains(blob, append([]byte(spelled), 0)) {
			t.Errorf("%s was not written as %q", canon, spelled)
		}
		// And the pair is closed: what we write, we read back as the
		// canonical name, so a round trip through a .wv keeps the field.
		if v := Parse(blob)[canon]; len(v) != 1 || v[0] != "v" {
			t.Errorf("%s written as %q read back as %v", canon, spelled, v)
		}
	}
}

// TestBuildMergesMultipleValues pins the one-item-per-key rule. An APEv2 key
// is unique within a tag and a multi-valued field is one item whose values are
// NUL-separated. Emitting one item per value instead is not merely
// non-canonical, it is lossy: a reader that looks the key up by name returns
// the first match, so every value after the first is unreachable to everyone
// but us. Two spellings of one field merge for the same reason, since the
// alternative is the duplicate key the format forbids.
func TestBuildMergesMultipleValues(t *testing.T) {
	blob := Build([]Tag{
		{Key: "ARTIST", Value: "A"},
		{Key: "ARTIST", Value: "B"},
		{Key: "ARTIST", Value: "C"},
		{Key: "YEAR", Value: "1999"},
		{Key: "RECORDINGDATE", Value: "2001"},
	})
	if n := binary.LittleEndian.Uint32(blob[len(blob)-FooterLen+16:]); n != 2 {
		t.Errorf("the footer declares %d items, want 2 (one per key)", n)
	}
	if n := bytes.Count(blob, append([]byte("ARTIST"), 0)); n != 1 {
		t.Errorf("ARTIST appears as %d items, want 1", n)
	}
	got := Parse(blob)
	if v := got["ARTIST"]; len(v) != 3 || v[0] != "A" || v[1] != "B" || v[2] != "C" {
		t.Errorf("ARTIST read back as %v, want all three values", v)
	}
	if v := got["RECORDINGDATE"]; len(v) != 2 {
		t.Errorf("the two date spellings read back as %v, want one merged item", v)
	}
}

// TestBuildDropsEverything pins the nil return: a caller with nothing writable
// must get no block at all rather than an empty one, since a zero-item tag
// after the audio is a structure every reader then has to peel for nothing.
func TestBuildDropsEverything(t *testing.T) {
	for _, tags := range [][]Tag{nil, {}, {{Key: "bad=key", Value: "x"}}, {{Key: "TITLE", Value: ""}}} {
		if got := Build(tags); got != nil {
			t.Errorf("Build(%v) produced %d bytes, want nothing", tags, len(got))
		}
	}
}

// TestBuildCapsSize pins the write cap: one oversized value is skipped, and
// the small descriptive tags after it still land.
func TestBuildCapsSize(t *testing.T) {
	tags := []Tag{
		{Key: "TITLE", Value: "kept"},
		{Key: "HUGE", Value: strings.Repeat("x", maxWriteBytes)},
		{Key: "ARTIST", Value: "also kept"},
	}
	got := Parse(Build(tags))
	if len(got) != 2 || got["TITLE"][0] != "kept" || got["ARTIST"][0] != "also kept" {
		t.Errorf("read back %v, want the two small tags", got)
	}
}
