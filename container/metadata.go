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

// Chapterer is implemented by demuxers that parse chapter markers, in the
// same idiom as Warner and Indexer: an honest capability gate rather than
// a method every demuxer carries, since a container with no chapter form
// has nothing to answer and does not implement it.
//
// Chapters is a field read, so asking is free: a demuxer that implements
// this resolved its chapters during the header parse. The parse itself is
// not free, and an mp4 chapter text track is why: its chapters live in a
// sample table, which costs one read each. That is the reason they are
// resolved once, with the header, rather than per caller.
type Chapterer interface {
	Chapters() []Chapter
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
