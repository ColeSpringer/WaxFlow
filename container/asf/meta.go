package asf

import (
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/colespringer/waxflow/container"
)

// descriptorKeys maps descriptor names onto the canonical uppercase
// vocabulary, keyed by the name trimmed and lowercased, since ASF names
// compare without case. It and descriptorKey read every name as the tag
// library does (its internal/mapping/asf.go), because both readers speak for
// a .wma source and a field the two key differently arrives twice. An empty
// key is a name neither reads: a GUID, the encoder's bookkeeping, the
// zero-based WM/Track beside WM/TrackNumber.
//
// WM/ToolName and WM/EncodingSettings are the one departure: the library
// reads them as ENCODER, but ffmpeg stamps the setting on every file it
// writes, so an untagged file would not read as one here.
var descriptorKeys = map[string]string{
	"title":                      "TITLE",
	"author":                     "ARTIST",
	"copyright":                  "COPYRIGHT",
	"description":                "COMMENT",
	"wm/albumtitle":              "ALBUM",
	"wm/albumartist":             "ALBUMARTIST",
	"wm/composer":                "COMPOSER",
	"wm/writer":                  "LYRICIST",
	"wm/conductor":               "CONDUCTOR",
	"wm/producer":                "PRODUCER",
	"wm/engineer":                "ENGINEER",
	"wm/mixer":                   "MIXER",
	"wm/modifiedby":              "REMIXER",
	"wm/genre":                   "GENRE",
	"wm/year":                    "RECORDINGDATE",
	"wm/originalreleaseyear":     "ORIGINALDATE",
	"wm/originalreleasetime":     "ORIGINALDATE",
	"wm/tracknumber":             "TRACKNUMBER",
	"wm/partofset":               "DISCNUMBER",
	"wm/setsubtitle":             "DISCSUBTITLE",
	"wm/publisher":               "LABEL",
	"wm/isrc":                    "ISRC",
	"wm/barcode":                 "BARCODE",
	"wm/catalogno":               "CATALOGNUMBER",
	"wm/media":                   "MEDIA",
	"wm/lyrics":                  "LYRICS",
	"wm/contentgroupdescription": "GROUPING",
	"wm/encodedby":               "ENCODEDBY",
	"wm/toolname":                "",
	"wm/encodingsettings":        "",
	"wm/beatsperminute":          "BPM",
	"wm/titlesortorder":          "TITLESORT",
	"wm/artistsortorder":         "ARTISTSORT",
	"wm/albumsortorder":          "ALBUMSORT",
	"wm/albumartistsortorder":    "ALBUMARTISTSORT",
	"wm/composersortorder":       "COMPOSERSORT",
	"wm/iscompilation":           "COMPILATION",
	"wm/mediaclassprimaryid":     "",
	"wm/mediaclasssecondaryid":   "",
	"wm/wmcollectiongroupid":     "",
	"wm/wmcollectionid":          "",
	"wm/wmcontentid":             "",
	"wm/uniquefileidentifier":    "",
	"wm/provider style":          "",
	"wm/encodingtime":            "",
	"wm/mcdi":                    "",
	"wm/track":                   "",
	"rating":                     "",
	"aspectratiox":               "",
	"aspectratioy":               "",
	"isvbr":                      "",
	"wmfsdkversion":              "",
	"wmfsdkneeded":               "",
	"deviceconformancetemplate":  "",
	"buffer average":             "",
	"vbr peak":                   "",
	// A cover, which the library reads as a picture whatever its type.
	"wm/picture": "",

	"musicbrainz/album id":              "MUSICBRAINZ_ALBUMID",
	"musicbrainz/release group id":      "MUSICBRAINZ_RELEASEGROUPID",
	"musicbrainz/track id":              "MUSICBRAINZ_TRACKID",
	"musicbrainz/release track id":      "MUSICBRAINZ_RELEASETRACKID",
	"musicbrainz/work id":               "MUSICBRAINZ_WORKID",
	"musicbrainz/disc id":               "MUSICBRAINZ_DISCID",
	"musicbrainz/artist id":             "MUSICBRAINZ_ARTISTID",
	"musicbrainz/album artist id":       "MUSICBRAINZ_ALBUMARTISTID",
	"musicbrainz/album status":          "RELEASESTATUS",
	"musicbrainz/album type":            "RELEASETYPE",
	"musicbrainz/album release country": "RELEASECOUNTRY",
	"acoustid/id":                       "ACOUSTID_ID",
	"acoustid/fingerprint":              "ACOUSTID_FINGERPRINT",
	"replaygain_track_gain":             "REPLAYGAIN_TRACK_GAIN",
	"replaygain_track_peak":             "REPLAYGAIN_TRACK_PEAK",
	"replaygain_album_gain":             "REPLAYGAIN_ALBUM_GAIN",
	"replaygain_album_peak":             "REPLAYGAIN_ALBUM_PEAK",
}

// sharedAliases is the library's alias table every codec reads through
// (tag/aliases.go), the spellings whose key is not the name uppercased. A
// name with no row of its own is folded through it.
var sharedAliases = map[string]string{
	"DATE":                    "RECORDINGDATE",
	"YEAR":                    "RECORDINGDATE",
	"ORIGINALYEAR":            "ORIGINALDATE",
	"TOTALTRACKS":             "TRACKTOTAL",
	"TOTALDISCS":              "DISCTOTAL",
	"ORGANIZATION":            "LABEL",
	"UNSYNCEDLYRICS":          "LYRICS",
	"USLT":                    "LYRICS",
	"DISC":                    "DISCNUMBER",
	"TRACK":                   "TRACKNUMBER",
	"ALBUM ARTIST":            "ALBUMARTIST",
	"ALBUM_ARTIST":            "ALBUMARTIST",
	"DJ MIXER":                "DJMIXER",
	"DJ_MIXER":                "DJMIXER",
	"DJ-MIXER":                "DJMIXER",
	"MUSICBRAINZ_ALBUMSTATUS": "RELEASESTATUS",
	"MUSICBRAINZ_ALBUMTYPE":   "RELEASETYPE",
	"LEAD_PERFORMER":          "ARTIST",
	"DATE_RECORDED":           "RECORDINGDATE",
	"DATE_RELEASED":           "RELEASEDATE",
	"DATE_RELEASE":            "RELEASEDATE",
	"DATE_ORIGINAL":           "ORIGINALDATE",
	"ORIGINAL_DATE":           "ORIGINALDATE",
	"ENCODED_BY":              "ENCODEDBY",
	"PART_NUMBER":             "TRACKNUMBER",
	"TOTAL_PARTS":             "TRACKTOTAL",
	"TOTAL_DISCS":             "DISCTOTAL",
	"CATALOG_NUMBER":          "CATALOGNUMBER",
	"PUBLISHER":               "LABEL",
	"REMIXED_BY":              "REMIXER",
	"CONTENT_GROUP":           "GROUPING",
}

// descriptorKey is the canonical key a descriptor name reads as, or "" for
// none. A name with no row is keyed by its text after the first "/", folded
// through sharedAliases, so WM/Provider is PROVIDER: the distributor, not the
// label. A byte past ASCII keys nothing, where the library would fold some
// through Unicode case mapping.
func descriptorKey(name string) string {
	norm := strings.TrimSpace(name)
	if hasNonASCII(norm) {
		return ""
	}
	return resolveName(strings.ToLower(norm))
}

// resolveName keys a trimmed, lowercased ASCII name.
func resolveName(norm string) string {
	if key, ok := descriptorKeys[norm]; ok {
		return key
	}
	if _, bare, ok := strings.Cut(norm, "/"); ok {
		norm = strings.TrimSpace(bare)
	}
	key := strings.ToUpper(norm)
	if alias, ok := sharedAliases[key]; ok {
		return alias
	}
	if !container.ValidTagKey(key) {
		return ""
	}
	return key
}

// foldsOntoPair reports a name past ASCII that the library's Unicode case
// folding reads as a number or total key, which descriptorKey refuses.
func foldsOntoPair(name string) bool {
	t := strings.TrimSpace(name)
	if !hasNonASCII(t) {
		return false
	}
	for _, f := range [...]string{strings.ToLower(t), strings.ToUpper(t)} {
		if !hasNonASCII(f) && pairKeys[resolveName(strings.ToLower(f))] {
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

var pairKeys = map[string]bool{"TRACKNUMBER": true, "TRACKTOTAL": true, "DISCNUMBER": true, "DISCTOTAL": true}

// pairSeen gathers what the tag library's read of the header holds under the
// number and total keys, which decides its number split: the distinct values
// as written, since it compares them untrimmed, up to two a key. blind is a
// read the demuxer could not follow, a count it reports as unknown
// (container.SplitNumberPairs). It counts every value the library reads,
// whether or not addTag keeps it.
type pairSeen struct {
	vals  map[string][]string
	blind bool
}

func (p *pairSeen) note(key, raw string) {
	if !pairKeys[key] || raw == "" || len(p.vals[key]) == 2 || slices.Contains(p.vals[key], raw) {
		return
	}
	if p.vals == nil {
		p.vals = map[string][]string{}
	}
	p.vals[key] = append(p.vals[key], raw)
}

func (p *pairSeen) count(key string) int {
	if p.blind {
		return -1
	}
	return len(p.vals[key])
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
	for range count {
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
	key := descriptorKey(name)
	if key == "" {
		if d.counted && foldsOntoPair(name) {
			d.pairs.blind = true
		}
		return
	}
	raw, ok := descriptorText(valType, value)
	if !ok {
		return
	}
	if d.counted {
		d.pairs.note(key, raw)
	}
	if valType == valueUnicode && len(value) > maxTagBytes {
		return
	}
	d.addTag(key, strings.TrimSpace(raw))
}

// descriptorText renders a descriptor value as the tag library does, before
// any trim: text up to its NUL, numbers in decimal, and a boolean as "1" or
// "0" from the 32 bits the specification gives it or the 16 some writers use.
// Byte arrays and types the specification does not name have no text.
func descriptorText(valType uint16, value []byte) (string, bool) {
	switch valType {
	case valueUnicode:
		return utf16String(value), true
	case valueBool:
		var n uint32
		switch len(value) {
		case 2:
			n = uint32(le.Uint16(value))
		case 4:
			n = le.Uint32(value)
		default:
			return "", false
		}
		if n != 0 {
			return "1", true
		}
		return "0", true
	case valueDWord:
		if len(value) >= 4 {
			return strconv.FormatUint(uint64(le.Uint32(value)), 10), true
		}
	case valueQWord:
		if len(value) >= 8 {
			return strconv.FormatUint(le.Uint64(value), 10), true
		}
	case valueWord:
		if len(value) >= 2 {
			return strconv.FormatUint(uint64(le.Uint16(value)), 10), true
		}
	}
	return "", false
}

// splitNumberPairs splits a track or disc number that holds its total once
// every tag object is read, where the library's read splits it.
func (d *Demuxer) splitNumberPairs() {
	if d.tags != nil {
		container.SplitNumberPairs(d.tags, d.pairs.count)
	}
}

// addTag records one canonical tag value, in file order. A value already
// present under the same key is dropped rather than repeated: ffmpeg's ASF
// muxer writes the title, author, and copyright into both tag objects, and a
// multi-valued field is not what that duplication means. The tags kept are
// bounded in keys, values a key and text over the whole header, and the
// first value a bound turns away is noted once.
func (d *Demuxer) addTag(key, value string) {
	if key == "" || value == "" || len(value) > maxTagBytes || !container.ValidTagKey(key) {
		return
	}
	kv := [2]string{key, value}
	if d.tagSeen[kv] {
		return
	}
	vals, has := d.tags[key]
	if !has && len(d.tags) >= maxTags || len(vals) >= maxTagValues || d.tagText+len(value) > maxTagText {
		if !d.tagsCut {
			d.tagsCut = true
			d.note(-1, "the header holds more tags than the %d keys, %d values a key or %d bytes of text read from it", maxTags, maxTagValues, maxTagText)
		}
		return
	}
	if d.tags == nil {
		d.tags = make(map[string][]string)
		d.tagSeen = make(map[[2]string]bool)
	}
	d.tags[key] = append(vals, value)
	d.tagSeen[kv] = true
	d.tagText += len(value)
}

// utf16String decodes a UTF-16LE string up to the trailing NUL every ASF
// writer includes in the declared length, verbatim. An odd trailing byte is
// ignored, and unpaired surrogates come back as the replacement rune, so the
// result is always valid UTF-8. A marker description is read through it: a
// chapter title is copied as it stands, so a reader of the same file through
// the tag library or ffprobe sees the same title.
func utf16String(b []byte) string {
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u := le.Uint16(b[i:])
		if u == 0 {
			break
		}
		units = append(units, u)
	}
	return string(utf16.Decode(units))
}

// utf16Text is utf16String for a tag value: one past the tag cap is dropped,
// and the text is trimmed, since a tagger's padding is not part of a field.
func utf16Text(b []byte) string {
	if len(b) > maxTagBytes {
		return ""
	}
	return strings.TrimSpace(utf16String(b))
}
