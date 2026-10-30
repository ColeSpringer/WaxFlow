// Package m3u8 writes and parses M3U8 playlists, the text half of an HLS
// presentation: a master playlist lists the variants, a media playlist one
// variant's segments.
//
// Public since 2026-10, having been internal first (ADR-0002). A host that
// renders a presentation through the exported segmenter writes the daemon's
// playlist text with it, from the rows waxflow.SegmentPlan.MediaSegments and
// MasterVariant give.
//
// M3U8 has no escape, so Master and Media refuse a value the text cannot
// carry rather than write a playlist that reads back as something else, and
// what they write ParseMaster and ParseMedia read back as given, durations to
// the five decimals EXTINF is written with. A URI is written verbatim, so it
// must already be RFC 3986's characters: percent-encoding is the caller's job.
package m3u8

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/colespringer/waxflow/waxerr"
)

// MasterVariant is one rung of the master playlist's ladder.
type MasterVariant struct {
	// URI is the media playlist URL (relative, query included).
	URI string
	// Bandwidth is the peak bandwidth bound in bits per second.
	Bandwidth int
	// Codecs is the RFC 6381 CODECS attribute value; empty omits it.
	Codecs string
}

// Master renders the master playlist. EXT-X-VERSION 7 is the CMAF/fMP4
// floor (EXT-X-MAP in the media playlists). It refuses an empty ladder, a
// negative bandwidth, and a URI or CODECS value the text cannot carry.
func Master(variants []MasterVariant) (string, error) {
	if len(variants) == 0 {
		return "", refuse("a master playlist lists at least one variant")
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	for i, v := range variants {
		if f := lineFault(v.URI); f != "" {
			return "", refuse("variant %d URI %q %s", i, v.URI, f)
		}
		if f := textFault(v.Codecs); f != "" {
			return "", refuse("variant %d CODECS %q %s", i, v.Codecs, f)
		}
		if v.Bandwidth < 0 {
			return "", refuse("variant %d BANDWIDTH %d is negative", i, v.Bandwidth)
		}
		codecs := ""
		if v.Codecs != "" {
			codecs = `,CODECS="` + v.Codecs + `"`
		}
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d%s\n%s\n", v.Bandwidth, codecs, v.URI)
	}
	return checked(b.String())
}

// MediaSegment is one segment line of a media playlist.
type MediaSegment struct {
	// URI is the segment URL (relative, query included).
	URI string
	// Seconds is the segment duration; Media writes it to five decimals.
	Seconds float64
}

// maxSeconds is the longest segment Media writes, so the target duration fits
// an int on a 32-bit platform.
const maxSeconds = math.MaxInt32

// MaxSegments is the most segments Media lists: two lines each, and nine
// around them, fill MaxLines.
const MaxSegments = (MaxLines - 9) / 2

// Media renders a VOD media playlist: init map, every segment listed with
// its exact duration (the plan knows them all, so players get instant
// playlist-driven seek), and ENDLIST.
//
// The target duration is the longest segment rounded up, except that less
// than a millionth of a second over a whole one rounds down: RFC 8216 asks
// only that each EXTINF, rounded to the nearest integer, be at most the
// target. A duration must be finite, not negative, and at most math.MaxInt32
// seconds.
func Media(initURI string, segments []MediaSegment) (string, error) {
	if f := uriFault(initURI); f != "" {
		return "", refuse("init URI %q %s", initURI, f)
	}
	if len(segments) > MaxSegments {
		return "", refuse("%d segments are past the %d a playlist lists", len(segments), MaxSegments)
	}
	maxDur := 0.0
	for i, s := range segments {
		if f := lineFault(s.URI); f != "" {
			return "", refuse("segment %d URI %q %s", i, s.URI, f)
		}
		// Written so a NaN, which compares false, is refused too.
		if !(s.Seconds >= 0 && s.Seconds <= maxSeconds) {
			return "", refuse("segment %d lasts %v seconds, not 0 to %d", i, s.Seconds, maxSeconds)
		}
		maxDur = max(maxDur, s.Seconds)
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int(maxDur+0.999999))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s\"\n", initURI)
	for _, s := range segments {
		// Abs drops a negative zero's sign, which EXTINF cannot spell.
		fmt.Fprintf(&b, "#EXTINF:%.5f,\n%s\n", math.Abs(s.Seconds), s.URI)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return checked(b.String())
}

// uriChars are RFC 3986's unreserved and reserved characters past letters and
// digits; with the percent escape they are everything a URI is spelled with.
const uriChars = "-._~:/?#[]@!$&'()*+,;="

// uriFault says why the text cannot carry uri as a URI, or "" when it can.
// RFC 8216 asks for RFC 3986 URIs, so anything outside their characters (a
// space, a quote, a control, any non-ASCII byte) must arrive percent-encoded,
// which also keeps every reader splitting the lines where this one does.
func uriFault(uri string) string {
	if uri == "" {
		return "is empty"
	}
	for i := 0; i < len(uri); i++ {
		switch c := uri[i]; {
		case c == '%':
			if i+2 >= len(uri) || !isHex(uri[i+1]) || !isHex(uri[i+2]) {
				return "holds a % that starts no escape"
			}
			i += 2
		case 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
			strings.IndexByte(uriChars, c) >= 0:
		default:
			r, _ := utf8.DecodeRuneInString(uri[i:])
			return fmt.Sprintf("holds %q, which a URI percent-encodes", r)
		}
	}
	return ""
}

// lineFault is uriFault for a URI on a line of its own, which also may not
// read as a tag.
func lineFault(uri string) string {
	if strings.HasPrefix(uri, "#") {
		return "starts with #, which reads as a tag"
	}
	return uriFault(uri)
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// textFault says why the text cannot carry s, or "" when it can: RFC 8216
// allows only UTF-8 and no control characters in a playlist (section 4.1),
// and no double quote in a quoted string (4.2).
func textFault(s string) string {
	if !utf8.ValidString(s) {
		return "is not UTF-8"
	}
	for _, r := range s {
		switch {
		case r < 0x20 || r >= 0x7f && r <= 0x9f:
			return fmt.Sprintf("holds the control character %U", r)
		case r == '"':
			return "holds a double quote"
		}
	}
	return ""
}

// checked holds a written playlist to the parsers' size caps.
func checked(text string) (string, error) {
	if f := capFault(text); f != "" {
		return "", refuse("%s", f)
	}
	return text, nil
}

func refuse(format string, args ...any) error {
	return waxerr.New(waxerr.CodeInvalidRequest, "m3u8: "+fmt.Sprintf(format, args...))
}
