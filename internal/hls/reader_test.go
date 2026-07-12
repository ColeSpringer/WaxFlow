package hls

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

// TestIsMasterPlaylist pins the line-anchored master/media discrimination: a
// media playlist that merely mentions the STREAM-INF tag text in a title is not
// a master, while a real STREAM-INF tag line is.
func TestIsMasterPlaylist(t *testing.T) {
	master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv0.m3u8\n"
	media := "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,see #EXT-X-STREAM-INF notes\nseg0.m4s\n#EXT-X-ENDLIST\n"
	if !isMasterPlaylist([]byte(master)) {
		t.Error("real master with a STREAM-INF tag line not detected")
	}
	if isMasterPlaylist([]byte(media)) {
		t.Error("media playlist mentioning the tag in a title misdetected as master")
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
