package hls

import (
	"fmt"
	"strconv"
	"strings"
)

// The parsers here are the read side of the M3U8 writers in playlist.go: they
// round-trip Master/Media output and read the third-party playlists the HLS
// client (a later milestone) follows. Playlists are attacker-influenced input
// once the client fetches over the network, so parsing is bounded (a line cap
// and per-line length cap) and tolerant of unknown tags, which HLS requires a
// client to ignore.

const (
	// maxPlaylistLines caps the lines one playlist may hold. A VOD media
	// playlist has one #EXTINF plus one URI per segment, so this clears many
	// hours of short segments; past it the playlist is refused rather than
	// grown without bound.
	maxPlaylistLines = 1 << 20
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
		return MasterPlaylist{}, fmt.Errorf("hls: EXT-X-STREAM-INF with no URI line")
	}
	if len(pl.Variants) == 0 {
		return MasterPlaylist{}, fmt.Errorf("hls: master playlist has no variants")
	}
	return pl, nil
}

// ParseMedia parses a media playlist: the target duration, media sequence,
// init map, and the #EXTINF-timed segments. It is the inverse of Media.
func ParseMedia(s string) (MediaPlaylist, error) {
	lines, err := splitLines(s)
	if err != nil {
		return MediaPlaylist{}, err
	}
	var pl MediaPlaylist
	pendingDur := -1.0 // duration set by an #EXTINF awaiting its URI line
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
			pl.InitURI = attrs["URI"]
		case strings.HasPrefix(ln, "#EXTINF:"):
			// #EXTINF:<duration>,<title>. The duration runs to the first comma
			// (or the line end when the title is omitted).
			v := ln[len("#EXTINF:"):]
			if i := strings.IndexByte(v, ','); i >= 0 {
				v = v[:i]
			}
			d, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				return MediaPlaylist{}, fmt.Errorf("hls: bad EXTINF duration %q", v)
			}
			pendingDur = d
		case ln == "#EXT-X-ENDLIST":
			pl.End = true
		case ln == "" || strings.HasPrefix(ln, "#"):
			// Blank lines and unrecognized tags/comments: ignored.
		default:
			// A URI line completes the segment its #EXTINF opened. A URI with no
			// preceding #EXTINF is malformed in a media playlist.
			if pendingDur < 0 {
				return MediaPlaylist{}, fmt.Errorf("hls: media segment URI %q with no EXTINF", ln)
			}
			pl.Segments = append(pl.Segments, MediaSegment{URI: ln, Seconds: pendingDur})
			pendingDur = -1
		}
	}
	if pendingDur >= 0 {
		return MediaPlaylist{}, fmt.Errorf("hls: EXTINF with no segment URI")
	}
	return pl, nil
}

// splitLines splits a playlist into trimmed, non-empty-significant lines,
// validating the #EXTM3U signature and the bounds. Carriage returns (CRLF
// playlists) and surrounding whitespace are stripped.
func splitLines(s string) ([]string, error) {
	raw := strings.Split(s, "\n")
	if len(raw) > maxPlaylistLines {
		return nil, fmt.Errorf("hls: playlist of %d lines exceeds the %d cap", len(raw), maxPlaylistLines)
	}
	out := make([]string, 0, len(raw))
	seenTag := false
	for _, ln := range raw {
		if len(ln) > maxLineLen {
			return nil, fmt.Errorf("hls: playlist line of %d bytes exceeds the %d cap", len(ln), maxLineLen)
		}
		ln = strings.TrimSpace(ln)
		if !seenTag {
			if ln == "" {
				continue
			}
			if ln != "#EXTM3U" {
				return nil, fmt.Errorf("hls: playlist does not start with #EXTM3U")
			}
			seenTag = true
			continue
		}
		out = append(out, ln)
	}
	if !seenTag {
		return nil, fmt.Errorf("hls: empty playlist (no #EXTM3U)")
	}
	return out, nil
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
