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
	blob, err := Build(tags)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
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
// for a handful of fields, and writing the canonical name puts each of them
// where no reader looks: ffmpeg's APEv2 converter, the reference tools,
// foobar2000 and the tag library's own APE writer all spell them this way.
// Build used to compute the canonical name only to test it and then write the
// caller's raw key, so a .wv carried TRACKNUMBER, RECORDINGDATE and ALBUMARTIST
// where a wavpack-written one carries Track, Year and Album Artist.
//
// A read alias earns a row here only when its key's APEv2 name differs by more
// than case: Catalog does, LABEL (which ORGANIZATION and PUBLISHER fold onto)
// does not.
func TestBuildSpellsAPEv2Keys(t *testing.T) {
	want := map[string]string{
		"TRACKNUMBER":   "Track",
		"DISCNUMBER":    "Disc",
		"RECORDINGDATE": "Year",
		"ALBUMARTIST":   "Album Artist",
		"CATALOGNUMBER": "Catalog",
		"TITLE":         "TITLE", // no APEv2 spelling of its own; case is not a difference
		"LABEL":         "LABEL",
	}
	for canon, spelled := range want {
		blob, err := Build([]Tag{{Key: canon, Value: "v"}})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
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
	blob, err := Build([]Tag{
		{Key: "ARTIST", Value: "A"},
		{Key: "ARTIST", Value: "B"},
		{Key: "ARTIST", Value: "C"},
		{Key: "YEAR", Value: "1999"},
		{Key: "RECORDINGDATE", Value: "2001"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
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
		got, err := Build(tags)
		if err != nil {
			t.Errorf("Build(%v): %v", tags, err)
		}
		if got != nil {
			t.Errorf("Build(%v) produced %d bytes, want nothing", tags, len(got))
		}
	}
}

// TestBuildRefusesOversized pins the write cap as a refusal: a value that does
// not fit the block fails the whole render rather than being skipped, since a
// block written without it is a file missing a tag the caller asked to embed.
// The error names the key and the cap so the caller knows what to trim.
func TestBuildRefusesOversized(t *testing.T) {
	tags := []Tag{
		{Key: "TITLE", Value: "kept"},
		{Key: "HUGE", Value: strings.Repeat("x", maxWriteBytes)},
		{Key: "ARTIST", Value: "also kept"},
	}
	got, err := Build(tags)
	if err == nil {
		t.Fatalf("Build rendered %d bytes over an oversized value, want a refusal", len(got))
	}
	if got != nil {
		t.Errorf("a refused Build returned %d bytes, want nil", len(got))
	}
	for _, want := range []string{"HUGE", "49152"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestBuildFillsToTheCap checks the bound is the block's, not one value's: tags
// that exactly reach maxWriteBytes render, and one byte more is refused. A cap
// applied off by a whole footer would pass the refusal test above and still be
// wrong here.
func TestBuildFillsToTheCap(t *testing.T) {
	// 9 bytes of item overhead (4 size, 4 flags, 1 NUL) plus the key, and the
	// header and footer the block carries either way.
	fit := maxWriteBytes - 2*FooterLen - 9 - len("HUGE")
	blob, err := Build([]Tag{{Key: "HUGE", Value: strings.Repeat("x", fit)}})
	if err != nil {
		t.Fatalf("a value filling the block exactly was refused: %v", err)
	}
	if len(blob) != maxWriteBytes {
		t.Errorf("the full block rendered %d bytes, want %d", len(blob), maxWriteBytes)
	}
	if got := Parse(blob); len(got["HUGE"]) != 1 || len(got["HUGE"][0]) != fit {
		t.Error("the full block did not read back")
	}
	if _, err := Build([]Tag{{Key: "HUGE", Value: strings.Repeat("x", fit+1)}}); err == nil {
		t.Error("one byte past the cap rendered anyway")
	}
}

// TestBuildOwnsKeyFiltering pins that Build alone decides which keys are
// writable. The two muxers used to run container.ValidTagKey over their tags
// before handing them here, which was canonical's rule written a second time
// and free to drift from it; this is what makes dropping that pass safe. The
// keys below are the three canonical rejects: the separator a reader would
// split on, a control byte, and a key past the length it accepts.
func TestBuildOwnsKeyFiltering(t *testing.T) {
	blob, err := Build([]Tag{
		{Key: "TITLE", Value: "Kept"},
		{Key: "with=equals", Value: "no reader could ask for it"},
		{Key: "nul\x00key", Value: "nor this"},
		{Key: strings.Repeat("K", 256), Value: "nor a key past 255 bytes"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got := Parse(blob)
	if len(got) != 1 || len(got["TITLE"]) != 1 || got["TITLE"][0] != "Kept" {
		t.Errorf("read back %v, want the one writable tag", got)
	}
}

// TestParseAliasesMatchTheMapper pins the read-side spellings the tag library
// also folds. Both readers now speak for a .wv or .ape source: the mapper's
// view wins per key and the Tagger fold fills in the keys it lacks, so a
// spelling the two fold differently surfaces as two tags carrying one value,
// written back as two items. The cases are the item names real APEv2 taggers
// write whose canonical key is not just the name uppercased.
func TestParseAliasesMatchTheMapper(t *testing.T) {
	// Every spelling the library resolves through its APE convention table or
	// the shared alias table, with the canonical key it resolves to. Anything
	// the library folds and this does not splits one value across two keys;
	// the cases below the blank line are the ones a bare uppercase would miss.
	for native, canon := range map[string]string{
		"Date":                    "RECORDINGDATE",
		"OriginalYear":            "ORIGINALDATE",
		"Publisher":               "LABEL",
		"Catalog":                 "CATALOGNUMBER",
		"MUSICBRAINZ_ALBUMSTATUS": "RELEASESTATUS",
		"MUSICBRAINZ_ALBUMTYPE":   "RELEASETYPE",

		"Track": "TRACKNUMBER", "Disc": "DISCNUMBER", "Year": "RECORDINGDATE",
		"Album Artist": "ALBUMARTIST", "ALBUM_ARTIST": "ALBUMARTIST",
		"DJ MIXER": "DJMIXER", "DJ_MIXER": "DJMIXER", "DJ-MIXER": "DJMIXER",
		"TOTALTRACKS": "TRACKTOTAL", "TOTALDISCS": "DISCTOTAL",
		"PART_NUMBER": "TRACKNUMBER", "TOTAL_PARTS": "TRACKTOTAL",
		"TOTAL_DISCS": "DISCTOTAL", "ORGANIZATION": "LABEL",
		"CATALOG_NUMBER": "CATALOGNUMBER", "LEAD_PERFORMER": "ARTIST",
		"DATE_RECORDED": "RECORDINGDATE", "DATE_RELEASED": "RELEASEDATE",
		"DATE_RELEASE": "RELEASEDATE", "DATE_ORIGINAL": "ORIGINALDATE",
		"ORIGINAL_DATE": "ORIGINALDATE", "ENCODED_BY": "ENCODEDBY",
		"REMIXED_BY": "REMIXER", "CONTENT_GROUP": "GROUPING",
		"UNSYNCEDLYRICS": "LYRICS",
	} {
		got := Parse(build(true, item{key: native, value: "v"}))
		if vs := got[canon]; len(vs) != 1 || vs[0] != "v" {
			t.Errorf("item %q read back as %v, want it under %s (the mapper's key)", native, got, canon)
		}
	}
}

// TestParseFoldsNoMoreThanTheMapper is the agreement's other direction. A
// spelling this folds and the library does not splits the value exactly as a
// spelling only the library folds does, so the table may not run ahead either.
// "Record Date" is the case that was: the library has no entry for it in
// either table and reads it as the custom key of that name.
func TestParseFoldsNoMoreThanTheMapper(t *testing.T) {
	for _, native := range []string{"Record Date"} {
		got := Parse(build(true, item{key: native, value: "v"}))
		if vs := got["RECORD DATE"]; len(vs) != 1 || vs[0] != "v" {
			t.Errorf("item %q read back as %v, want it under its own name like the mapper", native, got)
		}
	}
}

// TestBuildDropsReservedItemNames pins the specification's four forbidden item
// names. Each is the magic another structure is found by, so writing one
// plants a false signature inside the tag block of the very file readers scan
// for it. They are a representability rule like the charset above, not a size
// refusal: the format cannot hold them, so Build skips them. The comparison is
// on the canonical uppercase form, which is also how APEv2 readers compare
// keys. Mixed-case spellings pin that fold.
func TestBuildDropsReservedItemNames(t *testing.T) {
	blob, err := Build([]Tag{
		{Key: "TITLE", Value: "Kept"},
		{Key: "TAG", Value: "an ID3v1 trailer's magic"},
		{Key: "id3", Value: "an ID3v2 header's magic"},
		{Key: "OggS", Value: "an Ogg page's magic"},
		{Key: "MP+", Value: "a Musepack stream's magic"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got := Parse(blob)
	if len(got) != 1 || len(got["TITLE"]) != 1 || got["TITLE"][0] != "Kept" {
		t.Errorf("read back %v, want the one writable tag", got)
	}
	// The footer count is the structural half: Parse folding a leaked item
	// away would hide it, a count of 1 cannot. The byte scan is the belt on
	// top; NUL-terminated needles can in principle match inside a multi-value
	// payload, but the only kept value here is "Kept".
	if n := binary.LittleEndian.Uint32(blob[len(blob)-FooterLen+16:]); n != 1 {
		t.Errorf("the footer declares %d items, want 1 (TITLE alone)", n)
	}
	for _, magic := range []string{"TAG", "ID3", "OGGS", "MP+"} {
		if bytes.Contains(blob, append([]byte(magic), 0)) {
			t.Errorf("the block carries an item named %q", magic)
		}
	}
	// Reading stays unchanged: a foreign file already carrying one keeps its
	// value visible, the same as the reference reader, so a transfer can still
	// see what the file holds even though no WaxFlow writer will re-emit it.
	foreign := build(true, item{key: "TAG", value: "already on disk"})
	if v := Parse(foreign)["TAG"]; len(v) != 1 || v[0] != "already on disk" {
		t.Errorf("a file-borne reserved item read back as %v", v)
	}
}

// TestParseItemsCount: the count a header record states bounds the walk, zero
// reads nothing, and a count outside the cap reads up to the cap.
func TestParseItemsCount(t *testing.T) {
	tag := build(false, item{key: "Artist", value: "A"}, item{key: "Title", value: "T"})
	items := tag[:len(tag)-FooterLen]
	for _, tc := range []struct {
		count int
		want  int
	}{{0, 0}, {1, 1}, {2, 2}, {3, 2}, {-1, 2}, {maxItems + 1, 2}} {
		got := ParseItems(items, tc.count)
		if len(got) != tc.want {
			t.Errorf("count %d read %d items %v, want %d", tc.count, len(got), got, tc.want)
		}
		if tc.want == 1 && got["ARTIST"] == nil {
			t.Errorf("count 1 read %v, want the first item", got)
		}
	}
}
