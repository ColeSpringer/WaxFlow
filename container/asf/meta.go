package asf

import (
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/colespringer/waxflow/container"
)

// descriptorKeys maps the Extended Content Description names onto the
// canonical uppercase vocabulary, the same table shape mp4's ilstTextKeys has.
// Windows Media spells its own fields "WM/..."; ffmpeg's ASF muxer writes a
// handful of bare lowercase names alongside them, and both appear in files in
// circulation, so both are listed.
var descriptorKeys = map[string]string{
	"WM/AlbumTitle":              "ALBUM",
	"WM/AlbumArtist":             "ALBUMARTIST",
	"WM/Composer":                "COMPOSER",
	"WM/Genre":                   "GENRE",
	"WM/Year":                    "RECORDINGDATE",
	"WM/Publisher":               "LABEL",
	"WM/Lyrics":                  "LYRICS",
	"WM/ISRC":                    "ISRC",
	"WM/EncodedBy":               "ENCODEDBY",
	"WM/Provider":                "ORGANIZATION",
	"WM/OriginalAlbumTitle":      "ORIGINALALBUM",
	"WM/AuthorURL":               "WEBSITE",
	"title":                      "TITLE",
	"Author":                     "ARTIST",
	"Description":                "COMMENT",
	"copyright":                  "COPYRIGHT",
	"date":                       "RECORDINGDATE",
	"WM/ContentGroupDescription": "GROUPING",
}

// numberPairKeys are the descriptors that carry an "n" or "n/total" pair,
// split across two canonical keys the way mp4 splits trkn and disk.
var numberPairKeys = map[string][2]string{
	"WM/TrackNumber": {"TRACKNUMBER", "TRACKTOTAL"},
	"WM/PartOfSet":   {"DISCNUMBER", "DISCTOTAL"},
	"track":          {"TRACKNUMBER", "TRACKTOTAL"},
	"disc":           {"DISCNUMBER", "DISCTOTAL"},
}

// Descriptor value data types, from the Extended Content Description Object.
const (
	valueUnicode = 0
	valueBytes   = 1
	valueBool    = 2
	valueDWord   = 3
	valueQWord   = 4
	valueWord    = 5
)

// parseContentDescription reads the five fixed fields of the Content
// Description Object. It is the older of the two tag forms and holds only
// these; writers that fill both put the same strings in each, which the
// dedupe in addTag collapses.
func (d *Demuxer) parseContentDescription(body []byte) {
	if len(body) < 10 {
		return
	}
	keys := [5]string{"TITLE", "ARTIST", "COPYRIGHT", "COMMENT", "RATING"}
	var lens [5]int
	total := 10
	for i := range lens {
		lens[i] = int(le.Uint16(body[i*2:]))
		total += lens[i]
	}
	if total > len(body) {
		return
	}
	p := 10
	for i, n := range lens {
		if keys[i] != "RATING" {
			d.addTag(keys[i], utf16Text(body[p:p+n]))
		}
		p += n
	}
}

// parseExtendedContentDescription reads the descriptor list: the WM/... fields
// most taggers write. A descriptor that does not fit is where the walk stops,
// since the list is length-prefixed and cannot be resynchronized.
func (d *Demuxer) parseExtendedContentDescription(body []byte) {
	if len(body) < 2 {
		return
	}
	count := int(le.Uint16(body))
	p := 2
	for i := 0; i < count; i++ {
		if p+2 > len(body) {
			return
		}
		nameLen := int(le.Uint16(body[p:]))
		p += 2
		if nameLen > len(body)-p {
			return
		}
		name := utf16Text(body[p : p+nameLen])
		p += nameLen
		if p+4 > len(body) {
			return
		}
		valType := le.Uint16(body[p:])
		valLen := int(le.Uint16(body[p+2:]))
		p += 4
		if valLen > len(body)-p {
			return
		}
		value := body[p : p+valLen]
		p += valLen
		d.addDescriptor(name, valType, value)
	}
}

// addDescriptor maps one descriptor onto the canonical vocabulary. Byte arrays
// are dropped: the only one that matters is WM/Picture, and cover art is not
// what Tagger serves (see container.Tagger).
func (d *Demuxer) addDescriptor(name string, valType uint16, value []byte) {
	text := ""
	switch valType {
	case valueUnicode:
		text = utf16Text(value)
	case valueDWord:
		if len(value) >= 4 {
			text = strconv.FormatUint(uint64(le.Uint32(value)), 10)
		}
	case valueQWord:
		if len(value) >= 8 {
			text = strconv.FormatUint(le.Uint64(value), 10)
		}
	case valueWord:
		if len(value) >= 2 {
			text = strconv.FormatUint(uint64(le.Uint16(value)), 10)
		}
	case valueBool, valueBytes:
		return
	default:
		return
	}
	if text == "" {
		return
	}
	if pair, ok := numberPairKeys[name]; ok {
		num, total, _ := strings.Cut(text, "/")
		d.addTag(pair[0], strings.TrimSpace(num))
		d.addTag(pair[1], strings.TrimSpace(total))
		return
	}
	if key, ok := descriptorKeys[name]; ok {
		d.addTag(key, text)
	}
}

// addTag records one canonical tag value, in file order. A value already
// present under the same key is dropped rather than repeated: ffmpeg's ASF
// muxer writes the title, author, and copyright into both tag objects, and a
// multi-valued field is not what that duplication means.
func (d *Demuxer) addTag(key, value string) {
	if key == "" || value == "" || len(value) > maxTagBytes || len(d.tags) >= maxTags {
		return
	}
	if !container.ValidTagKey(key) {
		return
	}
	for _, v := range d.tags[key] {
		if v == value {
			return
		}
	}
	if d.tags == nil {
		d.tags = make(map[string][]string)
	}
	d.tags[key] = append(d.tags[key], value)
}

// utf16Text decodes a UTF-16LE string, dropping the trailing NUL every ASF
// writer includes in the declared length. An odd trailing byte is ignored, and
// unpaired surrogates come back as the replacement rune, so the result is
// always valid UTF-8.
func utf16Text(b []byte) string {
	if len(b) > maxTagBytes {
		return ""
	}
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u := le.Uint16(b[i:])
		if u == 0 {
			break
		}
		units = append(units, u)
	}
	s := string(utf16.Decode(units))
	if !utf8.ValidString(s) {
		return ""
	}
	return strings.TrimSpace(s)
}
