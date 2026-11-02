package m3u8

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/colespringer/waxflow/waxerr"
)

// The parsers here are the read side of the M3U8 writers in playlist.go: they
// round-trip Master/Media output and read third-party playlists. Playlists
// are attacker-influenced input once a client fetches them over the network,
// so parsing is bounded (a line cap and per-line length cap) and tolerant of
// unknown tags, which HLS requires a client to ignore. Damage, and what RFC
// 8216 says a client must fail on, is refused as malformed input; a playlist
// past a cap, or one using a tag the parsed types cannot describe, as
// unsupported.

// MaxLines caps the lines one playlist may hold, counted as its newlines plus
// one, so the newline a playlist ends with opens an empty last line. A VOD
// media playlist spends two lines a segment, so this clears many hours of
// short segments; past it the playlist is refused rather than grown without
// bound.
const MaxLines = 1 << 20

const (
	// maxLineLen caps a single line. URIs and attribute lists are short; a
	// longer line is a malformed or hostile playlist.
	maxLineLen = 64 << 10
)

// MasterPlaylist is a parsed master playlist: the renditions its EXT-X-MEDIA
// tags list, the variant ladder, and the version it declares.
type MasterPlaylist struct {
	Version  int
	Media    []Rendition
	Variants []MasterVariant
}

// MediaPlaylist is a parsed media playlist. End reports whether the playlist
// carried #EXT-X-ENDLIST (a complete VOD list); a live playlist leaves it
// false and is reloaded. InitURI is the #EXT-X-MAP init segment, empty for a
// legacy TS playlist with none.
type MediaPlaylist struct {
	Version        int
	TargetDuration int
	MediaSequence  int
	InitURI        string
	Segments       []MediaSegment
	End            bool
}

// IsMaster reports whether s is a master playlist: an EXT-X-STREAM-INF tag
// starts one of its lines. The anchor matters, since a media playlist may
// mention the tag in a title. It allocates nothing, so a client can sort a
// fetched playlist before either parser's caps run.
func IsMaster(s string) bool {
	for rest, more := s, true; more; {
		var ln string
		ln, rest, more = strings.Cut(rest, "\n")
		if name, _ := tagOf(strings.TrimSpace(ln)); name == "EXT-X-STREAM-INF" {
			return true
		}
	}
	return false
}

// ParseMaster parses a master playlist: the #EXT-X-MEDIA renditions and the
// #EXT-X-STREAM-INF variants with their URIs. It is the inverse of Master.
//
// Beside the refusals the package doc lists, it refuses as malformed a
// rendition or variant missing an attribute RFC 8216 requires, a STREAM-INF
// and URI line that do not pair, and an AUDIO naming no TYPE=AUDIO group; a
// BANDWIDTH past an int is unsupported. An EXT-X-MEDIA whose TYPE RFC 8216
// does not define is dropped (6.3.1); an undefined DEFAULT or AUTOSELECT
// value reads as NO.
func ParseMaster(s string) (MasterPlaylist, error) {
	lines, err := splitLines(s)
	if err != nil {
		return MasterPlaylist{}, err
	}
	var pl MasterPlaylist
	var pending *MasterVariant // set by a STREAM-INF awaiting its URI line
	seen := map[string]bool{}
	dropped := map[string]string{} // group ID to the undefined TYPE that dropped a member
	for _, ln := range lines {
		name, value := tagOf(ln)
		if err := tagRules(name, true, seen); err != nil {
			return MasterPlaylist{}, err
		}
		switch name {
		case "EXT-X-VERSION":
			pl.Version = atoiDefault(value, pl.Version)
		case "EXT-X-MEDIA":
			r, ok, err := renditionOf(value)
			switch {
			case err != nil:
				return MasterPlaylist{}, err
			case ok:
				pl.Media = append(pl.Media, r)
			default:
				dropped[r.GroupID] = r.Type
			}
		case "EXT-X-STREAM-INF":
			if pending != nil {
				return MasterPlaylist{}, malformed("EXT-X-STREAM-INF with no URI line before the next")
			}
			v, err := variantOf(value)
			if err != nil {
				return MasterPlaylist{}, err
			}
			pending = &v
		case "":
			// A blank line or a comment is read past; any other line is a URI,
			// which belongs to the STREAM-INF above it.
			if ln == "" || ln[0] == '#' {
				continue
			}
			if pending == nil {
				return MasterPlaylist{}, malformed("URI line %q with no EXT-X-STREAM-INF", ln)
			}
			pending.URI = ln
			pl.Variants = append(pl.Variants, *pending)
			pending = nil
		}
		// Any other tag moves no variant's media and is read past (6.3.1).
	}
	if pending != nil {
		return MasterPlaylist{}, malformed("EXT-X-STREAM-INF with no URI line")
	}
	if len(pl.Variants) == 0 {
		return MasterPlaylist{}, malformed("master playlist has no variants")
	}
	// AUDIO may name a group listed after its variant, so it is checked last.
	audio := map[string]bool{}
	for _, r := range pl.Media {
		if r.Type == "AUDIO" {
			audio[r.GroupID] = true
		}
	}
	for i, v := range pl.Variants {
		if v.Audio == "" || audio[v.Audio] {
			continue
		}
		if t, ok := dropped[v.Audio]; ok {
			return MasterPlaylist{}, malformed("variant %d AUDIO %q names a group whose EXT-X-MEDIA has TYPE %q, which RFC 8216 does not define", i, v.Audio, t)
		}
		return MasterPlaylist{}, malformed("variant %d AUDIO %q names no TYPE=AUDIO group", i, v.Audio)
	}
	return pl, nil
}

// variantOf reads an EXT-X-STREAM-INF attribute list.
func variantOf(list string) (MasterVariant, error) {
	a, err := parseAttrs(list)
	if err != nil {
		return MasterVariant{}, err
	}
	if !a.has("BANDWIDTH") {
		return MasterVariant{}, malformed("EXT-X-STREAM-INF with no BANDWIDTH")
	}
	bw, err := a.bare("BANDWIDTH")
	if err != nil {
		return MasterVariant{}, err
	}
	n, ok := decimalInteger(bw)
	if !ok {
		return MasterVariant{}, malformed("BANDWIDTH %q is not a decimal-integer", bw)
	}
	if n > math.MaxInt {
		return MasterVariant{}, unsupported("BANDWIDTH %d is past what an int holds", n)
	}
	// No GROUP-ID is empty, so an empty AUDIO names no group.
	if a.has("AUDIO") && a.str("AUDIO") == "" {
		return MasterVariant{}, malformed("EXT-X-STREAM-INF with an empty AUDIO")
	}
	return MasterVariant{Bandwidth: int(n), Codecs: a.str("CODECS"), Audio: a.str("AUDIO")}, nil
}

// decimalInteger reads RFC 8216's decimal-integer: 1 to 20 digits, at most
// 2^64-1.
func decimalInteger(v string) (uint64, bool) {
	if len(v) == 0 || len(v) > 20 || strings.Trim(v, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseUint(v, 10, 64)
	return n, err == nil
}

// renditionOf reads an EXT-X-MEDIA attribute list. ok is false for a tag a
// client ignores, one whose TYPE RFC 8216 does not define (6.3.1); r then
// carries that TYPE and the GROUP-ID.
func renditionOf(list string) (r Rendition, ok bool, err error) {
	a, err := parseAttrs(list)
	if err != nil {
		return Rendition{}, false, err
	}
	typ, err := a.bare("TYPE")
	switch {
	case err != nil:
		return Rendition{}, false, err
	case typ == "":
		return Rendition{}, false, malformed("EXT-X-MEDIA with no TYPE")
	case !slices.Contains(renditionTypes, typ):
		return Rendition{Type: typ, GroupID: a.str("GROUP-ID")}, false, nil
	}
	def, err := yes(a, "DEFAULT")
	if err != nil {
		return Rendition{}, false, err
	}
	auto, err := yes(a, "AUTOSELECT")
	if err != nil {
		return Rendition{}, false, err
	}
	for _, name := range [...]string{"GROUP-ID", "NAME"} {
		if a.str(name) == "" {
			return Rendition{}, false, malformed("EXT-X-MEDIA with no %s", name)
		}
	}
	for _, name := range [...]string{"URI", "INSTREAM-ID"} {
		if a.has(name) && a.str(name) == "" {
			return Rendition{}, false, malformed("EXT-X-MEDIA with an empty %s", name)
		}
	}
	if f := typeFault(typ, a.has("URI"), a.has("INSTREAM-ID")); f != "" {
		return Rendition{}, false, malformed("EXT-X-MEDIA %s", f)
	}
	return Rendition{
		Type: typ, GroupID: a.str("GROUP-ID"), Name: a.str("NAME"), URI: a.str("URI"),
		Language: a.str("LANGUAGE"), Default: def, Autoselect: auto,
		Channels: a.str("CHANNELS"), InstreamID: a.str("INSTREAM-ID"),
	}, true, nil
}

// renditionTypes are EXT-X-MEDIA's TYPE values.
var renditionTypes = []string{"AUDIO", "VIDEO", "SUBTITLES", "CLOSED-CAPTIONS"}

// typeFault says how a rendition breaks the rules RFC 8216 sets per TYPE on
// its URI and INSTREAM-ID (4.3.4.1, 4.3.4.2.1), or "" when it keeps them.
// ParseMaster and Master share it.
func typeFault(typ string, hasURI, hasID bool) string {
	captions := typ == "CLOSED-CAPTIONS"
	switch {
	case captions && hasURI:
		return "TYPE=CLOSED-CAPTIONS with a URI"
	case captions && !hasID:
		return "TYPE=CLOSED-CAPTIONS with no INSTREAM-ID"
	case !captions && hasID:
		return "TYPE=" + typ + " with an INSTREAM-ID"
	case typ == "SUBTITLES" && !hasURI:
		return "TYPE=SUBTITLES with no URI"
	}
	return ""
}

// yes reads an optional YES/NO attribute. A value RFC 8216 does not define
// reads as NO, where 6.3.1 would drop the tag: these only steer the choice
// within a group, and a dropped rendition loses the media a variant names.
func yes(a attrs, name string) (bool, error) {
	v, err := a.bare(name)
	return v == "YES", err
}

// ParseMedia parses a media playlist: the target duration, media sequence,
// init map, and the #EXTINF-timed segments. It is the inverse of Media.
//
// It refuses, as unsupported, a playlist whose segments a MediaPlaylist cannot
// describe: a byte range, encryption, a gap, a delta update, an I-frame
// playlist, or more than one init. Beside the refusals the package doc lists,
// a repeated media playlist tag is malformed. Every other tag is read past.
func ParseMedia(s string) (MediaPlaylist, error) {
	lines, err := splitLines(s)
	if err != nil {
		return MediaPlaylist{}, err
	}
	var pl MediaPlaylist
	pending, dur := false, 0.0 // an #EXTINF awaiting its URI line
	seen := map[string]bool{}
	for _, ln := range lines {
		name, value := tagOf(ln)
		if err := tagRules(name, false, seen); err != nil {
			return MediaPlaylist{}, err
		}
		switch name {
		case "EXT-X-VERSION":
			pl.Version = atoiDefault(value, pl.Version)
		case "EXT-X-TARGETDURATION":
			pl.TargetDuration = atoiDefault(value, pl.TargetDuration)
		case "EXT-X-MEDIA-SEQUENCE":
			pl.MediaSequence = atoiDefault(value, pl.MediaSequence)
		case "EXT-X-MAP":
			a, err := parseAttrs(value)
			if err != nil {
				return MediaPlaylist{}, err
			}
			switch uri := a.str("URI"); {
			case uri == "":
				return MediaPlaylist{}, malformed("EXT-X-MAP with no URI")
			case a.str("BYTERANGE") != "":
				return MediaPlaylist{}, unsupported("EXT-X-MAP with a BYTERANGE: the init is part of a resource")
			case pl.InitURI == "" && len(pl.Segments) > 0:
				return MediaPlaylist{}, unsupported("EXT-X-MAP after the first segment: one init serves every segment here")
			case pl.InitURI != "" && uri != pl.InitURI:
				return MediaPlaylist{}, unsupported("EXT-X-MAP names %q after %q: one init serves every segment here", uri, pl.InitURI)
			default:
				pl.InitURI = uri
			}
		case "EXTINF":
			// #EXTINF:<duration>,<title>. The duration runs to the first comma
			// (or the line end when the title is omitted).
			v := value
			if i := strings.IndexByte(v, ','); i >= 0 {
				v = v[:i]
			}
			if dur, pending = parseDuration(strings.TrimSpace(v)); !pending {
				return MediaPlaylist{}, malformed("bad EXTINF duration %q", v)
			}
		case "EXT-X-KEY":
			a, err := parseAttrs(value)
			if err != nil {
				return MediaPlaylist{}, err
			}
			switch m, err := a.bare("METHOD"); {
			case err != nil:
				return MediaPlaylist{}, err
			case m == "":
				return MediaPlaylist{}, malformed("EXT-X-KEY with no METHOD")
			case m != "NONE":
				return MediaPlaylist{}, unsupported("EXT-X-KEY METHOD=%q: encrypted segments are not read", m)
			}
		case "EXT-X-BYTERANGE":
			return MediaPlaylist{}, unsupported("EXT-X-BYTERANGE: a segment is part of a resource")
		case "EXT-X-GAP":
			return MediaPlaylist{}, unsupported("EXT-X-GAP: a listed segment is missing")
		case "EXT-X-SKIP":
			return MediaPlaylist{}, unsupported("EXT-X-SKIP: a delta update leaves segments out")
		case "EXT-X-I-FRAMES-ONLY":
			return MediaPlaylist{}, unsupported("EXT-X-I-FRAMES-ONLY: the segments are I-frames, not media")
		case "EXT-X-ENDLIST":
			pl.End = true
		case "":
			// A blank line or a comment is read past. Any other line is a URI,
			// which completes the segment its #EXTINF opened.
			if ln == "" || ln[0] == '#' {
				continue
			}
			if !pending {
				return MediaPlaylist{}, malformed("media segment URI %q with no EXTINF", ln)
			}
			pl.Segments = append(pl.Segments, MediaSegment{URI: ln, Seconds: dur})
			pending = false
		}
		// Any other tag is read past.
	}
	if pending {
		return MediaPlaylist{}, malformed("EXTINF with no segment URI")
	}
	return pl, nil
}

// parseDuration reads an EXTINF duration in RFC 8216's unsigned spellings,
// decimal-integer and decimal-floating-point: digits and at most one point.
// A sign, an exponent, NaN and Inf are none of them.
func parseDuration(v string) (float64, bool) {
	digits, points := 0, 0
	for i := range len(v) {
		switch {
		case v[i] >= '0' && v[i] <= '9':
			digits++
		case v[i] == '.':
			points++
		default:
			return 0, false
		}
	}
	if digits == 0 || points > 1 {
		return 0, false
	}
	d, err := strconv.ParseFloat(v, 64) // refuses a run of digits too long to be finite
	return d, err == nil
}

// tagOf splits a tag line into its name and what follows the colon. A line
// that is no tag (a URI, a blank line or a comment) has no name.
func tagOf(ln string) (name, value string) {
	if !strings.HasPrefix(ln, "#EXT") {
		return "", ""
	}
	name, value, _ = strings.Cut(ln[1:], ":")
	return name, value
}

// tagKind is the kind of playlist a tag belongs to (RFC 8216 4.3).
type tagKind int

const (
	eitherTag   tagKind = iota // basic or unknown
	masterTag                  // 4.3.4
	playlistTag                // 4.3.3, at most one of each
	segmentTag                 // 4.3.2
	bothTag                    // 4.3.5, in either playlist, at most one of each
)

func kindOf(name string) tagKind {
	switch name {
	case "EXT-X-MEDIA", "EXT-X-STREAM-INF", "EXT-X-I-FRAME-STREAM-INF",
		"EXT-X-SESSION-DATA", "EXT-X-SESSION-KEY":
		return masterTag
	case "EXT-X-TARGETDURATION", "EXT-X-MEDIA-SEQUENCE", "EXT-X-DISCONTINUITY-SEQUENCE",
		"EXT-X-ENDLIST", "EXT-X-PLAYLIST-TYPE", "EXT-X-I-FRAMES-ONLY":
		return playlistTag
	case "EXTINF", "EXT-X-BYTERANGE", "EXT-X-DISCONTINUITY", "EXT-X-KEY", "EXT-X-MAP",
		"EXT-X-PROGRAM-DATE-TIME", "EXT-X-DATERANGE":
		return segmentTag
	case "EXT-X-INDEPENDENT-SEGMENTS", "EXT-X-START":
		return bothTag
	}
	return eitherTag
}

// tagRules applies what both parsers refuse in any playlist: variable
// substitution, a tag of the other kind of playlist (RFC 8216 4.3.4), and a
// second EXT-X-VERSION (4.3.1.2), media playlist tag (4.3.3) or 4.3.5 tag.
// seen records the tags met once.
func tagRules(name string, master bool, seen map[string]bool) error {
	kind := kindOf(name)
	switch {
	case name == "EXT-X-DEFINE":
		return unsupported("EXT-X-DEFINE: variable substitution is not read")
	case master && (kind == playlistTag || kind == segmentTag):
		return malformed("%s in a master playlist", name)
	case !master && kind == masterTag:
		return malformed("%s in a media playlist", name)
	case name == "EXT-X-VERSION" || kind == playlistTag || kind == bothTag:
		if seen[name] {
			return malformed("more than one %s", name)
		}
		seen[name] = true
	}
	return nil
}

// byteOrderMark is U+FEFF in UTF-8.
const byteOrderMark = "\xef\xbb\xbf"

// splitLines splits a playlist into trimmed, non-empty-significant lines,
// validating the #EXTM3U signature and the bounds. Carriage returns (CRLF
// playlists) and surrounding whitespace are stripped.
func splitLines(s string) ([]string, error) {
	if f := capFault(s); f != "" {
		return nil, unsupported("%s", f)
	}
	if strings.HasPrefix(s, byteOrderMark) {
		return nil, malformed("playlist starts with a byte order mark (RFC 8216 4.1)")
	}
	var out []string
	seenTag := false
	for rest, more := s, true; more; {
		var ln string
		ln, rest, more = strings.Cut(rest, "\n")
		ln = strings.TrimSpace(ln)
		if !seenTag {
			if ln == "" {
				continue
			}
			if ln != "#EXTM3U" {
				return nil, malformed("playlist does not start with #EXTM3U")
			}
			seenTag = true
			continue
		}
		out = append(out, ln)
	}
	if !seenTag {
		return nil, malformed("empty playlist (no #EXTM3U)")
	}
	return out, nil
}

// capFault says how s is past the size caps, or "" when it is within them, in
// one pass that allocates nothing: the parsers run it before reading a line,
// and the writers hold their output to it, so what they write the parsers read.
func capFault(s string) string {
	lines := 1
	for {
		i := strings.IndexByte(s, '\n')
		ln := s
		if i >= 0 {
			ln = s[:i]
		}
		if len(ln) > maxLineLen {
			return fmt.Sprintf("playlist line of %d bytes exceeds the %d cap", len(ln), maxLineLen)
		}
		if i < 0 {
			return ""
		}
		if lines++; lines > MaxLines {
			return fmt.Sprintf("playlist of more than %d lines exceeds the cap", MaxLines)
		}
		s = s[i+1:]
	}
}

func malformed(format string, args ...any) error {
	return waxerr.New(waxerr.CodeMalformedInput, "m3u8: "+fmt.Sprintf(format, args...))
}

func unsupported(format string, args ...any) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, "m3u8: "+fmt.Sprintf(format, args...))
}

// attrs is a parsed attribute list: each value as written, quotes included,
// so each is read as the type RFC 8216 gives its attribute (4.2).
type attrs map[string]string

func (a attrs) has(name string) bool {
	_, ok := a[name]
	return ok
}

// str is a quoted-string attribute's value without its quotes. A bare one
// reads as written.
func (a attrs) str(name string) string {
	if v := a[name]; strings.HasPrefix(v, `"`) {
		return v[1 : len(v)-1]
	}
	return a[name]
}

// bare is a decimal-integer or enumerated-string attribute's value, which
// RFC 8216 writes unquoted; a quoted one is malformed.
func (a attrs) bare(name string) (string, error) {
	if v := a[name]; strings.HasPrefix(v, `"`) {
		return "", malformed("attribute %s is quoted (%q), where RFC 8216 writes it bare", name, v)
	}
	return a[name], nil
}

// parseAttrs parses an HLS attribute list: comma-separated NAME=VALUE pairs
// (RFC 8216 section 4.2). A value may be double-quoted, in which case it may
// itself contain commas. Keys are upper-cased already in practice, so they
// are compared verbatim. A pair with no name or no =, a name given twice, or
// a quote that wraps no whole value (it moves the commas the list splits on)
// is malformed; an empty pair (a stray comma) is skipped.
func parseAttrs(s string) (attrs, error) {
	a := attrs{}
	for _, kv := range splitAttrList(s) {
		if strings.TrimSpace(kv) == "" {
			continue
		}
		i := strings.IndexByte(kv, '=')
		if i < 0 || strings.TrimSpace(kv[:i]) == "" {
			return nil, malformed("attribute %q is not NAME=VALUE", strings.TrimSpace(kv))
		}
		key := strings.TrimSpace(kv[:i])
		val := strings.TrimSpace(kv[i+1:])
		wrapped := len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"'
		if q := strings.Count(kv, `"`); q != 0 && !(q == 2 && wrapped) {
			return nil, malformed("attribute %q has a stray quote", strings.TrimSpace(kv))
		}
		if a.has(key) {
			return nil, malformed("attribute %q given twice", key)
		}
		a[key] = val
	}
	return a, nil
}

// splitAttrList splits an attribute list on commas that are not inside a
// double-quoted value (a quoted CODECS list holds its own commas).
func splitAttrList(s string) []string {
	var out []string
	start := 0
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case ',':
			if !inQuote {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// atoiDefault parses a base-10 integer, returning def when the field is not a
// clean integer (a tolerant read for playlist numeric tags).
func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}
