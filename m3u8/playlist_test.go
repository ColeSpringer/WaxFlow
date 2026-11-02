package m3u8

import (
	"math"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/waxerr"
)

func TestPlaylists(t *testing.T) {
	master, err := Master(MasterPlaylist{Variants: []MasterVariant{
		{URI: "media.m3u8?v=abc", Bandwidth: 100000, Codecs: "Opus"},
		{URI: "media.m3u8?v=def", Bandwidth: 1500000, Codecs: "fLaC"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n",
		"#EXT-X-STREAM-INF:BANDWIDTH=100000,CODECS=\"Opus\"\nmedia.m3u8?v=abc\n",
		"#EXT-X-STREAM-INF:BANDWIDTH=1500000,CODECS=\"fLaC\"\nmedia.m3u8?v=def\n",
	} {
		if !strings.Contains(master, want) {
			t.Errorf("master missing %q:\n%s", want, master)
		}
	}

	media, err := Media("init.mp4?v=abc", []MediaSegment{
		{URI: "seg/0.m4s?v=abc", Seconds: 4},
		{URI: "seg/1.m4s?v=abc", Seconds: 2.02},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"#EXT-X-TARGETDURATION:4\n",
		"#EXT-X-PLAYLIST-TYPE:VOD\n",
		"#EXT-X-MAP:URI=\"init.mp4?v=abc\"\n",
		"#EXTINF:4.00000,\nseg/0.m4s?v=abc\n",
		"#EXTINF:2.02000,\nseg/1.m4s?v=abc\n",
		"#EXT-X-ENDLIST\n",
	} {
		if !strings.Contains(media, want) {
			t.Errorf("media playlist missing %q:\n%s", want, media)
		}
	}
	if !strings.HasSuffix(media, "#EXT-X-ENDLIST\n") {
		t.Error("ENDLIST is not last")
	}
}

// TestMasterRefusesWhatM3U8CannotCarry pins the writer's refusals: M3U8 has
// no escape, so a value that would change the playlist's structure, or read
// back as something else, is refused rather than written, and so is a
// rendition RFC 8216 forbids a server to write.
func TestMasterRefusesWhatM3U8CannotCarry(t *testing.T) {
	ladder := func(vs ...MasterVariant) MasterPlaylist { return MasterPlaylist{Variants: vs} }
	v := MasterVariant{URI: "v.m3u8", Bandwidth: 1}
	with := func(rs ...Rendition) MasterPlaylist { return MasterPlaylist{Media: rs, Variants: []MasterVariant{v}} }
	aud := Rendition{Type: "AUDIO", GroupID: "a", Name: "n"}
	cc := Rendition{Type: "CLOSED-CAPTIONS", GroupID: "cc", Name: "n", InstreamID: "CC1"}
	edit := func(r Rendition, f func(*Rendition)) Rendition { f(&r); return r }
	for _, tc := range []struct {
		name string
		pl   MasterPlaylist
	}{
		{"no variants", ladder()},
		{"renditions but no variants", MasterPlaylist{Media: []Rendition{aud}}},
		{"empty URI", ladder(MasterVariant{URI: "", Bandwidth: 1, Codecs: "Opus"})},
		{"URI read as a tag", ladder(MasterVariant{URI: "#v.m3u8", Bandwidth: 1, Codecs: "Opus"})},
		{"LF in URI", ladder(MasterVariant{URI: "v.m3u8\n#EXT-X-ENDLIST", Bandwidth: 1, Codecs: "Opus"})},
		{"CR in URI", ladder(MasterVariant{URI: "v\r.m3u8", Bandwidth: 1, Codecs: "Opus"})},
		{"quote in URI", ladder(MasterVariant{URI: `v".m3u8`, Bandwidth: 1, Codecs: "Opus"})},
		{"padded URI", ladder(MasterVariant{URI: "v.m3u8 ", Bandwidth: 1, Codecs: "Opus"})},
		{"space in URI", ladder(MasterVariant{URI: "v 0.m3u8", Bandwidth: 1, Codecs: "Opus"})},
		{"quote in CODECS", ladder(MasterVariant{URI: "v.m3u8", Bandwidth: 1, Codecs: `Opus",X="`})},
		{"LF in CODECS", ladder(MasterVariant{URI: "v.m3u8", Bandwidth: 1, Codecs: "Opus\nv2.m3u8"})},
		{"control in CODECS", ladder(MasterVariant{URI: "v.m3u8", Bandwidth: 1, Codecs: "Op\x00us"})},
		{"negative BANDWIDTH", ladder(MasterVariant{URI: "v.m3u8", Bandwidth: -1, Codecs: "Opus"})},
		{"second variant bad", ladder(
			MasterVariant{URI: "v0.m3u8", Bandwidth: 1, Codecs: "Opus"}, MasterVariant{URI: "", Bandwidth: 1, Codecs: "Opus"})},

		{"no TYPE", with(edit(aud, func(r *Rendition) { r.Type = "" }))},
		{"unknown TYPE", with(edit(aud, func(r *Rendition) { r.Type = "TEXT" }))},
		{"lower-case TYPE", with(edit(aud, func(r *Rendition) { r.Type = "audio" }))},
		{"no GROUP-ID", with(edit(aud, func(r *Rendition) { r.GroupID = "" }))},
		{"no NAME", with(edit(aud, func(r *Rendition) { r.Name = "" }))},
		{"quote in GROUP-ID", with(edit(aud, func(r *Rendition) { r.GroupID = `a",X="` }))},
		{"LF in NAME", with(edit(aud, func(r *Rendition) { r.Name = "n\n#EXT-X-ENDLIST" }))},
		{"control in LANGUAGE", with(edit(aud, func(r *Rendition) { r.Language = "e\x01n" }))},
		{"quote in CHANNELS", with(edit(aud, func(r *Rendition) { r.Channels = `2"` }))},
		{"space in rendition URI", with(edit(aud, func(r *Rendition) { r.URI = "a b.m3u8" }))},
		{"empty-escape rendition URI", with(edit(aud, func(r *Rendition) { r.URI = "a%.m3u8" }))},
		{"captions with a URI", with(edit(cc, func(r *Rendition) { r.URI = "cc.m3u8" }))},
		{"captions with no INSTREAM-ID", with(edit(cc, func(r *Rendition) { r.InstreamID = "" }))},
		{"INSTREAM-ID CC5", with(edit(cc, func(r *Rendition) { r.InstreamID = "CC5" }))},
		{"INSTREAM-ID SERVICE64", with(edit(cc, func(r *Rendition) { r.InstreamID = "SERVICE64" }))},
		{"INSTREAM-ID SERVICE07", with(edit(cc, func(r *Rendition) { r.InstreamID = "SERVICE07" }))},
		{"quote in INSTREAM-ID", with(edit(cc, func(r *Rendition) { r.InstreamID = `CC1"` }))},
		{"INSTREAM-ID on audio", with(edit(aud, func(r *Rendition) { r.InstreamID = "CC1" }))},
		{"subtitles with no URI", with(Rendition{Type: "SUBTITLES", GroupID: "s", Name: "n"})},
		{"two defaults in a group", with(edit(aud, func(r *Rendition) { r.Default = true }),
			Rendition{Type: "AUDIO", GroupID: "a", Name: "m", Default: true})},
		{"a NAME twice in a group", with(aud, aud)},
		{"AUDIO naming no group", ladder(MasterVariant{URI: "v.m3u8", Bandwidth: 1, Audio: "a"})},
		{"AUDIO naming a video group", MasterPlaylist{Media: []Rendition{{Type: "VIDEO", GroupID: "a", Name: "n"}},
			Variants: []MasterVariant{{URI: "v.m3u8", Bandwidth: 1, Audio: "a"}}}},
		{"quote in AUDIO", MasterPlaylist{Media: []Rendition{aud},
			Variants: []MasterVariant{{URI: "v.m3u8", Bandwidth: 1, Audio: `a"`}}}},
	} {
		text, err := Master(tc.pl)
		if err == nil {
			t.Errorf("%s: written:\n%s", tc.name, text)
			continue
		}
		if got := waxerr.CodeOf(err); got != waxerr.CodeInvalidRequest {
			t.Errorf("%s: code %s, want %s (%v)", tc.name, got, waxerr.CodeInvalidRequest, err)
		}
		if text != "" {
			t.Errorf("%s: refused, but returned text", tc.name)
		}
	}
}

// TestMasterWritesRenditions pins the rendition and variant lines exactly,
// and what the group rules leave writable: a GROUP-ID under two TYPEs is two
// groups, a NAME may repeat across groups, and DEFAULT=YES needs no
// AUTOSELECT (RFC 8216 4.3.4.1).
func TestMasterWritesRenditions(t *testing.T) {
	text, err := Master(MasterPlaylist{
		Media: []Rendition{
			{Type: "AUDIO", GroupID: "aud", Name: "English", URI: "audio/en.m3u8", Language: "en",
				Default: true, Autoselect: true, Channels: "2"},
			{Type: "CLOSED-CAPTIONS", GroupID: "cc", Name: "English", InstreamID: "CC1"},
		},
		Variants: []MasterVariant{{URI: "video.m3u8", Bandwidth: 1000000, Codecs: "avc1.4d401f,mp4a.40.2", Audio: "aud"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n" +
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"English\",LANGUAGE=\"en\",DEFAULT=YES,AUTOSELECT=YES," +
		"CHANNELS=\"2\",URI=\"audio/en.m3u8\"\n" +
		"#EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS,GROUP-ID=\"cc\",NAME=\"English\",INSTREAM-ID=\"CC1\"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1000000,CODECS=\"avc1.4d401f,mp4a.40.2\",AUDIO=\"aud\"\nvideo.m3u8\n"
	if text != want {
		t.Errorf("master:\n%s\nwant:\n%s", text, want)
	}

	legal := MasterPlaylist{
		Media: []Rendition{
			{Type: "AUDIO", GroupID: "g", Name: "main", Default: true},
			{Type: "VIDEO", GroupID: "g", Name: "main", Default: true, URI: "cam.m3u8"},
			{Type: "AUDIO", GroupID: "h", Name: "main", Default: true},
		},
		Variants: []MasterVariant{{URI: "v.m3u8", Bandwidth: 1, Audio: "g"}, {URI: "w.m3u8", Bandwidth: 2, Audio: "h"}},
	}
	text, err = Master(legal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"g\",NAME=\"main\",DEFAULT=YES\n") {
		t.Errorf("DEFAULT=YES written with an AUTOSELECT, or not at all:\n%s", text)
	}
	got, err := ParseMaster(text)
	if err != nil || !reflect.DeepEqual(got.Media, legal.Media) || !reflect.DeepEqual(got.Variants, legal.Variants) {
		t.Errorf("read back %+v (%v)", got, err)
	}
}

// TestMediaRefusesWhatM3U8CannotCarry is Master's refusal table for a media
// playlist, plus the durations: EXTINF is an unsigned decimal, so a negative
// or non-finite one has no spelling, and one whose target duration an int
// cannot hold on a 32-bit platform is refused with them.
func TestMediaRefusesWhatM3U8CannotCarry(t *testing.T) {
	seg := []MediaSegment{{URI: "0.m4s", Seconds: 4}}
	at := func(uri string, secs float64) []MediaSegment {
		return []MediaSegment{{URI: "0.m4s", Seconds: 4}, {URI: uri, Seconds: secs}}
	}
	many := make([]MediaSegment, MaxLines/2)
	for i := range many {
		many[i] = MediaSegment{URI: "s.m4s", Seconds: 1}
	}
	for _, tc := range []struct {
		name string
		init string
		segs []MediaSegment
	}{
		{"empty init", "", seg},
		{"quote in init", `init.mp4"`, seg},
		{"LF in init", "init.mp4\n#EXT-X-ENDLIST", seg},
		{"empty segment URI", "init.mp4", at("", 4)},
		{"segment URI read as a tag", "init.mp4", at("#EXT-X-ENDLIST", 4)},
		{"LF in segment URI", "init.mp4", at("1.m4s\n2.m4s", 4)},
		{"CR in segment URI", "init.mp4", at("1.m4s\r", 4)},
		{"tab in segment URI", "init.mp4", at("1\t.m4s", 4)},
		{"C1 control in segment URI", "init.mp4", at("1\u0085.m4s", 4)},
		{"padded segment URI", "init.mp4", at(" 1.m4s", 4)},
		{"segment URI not UTF-8", "init.mp4", at("1\xff.m4s", 4)},
		{"space in segment URI", "init.mp4", at("a b.m4s", 4)},
		{"NBSP in segment URI", "init.mp4", at("a\u00a0b.m4s", 4)},
		{"line separator in segment URI", "init.mp4", at("a\u2028b.m4s", 4)},
		{"unencoded non-ASCII segment URI", "init.mp4", at("caf\u00e9.m4s", 4)},
		{"backslash in segment URI", "init.mp4", at(`a\b.m4s`, 4)},
		{"bare percent in segment URI", "init.mp4", at("100%.m4s", 4)},
		{"short escape in segment URI", "init.mp4", at("a%2", 4)},
		{"bad escape in segment URI", "init.mp4", at("a%zz.m4s", 4)},
		{"padded init", " init.mp4", seg},
		{"brace in init", "init{1}.mp4", seg},
		{"NaN seconds", "init.mp4", at("1.m4s", math.NaN())},
		{"infinite seconds", "init.mp4", at("1.m4s", math.Inf(1))},
		{"negative seconds", "init.mp4", at("1.m4s", -0.5)},
		{"seconds past an int32 target", "init.mp4", at("1.m4s", math.MaxInt32+0.5)},
		{"line past the cap", "init.mp4", at(strings.Repeat("a", maxLineLen+1), 4)},
		{"lines past the cap", "init.mp4", many},
	} {
		text, err := Media(tc.init, tc.segs)
		if err == nil {
			if len(text) > 200 {
				text = text[:200] + "..."
			}
			t.Errorf("%s: written:\n%s", tc.name, text)
			continue
		}
		if got := waxerr.CodeOf(err); got != waxerr.CodeInvalidRequest {
			t.Errorf("%s: code %s, want %s (%v)", tc.name, got, waxerr.CodeInvalidRequest, err)
		}
		if text != "" {
			t.Errorf("%s: refused, but returned text", tc.name)
		}
	}
}

// TestMediaQuotesTheInitURI pins the init URI's rules, which are not a URI
// line's: inside EXT-X-MAP's quotes a leading # (a fragment) reads back as
// written, and so do the commas and equals signs an attribute list splits on.
func TestMediaQuotesTheInitURI(t *testing.T) {
	for _, init := range []string{"#init.mp4", "init,a=b.mp4"} {
		text, err := Media(init, []MediaSegment{{URI: "0.m4s", Seconds: 4}})
		if err != nil {
			t.Errorf("%q: %v", init, err)
			continue
		}
		pl, err := ParseMedia(text)
		if err != nil {
			t.Fatalf("%q: ParseMedia: %v", init, err)
		}
		if pl.InitURI != init {
			t.Errorf("init URI %q reads back as %q", init, pl.InitURI)
		}
	}
}

// TestMediaWritesEncodedURIs pins what the URI rule leaves writable: RFC
// 3986's characters, reserved ones included, and its percent escapes, which
// is how a caller writes a space or a non-ASCII name.
func TestMediaWritesEncodedURIs(t *testing.T) {
	uris := []string{"caf%C3%A9.m4s", "a%20b.m4s", "seg/0.m4s?v=a-b_c~d&exp=1&sig=AbC%2F",
		"a:b@c!$&'()*+,;=.m4s", "seg.m4s#t=1", "https://h.example/[x]/0.m4s"}
	var segs []MediaSegment
	for _, u := range uris {
		segs = append(segs, MediaSegment{URI: u, Seconds: 1})
	}
	text, err := Media("i%20t.mp4", segs)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := ParseMedia(text)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pl.Segments, segs) || pl.InitURI != "i%20t.mp4" {
		t.Errorf("read back %+v", pl)
	}
}

// TestMasterOmitsEmptyCodecs pins an unknown CODECS as no attribute rather
// than CODECS="", an empty list a player can do nothing with; RFC 8216 makes
// the attribute a SHOULD, and a missing one reads back as empty.
func TestMasterOmitsEmptyCodecs(t *testing.T) {
	text, err := Master(MasterPlaylist{Variants: []MasterVariant{{URI: "v.m3u8", Bandwidth: 64000}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "#EXT-X-STREAM-INF:BANDWIDTH=64000\nv.m3u8\n"; !strings.HasSuffix(text, want) {
		t.Errorf("master:\n%s\nwant it to end:\n%s", text, want)
	}
	pl, err := ParseMaster(text)
	if err != nil {
		t.Fatal(err)
	}
	if got := pl.Variants[0]; got != (MasterVariant{URI: "v.m3u8", Bandwidth: 64000}) {
		t.Errorf("read back %+v", got)
	}
}

// TestMediaWritesUpToTheParsersCap pins the writer's size refusal to the
// parser's exactly: MaxSegments segments are written and read, and one more
// is refused. The ceiling comes from the writer's own line count, two lines a
// segment past a one-segment playlist's, so MaxSegments is held to it.
func TestMediaWritesUpToTheParsersCap(t *testing.T) {
	one, err := Media("i.mp4", []MediaSegment{{URI: "s", Seconds: 1}})
	if err != nil {
		t.Fatal(err)
	}
	most := 1 + (MaxLines-(strings.Count(one, "\n")+1))/2
	if most != MaxSegments {
		t.Errorf("MaxSegments is %d, but the writer's ceiling is %d", MaxSegments, most)
	}
	segs := make([]MediaSegment, most+1)
	for i := range segs {
		segs[i] = MediaSegment{URI: "s", Seconds: 1}
	}
	text, err := Media("i.mp4", segs[:most])
	if err != nil {
		t.Fatalf("%d segments: %v", most, err)
	}
	if _, err := ParseMedia(text); err != nil {
		t.Fatalf("%d segments written but not read: %v", most, err)
	}
	if _, err := Media("i.mp4", segs); err == nil {
		t.Errorf("%d segments written, past the %d-line cap", most+1, MaxLines)
	}
}

// TestMediaWritesTheEdges pins what the refusals leave writable: a zero
// duration, a negative zero spelled without its sign (RFC 8216 EXTINF has
// none), and the longest duration a 32-bit int's target covers.
func TestMediaWritesTheEdges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		secs   float64
		extinf string
		target string
	}{
		{"zero", 0, "#EXTINF:0.00000,\n", "#EXT-X-TARGETDURATION:0\n"},
		{"negative zero", math.Copysign(0, -1), "#EXTINF:0.00000,\n", "#EXT-X-TARGETDURATION:0\n"},
		{"int32 ceiling", math.MaxInt32, "#EXTINF:2147483647.00000,\n", "#EXT-X-TARGETDURATION:2147483647\n"},
	} {
		text, err := Media("init.mp4", []MediaSegment{{URI: "0.m4s", Seconds: tc.secs}})
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		for _, want := range []string{tc.extinf, tc.target} {
			if !strings.Contains(text, want) {
				t.Errorf("%s: missing %q:\n%s", tc.name, want, text)
			}
		}
	}
}

// extinfValue matches RFC 8216's decimal-floating-point as Media spells it.
var extinfValue = regexp.MustCompile(`(?m)^#EXTINF:([0-9]+\.[0-9]{5}),$`)

// FuzzMediaRoundTrip holds Media to ParseMedia: whatever Media writes reads
// back as given, durations to five decimals, its EXTINF unsigned, and no
// segment's duration rounds past the target duration (RFC 8216 4.3.3.1). A
// refusal is invalid-request.
func FuzzMediaRoundTrip(f *testing.F) {
	f.Add("init.mp4?v=abc", "seg/0.m4s?v=abc", 4.0, "seg/1.m4s?v=abc", 2.02)
	f.Add("init.mp4", "0.m4s", 0.0, "1.m4s", math.Copysign(0, -1))
	f.Add("init,a=b.mp4", "0.m4s", 4.4999996, "1.m4s", 3.0000004)
	f.Add(`in"it.mp4`, " 0.m4s", 1.5, "#1.m4s", -1.0)
	f.Add("init.mp4", "0.m4s", 1e300, "1.m4s", math.Inf(1))
	f.Fuzz(func(t *testing.T, initURI, uri0 string, secs0 float64, uri1 string, secs1 float64) {
		segs := []MediaSegment{{URI: uri0, Seconds: secs0}, {URI: uri1, Seconds: secs1}}
		text, err := Media(initURI, segs)
		if err != nil {
			if got := waxerr.CodeOf(err); got != waxerr.CodeInvalidRequest {
				t.Fatalf("refusal code %s, want %s: %v", got, waxerr.CodeInvalidRequest, err)
			}
			return
		}
		pl, err := ParseMedia(text)
		if err != nil {
			t.Fatalf("ParseMedia refused what Media wrote: %v\n%s", err, text)
		}
		if pl.InitURI != initURI || !pl.End || len(pl.Segments) != len(segs) {
			t.Fatalf("read back %+v from:\n%s", pl, text)
		}
		for i, s := range pl.Segments {
			if s.URI != segs[i].URI {
				t.Fatalf("segment %d URI %q, wrote %q", i, s.URI, segs[i].URI)
			}
			// %.5f rounds by at most 5e-6; the parse adds well under 1e-6
			// below the 2^31 bound.
			if d := math.Abs(s.Seconds - segs[i].Seconds); d > 6e-6 {
				t.Fatalf("segment %d reads %v seconds, wrote %v", i, s.Seconds, segs[i].Seconds)
			}
			if r := math.Round(s.Seconds); r > float64(pl.TargetDuration) {
				t.Fatalf("segment %d rounds to %v, past the target duration %d", i, r, pl.TargetDuration)
			}
		}
		if n := len(extinfValue.FindAllString(text, -1)); n != len(segs) {
			t.Fatalf("%d of %d EXTINF values are unsigned decimals:\n%s", n, len(segs), text)
		}
	})
}

// FuzzMasterRoundTrip holds Master to ParseMaster the same way, over one
// variant and, when typ is set, one rendition the variant may name.
func FuzzMasterRoundTrip(f *testing.F) {
	f.Add("media.m3u8?v=abc", 128000, "Opus", "", "", "", "", "", false, false, "", "", false)
	f.Add("v.m3u8", 0, "mp4a.40.2,avc1.4d401f", "", "", "", "", "", false, false, "", "", false)
	f.Add("v.m3u8", 1, ` mp4a,BANDWIDTH=5 `, "", "", "", "", "", false, false, "", "", false)
	f.Add(" v.m3u8", -1, `Op"us`, "", "", "", "", "", false, false, "", "", false)
	f.Add("v.m3u8", 1, "Opus", "AUDIO", "aud", "English, main", "a.m3u8?x=1,y=2", "en", true, false, "16/JOC", "", true)
	f.Add("v.m3u8", 1, "Opus", "CLOSED-CAPTIONS", "cc", " pad ", "", "", false, true, "", "SERVICE9", false)
	f.Add("v.m3u8", 1, "Opus", "SUBTITLES", "s", "n", "s.m3u8", "", false, false, "", "CC1", true)
	f.Fuzz(func(t *testing.T, uri string, bandwidth int, codecs, typ, group, name, ruri, lang string,
		def, auto bool, channels, instream string, named bool) {
		pl := MasterPlaylist{Variants: []MasterVariant{{URI: uri, Bandwidth: bandwidth, Codecs: codecs}}}
		if typ != "" {
			pl.Media = []Rendition{{Type: typ, GroupID: group, Name: name, URI: ruri, Language: lang,
				Default: def, Autoselect: auto, Channels: channels, InstreamID: instream}}
			if named {
				pl.Variants[0].Audio = group
			}
		}
		text, err := Master(pl)
		if err != nil {
			if got := waxerr.CodeOf(err); got != waxerr.CodeInvalidRequest {
				t.Fatalf("refusal code %s, want %s: %v", got, waxerr.CodeInvalidRequest, err)
			}
			return
		}
		got, err := ParseMaster(text)
		if err != nil {
			t.Fatalf("ParseMaster refused what Master wrote: %v\n%s", err, text)
		}
		if !reflect.DeepEqual(got.Variants, pl.Variants) || !reflect.DeepEqual(got.Media, pl.Media) {
			t.Fatalf("read back %+v, wrote %+v:\n%s", got, pl, text)
		}
	})
}
