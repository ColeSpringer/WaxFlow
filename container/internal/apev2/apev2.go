// Package apev2 parses APEv2 tags: the key/value block WavPack and Monkey's
// Audio files carry, almost always appended after the audio. The demuxers for
// those formats both need to find the block (to keep it out of the audio
// stream) and to read it (to surface tags), so the two halves share one
// parser here.
//
// The flacn and mpa demuxers only ever need to recognize the block's extent,
// which they do inline while peeling trailers; they are deliberately not
// rewritten onto this package, since a recognizer that must not allocate is a
// different job from a parser.
package apev2

import (
	"bytes"
	"encoding/binary"
	"strings"
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
	// The size field covers the items plus this footer; a header, when
	// present, is an equally sized preamble the field does not count.
	total = int64(binary.LittleEndian.Uint32(f[12:16]))
	hasHeader = flags&flagHasHeader != 0
	if hasHeader {
		total += FooterLen
	}
	if total < FooterLen || total > maxTagBytes {
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
// picture needs an opt-in accessor rather than a field read.
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

	out := map[string][]string{}
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
		if flags&typeMask != 0 || size > maxValueBytes {
			continue // binary or locator payload, or a value too large to be text
		}
		name := canonical(string(key))
		if name == "" {
			continue
		}
		// A multi-valued item stores its values NUL-separated.
		for _, v := range strings.Split(string(value), "\x00") {
			if v != "" {
				out[name] = append(out[name], v)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// aliases maps the APEv2 spellings that differ from the canonical uppercase
// vocabulary onto it. Everything else passes through uppercased, which is
// already correct for the common fields and for REPLAYGAIN_*.
var aliases = map[string]string{
	"TRACK":        "TRACKNUMBER",
	"DISC":         "DISCNUMBER",
	"YEAR":         "RECORDINGDATE",
	"ALBUM ARTIST": "ALBUMARTIST",
	"RECORD DATE":  "RECORDINGDATE",
}

// apeSpelling is the write direction of aliases: the APEv2 spellings for the
// canonical keys whose names, not merely whose case, differ from it. Writing
// the canonical name instead puts the field where no reader looks -- ffmpeg's
// APEv2 converter lists exactly these four, and the reference tools and
// foobar2000 spell them the same way. Keys that differ only in case are left
// alone; readers fold case, and there is no evidence to spend a table on.
//
// RECORDINGDATE has two aliases pointing at it and only one way back: Year is
// the documented APEv2 key, and Record Date the rarer synonym.
var apeSpelling = map[string]string{
	"TRACKNUMBER":   "Track",
	"DISCNUMBER":    "Disc",
	"RECORDINGDATE": "Year",
	"ALBUMARTIST":   "Album Artist",
}

// canonical uppercases an item key and maps it onto the canonical vocabulary,
// returning "" for a key no muxer could write back. The accepted range is
// container.ValidTagKey's, restated rather than imported: this package sits
// under container and the predicate is three lines.
func canonical(key string) string {
	if key == "" || len(key) > 255 {
		return ""
	}
	for i := 0; i < len(key); i++ {
		if c := key[i]; c < 0x20 || c > 0x7d || c == '=' {
			return ""
		}
	}
	up := strings.ToUpper(key)
	if alias, ok := aliases[up]; ok {
		return alias
	}
	return up
}

// Tag is one item to write. It mirrors container.Tag without importing it, for
// the same reason canonical restates ValidTagKey: this package sits under
// container and the two fields cost less than the dependency.
type Tag struct {
	Key   string
	Value string
}

// maxWriteBytes bounds a rendered tag. The engine passes a small descriptive
// set; anything past this is dropped rather than growing a file's trailer
// without limit.
const maxWriteBytes = 48 << 10

// Build renders tags as a whole APEv2 block: header, items, footer. It returns
// nil when nothing renders, which is the caller's signal to write no trailer at
// all rather than an empty one.
//
// The header is optional in the format and written anyway: a reader peeling
// trailers backward finds the footer's declared extent, and a header at the
// other end of it is what confirms that extent rather than trusting it. Size
// and StartsTag are the reading half of exactly that.
func Build(tags []Tag) []byte {
	// An APEv2 key is unique within a tag: a multi-valued field is one item
	// whose values are NUL-separated, which is the form Parse reads back. One
	// item per value instead is not merely non-canonical but lossy, since a
	// reader looking the key up by name returns the first match and never sees
	// the rest. Grouping is by the canonical name, so two source spellings of
	// one field (YEAR and RECORDINGDATE) merge rather than emitting the
	// duplicate key the format forbids. First-seen order is kept, since map
	// iteration would break byte determinism.
	type item struct {
		key    string
		values []string
	}
	var grouped []item
	at := make(map[string]int, len(tags))
	for _, t := range tags {
		name := canonical(t.Key)
		if name == "" || t.Value == "" {
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
		at[name] = len(grouped)
		grouped = append(grouped, item{key: key, values: []string{t.Value}})
	}

	items, count := []byte(nil), uint32(0)
	for _, g := range grouped {
		value := strings.Join(g.values, "\x00")
		if len(items)+FooterLen+9+len(g.key)+len(value) > maxWriteBytes {
			// Skip just the item that does not fit: one oversized value must
			// not erase the small descriptive tags after it.
			continue
		}
		items = binary.LittleEndian.AppendUint32(items, uint32(len(value)))
		items = binary.LittleEndian.AppendUint32(items, 0) // UTF-8 text
		items = append(items, g.key...)
		items = append(items, 0)
		items = append(items, value...)
		count++
	}
	if count == 0 {
		return nil
	}
	// The size field covers the items and the footer, not the header, which is
	// what Size reads back.
	size := uint32(len(items) + FooterLen)
	out := preamble(size, count, flagHasHeader|flagIsHeader)
	out = append(out, items...)
	return append(out, preamble(size, count, flagHasHeader)...)
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
