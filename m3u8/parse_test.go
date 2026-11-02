package m3u8

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/waxerr"
)

// TestMasterRoundTrip writes a master playlist and parses it back, expecting
// the renditions and the variant ladder to survive intact.
func TestMasterRoundTrip(t *testing.T) {
	pl := MasterPlaylist{
		Media: []Rendition{
			{Type: "AUDIO", GroupID: "aud", Name: "English", URI: "audio/en.m3u8", Language: "en",
				Default: true, Autoselect: true, Channels: "2"},
			{Type: "AUDIO", GroupID: "aud", Name: "Commentary, director", URI: "audio/c.m3u8?v=a,b", Language: "en"},
			{Type: "CLOSED-CAPTIONS", GroupID: "cc", Name: "English", InstreamID: "SERVICE63"},
		},
		Variants: []MasterVariant{
			{URI: "audio_64k/index.m3u8", Bandwidth: 64000, Codecs: "mp4a.40.2"},
			{URI: "audio_128k/index.m3u8?v=abc", Bandwidth: 128000, Codecs: "Opus", Audio: "aud"},
		},
	}
	text, err := Master(pl)
	if err != nil {
		t.Fatalf("Master: %v", err)
	}
	got, err := ParseMaster(text)
	if err != nil {
		t.Fatalf("ParseMaster: %v", err)
	}
	if got.Version != 7 {
		t.Errorf("Version = %d, want 7", got.Version)
	}
	if !reflect.DeepEqual(got.Media, pl.Media) {
		t.Errorf("renditions round-trip mismatch:\n got %+v\nwant %+v", got.Media, pl.Media)
	}
	if !reflect.DeepEqual(got.Variants, pl.Variants) {
		t.Errorf("variants round-trip mismatch:\n got %+v\nwant %+v", got.Variants, pl.Variants)
	}
}

// TestMediaRoundTrip writes a VOD media playlist and parses it back.
func TestMediaRoundTrip(t *testing.T) {
	segs := []MediaSegment{
		{URI: "0.m4s", Seconds: 4.0},
		{URI: "1.m4s", Seconds: 4.0},
		{URI: "2.m4s", Seconds: 2.5},
	}
	text, err := Media("init.mp4", segs)
	if err != nil {
		t.Fatalf("Media: %v", err)
	}
	pl, err := ParseMedia(text)
	if err != nil {
		t.Fatalf("ParseMedia: %v", err)
	}
	if !pl.End {
		t.Error("End = false, want true for a VOD (#EXT-X-ENDLIST) playlist")
	}
	if pl.InitURI != "init.mp4" {
		t.Errorf("InitURI = %q, want init.mp4", pl.InitURI)
	}
	if pl.TargetDuration != 4 {
		t.Errorf("TargetDuration = %d, want 4", pl.TargetDuration)
	}
	if !reflect.DeepEqual(pl.Segments, segs) {
		t.Errorf("segments round-trip mismatch:\n got %+v\nwant %+v", pl.Segments, segs)
	}
}

// TestCodecsAttrWithCommas checks that a quoted CODECS value holding its own
// commas is not split at the attribute level.
func TestCodecsAttrWithCommas(t *testing.T) {
	master := "#EXTM3U\n#EXT-X-VERSION:7\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=256000,CODECS=\"mp4a.40.2,avc1.4d401f\"\nv.m3u8\n"
	got, err := ParseMaster(master)
	if err != nil {
		t.Fatalf("ParseMaster: %v", err)
	}
	if len(got.Variants) != 1 || got.Variants[0].Codecs != "mp4a.40.2,avc1.4d401f" {
		t.Errorf("CODECS with commas mis-parsed: %+v", got.Variants)
	}
}

// TestParseMasterRenditions pins how EXT-X-MEDIA reads: one Rendition a tag,
// a variant's AUDIO naming its group wherever the group is listed, and a tag
// with an enumerated value RFC 8216 does not define dropped (6.3.1).
func TestParseMasterRenditions(t *testing.T) {
	const variant = "#EXT-X-STREAM-INF:BANDWIDTH=1\nv.m3u8\n"
	for _, tc := range []struct {
		name     string
		in       string
		media    []Rendition
		variants []MasterVariant
	}{
		{
			name: "an audio rendition a variant names",
			in: "#EXTM3U\n#EXT-X-VERSION:7\n" +
				"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"English\",LANGUAGE=\"en\",DEFAULT=YES," +
				"AUTOSELECT=YES,CHANNELS=\"2\",FORCED=NO,URI=\"audio/en.m3u8\"\n" +
				"#EXT-X-STREAM-INF:BANDWIDTH=1000000,CODECS=\"avc1.4d401f,mp4a.40.2\",AUDIO=\"aud\"\nvideo.m3u8\n",
			media: []Rendition{{Type: "AUDIO", GroupID: "aud", Name: "English", URI: "audio/en.m3u8",
				Language: "en", Default: true, Autoselect: true, Channels: "2"}},
			variants: []MasterVariant{{URI: "video.m3u8", Bandwidth: 1000000, Codecs: "avc1.4d401f,mp4a.40.2", Audio: "aud"}},
		},
		{
			name: "a group listed after the variant naming it",
			in: "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"aud\"\nv.m3u8\n" +
				"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"Main\"\n",
			media:    []Rendition{{Type: "AUDIO", GroupID: "aud", Name: "Main"}},
			variants: []MasterVariant{{URI: "v.m3u8", Bandwidth: 1, Audio: "aud"}},
		},
		{
			name: "an unrecognized TYPE drops its tag",
			in: "#EXTM3U\n#EXT-X-MEDIA:TYPE=TEXT,GROUP-ID=\"t\",NAME=\"n\"\n#EXT-X-MEDIA:TYPE=TEXT,GROUP-ID=\"t\"\n" +
				"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"kept\"\n" + variant,
			media:    []Rendition{{Type: "AUDIO", GroupID: "a", Name: "kept"}},
			variants: []MasterVariant{{URI: "v.m3u8", Bandwidth: 1}},
		},
		{
			name: "an undefined DEFAULT or AUTOSELECT reads as NO",
			in: "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"n\",DEFAULT=yes,URI=\"a.m3u8\"\n" +
				"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"m\",AUTOSELECT=SOMETIMES\n" +
				"#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"a\"\nv.m3u8\n",
			media: []Rendition{{Type: "AUDIO", GroupID: "a", Name: "n", URI: "a.m3u8"},
				{Type: "AUDIO", GroupID: "a", Name: "m"}},
			variants: []MasterVariant{{URI: "v.m3u8", Bandwidth: 1, Audio: "a"}},
		},
		{
			name: "group rules a server broke are read as written",
			in: "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"one\",DEFAULT=YES,AUTOSELECT=NO\n" +
				"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"one\",DEFAULT=YES\n" + variant,
			media: []Rendition{{Type: "AUDIO", GroupID: "a", Name: "one", Default: true},
				{Type: "AUDIO", GroupID: "a", Name: "one", Default: true}},
			variants: []MasterVariant{{URI: "v.m3u8", Bandwidth: 1}},
		},
		{
			name: "one GROUP-ID under two TYPEs is two groups",
			in: "#EXTM3U\n#EXT-X-MEDIA:TYPE=VIDEO,GROUP-ID=\"g\",NAME=\"cam\",URI=\"cam.m3u8\"\n" +
				"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"g\",NAME=\"mix\"\n" +
				"#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"g\",VIDEO=\"g\"\nv.m3u8\n",
			media: []Rendition{{Type: "VIDEO", GroupID: "g", Name: "cam", URI: "cam.m3u8"},
				{Type: "AUDIO", GroupID: "g", Name: "mix"}},
			variants: []MasterVariant{{URI: "v.m3u8", Bandwidth: 1, Audio: "g"}},
		},
		{
			name: "subtitles and captions",
			in: "#EXTM3U\n#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=\"English\",LANGUAGE=\"en\",URI=\"subs/en.m3u8\"\n" +
				"#EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS,GROUP-ID=\"cc\",NAME=\"English\",INSTREAM-ID=\"CC1\"\n" +
				"#EXT-X-STREAM-INF:BANDWIDTH=1,SUBTITLES=\"subs\",CLOSED-CAPTIONS=\"cc\"\nv.m3u8\n",
			media: []Rendition{{Type: "SUBTITLES", GroupID: "subs", Name: "English", URI: "subs/en.m3u8", Language: "en"},
				{Type: "CLOSED-CAPTIONS", GroupID: "cc", Name: "English", InstreamID: "CC1"}},
			variants: []MasterVariant{{URI: "v.m3u8", Bandwidth: 1}},
		},
	} {
		pl, err := ParseMaster(tc.in)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(pl.Media, tc.media) {
			t.Errorf("%s: renditions\n got %+v\nwant %+v", tc.name, pl.Media, tc.media)
		}
		if !reflect.DeepEqual(pl.Variants, tc.variants) {
			t.Errorf("%s: variants\n got %+v\nwant %+v", tc.name, pl.Variants, tc.variants)
		}
	}
}

// TestLiveMediaPlaylist checks that a playlist without #EXT-X-ENDLIST parses
// as not-ended (the live case the client keeps reloading).
func TestLiveMediaPlaylist(t *testing.T) {
	live := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:10\n" +
		"#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:4.00000,\n10.m4s\n"
	pl, err := ParseMedia(live)
	if err != nil {
		t.Fatalf("ParseMedia: %v", err)
	}
	if pl.End {
		t.Error("End = true, want false for a live playlist")
	}
	if pl.MediaSequence != 10 {
		t.Errorf("MediaSequence = %d, want 10", pl.MediaSequence)
	}
	if len(pl.Segments) != 1 {
		t.Fatalf("segments = %d, want 1", len(pl.Segments))
	}
}

// TestParseRejects checks the guards and their codes: damage is malformed
// input; a playlist past the size caps, or one whose segments MediaPlaylist
// cannot describe, is refused as unsupported. want, when set, is what the
// message has to name.
func TestParseRejects(t *testing.T) {
	const (
		malformed   = waxerr.CodeMalformedInput
		unsupported = waxerr.CodeUnsupportedFormat
		variant     = "#EXT-X-STREAM-INF:BANDWIDTH=1\nv.m3u8\n"
		segment     = "#EXTINF:4,\n0.m4s\n"
	)
	for _, tc := range []struct {
		name   string
		master bool
		in     string
		code   waxerr.Code
		want   string
	}{
		{"no signature", false, "#EXT-X-VERSION:7\nv.m3u8\n", malformed, ""},
		{"empty", false, "\n\n", malformed, ""},
		{"stream no uri", true, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\n", malformed, ""},
		{"no variants", true, "#EXTM3U\n#EXT-X-VERSION:7\n", malformed, ""},
		{"uri no extinf", false, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n0.m4s\n", malformed, ""},
		{"bad extinf", false, "#EXTM3U\n#EXTINF:notanumber,\n0.m4s\n", malformed, ""},
		{"extinf no uri", false, "#EXTM3U\n#EXTINF:4.0,\n", malformed, ""},
		{"NaN duration", false, "#EXTM3U\n#EXTINF:NaN,\n0.m4s\n", malformed, "duration"},
		{"infinite duration", false, "#EXTM3U\n#EXTINF:Inf,\n0.m4s\n", malformed, "duration"},
		{"unknown-length duration", false, "#EXTM3U\n#EXTINF:-1,\n0.m4s\n", malformed, "duration"},
		{"signed duration", false, "#EXTM3U\n#EXTINF:+4,\n0.m4s\n", malformed, "duration"},
		{"exponent duration", false, "#EXTM3U\n#EXTINF:4e0,\n0.m4s\n", malformed, "duration"},
		{"trailing NaN extinf", false, "#EXTM3U\n#EXTINF:4,\n0.m4s\n#EXTINF:NaN,\n", malformed, ""},
		{"byte range", false, "#EXTM3U\n#EXTINF:4,\n#EXT-X-BYTERANGE:1000@800\nall.mp4\n", unsupported, "EXT-X-BYTERANGE"},
		{"init byte range", false, "#EXTM3U\n#EXT-X-MAP:URI=\"all.mp4\",BYTERANGE=\"800@0\"\n#EXTINF:4,\n0.m4s\n", unsupported, "BYTERANGE"},
		{"encrypted", false, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\"\n#EXTINF:4,\n0.m4s\n", unsupported, "EXT-X-KEY"},
		{"sample encrypted", false, "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"k\"\n#EXTINF:4,\n0.m4s\n", unsupported, "EXT-X-KEY"},
		{"gap", false, "#EXTM3U\n#EXTINF:4,\n#EXT-X-GAP\n0.m4s\n", unsupported, "EXT-X-GAP"},
		{"delta update", false, "#EXTM3U\n#EXT-X-SKIP:SKIPPED-SEGMENTS=3\n#EXTINF:4,\n0.m4s\n", unsupported, "EXT-X-SKIP"},
		{"I-frame playlist", false, "#EXTM3U\n#EXT-X-I-FRAMES-ONLY\n#EXTINF:4,\n0.m4s\n", unsupported, "EXT-X-I-FRAMES-ONLY"},
		{"second init", false, "#EXTM3U\n#EXT-X-MAP:URI=\"a.mp4\"\n#EXTINF:4,\n0.m4s\n#EXT-X-MAP:URI=\"b.mp4\"\n#EXTINF:4,\n1.m4s\n", unsupported, "EXT-X-MAP"},
		{"init after a segment", false, "#EXTM3U\n#EXTINF:4,\n0.ts\n#EXT-X-MAP:URI=\"i.mp4\"\n#EXTINF:4,\n1.m4s\n", unsupported, "EXT-X-MAP"},
		{"line past the cap", false, "#EXTM3U\n" + strings.Repeat("a", maxLineLen+1) + "\n", unsupported, ""},
		{"lines past the cap", false, "#EXTM3U" + strings.Repeat("\n", MaxLines), unsupported, ""},

		// EXT-X-MEDIA (RFC 8216 4.3.4.1; 4.3.4.2.1 for SUBTITLES).
		{"rendition with no TYPE", true, "#EXTM3U\n#EXT-X-MEDIA:GROUP-ID=\"a\",NAME=\"n\"\n" + variant, malformed, "TYPE"},
		{"rendition with no GROUP-ID", true, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,NAME=\"n\"\n" + variant, malformed, "GROUP-ID"},
		{"rendition with an empty GROUP-ID", true, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"\",NAME=\"n\"\n" + variant, malformed, "GROUP-ID"},
		{"rendition with no NAME", true, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\"\n" + variant, malformed, "NAME"},
		{"captions with a URI", true, "#EXTM3U\n#EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS,GROUP-ID=\"cc\",NAME=\"n\",INSTREAM-ID=\"CC1\",URI=\"c.m3u8\"\n" + variant, malformed, "URI"},
		{"subtitles with no URI", true, "#EXTM3U\n#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"s\",NAME=\"n\"\n" + variant, malformed, "URI"},
		{"captions with no INSTREAM-ID", true, "#EXTM3U\n#EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS,GROUP-ID=\"cc\",NAME=\"n\"\n" + variant, malformed, "INSTREAM-ID"},
		{"INSTREAM-ID on audio", true, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"n\",INSTREAM-ID=\"CC1\"\n" + variant, malformed, "INSTREAM-ID"},

		// EXT-X-STREAM-INF (4.3.4.2).
		{"AUDIO names no group", true, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"a\"\nv.m3u8\n", malformed, "AUDIO"},
		{"AUDIO names a video group", true, "#EXTM3U\n#EXT-X-MEDIA:TYPE=VIDEO,GROUP-ID=\"a\",NAME=\"n\"\n#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"a\"\nv.m3u8\n", malformed, "AUDIO"},
		{"AUDIO names a dropped rendition", true, "#EXTM3U\n#EXT-X-MEDIA:TYPE=audio,GROUP-ID=\"a\",NAME=\"n\"\n#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"a\"\nv.m3u8\n", malformed, `TYPE "audio"`},
		{"empty AUDIO", true, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"n\"\n#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"\"\nv.m3u8\n", malformed, "AUDIO"},
		{"empty rendition URI", true, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"n\",URI=\"\"\n" + variant, malformed, "URI"},
		{"empty INSTREAM-ID", true, "#EXTM3U\n#EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS,GROUP-ID=\"cc\",NAME=\"n\",INSTREAM-ID=\"\"\n" + variant, malformed, "INSTREAM-ID"},
		{"empty TYPE", true, "#EXTM3U\n#EXT-X-MEDIA:TYPE=,GROUP-ID=\"a\",NAME=\"n\"\n" + variant, malformed, "TYPE"},
		{"quoted BANDWIDTH", true, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=\"5\"\nv.m3u8\n", malformed, "BANDWIDTH"},
		{"quoted TYPE", true, "#EXTM3U\n#EXT-X-MEDIA:TYPE=\"AUDIO\",GROUP-ID=\"a\",NAME=\"n\"\n" + variant, malformed, "TYPE"},
		{"quoted DEFAULT", true, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"n\",DEFAULT=\"YES\"\n" + variant, malformed, "DEFAULT"},
		{"no BANDWIDTH", true, "#EXTM3U\n#EXT-X-STREAM-INF:CODECS=\"Opus\"\nv.m3u8\n", malformed, "BANDWIDTH"},
		{"negative BANDWIDTH", true, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=-1\nv.m3u8\n", malformed, "BANDWIDTH"},
		{"signed BANDWIDTH", true, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=+5\nv.m3u8\n", malformed, "BANDWIDTH"},
		{"BANDWIDTH not a number", true, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=abc\nv.m3u8\n", malformed, "BANDWIDTH"},
		{"empty BANDWIDTH", true, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=\nv.m3u8\n", malformed, "BANDWIDTH"},
		{"BANDWIDTH of 21 digits", true, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=000000000000000000001\nv.m3u8\n", malformed, "BANDWIDTH"},
		{"BANDWIDTH past 2^64-1", true, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=18446744073709551616\nv.m3u8\n", malformed, "BANDWIDTH"},
		{"BANDWIDTH past an int", true, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=9223372036854775808\nv.m3u8\n", unsupported, "BANDWIDTH"},
		{"two STREAM-INF before a URI", true, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\n" + variant, malformed, "EXT-X-STREAM-INF"},
		{"URI with no STREAM-INF", true, "#EXTM3U\nstray.m3u8\n" + variant, malformed, "stray.m3u8"},
		{"bare STREAM-INF", true, "#EXTM3U\n#EXT-X-STREAM-INF\nv0.m3u8\n" + variant, malformed, "EXT-X-STREAM-INF"},

		// One playlist, one kind, one version (4.3.1.2, 4.3.3, 4.3.4).
		{"two VERSION tags in a master", true, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-VERSION:7\n" + variant, malformed, "EXT-X-VERSION"},
		{"two VERSION tags in a media playlist", false, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-VERSION:3\n" + segment, malformed, "EXT-X-VERSION"},
		{"media playlist tag in a master", true, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n" + variant, malformed, "EXT-X-TARGETDURATION"},
		{"segment tag in a master", true, "#EXTM3U\n#EXT-X-KEY:METHOD=NONE\n" + variant, malformed, "EXT-X-KEY"},
		{"EXTINF in a master", true, "#EXTM3U\n" + variant + segment, malformed, "EXTINF"},
		{"ENDLIST in a master", true, "#EXTM3U\n" + variant + "#EXT-X-ENDLIST\n", malformed, "EXT-X-ENDLIST"},
		{"rendition in a media playlist", false, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"n\"\n" + segment, malformed, "EXT-X-MEDIA"},
		{"session data in a media playlist", false, "#EXTM3U\n#EXT-X-SESSION-DATA:DATA-ID=\"d\",VALUE=\"v\"\n" + segment, malformed, "EXT-X-SESSION-DATA"},
		{"STREAM-INF alone in a media playlist", false, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\n", malformed, "EXT-X-STREAM-INF"},
		{"two TARGETDURATION tags", false, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-TARGETDURATION:6\n" + segment, malformed, "EXT-X-TARGETDURATION"},
		{"two MEDIA-SEQUENCE tags", false, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:1\n#EXT-X-MEDIA-SEQUENCE:2\n" + segment, malformed, "EXT-X-MEDIA-SEQUENCE"},
		{"two DISCONTINUITY-SEQUENCE tags", false, "#EXTM3U\n#EXT-X-DISCONTINUITY-SEQUENCE:1\n#EXT-X-DISCONTINUITY-SEQUENCE:1\n" + segment, malformed, "EXT-X-DISCONTINUITY-SEQUENCE"},
		{"two ENDLIST tags", false, "#EXTM3U\n" + segment + "#EXT-X-ENDLIST\n#EXT-X-ENDLIST\n", malformed, "EXT-X-ENDLIST"},
		{"two PLAYLIST-TYPE tags", false, "#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-PLAYLIST-TYPE:EVENT\n" + segment, malformed, "EXT-X-PLAYLIST-TYPE"},
		{"two INDEPENDENT-SEGMENTS tags in a master", true, "#EXTM3U\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-INDEPENDENT-SEGMENTS\n" + variant, malformed, "EXT-X-INDEPENDENT-SEGMENTS"},
		{"two INDEPENDENT-SEGMENTS tags in a media playlist", false, "#EXTM3U\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-INDEPENDENT-SEGMENTS\n" + segment, malformed, "EXT-X-INDEPENDENT-SEGMENTS"},
		{"two START tags in a master", true, "#EXTM3U\n#EXT-X-START:TIME-OFFSET=0\n#EXT-X-START:TIME-OFFSET=0\n" + variant, malformed, "EXT-X-START"},
		{"two START tags in a media playlist", false, "#EXTM3U\n#EXT-X-START:TIME-OFFSET=0\n#EXT-X-START:TIME-OFFSET=0\n" + segment, malformed, "EXT-X-START"},
		{"bare KEY", false, "#EXTM3U\n#EXT-X-KEY\n" + segment, malformed, "METHOD"},
		{"KEY with no METHOD", false, "#EXTM3U\n#EXT-X-KEY:URI=\"k\"\n" + segment, malformed, "METHOD"},
		{"quoted METHOD", false, "#EXTM3U\n#EXT-X-KEY:METHOD=\"NONE\"\n" + segment, malformed, "METHOD"},
		{"bare MAP", false, "#EXTM3U\n#EXT-X-MAP\n" + segment, malformed, "URI"},
		{"MAP with no URI", false, "#EXTM3U\n#EXT-X-MAP:BYTERANGE=\"800@0\"\n" + segment, malformed, "URI"},

		// Variable substitution is rfc8216bis's; an unsubstituted URI cannot
		// be fetched.
		{"EXT-X-DEFINE in a master", true, "#EXTM3U\n#EXT-X-DEFINE:NAME=\"x\",VALUE=\"y\"\n" + variant, unsupported, "EXT-X-DEFINE"},
		{"EXT-X-DEFINE in a media playlist", false, "#EXTM3U\n#EXT-X-DEFINE:NAME=\"x\",VALUE=\"y\"\n" + segment, unsupported, "EXT-X-DEFINE"},

		// Attribute lists (4.2) and the BOM (4.1).
		{"repeated attribute in a master", true, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,BANDWIDTH=2\nv.m3u8\n", malformed, "BANDWIDTH"},
		{"repeated rendition attribute", true, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"n\",NAME=\"m\"\n" + variant, malformed, "NAME"},
		{"repeated attribute in a media playlist", false, "#EXTM3U\n#EXT-X-MAP:URI=\"a.mp4\",URI=\"b.mp4\"\n" + segment, malformed, "URI"},
		{"attribute with no = in a master", true, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,JUNK\nv.m3u8\n", malformed, "JUNK"},
		{"attribute with no = in a media playlist", false, "#EXTM3U\n#EXT-X-KEY:METHOD=NONE,JUNK\n" + segment, malformed, "JUNK"},
		{"attribute with no name", true, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,=5\nv.m3u8\n", malformed, "=5"},
		{"BOM before a media playlist", false, "\xef\xbb\xbf#EXTM3U\n" + segment, malformed, "byte order mark"},
		{"BOM before a master", true, "\xef\xbb\xbf#EXTM3U\n" + variant, malformed, "byte order mark"},
		{"open quote in a master", true, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"main\",URI=\"audio.m3u8\"\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=1,CODECS=\"avc1.64001f,mp4a.40.2,AUDIO=\"aud\"\nvideo.m3u8\n", malformed, "quote"},
		{"quote inside a quoted value", true, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,CODECS=\"a\"b\"c\"\nv.m3u8\n", malformed, "quote"},
		{"quote inside an unquoted value", true, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,CODECS=a\"b\"\nv.m3u8\n", malformed, "quote"},
		{"quoted attribute name", true, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,\"AUDIO\"=\"a\"\nv.m3u8\n", malformed, "quote"},
		{"open quote in a media playlist", false, "#EXTM3U\n#EXT-X-MAP:URI=\"i.mp4\n" + segment, malformed, "quote"},
	} {
		parse := func(s string) error { _, err := ParseMedia(s); return err }
		if tc.master {
			parse = func(s string) error { _, err := ParseMaster(s); return err }
		}
		err := parse(tc.in)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if got := waxerr.CodeOf(err); got != tc.code {
			t.Errorf("%s: code %s, want %s (%v)", tc.name, got, tc.code, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %q does not name %q", tc.name, err, tc.want)
		}
	}
}

// TestParseReadsWhatItCanDescribe is the other side of the refusals: tags that
// leave every listed segment a whole, clear resource behind the one init are
// read past, and durations in either of RFC 8216's unsigned spellings parse.
func TestParseReadsWhatItCanDescribe(t *testing.T) {
	in := "#EXTM3U\r\n#EXT-X-VERSION:7\r\n#EXT-X-KEY:METHOD=NONE\r\n#EXT-X-MAP:URI=\"i.mp4\"\r\n" +
		"#EXT-X-PROGRAM-DATE-TIME:2026-10-30T00:00:00Z\r\n#EXTINF:4,\r\n0.m4s\r\n" +
		"#EXT-X-DISCONTINUITY\r\n#EXT-X-MAP:URI=\"i.mp4\"\r\n#EXTINF:2.5,title\r\n1.m4s\r\n" +
		"#EXTINF:.5,\r\n2.m4s\r\n#EXTINF:3.,\r\n3.m4s\r\n#EXT-X-ENDLIST\r\n"
	pl, err := ParseMedia(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []MediaSegment{{"0.m4s", 4}, {"1.m4s", 2.5}, {"2.m4s", 0.5}, {"3.m4s", 3}}
	if pl.InitURI != "i.mp4" || !pl.End || !reflect.DeepEqual(pl.Segments, want) {
		t.Errorf("read %+v", pl)
	}

	// A master's tags that do not move a variant's media are read past
	// (RFC 8216 6.3.1), with the attributes nothing selects on and a stray
	// comma.
	master := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-START:TIME-OFFSET=0\n" +
		"#EXT-X-SESSION-DATA:DATA-ID=\"com.example.title\",VALUE=\"t\"\n" +
		"#EXT-X-SESSION-KEY:METHOD=SAMPLE-AES,URI=\"k\"\n#EXT-X-CONTENT-STEERING:SERVER-URI=\"s.json\"\n" +
		"#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=1,URI=\"i.m3u8\"\n#EXT-X-SOMETHING-NEW:X=1\n# a comment\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1,CODECS=\"Opus\",RESOLUTION=1x1,CLOSED-CAPTIONS=NONE,\nv.m3u8\n"
	mpl, err := ParseMaster(master)
	if err != nil {
		t.Fatal(err)
	}
	if want := []MasterVariant{{URI: "v.m3u8", Bandwidth: 1, Codecs: "Opus"}}; !reflect.DeepEqual(mpl.Variants, want) {
		t.Errorf("read %+v", mpl)
	}

	// A quoted-string written bare reads as written.
	bare := "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=a,NAME=Main,URI=a.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1,CODECS=Opus,AUDIO=a\nv.m3u8\n"
	if bpl, err := ParseMaster(bare); err != nil ||
		!reflect.DeepEqual(bpl.Media, []Rendition{{Type: "AUDIO", GroupID: "a", Name: "Main", URI: "a.m3u8"}}) ||
		!reflect.DeepEqual(bpl.Variants, []MasterVariant{{URI: "v.m3u8", Bandwidth: 1, Codecs: "Opus", Audio: "a"}}) {
		t.Errorf("bare quoted-strings read as %+v (%v)", bpl, err)
	}

	// ENDLIST takes no value; one written with a colon is still the tag the
	// repeat rule counts, so it ends the list.
	if pl, err := ParseMedia("#EXTM3U\n#EXTINF:4,\n0.m4s\n#EXT-X-ENDLIST:\n"); err != nil || !pl.End {
		t.Errorf("an EXT-X-ENDLIST: read as End=%v (%v)", pl.End, err)
	}
}

// TestParseRefusalsQuoteThePlaylist: a refusal that names playlist bytes
// quotes them, so a control byte in a fetched playlist never reaches a log
// raw.
func TestParseRefusalsQuoteThePlaylist(t *testing.T) {
	const esc = "\x1b[2J"
	master := func(s string) error { _, err := ParseMaster(s); return err }
	media := func(s string) error { _, err := ParseMedia(s); return err }
	for _, tc := range []struct {
		parse func(string) error
		in    string
	}{
		{master, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1," + esc + "=1," + esc + "=2\nv.m3u8\n"},
		{master, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1," + esc + "\nv.m3u8\n"},
		{master, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=" + esc + "\nv.m3u8\n"},
		{master, "#EXTM3U\n" + esc + ".m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv.m3u8\n"},
		{master, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"" + esc + "\"\nv.m3u8\n"},
		{master, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,CODECS=\"" + esc + "\nv.m3u8\n"},
		{media, "#EXTM3U\n#EXT-X-KEY:METHOD=" + esc + "\n#EXTINF:4,\n0.m4s\n"},
		{media, "#EXTM3U\n#EXT-X-MAP:URI=\"a.mp4\"\n#EXTINF:4,\n0.m4s\n#EXT-X-MAP:URI=\"" + esc + "\"\n"},
		{media, "#EXTM3U\n#EXTINF:" + esc + ",\n0.m4s\n"},
		{media, "#EXTM3U\n" + esc + ".m4s\n"},
	} {
		if err := tc.parse(tc.in); err == nil || strings.ContainsRune(err.Error(), 0x1b) {
			t.Errorf("parse(%q): %q", tc.in, err)
		}
	}
}

// TestParseRefusesTheOtherKindsTags walks RFC 8216's tag tables: a client
// fails a playlist that mixes a master tag with a media playlist or media
// segment tag (4.3.4), so each parser refuses every tag of the other kind.
func TestParseRefusesTheOtherKindsTags(t *testing.T) {
	master := []string{"EXT-X-MEDIA", "EXT-X-STREAM-INF", "EXT-X-I-FRAME-STREAM-INF",
		"EXT-X-SESSION-DATA", "EXT-X-SESSION-KEY"}
	media := []string{"EXT-X-TARGETDURATION", "EXT-X-MEDIA-SEQUENCE", "EXT-X-DISCONTINUITY-SEQUENCE",
		"EXT-X-ENDLIST", "EXT-X-PLAYLIST-TYPE", "EXT-X-I-FRAMES-ONLY", "EXTINF", "EXT-X-BYTERANGE",
		"EXT-X-DISCONTINUITY", "EXT-X-KEY", "EXT-X-MAP", "EXT-X-PROGRAM-DATE-TIME", "EXT-X-DATERANGE"}
	for _, tag := range master {
		_, err := ParseMedia("#EXTM3U\n#" + tag + "\n#EXTINF:4,\n0.m4s\n")
		if waxerr.CodeOf(err) != waxerr.CodeMalformedInput || !strings.Contains(fmt.Sprint(err), tag+" in a media playlist") {
			t.Errorf("ParseMedia with %s: %v", tag, err)
		}
	}
	for _, tag := range media {
		_, err := ParseMaster("#EXTM3U\n#" + tag + "\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv.m3u8\n")
		if waxerr.CodeOf(err) != waxerr.CodeMalformedInput || !strings.Contains(fmt.Sprint(err), tag+" in a master playlist") {
			t.Errorf("ParseMaster with %s: %v", tag, err)
		}
	}
}

// TestParseReadsAtTheCaps pins the caps' other edge: a playlist of exactly
// MaxLines lines, one of them maxLineLen bytes long, is read.
func TestParseReadsAtTheCaps(t *testing.T) {
	head := "#EXTM3U\n#EXTINF:4.0,\n" + strings.Repeat("a", maxLineLen) + "\n"
	in := head + strings.Repeat("\n", MaxLines-strings.Count(head, "\n")-1)
	if n := strings.Count(in, "\n") + 1; n != MaxLines {
		t.Fatalf("fixture has %d lines, want %d", n, MaxLines)
	}
	pl, err := ParseMedia(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Segments) != 1 || len(pl.Segments[0].URI) != maxLineLen {
		t.Errorf("read %d segments, want the one %d-byte URI", len(pl.Segments), maxLineLen)
	}
}

// TestIsMaster pins the line-anchored master/media discrimination: a media
// playlist that merely mentions the STREAM-INF tag text in a title is not a
// master, while a real STREAM-INF tag line is, CRLF or not.
func TestIsMaster(t *testing.T) {
	for in, want := range map[string]bool{
		"#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv0.m3u8\n":                                     true,
		"#EXTM3U\r\n  #EXT-X-STREAM-INF:BANDWIDTH=1\r\nv0.m3u8":                                 true,
		"#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,see #EXT-X-STREAM-INF notes\nseg0.m4s\n": false,
		"#EXTM3U\n#EXT-X-STREAM-INFO:X=1\nv0.m3u8\n":                                            false,
		"#EXTM3U\n#EXT-X-STREAM-INF\nv0.m3u8\n":                                                 true,
		"":                                                                                      false,
	} {
		if got := IsMaster(in); got != want {
			t.Errorf("IsMaster(%q) = %v, want %v", in, got, want)
		}
	}
}

// FuzzParse holds both parsers on arbitrary input: a refusal carries one of
// their two codes, and what they accept is what their types promise: a
// finite, non-negative duration and a URI on every segment and variant, every
// rendition a named member of a group with the URI and INSTREAM-ID its TYPE
// calls for, and every AUDIO naming an audio group.
func FuzzParse(f *testing.F) {
	f.Add("#EXTM3U\n#EXT-X-MAP:URI=\"i.mp4\"\n#EXTINF:4,\n0.m4s\n#EXTINF:.5,\n1.m4s\n#EXT-X-ENDLIST\n")
	f.Add("#EXTM3U\r\n#EXT-X-STREAM-INF:BANDWIDTH=1,CODECS=\"a,b\"\r\nv.m3u8\r\n")
	f.Add("#EXTM3U\n#EXTINF:NaN,\n0.m4s\n#EXTINF:-1,\n")
	f.Add("#EXTM3U\n#EXT-X-KEY:METHOD=NONE\n#EXT-X-BYTERANGE:1@0\n#EXTINF:4,\n0.m4s\n")
	f.Add("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"a\"\nv.m3u8\n" +
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"n\",DEFAULT=YES,URI=\"a.m3u8\"\n" +
		"#EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS,GROUP-ID=\"c\",NAME=\"n\",INSTREAM-ID=\"CC1\"\n" +
		"#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"s\",NAME=\"n\",URI=\"s.m3u8\"\n")
	f.Add("\xef\xbb\xbf#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:4\n#EXT-X-DEFINE:NAME=\"x\"\n")
	f.Add("#EXTM3U\n#EXT-X-START:TIME-OFFSET=0\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=a,NAME=\"n\",DEFAULT=yes,URI=\"\"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=\"1\",AUDIO=\"\"\nv.m3u8\n#EXT-X-KEY\n#EXT-X-MAP\n")
	coded := func(t *testing.T, err error) {
		if c := waxerr.CodeOf(err); c != waxerr.CodeMalformedInput && c != waxerr.CodeUnsupportedFormat {
			t.Fatalf("refused with code %s: %v", c, err)
		}
	}
	f.Fuzz(func(t *testing.T, s string) {
		if pl, err := ParseMedia(s); err != nil {
			coded(t, err)
		} else {
			for i, seg := range pl.Segments {
				if !(seg.Seconds >= 0) || math.IsInf(seg.Seconds, 0) || seg.URI == "" {
					t.Fatalf("segment %d read as %+v", i, seg)
				}
			}
		}
		if pl, err := ParseMaster(s); err != nil {
			coded(t, err)
		} else {
			audio := map[string]bool{}
			for i, r := range pl.Media {
				ok := r.GroupID != "" && r.Name != "" && (r.InstreamID == "") == (r.Type != "CLOSED-CAPTIONS")
				switch r.Type {
				case "AUDIO":
					audio[r.GroupID] = true
				case "VIDEO":
				case "SUBTITLES":
					ok = ok && r.URI != ""
				case "CLOSED-CAPTIONS":
					ok = ok && r.URI == ""
				default:
					ok = false
				}
				if !ok {
					t.Fatalf("rendition %d read as %+v", i, r)
				}
			}
			for i, v := range pl.Variants {
				if v.URI == "" || v.Bandwidth < 0 || v.Audio != "" && !audio[v.Audio] {
					t.Fatalf("variant %d read as %+v", i, v)
				}
			}
		}
	})
}
