package container

import "time"

// Tag is one canonical metadata field for muxers that can embed tags in
// their stream form. Keys use the uppercase Vorbis/Picard vocabulary
// (TITLE, ARTIST, ALBUM, REPLAYGAIN_TRACK_GAIN, ...); each muxer maps
// the keys it can represent natively and skips the rest silently, since
// the caller decides what to offer and the format decides what it can
// hold. A multi-valued field repeats its key, one Tag per value.
type Tag struct {
	Key   string
	Value string
}

// Chapter is one chapter marker for muxers (and demuxers) that carry
// chapters. End is zero for start-only chapter forms (Nero chpl), which
// players read as "until the next chapter, or end of stream".
type Chapter struct {
	Start time.Duration
	End   time.Duration
	Title string
}

// Picture is embedded cover art for muxers that can embed it.
type Picture struct {
	MIME string
	Data []byte
}

// ValidTagKey reports whether key is legal as a Vorbis comment field
// name: printable ASCII 0x20 to 0x7D excluding '=', non-empty. The
// canonical vocabulary always passes; the check guards the muxers that
// write caller-supplied keys verbatim, where a stray '=' would corrupt
// the comment's key/value split and out-of-range bytes violate the
// spec. Muxers skip an invalid key rather than mangling it.
func ValidTagKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		if c := key[i]; c < 0x20 || c > 0x7D || c == '=' {
			return false
		}
	}
	return true
}
