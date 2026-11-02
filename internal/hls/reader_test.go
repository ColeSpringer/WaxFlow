package hls

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/m3u8"
	"github.com/colespringer/waxflow/waxerr"
)

// mapFetcher serves fixed bytes per URL, so the reader's playlist-following
// contract is tested without a real server or real segments.
type mapFetcher map[string]string

func (m mapFetcher) Fetch(_ context.Context, url string) ([]byte, error) {
	if v, ok := m[url]; ok {
		return []byte(v), nil
	}
	return nil, errNotFound(url)
}

type errNotFound string

func (e errNotFound) Error() string { return "not found: " + string(e) }

// recordingFetcher serves a mapFetcher and records every URL asked for.
type recordingFetcher struct {
	mapFetcher
	urls []string
}

func (r *recordingFetcher) Fetch(ctx context.Context, url string) ([]byte, error) {
	r.urls = append(r.urls, url)
	return r.mapFetcher.Fetch(ctx, url)
}

// renditionMasters are masters whose variant names an AUDIO group, each with
// the media playlist URL a client plays (RFC 8216 4.3.4.2.1). Only that URL
// is served, so following the variant's own URI, or the wrong rendition,
// fails the fetch.
var renditionMasters = []struct{ name, master, want string }{
	{"the rendition's playlist",
		"#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"main\",URI=\"audio.m3u8\"\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"aud\"\nvideo.m3u8\n",
		"http://h/audio.m3u8"},
	{"no rendition URI: the audio rides in the variant",
		"#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"main\"\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"aud\"\nvideo.m3u8\n",
		"http://h/video.m3u8"},
	{"the group's default, listed second",
		"#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"first\",URI=\"first.m3u8\"\n" +
			"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"main\",DEFAULT=YES,URI=\"main.m3u8\"\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"aud\"\nvideo.m3u8\n",
		"http://h/main.m3u8"},
}

const vodMedia = "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:4,\nseg0.m4s\n#EXT-X-ENDLIST\n"

// TestVODFollowsTheAudioRendition: a variant naming an AUDIO group is read
// from that group's rendition playlist, its default first.
func TestVODFollowsTheAudioRendition(t *testing.T) {
	for _, tc := range renditionMasters {
		f := mapFetcher{"http://h/master.m3u8": tc.master, tc.want: vodMedia}
		rf := retryingFetcher{f: f, timeout: time.Second}
		got, media, err := resolveMediaPlaylist(context.Background(), rf, "http://h/master.m3u8", 0)
		if err != nil || got != tc.want || media.InitURI != "init.mp4" {
			t.Errorf("%s: read %q (%v), want %q", tc.name, got, err, tc.want)
		}
	}
}

// TestAudioRenditionPrefersDefaultThenAutoselect pins the choice within a
// group: DEFAULT=YES, then AUTOSELECT=YES, then the first listed, among the
// TYPE=AUDIO members of the variant's group only.
func TestAudioRenditionPrefersDefaultThenAutoselect(t *testing.T) {
	r := func(typ, group, name string, def, auto bool) m3u8.Rendition {
		return m3u8.Rendition{Type: typ, GroupID: group, Name: name, Default: def, Autoselect: auto}
	}
	for _, tc := range []struct {
		name  string
		media []m3u8.Rendition
		want  string // "" when no rendition is chosen
	}{
		{"default over autoselect", []m3u8.Rendition{r("AUDIO", "a", "auto", false, true), r("AUDIO", "a", "def", true, true)}, "def"},
		{"autoselect over first", []m3u8.Rendition{r("AUDIO", "a", "first", false, false), r("AUDIO", "a", "auto", false, true)}, "auto"},
		{"first when nothing is marked", []m3u8.Rendition{r("AUDIO", "a", "first", false, false), r("AUDIO", "a", "second", false, false)}, "first"},
		{"audio members only", []m3u8.Rendition{r("VIDEO", "a", "video", true, true), r("AUDIO", "a", "audio", false, false)}, "audio"},
		{"the variant's group only", []m3u8.Rendition{r("AUDIO", "b", "other", true, true), r("AUDIO", "a", "mine", false, false)}, "mine"},
	} {
		v := m3u8.MasterVariant{URI: "v.m3u8", Bandwidth: 1, Audio: "a"}
		got, ok := audioRendition(m3u8.MasterPlaylist{Media: tc.media, Variants: []m3u8.MasterVariant{v}}, v)
		if !ok || got.Name != tc.want {
			t.Errorf("%s: chose %q (%v), want %q", tc.name, got.Name, ok, tc.want)
		}
	}
	v := m3u8.MasterVariant{URI: "v.m3u8", Bandwidth: 1}
	pl := m3u8.MasterPlaylist{Media: []m3u8.Rendition{r("AUDIO", "a", "x", true, true), r("AUDIO", "", "y", true, true)},
		Variants: []m3u8.MasterVariant{v}}
	if got, ok := audioRendition(pl, v); ok {
		t.Errorf("a variant naming no group chose %+v", got)
	}
}

// videoOnlyInit is a CMAF init header whose one track is video: a FLAC init
// with its handler retyped.
func videoOnlyInit(t *testing.T) []byte {
	t.Helper()
	si := flac.StreamInfo{MinBlock: 4096, MaxBlock: 4096, Rate: 48000, Channels: 2, Bits: 16}
	cfg, err := si.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	init, err := mp4.InitSegment(container.Track{Codec: codec.FLAC, CodecConfig: cfg, Fmt: si.PCMFormat()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mp4.NewFragmentedDemuxer(init, container.BytesSource(nil)); err != nil {
		t.Fatalf("the audio init does not open: %v", err)
	}
	if n := bytes.Count(init, []byte("soun")); n != 1 {
		t.Fatalf("the init holds %d soun handlers, want 1", n)
	}
	return bytes.Replace(init, []byte("soun"), []byte("vide"), 1)
}

// TestVODRefusesAVideoOnlyInitBeforeFetchingSegments: a variant whose init
// holds no audio track fails on the init, before any segment downloads.
func TestVODRefusesAVideoOnlyInitBeforeFetchingSegments(t *testing.T) {
	media := "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MAP:URI=\"init.mp4\"\n"
	f := &recordingFetcher{mapFetcher: mapFetcher{"http://h/init.mp4": string(videoOnlyInit(t))}}
	for n := range 4 {
		media += fmt.Sprintf("#EXTINF:4,\nseg%d.m4s\n", n)
		f.mapFetcher[fmt.Sprintf("http://h/seg%d.m4s", n)] = "segment"
	}
	f.mapFetcher["http://h/media.m3u8"] = media + "#EXT-X-ENDLIST\n"
	_, err := OpenVOD(context.Background(), f, "http://h/media.m3u8", nil)
	if waxerr.CodeOf(err) != waxerr.CodeUnsupportedFormat || !strings.Contains(fmt.Sprint(err), "no audio track") {
		t.Fatalf("err = %v, want the unsupported no-audio-track refusal", err)
	}
	for _, u := range f.urls {
		if strings.Contains(u, "seg") {
			t.Errorf("fetched %s before refusing the init", u)
		}
	}
}

// TestOpenVODRejectsLive confirms a media playlist with no EXT-X-ENDLIST (a live
// playlist) is refused by OpenVOD, which reads complete presentations only.
func TestOpenVODRejectsLive(t *testing.T) {
	live := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:4\n" +
		"#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:4.0,\nseg0.m4s\n" // no #EXT-X-ENDLIST
	f := mapFetcher{"http://h/media.m3u8": live}
	_, err := OpenVOD(context.Background(), f, "http://h/media.m3u8", nil)
	if err == nil || !strings.Contains(err.Error(), "ENDLIST") {
		t.Fatalf("err = %v, want an ENDLIST rejection", err)
	}
}

// TestOpenVODRejectsNoInit confirms a complete playlist with no EXT-X-MAP init
// segment (a legacy TS presentation) is refused: only fragmented-MP4 is read.
func TestOpenVODRejectsNoInit(t *testing.T) {
	noInit := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:4\n" +
		"#EXTINF:4.0,\nseg0.ts\n#EXT-X-ENDLIST\n"
	f := mapFetcher{"http://h/media.m3u8": noInit}
	_, err := OpenVOD(context.Background(), f, "http://h/media.m3u8", nil)
	if err == nil || !strings.Contains(err.Error(), "EXT-X-MAP") {
		t.Fatalf("err = %v, want an EXT-X-MAP rejection", err)
	}
}

// TestOpenVODVariantRange confirms a master playlist with a too-large variant
// index fails cleanly (before any segment fetch).
func TestOpenVODVariantRange(t *testing.T) {
	master := "#EXTM3U\n#EXT-X-VERSION:7\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=128000,CODECS=\"mp4a.40.2\"\nlow.m3u8\n"
	f := mapFetcher{"http://h/master.m3u8": master}
	_, err := OpenVOD(context.Background(), f, "http://h/master.m3u8", &ClientOptions{VariantIndex: 3})
	if err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("err = %v, want an out-of-range variant rejection", err)
	}
}

// TestHTTPFetcherLimit confirms the fetcher refuses (does not truncate) a
// response larger than its cap, so no single oversized resource can exhaust
// memory before the aggregate caps apply.
func TestHTTPFetcherLimit(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	// Under the cap: the full body comes back.
	got, err := (HTTPFetcher{MaxResponseBytes: 2000}).Fetch(context.Background(), srv.URL)
	if err != nil || len(got) != 1000 {
		t.Fatalf("under-cap fetch: got %d bytes, err %v", len(got), err)
	}
	// Over the cap: refused, not silently truncated.
	if _, err := (HTTPFetcher{MaxResponseBytes: 100}).Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("a response over the cap must be refused")
	}
}

// TestResolveURL pins relative-URI resolution against the playlist base, the
// RFC 3986 behavior segment and init URIs rely on.
func TestResolveURL(t *testing.T) {
	for _, tc := range []struct{ base, ref, want string }{
		{"http://h/a/media.m3u8", "seg0.m4s", "http://h/a/seg0.m4s"},
		{"http://h/a/media.m3u8", "../b/seg0.m4s", "http://h/b/seg0.m4s"},
		{"http://h/a/media.m3u8", "http://cdn/x.m4s", "http://cdn/x.m4s"},
		{"http://h/a/media.m3u8", "/root.m4s", "http://h/root.m4s"},
	} {
		got, err := resolveURL(tc.base, tc.ref)
		if err != nil {
			t.Fatalf("resolveURL(%q,%q): %v", tc.base, tc.ref, err)
		}
		if got != tc.want {
			t.Errorf("resolveURL(%q,%q) = %q, want %q", tc.base, tc.ref, got, tc.want)
		}
	}
}
