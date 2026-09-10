package oracletest

// The end-to-end half of the fragmented-MP4 metadata story: cli/ only ever
// sees the hasArt flag, because a picture is too large to hold per demuxer on
// the streaming path, and server/ cannot wire a mapper (it is in the
// depcheck-gated stdlib-only tree). Here is the only place the real mapper and
// the real /art route meet, so it is the only place the bytes can be checked.

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	waxlabel "github.com/colespringer/waxlabel"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
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

// tinyWebP is a 1x1 lossless WebP: the RIFF/WEBP form wrapper around a
// minimal VP8L chunk. Small enough to inline, real enough that an image
// sniffer names it.
func tinyWebP(t *testing.T) []byte {
	t.Helper()
	b, err := hex.DecodeString(
		"524946461a000000574542505650384c0d0000002f0000001007102411" +
			"0888818f00")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestWebPCoverKeepsItsTypeThroughMP4 pins the ilst covr type map from the
// far side. The atom's well-known type has values for GIF, JPEG, PNG and BMP
// and nothing else, so a WebP cover goes out as 0 (raw binary), the way
// TagLib writes a type it cannot name; typing it 13 would tell every reader
// to parse a WebP as a JPEG. waxlabel sniffs the bytes on the way back in, so
// the cover keeps its real MIME across the round trip either way, which is
// what makes the wrong type invisible until a reader trusts the atom.
func TestWebPCoverKeepsItsTypeThroughMP4(t *testing.T) {
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	webp := tinyWebP(t)
	raw := transcodeToBytes(t, f, 4410, waxflow.TranscodeOptions{
		Format:    "alac",
		Container: "fragmented",
		Tags:      []container.Tag{{Key: "TITLE", Value: "Lossless WebP"}},
		Art:       &container.Picture{MIME: "image/webp", Data: webp},
	})

	if got := covrDataType(t, raw); got != 0 {
		t.Errorf("covr well-known type = %d for a WebP cover, want 0 (raw binary)", got)
	}

	doc, err := waxlabel.Parse(t.Context(), container.BytesSource(raw))
	if err != nil {
		t.Fatalf("waxlabel.Parse: %v", err)
	}
	pics := doc.Pictures()
	if len(pics) != 1 {
		t.Fatalf("waxlabel found %d pictures, want 1", len(pics))
	}
	if pics[0].MIME != "image/webp" {
		t.Errorf("cover MIME = %q, want image/webp", pics[0].MIME)
	}
	if !bytes.Equal(pics[0].Data, webp) {
		t.Errorf("cover bytes differ (%d read, %d embedded)", len(pics[0].Data), len(webp))
	}
}

// covrDataType returns the well-known type of the covr item's data atom, read
// off the wire: [size]["covr"][size]["data"][type][locale][payload].
func covrDataType(t *testing.T, raw []byte) uint32 {
	t.Helper()
	i := bytes.Index(raw, []byte("covr"))
	if i < 0 || len(raw) < i+16 {
		t.Fatal("no covr atom in the output")
	}
	if got := string(raw[i+8 : i+12]); got != "data" {
		t.Fatalf("covr child box is %q, want data", got)
	}
	return binary.BigEndian.Uint32(raw[i+12 : i+16])
}
