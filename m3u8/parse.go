package m3u8

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/colespringer/waxflow/waxerr"
)

// The parsers here are the read side of the M3U8 writers in playlist.go: they
// round-trip Master/Media output and read third-party playlists. Playlists
// are attacker-influenced input once a client fetches them over the network,
// so parsing is bounded (a line cap and per-line length cap) and tolerant of
// unknown tags, which HLS requires a client to ignore. Damage is refused as
// malformed input; a playlist past a cap, or one using a tag the parsed types
// cannot describe, as unsupported.

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

// MasterPlaylist is a parsed master playlist: the variant ladder plus the
// version it declares.
type MasterPlaylist struct {
	Version  int
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
		if strings.HasPrefix(strings.TrimSpace(ln), "#EXT-X-STREAM-INF") {
			return true
		}
	}
	return false
}

// ParseMaster parses a master playlist: the #EXT-X-STREAM-INF variants and
// their URIs. It is the inverse of Master.
func ParseMaster(s string) (MasterPlaylist, error) {
	lines, err := splitLines(s)
	if err != nil {
		return MasterPlaylist{}, err
	}
	var pl MasterPlaylist
	var pending *MasterVariant // set by a STREAM-INF awaiting its URI line
	for _, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "#EXT-X-VERSION:"):
			pl.Version = atoiDefault(ln[len("#EXT-X-VERSION:"):], pl.Version)
		case strings.HasPrefix(ln, "#EXT-X-STREAM-INF:"):
			attrs := parseAttrs(ln[len("#EXT-X-STREAM-INF:"):])
			v := MasterVariant{
				Bandwidth: atoiDefault(attrs["BANDWIDTH"], 0),
				Codecs:    attrs["CODECS"],
			}
			pending = &v
		case ln == "" || strings.HasPrefix(ln, "#"):
			// Blank lines and other tags/comments: ignored (a client must skip
			// tags it does not recognize).
		default:
			// A URI line. It belongs to the STREAM-INF immediately above it;
			// a bare URI with no preceding STREAM-INF is not a variant.
			if pending != nil {
				pending.URI = ln
				pl.Variants = append(pl.Variants, *pending)
				pending = nil
			}
		}
	}
	if pending != nil {
		return MasterPlaylist{}, malformed("EXT-X-STREAM-INF with no URI line")
	}
	if len(pl.Variants) == 0 {
		return MasterPlaylist{}, malformed("master playlist has no variants")
	}
	return pl, nil
}

// ParseMedia parses a media playlist: the target duration, media sequence,
// init map, and the #EXTINF-timed segments. It is the inverse of Media.
//
// It refuses, as unsupported, a playlist whose segments a MediaPlaylist cannot
// describe: a byte range, encryption, a gap, a delta update, an I-frame
// playlist, or more than one init. Every other tag is read past.
func ParseMedia(s string) (MediaPlaylist, error) {
	lines, err := splitLines(s)
	if err != nil {
		return MediaPlaylist{}, err
	}
	var pl MediaPlaylist
	pending, dur := false, 0.0 // an #EXTINF awaiting its URI line
	for _, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "#EXT-X-VERSION:"):
			pl.Version = atoiDefault(ln[len("#EXT-X-VERSION:"):], pl.Version)
		case strings.HasPrefix(ln, "#EXT-X-TARGETDURATION:"):
			pl.TargetDuration = atoiDefault(ln[len("#EXT-X-TARGETDURATION:"):], pl.TargetDuration)
		case strings.HasPrefix(ln, "#EXT-X-MEDIA-SEQUENCE:"):
			pl.MediaSequence = atoiDefault(ln[len("#EXT-X-MEDIA-SEQUENCE:"):], pl.MediaSequence)
		case strings.HasPrefix(ln, "#EXT-X-MAP:"):
			attrs := parseAttrs(ln[len("#EXT-X-MAP:"):])
			switch uri := attrs["URI"]; {
			case attrs["BYTERANGE"] != "":
				return MediaPlaylist{}, unsupported("EXT-X-MAP with a BYTERANGE: the init is part of a resource")
			case pl.InitURI == "" && len(pl.Segments) > 0:
				return MediaPlaylist{}, unsupported("EXT-X-MAP after the first segment: one init serves every segment here")
			case pl.InitURI != "" && uri != pl.InitURI:
				return MediaPlaylist{}, unsupported("EXT-X-MAP names %q after %q: one init serves every segment here", uri, pl.InitURI)
			default:
				pl.InitURI = uri
			}
		case strings.HasPrefix(ln, "#EXTINF:"):
			// #EXTINF:<duration>,<title>. The duration runs to the first comma
			// (or the line end when the title is omitted).
			v := ln[len("#EXTINF:"):]
			if i := strings.IndexByte(v, ','); i >= 0 {
				v = v[:i]
			}
			if dur, pending = parseDuration(strings.TrimSpace(v)); !pending {
				return MediaPlaylist{}, malformed("bad EXTINF duration %q", v)
			}
		case strings.HasPrefix(ln, "#EXT-X-KEY:"):
			if m := parseAttrs(ln[len("#EXT-X-KEY:"):])["METHOD"]; m != "NONE" {
				return MediaPlaylist{}, unsupported("EXT-X-KEY METHOD=%s: encrypted segments are not read", m)
			}
		case tagIs(ln, "#EXT-X-BYTERANGE"):
			return MediaPlaylist{}, unsupported("EXT-X-BYTERANGE: a segment is part of a resource")
		case tagIs(ln, "#EXT-X-GAP"):
			return MediaPlaylist{}, unsupported("EXT-X-GAP: a listed segment is missing")
		case tagIs(ln, "#EXT-X-SKIP"):
			return MediaPlaylist{}, unsupported("EXT-X-SKIP: a delta update leaves segments out")
		case tagIs(ln, "#EXT-X-I-FRAMES-ONLY"):
			return MediaPlaylist{}, unsupported("EXT-X-I-FRAMES-ONLY: the segments are I-frames, not media")
		case ln == "#EXT-X-ENDLIST":
			pl.End = true
		case ln == "" || strings.HasPrefix(ln, "#"):
			// Blank lines and unrecognized tags/comments: ignored.
		default:
			// A URI line completes the segment its #EXTINF opened. A URI with no
			// preceding #EXTINF is malformed in a media playlist.
			if !pending {
				return MediaPlaylist{}, malformed("media segment URI %q with no EXTINF", ln)
			}
			pl.Segments = append(pl.Segments, MediaSegment{URI: ln, Seconds: dur})
			pending = false
		}
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

// tagIs reports whether ln is the tag name, bare or with attributes.
func tagIs(ln, name string) bool {
	return ln == name || strings.HasPrefix(ln, name+":")
}

// splitLines splits a playlist into trimmed, non-empty-significant lines,
// validating the #EXTM3U signature and the bounds. Carriage returns (CRLF
// playlists) and surrounding whitespace are stripped.
func splitLines(s string) ([]string, error) {
	if f := capFault(s); f != "" {
		return nil, unsupported("%s", f)
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

// parseAttrs parses an HLS attribute list: comma-separated NAME=VALUE pairs
// (RFC 8216 section 4.2). A value may be double-quoted, in which case it may
// itself contain commas; the quotes are stripped from the returned value.
// Keys are upper-cased already in practice, so they are compared verbatim.
func parseAttrs(s string) map[string]string {
	attrs := map[string]string{}
	for _, kv := range splitAttrList(s) {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(kv[:i])
		val := strings.TrimSpace(kv[i+1:])
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}
		attrs[key] = val
	}
	return attrs
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
