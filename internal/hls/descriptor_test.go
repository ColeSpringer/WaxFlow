package hls

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/waxerr"
)

func TestDescriptorRoundTrip(t *testing.T) {
	d := Descriptor{
		Src: "lib/a.flac", ID: "1234-5678", Format: "opus",
		Bitrate: 96, Bits: 0, Rate: 48000, Ch: 2, Gain: "track", SegDur: 4,
	}
	got, err := DecodeDescriptor(d.Encode())
	if err != nil {
		t.Fatal(err)
	}
	d.Ver = DescriptorVersion
	if !reflect.DeepEqual(got, d) {
		t.Fatalf("round trip %+v, want %+v", got, d)
	}
}

// TestDescriptorCrossfadeRoundTrip pins that a timeline's crossfade survives the
// wire form, so a signed URL carries the blend the mint was asked for.
func TestDescriptorCrossfadeRoundTrip(t *testing.T) {
	d := Descriptor{Tl: strings.Repeat("a", 43), Format: "opus", CrossfadeSeconds: 0.25}
	got, err := DecodeDescriptor(d.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.CrossfadeSeconds != 0.25 {
		t.Fatalf("crossfadeSeconds round-tripped to %v, want 0.25", got.CrossfadeSeconds)
	}
}

func TestDescriptorLadder(t *testing.T) {
	d := Descriptor{Src: "s", ID: "i", Format: "opus", Bitrates: []int{64, 96, 160}}
	got, err := DecodeDescriptor(d.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Ladder()) != 3 {
		t.Fatalf("ladder %v", got.Ladder())
	}
	v := got.Variant(96)
	if v.Bitrate != 96 || v.Bitrates != nil {
		t.Fatalf("variant %+v", v)
	}
	single := Descriptor{Src: "s", ID: "i", Format: "opus"}
	if l := single.Ladder(); len(l) != 1 || l[0] != 0 {
		t.Fatalf("default ladder %v", l)
	}
}

func TestDescriptorRejections(t *testing.T) {
	enc := func(json string) string { return base64.RawURLEncoding.EncodeToString([]byte(json)) }
	cases := map[string]string{
		"empty":            "",
		"not-base64":       "!!!",
		"not-json":         enc("nope"),
		"trailing":         enc(`{"ver":1,"src":"s","id":"i","format":"opus"}{}`),
		"unknown-field":    enc(`{"ver":1,"src":"s","id":"i","format":"opus","bogus":1}`),
		"wrong-ver":        enc(`{"ver":2,"src":"s","id":"i","format":"opus"}`),
		"no-src":           enc(`{"ver":1,"id":"i","format":"opus"}`),
		"no-id":            enc(`{"ver":1,"src":"s","format":"opus"}`),
		"no-format":        enc(`{"ver":1,"src":"s","id":"i"}`),
		"bad-bits":         enc(`{"ver":1,"src":"s","id":"i","format":"opus","bits":20}`),
		"neg-bitrate":      enc(`{"ver":1,"src":"s","id":"i","format":"opus","bitrate":-1}`),
		"neg-segdur":       enc(`{"ver":1,"src":"s","id":"i","format":"opus","segDur":-1}`),
		"zero-ladder-rung": enc(`{"ver":1,"src":"s","id":"i","format":"opus","bitrates":[96,0]}`),
		"both-bitrates":    enc(`{"ver":1,"src":"s","id":"i","format":"opus","bitrate":96,"bitrates":[64]}`),
		"oversized":        enc(`{"ver":1,"src":"` + strings.Repeat("a", maxDescriptorBytes) + `","id":"i","format":"opus"}`),
		"neg-crossfade":    enc(`{"ver":1,"src":"s","id":"i","format":"opus","crossfadeSeconds":-1}`),
		// A crossfade blends a timeline's seam; a single source has none.
		"src-and-crossfade": enc(`{"ver":1,"src":"s","id":"i","format":"opus","crossfadeSeconds":0.5}`),
	}
	for name, v := range cases {
		if _, err := DecodeDescriptor(v); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
			t.Errorf("%s: err %v, want invalid-request", name, err)
		}
	}
}

func FuzzDecodeDescriptor(f *testing.F) {
	f.Add(Descriptor{Src: "lib/a.flac", ID: "1-2", Format: "opus", Bitrates: []int{64, 96}}.Encode())
	f.Add(Descriptor{Src: "s", ID: "i", Format: "flac", Bits: 24, SegDur: 6.5}.Encode())
	f.Add("AAAA")
	f.Fuzz(func(t *testing.T, s string) {
		d, err := DecodeDescriptor(s)
		if err != nil {
			return
		}
		// A decoded descriptor must survive its own canonical form.
		if _, err := DecodeDescriptor(d.Encode()); err != nil {
			t.Fatalf("re-encode of accepted descriptor rejected: %v", err)
		}
	})
}

func TestPlaylists(t *testing.T) {
	master := Master([]MasterVariant{
		{URI: "media.m3u8?v=abc", Bandwidth: 100000, Codecs: "Opus"},
		{URI: "media.m3u8?v=def", Bandwidth: 1500000, Codecs: "fLaC"},
	})
	for _, want := range []string{
		"#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n",
		"#EXT-X-STREAM-INF:BANDWIDTH=100000,CODECS=\"Opus\"\nmedia.m3u8?v=abc\n",
		"#EXT-X-STREAM-INF:BANDWIDTH=1500000,CODECS=\"fLaC\"\nmedia.m3u8?v=def\n",
	} {
		if !strings.Contains(master, want) {
			t.Errorf("master missing %q:\n%s", want, master)
		}
	}

	media := Media("init.mp4?v=abc", []MediaSegment{
		{URI: "seg/0.m4s?v=abc", Seconds: 4},
		{URI: "seg/1.m4s?v=abc", Seconds: 2.02},
	})
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
