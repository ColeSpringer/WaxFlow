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
