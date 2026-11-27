package container

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"
)

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
// chapters. End is zero for the start-only chapter forms (Musepack SV8
// chapter packets, ASF markers, the last chapter of a Nero chpl), which
// players read as "until the next chapter, or end of stream".
type Chapter struct {
	Start time.Duration
	End   time.Duration
	Title string
}

// SortChapters puts chs in the order Chapterer promises: by start, stably, so
// chapters sharing a start keep the order their container stored them in.
// Every demuxer with a chapter form sorts through it, since a container may
// let a writer leave its list unsorted.
func SortChapters(chs []Chapter) {
	slices.SortStableFunc(chs, func(a, b Chapter) int { return cmp.Compare(a.Start, b.Start) })
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
//
// Chapters are in start order, stable where starts tie, so a caller may
// take chapter n's end from chapter n+1's start; a demuxer sorts what its
// container let a writer leave unsorted. The slice is the demuxer's own and
// must not be mutated; format.Info hands it on to read-only consumers, the
// same rule as Tags.
type Chapterer interface {
	Chapters() []Chapter
}

// Tagger is implemented by demuxers that parse embedded tags, the same
// capability gate as Chapterer and for the same reason: a container with
// no tag form has nothing to answer and does not implement it.
//
// Tags is a field read, so asking is free: a demuxer that implements this
// resolved its tags during the header parse. Keys use the same canonical
// uppercase vocabulary Tag does, values in file order.
//
// Cover art is deliberately not here. A picture is megabytes, so it cannot
// be held per demuxer on the streaming path; serving it needs an opt-in
// accessor rather than a field read, which is the same reason
// meta.ReadOptions.Pictures exists.
//
// This is not dead code, though it can read that way: container/mp4,
// container/wv, and container/apen implement it, and the tag library reads
// every format WaxFlow decodes, so on a mapper-wired route the fold rarely has
// anything left to add. What it still serves is an embedder that wires no
// mapper at all, where it is the only tag source there is.
//
// Keep the keys a mapper would also produce. Nothing enforces it: the fold
// gives the mapper priority per key, so an atom exposed here under a spelling
// the mapper projects differently surfaces as two tags rather than one.
type Tagger interface {
	Tags() map[string][]string
}

// Picture is embedded cover art for muxers that can embed it.
type Picture struct {
	MIME string
	Data []byte
}

// ValidTagKey reports whether key is legal as a Vorbis comment field
// name: printable ASCII 0x20 to 0x7D excluding '=', non-empty, with no
// space at either edge. The canonical vocabulary always passes; the check
// guards the muxers that write caller-supplied keys verbatim, where a
// stray '=' would corrupt the comment's key/value split and out-of-range
// bytes violate the spec. An edge space is legal there and still refused:
// no key is spelled with one, so a reader either trims it onto another
// key's field or reads no key at all. Muxers skip an invalid key rather
// than mangling it.
func ValidTagKey(key string) bool {
	if key == "" || key[0] == ' ' || key[len(key)-1] == ' ' {
		return false
	}
	for i := 0; i < len(key); i++ {
		if c := key[i]; c < 0x20 || c > 0x7D || c == '=' {
			return false
		}
	}
	return true
}

// SplitNumberTotal splits a track or disc number that holds its total too:
// "3/12", or "3 of 12" as some taggers write it, the word in any case and
// with or without spaces around it. Each side is an integer or blank, and
// not both blank; they come back trimmed and otherwise as written, leading
// zeros included. Any other value comes back whole as num, with ok false,
// so text that merely holds a slash or the word ("Part 3 of 12") stays one
// value.
func SplitNumberTotal(v string) (num, total string, ok bool) {
	n, t, cut := strings.Cut(v, "/")
	if !cut {
		// A side of a pair holds no letters, so only the first "of" can
		// separate one.
		for i := 0; i+1 < len(v); i++ {
			if (v[i] == 'o' || v[i] == 'O') && (v[i+1] == 'f' || v[i+1] == 'F') {
				n, t, cut = v[:i], v[i+2:], true
				break
			}
		}
	}
	n, t = strings.TrimSpace(n), strings.TrimSpace(t)
	side := func(s string) bool {
		_, err := strconv.Atoi(s)
		return s == "" || err == nil
	}
	if !cut || n == "" && t == "" || !side(n) || !side(t) {
		return v, "", false
	}
	return n, t, true
}

// SplitNumberPairs splits each track or disc number in tags that holds its
// total (SplitNumberTotal) into the number and total keys, as the tag
// library does on a read. seen reports how many values the library's read of
// the same file holds under a key, counted by the library's rules, so a
// demuxer never derives a total the library does not: a number splits only
// when seen is one and tags holds that one value, a number side left blank
// drops the key, and the derived total is set only when seen is zero for it.
//
// A negative seen is a count the demuxer could not take. Then nothing is
// derived, and only a single value with a blank number side ("/12") goes,
// since the library either drops that key too or holds it with more values.
func SplitNumberPairs(tags map[string][]string, seen func(key string) int) {
	for _, keys := range [...][2]string{{"TRACKNUMBER", "TRACKTOTAL"}, {"DISCNUMBER", "DISCTOTAL"}} {
		vals := tags[keys[0]]
		n := seen(keys[0])
		if len(vals) != 1 || n != 1 && n >= 0 {
			continue
		}
		num, total, ok := SplitNumberTotal(vals[0])
		if !ok {
			continue
		}
		if n < 0 {
			if num == "" {
				delete(tags, keys[0])
			}
			continue
		}
		if num != "" {
			tags[keys[0]] = []string{num}
		} else {
			delete(tags, keys[0])
		}
		if _, has := tags[keys[1]]; total != "" && !has && seen(keys[1]) == 0 {
			tags[keys[1]] = []string{total}
		}
	}
}
