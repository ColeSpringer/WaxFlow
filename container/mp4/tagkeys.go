package mp4

import (
	"strings"

	"github.com/colespringer/waxflow/container"
)

// The MP4 tag vocabulary, shared by the reader and the muxer: which atom,
// freeform name or keys-store name spells which canonical key. It mirrors
// waxlabel's MP4 mapping row for row, so the demuxer's keys are ones the tag
// library also gives for the same file, and what the muxer writes reads back
// there under the key it was handed. Mirror both when either moves.

// ilstTextKeys maps the four-character text atoms onto canonical keys.
var ilstTextKeys = map[string]string{
	"\xa9nam": "TITLE",
	"\xa9ART": "ARTIST",
	"aART":    "ALBUMARTIST",
	"\xa9alb": "ALBUM",
	"\xa9wrt": "COMPOSER",
	"\xa9day": "RECORDINGDATE",
	"\xa9cmt": "COMMENT",
	"\xa9gen": "GENRE",
	"\xa9too": "ENCODER",
	"cprt":    "COPYRIGHT",
	"\xa9grp": "GROUPING",
	"\xa9lyr": "LYRICS",
	"desc":    "DESCRIPTION",
	"ldes":    "LONGDESCRIPTION",
	"soal":    "ALBUMSORT",
	"soaa":    "ALBUMARTISTSORT",
	"soar":    "ARTISTSORT",
	"sonm":    "TITLESORT",
	"soco":    "COMPOSERSORT",
	"\xa9wrk": "WORK",
	"\xa9mvn": "MOVEMENTNAME",
	"\xa9enc": "ENCODEDBY",
	"tvsh":    "SHOW",
	"tven":    "EPISODEID",
	"tvnn":    "NETWORK",
	"sosn":    "SHOWSORT",
}

// udtaTextKeys are the text atoms only a classic QuickTime udta holds;
// ilstTextKeys is consulted first there.
var udtaTextKeys = map[string]string{
	"\xa9swr": "ENCODER",
	"\xa9inf": "COMMENT",
}

// freeformKeys maps com.apple.iTunes freeform names onto canonical keys, in
// the spelling that is written.
var freeformKeys = map[string]string{
	"MusicBrainz Track Id":              "MUSICBRAINZ_TRACKID",
	"MusicBrainz Release Track Id":      "MUSICBRAINZ_RELEASETRACKID",
	"MusicBrainz Album Id":              "MUSICBRAINZ_ALBUMID",
	"MusicBrainz Release Group Id":      "MUSICBRAINZ_RELEASEGROUPID",
	"MusicBrainz Artist Id":             "MUSICBRAINZ_ARTISTID",
	"MusicBrainz Album Artist Id":       "MUSICBRAINZ_ALBUMARTISTID",
	"MusicBrainz Work Id":               "MUSICBRAINZ_WORKID",
	"MusicBrainz Disc Id":               "MUSICBRAINZ_DISCID",
	"Acoustid Id":                       "ACOUSTID_ID",
	"Acoustid Fingerprint":              "ACOUSTID_FINGERPRINT",
	"replaygain_track_gain":             "REPLAYGAIN_TRACK_GAIN",
	"replaygain_track_peak":             "REPLAYGAIN_TRACK_PEAK",
	"replaygain_album_gain":             "REPLAYGAIN_ALBUM_GAIN",
	"replaygain_album_peak":             "REPLAYGAIN_ALBUM_PEAK",
	"BARCODE":                           "BARCODE",
	"CATALOGNUMBER":                     "CATALOGNUMBER",
	"LABEL":                             "LABEL",
	"MEDIA":                             "MEDIA",
	"ISRC":                              "ISRC",
	"originaldate":                      "ORIGINALDATE",
	"NARRATOR":                          "NARRATOR",
	"LYRICIST":                          "LYRICIST",
	"MusicBrainz Album Release Country": "RELEASECOUNTRY",
	"MusicBrainz Album Status":          "RELEASESTATUS",
	"MusicBrainz Album Type":            "RELEASETYPE",
	"PRODUCER":                          "PRODUCER",
	"ENGINEER":                          "ENGINEER",
	"MIXER":                             "MIXER",
	"ARRANGER":                          "ARRANGER",
	"WRITER":                            "WRITER",
	"DJMIXER":                           "DJMIXER",
}

// atomOnlyKeys are the keys an integer or text atom stores, which a freeform
// of the key's own name also reads as.
var atomOnlyKeys = []string{
	"ITUNESADVISORY", "ITUNESGAPLESS", "SHOWMOVEMENT", "BPM",
	"WORK", "MOVEMENTNAME", "MOVEMENT", "MOVEMENTTOTAL", "ENCODEDBY",
}

// mdtaKeys maps the bare names of a keys store onto canonical keys: Apple's,
// and the names ffmpeg gives the iTunes atoms.
var mdtaKeys = map[string]string{
	"title":             "TITLE",
	"artist":            "ARTIST",
	"albumartist":       "ALBUMARTIST",
	"album":             "ALBUM",
	"composer":          "COMPOSER",
	"comment":           "COMMENT",
	"description":       "DESCRIPTION",
	"synopsis":          "LONGDESCRIPTION",
	"genre":             "GENRE",
	"encoder":           "ENCODER",
	"software":          "ENCODER",
	"copyright":         "COPYRIGHT",
	"date":              "RECORDINGDATE",
	"creationdate":      "RECORDINGDATE",
	"publisher":         "LABEL",
	"grouping":          "GROUPING",
	"lyrics":            "LYRICS",
	"sort_name":         "TITLESORT",
	"sort_artist":       "ARTISTSORT",
	"sort_album":        "ALBUMSORT",
	"sort_album_artist": "ALBUMARTISTSORT",
	"sort_composer":     "COMPOSERSORT",
	"sort_show":         "SHOWSORT",
	"compilation":       "COMPILATION",
	"gapless_playback":  "ITUNESGAPLESS",
	"media_type":        "MEDIATYPE",
	"rating":            "ITUNESADVISORY",
	"tmpo":              "BPM",
	"show":              "SHOW",
	"episode_id":        "EPISODEID",
	"network":           "NETWORK",
	"episode_sort":      "EPISODENUMBER",
	"season_number":     "SEASONNUMBER",
}

// mdtaPrefix is stripped from a keys-store name so Apple's and ffmpeg's
// spellings of one key match.
const mdtaPrefix = "com.apple.quicktime."

// keyAliases folds alternate spellings onto canonical keys.
var keyAliases = map[string]string{
	"DATE":                    "RECORDINGDATE",
	"YEAR":                    "RECORDINGDATE",
	"ORIGINALYEAR":            "ORIGINALDATE",
	"TOTALTRACKS":             "TRACKTOTAL",
	"TRACKTOTAL":              "TRACKTOTAL",
	"TOTALDISCS":              "DISCTOTAL",
	"DISCTOTAL":               "DISCTOTAL",
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

var (
	// textAtomOf and freeformNameOf are the write direction of the two tables.
	textAtomOf     = map[string]string{}
	freeformNameOf = map[string]string{}
	// freeformFold is the case fold of freeformKeys, for reading.
	freeformFold = map[string]string{}
)

func init() {
	for atom, key := range ilstTextKeys {
		textAtomOf[key] = atom
	}
	for name, key := range freeformKeys {
		freeformNameOf[key] = name
		freeformFold[normalizeName(name)] = key
	}
	for _, key := range atomOnlyKeys {
		freeformFold[key] = key
	}
}

// normalizeName trims and uppercases a native tag name for lookup.
// Separators are kept: an underscore is not a space.
func normalizeName(name string) string {
	return strings.ToUpper(strings.TrimSpace(name))
}

// aliasKey returns the canonical key an alternate spelling names.
func aliasKey(name string) (string, bool) {
	k, ok := keyAliases[strings.ToUpper(name)]
	return k, ok
}

// freeformKey returns the canonical key for a freeform name: the table's,
// then its case fold, then an alias spelling, then the name itself when it
// is already a key.
func freeformKey(name string) (string, bool) {
	if k, ok := freeformKeys[name]; ok {
		return k, true
	}
	norm := normalizeName(name)
	if k, ok := freeformFold[norm]; ok {
		return k, true
	}
	if k, ok := aliasKey(norm); ok {
		return k, true
	}
	return name, canonicalKey(name)
}

// mdtaKey returns the canonical key for a keys-store name, less mdtaPrefix:
// the table's as written, then an alias spelling, then the name folded to a
// custom key.
func mdtaKey(name string) (string, bool) {
	bare := strings.TrimPrefix(name, mdtaPrefix)
	if k, ok := mdtaKeys[bare]; ok {
		return k, true
	}
	if k, ok := aliasKey(bare); ok {
		return k, true
	}
	return foldKey(bare)
}

// canonicalKey reports whether key is spelled as a canonical key is: a
// legal tag key with no lowercase letter.
func canonicalKey(key string) bool {
	return container.ValidTagKey(key) && key == strings.ToUpper(key)
}

// foldKey folds a foreign name to a custom key: ASCII letters uppercased and
// every byte a key cannot hold replaced by an underscore. ok is false for a
// name that is blank or folds to no legal key.
func foldKey(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteByte(byte(r) - ('a' - 'A'))
		case r >= 0x20 && r <= 0x7D && r != '=':
			b.WriteByte(byte(r))
		default:
			b.WriteByte('_')
		}
	}
	return b.String(), true
}
