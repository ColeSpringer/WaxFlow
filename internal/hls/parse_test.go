package hls

import (
	"reflect"
	"testing"
)

// TestMasterRoundTrip writes a master playlist and parses it back, expecting
// the variant ladder to survive intact.
func TestMasterRoundTrip(t *testing.T) {
	variants := []MasterVariant{
		{URI: "audio_64k/index.m3u8", Bandwidth: 64000, Codecs: "mp4a.40.2"},
		{URI: "audio_128k/index.m3u8?v=abc", Bandwidth: 128000, Codecs: "Opus"},
	}
	got, err := ParseMaster(Master(variants))
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
	pl, err := ParseMedia(Media("init.mp4", segs))
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

// TestParseRejects checks the malformed-input guards.
func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"no signature":  "#EXT-X-VERSION:7\nv.m3u8\n",
		"stream no uri": "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\n",
		"uri no extinf": "#EXTM3U\n#EXT-X-TARGETDURATION:4\n0.m4s\n",
		"bad extinf":    "#EXTM3U\n#EXTINF:notanumber,\n0.m4s\n",
		"no variants":   "#EXTM3U\n#EXT-X-VERSION:7\n",
	}
	for name, in := range cases {
		if name == "no variants" || name == "stream no uri" {
			if _, err := ParseMaster(in); err == nil {
				t.Errorf("%s: ParseMaster accepted malformed input", name)
			}
			continue
		}
		if _, err := ParseMedia(in); err == nil {
			t.Errorf("%s: ParseMedia accepted malformed input", name)
		}
	}
}
