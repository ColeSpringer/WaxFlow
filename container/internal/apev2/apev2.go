// Package apev2 parses APEv2 tags: the key/value block WavPack and Monkey's
// Audio files carry, almost always appended after the audio. Finding the block
// (to keep it out of the audio stream) and reading it (to surface tags) are
// two halves of the same knowledge, so they share one parser here; the
// trailer package does the finding on top of Size and StartsTag.
//
// The mpa demuxer is deliberately not rewritten onto this package. It walks a
// trailer forward from where frames stopped parsing and accepts a truncated
// tag, so what it needs is "a block starts here", not the extent Size answers.
package apev2

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/colespringer/waxflow/container"
)

// FooterLen is the size of the fixed footer, and of the optional identical
// header that precedes the items.
const FooterLen = 32

// Hostile-input caps.
const (
	// maxItems bounds the item walk. Real tags hold a few dozen.
	maxItems = 4096
	// maxTagBytes bounds the whole block. Embedded cover art is what makes
	// APEv2 tags large; anything past this is not a tag we will read.
	maxTagBytes = 16 << 20
	// maxValueBytes bounds one text value, so a crafted length cannot turn
	// into a large string.
	maxValueBytes = 64 << 10
)

const (
	flagHasHeader = 1 << 31
	flagIsHeader  = 1 << 29
	// typeMask selects the item's value type; only text (0) is read.
	typeMask = 0x6
)

// Size reports the total byte length of the APEv2 tag whose 32-byte footer is
// at the end of b, or 0 when b does not end in one. The result covers the
// header when the footer says one is present, so subtracting it from the
// footer's end offset gives the tag's first byte. hasHeader reports that case,
// which is the caller's chance to confirm the extent: the tag's first bytes
// must then be the header's own magic.
func Size(b []byte) (total int64, hasHeader bool) {
	if len(b) < FooterLen {
		return 0, false
	}
	f := b[len(b)-FooterLen:]
	if string(f[:8]) != "APETAGEX" {
		return 0, false
	}
	flags := binary.LittleEndian.Uint32(f[20:24])
	if flags&flagIsHeader != 0 {
		return 0, false // a header, not a footer: the tag starts here rather than ends
	}
	// The size field covers the items plus this footer, so one under a
	// footer's own length is no tag's; a header, when present, is an equally
	// sized preamble the field does not count.
	total = int64(binary.LittleEndian.Uint32(f[12:16]))
	if total < FooterLen {
		return 0, false
	}
	hasHeader = flags&flagHasHeader != 0
	if hasHeader {
		total += FooterLen
	}
	if total > maxTagBytes {
		return 0, false
	}
	return total, hasHeader
}

// StartsTag reports whether b begins with the tag's optional header, the
// confirmation for an extent whose footer declared one.
func StartsTag(b []byte) bool {
	return len(b) >= FooterLen && string(b[:8]) == "APETAGEX" &&
		binary.LittleEndian.Uint32(b[20:24])&flagIsHeader != 0
}

// Parse reads the items of a whole APEv2 tag block (header if present, items,
// footer) into canonical uppercase keys, values in file order. Binary items
// such as cover art are skipped: this is the descriptive-tag surface, and a
// picture needs an opt-in accessor rather than a field read. A track or disc
// number holding its total reads as the two keys.
func Parse(tag []byte) map[string][]string {
	if len(tag) < FooterLen {
		return nil
	}
	f := tag[len(tag)-FooterLen:]
	if string(f[:8]) != "APETAGEX" {
		return nil
	}
	count := int(binary.LittleEndian.Uint32(f[16:20]))
	if count <= 0 {
		return nil
	}
	count = min(count, maxItems)
	items := tag[:len(tag)-FooterLen]
	if binary.LittleEndian.Uint32(f[20:24])&flagHasHeader != 0 {
		if len(items) < FooterLen {
			return nil
		}
		items = items[FooterLen:]
	}

	tags, seen := parseItems(items, count)
	if declared := binary.LittleEndian.Uint32(f[16:20]); declared > maxItems {
		seen.blind() // the library reads items past the cap
	}
	if tags != nil {
		container.SplitNumberPairs(tags, seen.count)
	}
	if len(tags) == 0 {
		return nil
	}
	return tags
}

// pairSeen counts the values the tag library's read of a tag holds under the
// track and disc number and total keys, which decides its number split. The
// library keeps an empty value ahead of or between others, where Parse drops
// it, and reads items Parse skips, so the count is taken off the raw items.
// The empty key marks a count Parse could not take.
type pairSeen map[string]int

var pairKeys = map[string]bool{"TRACKNUMBER": true, "TRACKTOTAL": true, "DISCNUMBER": true, "DISCTOTAL": true}

func (s pairSeen) count(key string) int {
	if s[""] != 0 {
		return -1
	}
	return s[key]
}

// blind marks the counts unknown (container.SplitNumberPairs).
func (s pairSeen) blind() { s[""] = 1 }

// note counts one text item as the library reads it: its values are the ones
// left once the trailing NULs, which are terminators, are cut.
func (s pairSeen) note(name string, value []byte) {
	if pairKeys[name] {
		s[name] += bytes.Count(bytes.TrimRight(value, "\x00"), []byte{0}) + 1
	} else if foldsOntoPair(name) {
		s.blind()
	}
}

// foldsOntoPair reports a name past ASCII that the library's Unicode case
// folding reads as a number or total key, which Parse refuses to read.
func foldsOntoPair(name string) bool {
	t := strings.TrimSpace(name)
	if !hasNonASCII(t) {
		return false
	}
	for _, f := range [...]string{strings.ToLower(t), strings.ToUpper(t)} {
		if !hasNonASCII(f) && pairKeys[canonical(f)] {
			return true
		}
	}
	return false
}

func hasNonASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return true
		}
	}
	return false
}

// RecordLen is a header or footer record less its 8-byte preamble: version,
// size, item count, flags and the reserved word.
const RecordLen = FooterLen - 8

// ParseHeaderRecord reads a header record stored without its preamble, the
// form a Musepack SV8 chapter tag puts in front of its items. Version 1000 or
// 2000 and the flag that marks a header rather than a footer identify one;
// anything else is not a record. It returns the item count the record
// states, which bounds ParseItems over the bytes that follow it.
func ParseHeaderRecord(b []byte) (count int, ok bool) {
	if len(b) < RecordLen {
		return 0, false
	}
	if v := binary.LittleEndian.Uint32(b[0:4]); v != 1000 && v != 2000 {
		return 0, false
	}
	if binary.LittleEndian.Uint32(b[12:16])&flagIsHeader == 0 {
		return 0, false
	}
	return int(binary.LittleEndian.Uint32(b[8:12])), true
}

// ParseItems reads a run of APEv2 items with nothing around them: no
// preamble, no header, no footer. count is the item count a header record
// elsewhere stated and bounds the walk, which also ends where the bytes do:
// zero reads nothing, and a count outside the item cap is read as the cap. A
// Musepack SV8 chapter packet carries its tag this way, the header record
// (minus the preamble) in front of the items.
func ParseItems(items []byte, count int) map[string][]string {
	if count < 0 || count > maxItems {
		count = maxItems
	}
	tags, _ := parseItems(items, count)
	return tags
}

// parseItems reads up to count items off the front of items, and counts the
// number pair values the library sees there.
func parseItems(items []byte, count int) (map[string][]string, pairSeen) {
	out := map[string][]string{}
	seen := pairSeen{}
	for range count {
		if len(items) < 8 {
			break
		}
		size := int64(binary.LittleEndian.Uint32(items[0:4]))
		flags := binary.LittleEndian.Uint32(items[4:8])
		items = items[8:]
		key := items
		if i := bytes.IndexByte(items, 0); i >= 0 {
			key, items = items[:i], items[i+1:]
		} else {
			break
		}
		if size < 0 || size > int64(len(items)) {
			break
		}
		value := items[:size]
		items = items[size:]
		if flags&typeMask != 0 {
			continue // binary or locator payload
		}
		name := canonical(string(key))
		if name == "" {
			seen.note(string(key), value)
			continue
		}
		seen.note(name, value)
		if size > maxValueBytes {
			continue // too large to be text
		}
		// A multi-valued item stores its values NUL-separated.
		for v := range strings.SplitSeq(text(value), "\x00") {
			if v != "" {
				out[name] = append(out[name], v)
			}
		}
	}
	if len(out) == 0 {
		return nil, seen
	}
	return out, seen
}

// text decodes an item value: UTF-8 as the format requires, and anything else
// as the Latin-1 of APEv1 and of out-of-spec writers, one byte to a code
// point, which is how the tag library reads it too.
func text(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

// aliases maps the APEv2 spellings that differ from the canonical uppercase
// vocabulary onto it. Everything else passes through uppercased, which is
// already correct for the common fields and for REPLAYGAIN_*.
//
// The set must fold exactly what the tag library folds for the same items,
// because both readers speak for a .wv or .ape source: the mapper's view wins
// per key and the Tagger fold fills in the keys it lacks, so a spelling only
// one side folds surfaces as two tags carrying one value (see the Tagger doc
// in container/metadata.go). The agreement runs both ways. Folding a spelling
// the library leaves alone splits the value just as folding one less does,
// which is why there is no entry for "Record Date": the library reads it as
// the custom key of that name, so this must too.
//
// The library resolves an APE item name through its APE convention table and
// then the shared alias table every codec reads through (internal/mapping/ape.go
// and tag/aliases.go). Mirror both when either moves;
// oracletest/apev2_mirror_test.go fails when they disagree.
var aliases = map[string]string{
	// Bare and separated user spellings.
	"TRACK":        "TRACKNUMBER",
	"DISC":         "DISCNUMBER",
	"YEAR":         "RECORDINGDATE",
	"ALBUM ARTIST": "ALBUMARTIST",
	"ALBUM_ARTIST": "ALBUMARTIST",
	"DJ MIXER":     "DJMIXER",
	"DJ_MIXER":     "DJMIXER",
	"DJ-MIXER":     "DJMIXER",
	// Dates. The library folds the Vorbis spellings and the Matroska native
	// ones onto the same three keys.
	"DATE":          "RECORDINGDATE",
	"ORIGINALYEAR":  "ORIGINALDATE",
	"DATE_RECORDED": "RECORDINGDATE",
	"DATE_RELEASED": "RELEASEDATE",
	"DATE_RELEASE":  "RELEASEDATE",
	"DATE_ORIGINAL": "ORIGINALDATE",
	"ORIGINAL_DATE": "ORIGINALDATE",
	// Totals. TRACKTOTAL and DISCTOTAL need no entry; they uppercase onto
	// themselves.
	"TOTALTRACKS": "TRACKTOTAL",
	"TOTALDISCS":  "DISCTOTAL",
	"PART_NUMBER": "TRACKNUMBER",
	"TOTAL_PARTS": "TRACKTOTAL",
	"TOTAL_DISCS": "DISCTOTAL",
	// Label and catalogue.
	"PUBLISHER":      "LABEL",
	"ORGANIZATION":   "LABEL",
	"CATALOG":        "CATALOGNUMBER",
	"CATALOG_NUMBER": "CATALOGNUMBER",
	// Roles and the rest.
	"LEAD_PERFORMER": "ARTIST",
	"REMIXED_BY":     "REMIXER",
	"CONTENT_GROUP":  "GROUPING",
	"ENCODED_BY":     "ENCODEDBY",
	"UNSYNCEDLYRICS": "LYRICS",
	"USLT":           "LYRICS", // the ID3 lyrics frame's name, as ffmpeg writes it
	// The legacy Picard spellings, still the current APE convention.
	"MUSICBRAINZ_ALBUMSTATUS": "RELEASESTATUS",
	"MUSICBRAINZ_ALBUMTYPE":   "RELEASETYPE",
}

// apeSpelling is the write direction of aliases: the APEv2 spellings for the
// canonical keys whose names, not merely whose case, differ from it. Writing
// the canonical name instead puts the field where no reader looks, and these
// are the names ffmpeg's APEv2 converter, the reference tools, foobar2000 and
// the tag library's own APE writer agree on. Keys that differ only in case are
// left alone; readers fold case, and there is no evidence to spend a table on.
//
// Several aliases point at RECORDINGDATE and only one leads back: Year is the
// documented APEv2 key, the rest are synonyms.
var apeSpelling = map[string]string{
	"TRACKNUMBER":   "Track",
	"DISCNUMBER":    "Disc",
	"RECORDINGDATE": "Year",
	"ALBUMARTIST":   "Album Artist",
	// Catalog is the fifth: reading "Catalog" onto CATALOGNUMBER without it
	// would write the canonical name back, which is neither what the tag
	// library's own APE writer emits nor what foobar2000 looks for. A read
	// alias whose key has no convention (ORGANIZATION onto LABEL) needs no row;
	// there the canonical name is already the written one.
	"CATALOGNUMBER": "Catalog",
}

// reservedItemNames are the canonical forms of the item names the APEv2
// specification forbids (ID3, TAG, OggS, MP+): each is the magic another
// structure is found by, so an item wearing one plants a false signature
// inside the tag block of the very file readers scan for it. APEv2 keys
// compare case-insensitively, so the uppercase form is the whole comparison.
// Build skips them like any other key the format cannot hold; Parse still
// reads one off a file that carries it, since the value is real either way.
var reservedItemNames = map[string]bool{"ID3": true, "TAG": true, "OGGS": true, "MP+": true}

// canonical trims an item key, uppercases it and maps it onto the canonical
// vocabulary, returning "" for a key no muxer could write back. The tag
// library trims too, so "ALBUM " is the ALBUM field there and must be here.
// Length is not the reader's business: the library reads a name of any
// length, and only Build holds names to the format's 255 bytes. A name past
// the value cap is refused anyway, since no tagger writes one.
func canonical(key string) string {
	key = strings.TrimSpace(key)
	if len(key) > maxValueBytes || !container.ValidTagKey(key) {
		return ""
	}
	up := strings.ToUpper(key)
	if alias, ok := aliases[up]; ok {
		return alias
	}
	return up
}

// writableName reports whether a text item may be written under name: 2 to
// 255 bytes of printable ASCII, not one of the reserved names, and not a
// Cover Art name, which taggers read as a binary picture item.
func writableName(name string) bool {
	if len(name) < 2 || len(name) > 255 || reservedItemNames[strings.ToUpper(name)] {
		return false
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c < 0x20 || c > 0x7e {
			return false
		}
	}
	return !coverName(name)
}

// coverName reports a picture item's name in any case: "Cover Art", alone or
// with a role in parentheses, or "Artist Picture".
func coverName(name string) bool {
	s := strings.ToLower(strings.TrimSpace(name))
	if s == "artist picture" {
		return true
	}
	rest, ok := strings.CutPrefix(s, "cover art")
	if !ok {
		return false
	}
	rest = strings.TrimSpace(rest)
	return rest == "" || rest[0] == '('
}

// Tag is one item to write, the engine's own tag shape.
type Tag = container.Tag

// writeItem is one item to write: its name as the format spells it, and its
// values.
type writeItem struct {
	key    string
	values []string
}

// group sorts tags into the items Build writes, in first-seen order, and
// names the keys it writes no item for, each once: a key that is not legal,
// and one whose item name the format forbids (writableName). A tag with no
// value is no item and is not named.
func group(tags []Tag) (grouped []writeItem, leftOut []string) {
	at := make(map[string]int, len(tags))
	skip := func(key string) {
		if !slices.Contains(leftOut, key) {
			leftOut = append(leftOut, key)
		}
	}
	for _, t := range tags {
		if t.Value == "" {
			continue
		}
		name := canonical(t.Key)
		if name == "" {
			skip(t.Key)
			continue
		}
		if i, ok := at[name]; ok {
			grouped[i].values = append(grouped[i].values, t.Value)
			continue
		}
		key := name
		if spelled, ok := apeSpelling[name]; ok {
			key = spelled
		}
		if !writableName(key) {
			skip(t.Key)
			continue
		}
		at[name] = len(grouped)
		grouped = append(grouped, writeItem{key: key, values: []string{t.Value}})
	}
	return grouped, leftOut
}

// LeftOut names the keys of tags that Build writes no item for, each once.
func LeftOut(tags []Tag) []string {
	_, leftOut := group(tags)
	return leftOut
}

// maxWriteBytes bounds a rendered tag. The engine passes a small descriptive
// set; a tag that would grow a file's trailer past this is refused rather than
// written, since there is no larger block to fall back on. Callers that project
// a source's tags onto an output fit them to the block first, so this
// refuses what a caller named, not what a file happened to carry.
const maxWriteBytes = 48 << 10

// Build renders tags as a whole APEv2 block: header, items, footer. It returns
// nil when nothing renders, which is the caller's signal to write no trailer at
// all rather than an empty one.
//
// A tag that does not fit the block is an error, not a skip. Skipping it writes
// a file missing metadata the caller asked to embed and reports success, so the
// loss reaches whoever reads the file back rather than whoever asked for it.
// The caller's answer is to trim the value and retry, which it can only do if
// it is told.
//
// The header is optional in the format and written anyway: a reader peeling
// trailers backward finds the footer's declared extent, and a header at the
// other end of it is what confirms that extent rather than trusting it. Size
// and StartsTag are the reading half of exactly that.
func Build(tags []Tag) ([]byte, error) {
	// An APEv2 key is unique within a tag: a multi-valued field is one item
	// whose values are NUL-separated, which is the form Parse reads back. One
	// item per value instead is not merely non-canonical but lossy, since a
	// reader looking the key up by name returns the first match and never sees
	// the rest. Grouping is by the canonical name, so two source spellings of
	// one field (YEAR and RECORDINGDATE) merge rather than emitting the
	// duplicate key the format forbids. First-seen order is kept, since map
	// iteration would break byte determinism.
	grouped, _ := group(tags)

	items, count := []byte(nil), uint32(0)
	for _, g := range grouped {
		value := strings.Join(g.values, "\x00")
		// Both preambles count: the block carries a header as well as a
		// footer, so a cap counting one of them lets the block run 32 bytes
		// past the size the refusal below quotes.
		//
		// The size reported is the whole block's, not this value's: an item
		// can overflow by being large or by being the one that crossed a
		// running total, and quoting a 30 KB value against a 48 KB cap reads
		// as a contradiction in the second case.
		if need := 2*FooterLen + len(items) + 9 + len(g.key) + len(value); need > maxWriteBytes {
			return nil, fmt.Errorf("%s does not fit: the APEv2 tag block would need %d bytes, over its %d-byte limit",
				g.key, need, maxWriteBytes)
		}
		items = binary.LittleEndian.AppendUint32(items, uint32(len(value)))
		items = binary.LittleEndian.AppendUint32(items, 0) // UTF-8 text
		items = append(items, g.key...)
		items = append(items, 0)
		items = append(items, value...)
		count++
	}
	if count == 0 {
		return nil, nil
	}
	// The size field covers the items and the footer, not the header, which is
	// what Size reads back.
	size := uint32(len(items) + FooterLen)
	out := preamble(size, count, flagHasHeader|flagIsHeader)
	out = append(out, items...)
	return append(out, preamble(size, count, flagHasHeader)...), nil
}

// preamble renders the 32-byte structure that opens and closes a tag; the two
// differ only in the header bit.
func preamble(size, count, flags uint32) []byte {
	b := make([]byte, 0, FooterLen)
	b = append(b, "APETAGEX"...)
	b = binary.LittleEndian.AppendUint32(b, 2000) // APEv2
	b = binary.LittleEndian.AppendUint32(b, size)
	b = binary.LittleEndian.AppendUint32(b, count)
	b = binary.LittleEndian.AppendUint32(b, flags)
	return append(b, make([]byte, 8)...) // reserved
}
