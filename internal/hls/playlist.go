package hls

import (
	"fmt"
	"strings"
)

// MasterVariant is one rung of the master playlist's ladder.
type MasterVariant struct {
	// URI is the media playlist URL (relative, query included).
	URI string
	// Bandwidth is the peak bandwidth bound in bits per second.
	Bandwidth int
	// Codecs is the RFC 6381 CODECS attribute value.
	Codecs string
}

// Master renders the master playlist. EXT-X-VERSION 7 is the CMAF/fMP4
// floor (EXT-X-MAP in the media playlists).
func Master(variants []MasterVariant) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	for _, v := range variants {
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,CODECS=\"%s\"\n%s\n", v.Bandwidth, v.Codecs, v.URI)
	}
	return b.String()
}

// MediaSegment is one segment line of a media playlist.
type MediaSegment struct {
	// URI is the segment URL (relative, query included).
	URI string
	// Seconds is the exact segment duration.
	Seconds float64
}

// Media renders a VOD media playlist: init map, every segment listed with
// its exact duration (the plan knows them all, so players get instant
// playlist-driven seek), and ENDLIST. The target duration is the ceiling
// of the longest segment, as RFC 8216 requires.
func Media(initURI string, segments []MediaSegment) string {
	maxDur := 0.0
	for _, s := range segments {
		maxDur = max(maxDur, s.Seconds)
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int(maxDur+0.999999))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s\"\n", initURI)
	for _, s := range segments {
		fmt.Fprintf(&b, "#EXTINF:%.5f,\n%s\n", s.Seconds, s.URI)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}
