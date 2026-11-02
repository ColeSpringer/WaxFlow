// Package m3u8 writes and parses M3U8 playlists, the text half of an HLS
// presentation: a master playlist lists the variants, a media playlist one
// variant's segments.
//
// Public since 2026-10, having been internal first (ADR-0002). A host that
// renders a presentation through the exported segmenter writes the daemon's
// playlist text with it, from the rows waxflow.SegmentPlan.MediaSegments and
// MasterVariant give.
//
// A master's EXT-X-MEDIA tags are Renditions, and a variant whose audio is
// one names its group in MasterVariant.Audio.
//
// Both parsers refuse as malformed what RFC 8216 says a client must fail on
// (a second EXT-X-VERSION, EXT-X-INDEPENDENT-SEGMENTS or EXT-X-START, a master
// tag beside a media playlist or segment tag), an attribute list that would
// lose an attribute (a pair with no name or =, a name given twice, a quote
// that wraps no whole value), a decimal-integer or enumerated-string value
// written in quotes, and a leading byte order mark. A quoted-string written
// bare reads as written. EXT-X-DEFINE is unsupported: no substitution is made.
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
	"slices"
	"strconv"
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
	// Audio is the GROUP-ID of the TYPE=AUDIO renditions the variant takes
	// its audio from; empty when the variant carries its own.
	Audio string
}

// Rendition is one EXT-X-MEDIA tag: an alternative audio, video, subtitle or
// caption stream a variant names by group. ParseMaster reads as written what
// a server must not write but a client can still use (a second DEFAULT=YES or
// a repeated NAME in a group, AUTOSELECT=NO beside DEFAULT=YES, an INSTREAM-ID
// outside RFC 8216's set), the first DEFAULT=YES member being the group's
// default; Master refuses to write them.
type Rendition struct {
	Type       string // AUDIO, VIDEO, SUBTITLES or CLOSED-CAPTIONS
	GroupID    string
	Name       string
	URI        string // empty for CLOSED-CAPTIONS, and for AUDIO or VIDEO the variant carries
	Language   string
	Default    bool
	Autoselect bool
	Channels   string // the CHANNELS attribute as written, e.g. "2" or "16/JOC"
	InstreamID string // CLOSED-CAPTIONS only; Master writes CC1 to CC4 or SERVICE1 to SERVICE63
}

// Master renders the master playlist: pl.Media as EXT-X-MEDIA tags, then the
// variants. EXT-X-VERSION 7 is the CMAF/fMP4 floor (EXT-X-MAP in the media
// playlists), so pl.Version is not written. It refuses an empty ladder, a
// negative bandwidth, a value the text cannot carry, a rendition its TYPE or
// group rules forbid (RFC 8216 4.3.4), and an AUDIO naming no audio group.
func Master(pl MasterPlaylist) (string, error) {
	if len(pl.Variants) == 0 {
		return "", refuse("a master playlist lists at least one variant")
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	type group struct{ typ, id string }
	defaults, names, audio := map[group]bool{}, map[[3]string]bool{}, map[string]bool{}
	for i, r := range pl.Media {
		if f := renditionFault(r); f != "" {
			return "", refuse("rendition %d %s", i, f)
		}
		g := group{r.Type, r.GroupID}
		if r.Default && defaults[g] {
			return "", refuse("rendition %d is a second DEFAULT=YES in %s group %q", i, r.Type, r.GroupID)
		}
		defaults[g] = defaults[g] || r.Default
		n := [3]string{r.Type, r.GroupID, r.Name}
		if names[n] {
			return "", refuse("rendition %d repeats NAME %q in %s group %q", i, r.Name, r.Type, r.GroupID)
		}
		names[n] = true
		audio[r.GroupID] = audio[r.GroupID] || r.Type == "AUDIO"
		fmt.Fprintf(&b, `#EXT-X-MEDIA:TYPE=%s,GROUP-ID="%s",NAME="%s"`, r.Type, r.GroupID, r.Name)
		quoted(&b, "LANGUAGE", r.Language)
		if r.Default {
			b.WriteString(",DEFAULT=YES")
		}
		if r.Autoselect {
			b.WriteString(",AUTOSELECT=YES")
		}
		quoted(&b, "CHANNELS", r.Channels)
		quoted(&b, "INSTREAM-ID", r.InstreamID)
		quoted(&b, "URI", r.URI)
		b.WriteByte('\n')
	}
	for i, v := range pl.Variants {
		if f := lineFault(v.URI); f != "" {
			return "", refuse("variant %d URI %q %s", i, v.URI, f)
		}
		if f := textFault(v.Codecs); f != "" {
			return "", refuse("variant %d CODECS %q %s", i, v.Codecs, f)
		}
		// A group's ID passed textFault, so naming one is AUDIO's check too.
		if v.Audio != "" && !audio[v.Audio] {
			return "", refuse("variant %d AUDIO %q names no TYPE=AUDIO rendition", i, v.Audio)
		}
		if v.Bandwidth < 0 {
			return "", refuse("variant %d BANDWIDTH %d is negative", i, v.Bandwidth)
		}
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d", v.Bandwidth)
		quoted(&b, "CODECS", v.Codecs)
		quoted(&b, "AUDIO", v.Audio)
		fmt.Fprintf(&b, "\n%s\n", v.URI)
	}
	return checked(b.String())
}

// quoted writes ,NAME="v" when v is set.
func quoted(b *strings.Builder, name, v string) {
	if v != "" {
		fmt.Fprintf(b, `,%s="%s"`, name, v)
	}
}

// renditionFault says why Master cannot write r, or "" when it can.
func renditionFault(r Rendition) string {
	switch {
	case !slices.Contains(renditionTypes, r.Type):
		return fmt.Sprintf("TYPE %q is not AUDIO, VIDEO, SUBTITLES or CLOSED-CAPTIONS", r.Type)
	case r.GroupID == "":
		return "has no GROUP-ID"
	case r.Name == "":
		return "has no NAME"
	}
	for _, a := range [...]struct{ name, v string }{{"GROUP-ID", r.GroupID}, {"NAME", r.Name},
		{"LANGUAGE", r.Language}, {"CHANNELS", r.Channels}, {"INSTREAM-ID", r.InstreamID}} {
		if f := textFault(a.v); f != "" {
			return fmt.Sprintf("%s %q %s", a.name, a.v, f)
		}
	}
	if f := uriFault(r.URI); r.URI != "" && f != "" {
		return fmt.Sprintf("URI %q %s", r.URI, f)
	}
	if f := typeFault(r.Type, r.URI != "", r.InstreamID != ""); f != "" {
		return f
	}
	if r.Type == "CLOSED-CAPTIONS" && !instreamID(r.InstreamID) {
		return fmt.Sprintf("INSTREAM-ID %q is not CC1 to CC4 or SERVICE1 to SERVICE63", r.InstreamID)
	}
	return ""
}

// instreamID reports whether id is one RFC 8216 allows: CC1 to CC4 or
// SERVICE1 to SERVICE63.
func instreamID(id string) bool {
	if n, ok := strings.CutPrefix(id, "CC"); ok {
		return len(n) == 1 && '1' <= n[0] && n[0] <= '4'
	}
	n, ok := strings.CutPrefix(id, "SERVICE")
	v, err := strconv.Atoi(n)
	return ok && err == nil && 1 <= v && v <= 63 && strconv.Itoa(v) == n
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
