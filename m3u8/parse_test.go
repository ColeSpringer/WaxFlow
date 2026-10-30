package m3u8

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/waxerr"
)

// TestMasterRoundTrip writes a master playlist and parses it back, expecting
// the variant ladder to survive intact.
func TestMasterRoundTrip(t *testing.T) {
	variants := []MasterVariant{
		{URI: "audio_64k/index.m3u8", Bandwidth: 64000, Codecs: "mp4a.40.2"},
		{URI: "audio_128k/index.m3u8?v=abc", Bandwidth: 128000, Codecs: "Opus"},
	}
	text, err := Master(variants)
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
	if !reflect.DeepEqual(got.Variants, variants) {
		t.Errorf("variants round-trip mismatch:\n got %+v\nwant %+v", got.Variants, variants)
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
		"": false,
	} {
		if got := IsMaster(in); got != want {
			t.Errorf("IsMaster(%q) = %v, want %v", in, got, want)
		}
	}
}

// FuzzParse holds both parsers on arbitrary input: a refusal carries one of
// their two codes, and what they accept is what their types promise, a
// finite, non-negative duration and a URI on every segment and variant.
func FuzzParse(f *testing.F) {
	f.Add("#EXTM3U\n#EXT-X-MAP:URI=\"i.mp4\"\n#EXTINF:4,\n0.m4s\n#EXTINF:.5,\n1.m4s\n#EXT-X-ENDLIST\n")
	f.Add("#EXTM3U\r\n#EXT-X-STREAM-INF:BANDWIDTH=1,CODECS=\"a,b\"\r\nv.m3u8\r\n")
	f.Add("#EXTM3U\n#EXTINF:NaN,\n0.m4s\n#EXTINF:-1,\n")
	f.Add("#EXTM3U\n#EXT-X-KEY:METHOD=NONE\n#EXT-X-BYTERANGE:1@0\n#EXTINF:4,\n0.m4s\n")
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
			for i, v := range pl.Variants {
				if v.URI == "" {
					t.Fatalf("variant %d read with no URI", i)
				}
			}
		}
	})
}
