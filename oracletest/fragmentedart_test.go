package oracletest

// The end-to-end half of the fragmented-MP4 metadata story: cli/ only ever
// sees the hasArt flag, because a picture is too large to hold per demuxer on
// the streaming path, and server/ cannot wire a mapper (it is in the
// depcheck-gated stdlib-only tree). Here is the only place the real mapper and
// the real /art route meet, so it is the only place the bytes can be checked.

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/server"
)

// writeFragmentedArt builds the fixture: a fragmented (CMAF) MP4 carrying a
// covr atom. Kept a function so the file is closed by the time the caller
// reads it back through the daemon, on the failure path as well as the happy
// one.
func writeFragmentedArt(t *testing.T, srcWAV, path string, png []byte) {
	t.Helper()
	raw, err := os.ReadFile(srcWAV)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	// The empty Container is the aac row's own default: fragmented.
	if _, err := waxflow.New().Transcode(t.Context(), container.BytesSource(raw), "wav", out,
		waxflow.TranscodeOptions{
			Format: "aac",
			Tags:   []container.Tag{{Key: "TITLE", Value: "Arty Fragment"}},
			Art:    &container.Picture{MIME: "image/png", Data: png},
		}); err != nil {
		t.Fatal(err)
	}
}

// TestFragmentedArtReachesTheEndpoint serves cover art off a fragmented (CMAF)
// MP4, which is the shape that used to lose it: the tag library refused the
// container at parse, and the container fallback deliberately carries no
// pictures, so hasArt was false and there was nothing for /art to serve. The
// covr atom lives in the initial movie box, which reads now.
//
// The fixture art is a real PNG rather than cli's fake JPEG on purpose: a
// successful sniff makes the MIME authoritative from the bytes, so the
// Content-Type assertion has one answer.
func TestFragmentedArtReachesTheEndpoint(t *testing.T) {
	env := newTestEnv(t, func(cfg *server.Config) { cfg.Meta = label.New() })
	png := tinyPNG(t)
	writeFragmentedArt(t, filepath.Join(env.root, "sine.wav"),
		filepath.Join(env.root, "arty.m4a"), png)

	resp := env.get(t, "/art?src=lib/arty.m4a", nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		// The body carries the error envelope's waxerr code, which is the whole
		// diagnosis on a 404: it says whether the mapper stopped reading covr
		// or the route never found the source.
		t.Fatalf("GET /art = %d, want 200: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got)
	}
	if !bytes.Equal(body, png) {
		t.Errorf("art bytes differ (%d served, %d embedded)", len(body), len(png))
	}
}
