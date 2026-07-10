// The HLS acceptance suite: signed master minting, ladder masters, VOD
// media playlists with exact segment counts, init and segment delivery
// through the variant workers, seek-restart latency, regeneration
// determinism, auth/signature/identity enforcement, and the
// unknown-length measure path.
package server_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/client"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// mintHLS mints a signed master URL through POST /sign.
func mintHLS(t *testing.T, env *testEnv, params map[string]string) string {
	t.Helper()
	c, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Sign(t.Context(), client.SignRequest{Path: "/hls/master.m3u8", Params: params})
	if err != nil {
		t.Fatal(err)
	}
	return resp.URL
}

// keyless fetches a URL with no API key: signed-URL auth only.
func keyless(t *testing.T, env *testEnv, pathAndQuery string) *http.Response {
	t.Helper()
	return env.get(t, pathAndQuery, map[string]string{"X-API-Key": ""})
}

// playlistLines returns the non-blank lines of an m3u8 body.
func playlistLines(body []byte) []string {
	var lines []string
	for _, l := range strings.Split(string(body), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// mediaURIs extracts the child URIs (non-tag lines) plus the EXT-X-MAP
// init URI from a media playlist.
func mediaURIs(t *testing.T, body []byte) (initURI string, segURIs []string, extinf []float64) {
	t.Helper()
	for _, l := range playlistLines(body) {
		switch {
		case strings.HasPrefix(l, "#EXT-X-MAP:URI=\""):
			initURI = strings.TrimSuffix(strings.TrimPrefix(l, "#EXT-X-MAP:URI=\""), "\"")
		case strings.HasPrefix(l, "#EXTINF:"):
			var d float64
			if _, err := fmt.Sscanf(l, "#EXTINF:%f,", &d); err != nil {
				t.Fatalf("bad EXTINF line %q", l)
			}
			extinf = append(extinf, d)
		case !strings.HasPrefix(l, "#"):
			segURIs = append(segURIs, l)
		}
	}
	return initURI, segURIs, extinf
}

func TestHLSEndToEnd(t *testing.T) {
	env := newTestEnv(t, nil)
	masterURL := mintHLS(t, env, map[string]string{"src": "lib/ramp.wav", "format": "opus", "bitrates": "64,96"})

	// Master: keyless (the signature is the auth), one rung per ladder entry.
	resp := keyless(t, env, masterURL)
	master := readBody(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/vnd.apple.mpegurl" {
		t.Fatalf("master = %d %s: %s", resp.StatusCode, resp.Header.Get("Content-Type"), master)
	}
	var mediaRefs []string
	for _, l := range playlistLines(master) {
		if !strings.HasPrefix(l, "#") {
			mediaRefs = append(mediaRefs, l)
		}
	}
	if len(mediaRefs) != 2 {
		t.Fatalf("master lists %d variants, want 2:\n%s", len(mediaRefs), master)
	}
	if !bytes.Contains(master, []byte(`CODECS="Opus"`)) {
		t.Fatalf("master missing Opus CODECS:\n%s", master)
	}

	// Media playlist of the first rung: VOD, exact segment count for the
	// 4 s ramp (one 4 s segment plus the padding-frame tail segment).
	resp = keyless(t, env, "/hls/"+mediaRefs[0])
	media := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("media = %d: %s", resp.StatusCode, media)
	}
	for _, want := range []string{"#EXT-X-PLAYLIST-TYPE:VOD", "#EXT-X-ENDLIST", "#EXT-X-TARGETDURATION:4", "#EXT-X-VERSION:7"} {
		if !bytes.Contains(media, []byte(want)) {
			t.Fatalf("media playlist missing %s:\n%s", want, media)
		}
	}
	initURI, segURIs, extinf := mediaURIs(t, media)
	if initURI == "" || len(segURIs) != 2 {
		t.Fatalf("init %q, %d segments (want 2):\n%s", initURI, len(segURIs), media)
	}
	// EXTINF durations sum to the decode total: 4 s plus the flushed
	// padding frame.
	var sum float64
	for _, d := range extinf {
		sum += d
	}
	if sum < 4.0 || sum > 4.05 {
		t.Fatalf("EXTINF sum %.5f, want just over 4 s", sum)
	}

	// Init header, twice: computed then cached, byte-identical.
	resp = keyless(t, env, "/hls/"+initURI)
	init1 := readBody(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/mp4" {
		t.Fatalf("init = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !bytes.Contains(init1, []byte("dOps")) || !bytes.Contains(init1, []byte("elst")) {
		t.Fatal("init header missing dOps or the delay edit list")
	}
	resp = keyless(t, env, "/hls/"+initURI)
	if init2 := readBody(t, resp); !bytes.Equal(init1, init2) {
		t.Fatal("cached init differs from the computed one")
	}

	// Segments in order: the variant worker spawns once and serves both.
	var segs [][]byte
	for _, uri := range segURIs {
		resp = keyless(t, env, "/hls/"+uri)
		seg := readBody(t, resp)
		if resp.StatusCode != 200 {
			t.Fatalf("segment %s = %d: %s", uri, resp.StatusCode, seg)
		}
		if !bytes.HasPrefix(seg[4:8], []byte("styp")) {
			t.Fatalf("segment does not start with styp: % x", seg[:12])
		}
		segs = append(segs, seg)
	}

	// The tail segment carries exactly the padding frame: 960 samples at
	// 48 kHz per the plan arithmetic.
	if got := segmentDecodeSamples(t, segs[1]); got != 960 {
		t.Fatalf("tail segment %d samples, want 960", got)
	}

	// Determinism across regeneration: wait for the worker, wipe the
	// segment files (leaving the index), refetch. The worker restarts at
	// segment 0 and must reproduce the bytes exactly.
	waitForIdle(t, env)
	removeSegmentFiles(t, env.cache)
	for i, uri := range segURIs {
		resp = keyless(t, env, "/hls/"+uri)
		if again := readBody(t, resp); !bytes.Equal(again, segs[i]) {
			t.Fatalf("regenerated segment %d differs from the original", i)
		}
	}
}

// segmentDecodeSamples sums the trun durations of a segment.
func segmentDecodeSamples(t *testing.T, seg []byte) int64 {
	t.Helper()
	var total int64
	for off := 0; off+8 <= len(seg); {
		size := int(binary.BigEndian.Uint32(seg[off:]))
		typ := string(seg[off+4 : off+8])
		if size < 8 || off+size > len(seg) {
			t.Fatalf("bad box %q at %d", typ, off)
		}
		if typ == "moof" {
			body := seg[off+8 : off+size]
			i := bytes.Index(body, []byte("trun"))
			if i < 0 {
				t.Fatal("moof without trun")
			}
			b := body[i+4:]
			n := int(binary.BigEndian.Uint32(b[4:]))
			at := 12
			for j := 0; j < n; j++ {
				total += int64(binary.BigEndian.Uint32(b[at:]))
				at += 8
			}
		}
		off += size
	}
	return total
}

// waitForIdle waits for every pipeline (HLS workers included) to finish.
func waitForIdle(t *testing.T, env *testEnv) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for env.srv.Metrics().SessionsActive.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("sessions never went idle")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// removeSegmentFiles deletes seg-*.m4s from every cache entry, forcing
// regeneration while the entry (and its meta) stays indexed.
func removeSegmentFiles(t *testing.T, cacheDir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(cacheDir, "v1", "*", "*", "seg-*.m4s"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no segment files to remove")
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHLSSeekRestartLatency(t *testing.T) {
	env := newTestEnv(t, nil)
	// A one-second segment ladder over the 4 s ramp: segments 0..4.
	masterURL := mintHLS(t, env, map[string]string{"src": "lib/ramp.wav", "format": "opus", "segDur": "1"})
	resp := keyless(t, env, masterURL)
	master := readBody(t, resp)
	var mediaRef string
	for _, l := range playlistLines(master) {
		if !strings.HasPrefix(l, "#") {
			mediaRef = l
		}
	}
	resp = keyless(t, env, "/hls/"+mediaRef)
	_, segURIs, _ := mediaURIs(t, readBody(t, resp))
	if len(segURIs) < 4 {
		t.Fatalf("%d segments, want at least 4", len(segURIs))
	}

	// Out-of-order fetches force worker restarts (each lands outside the
	// previous worker's lookahead); every one must arrive promptly. The
	// p95 target is <1 s; the hard bound here stays loose enough for a
	// loaded CI box.
	order := []int{len(segURIs) - 1, 0, 2, len(segURIs) - 2, 1}
	var times []time.Duration
	for _, n := range order {
		start := time.Now()
		resp := keyless(t, env, "/hls/"+segURIs[n])
		body := readBody(t, resp)
		took := time.Since(start)
		if resp.StatusCode != 200 {
			t.Fatalf("segment %d = %d: %s", n, resp.StatusCode, body)
		}
		times = append(times, took)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	t.Logf("seek-to-segment times (sorted): %v", times)
	if worst := times[len(times)-1]; worst > 3*time.Second {
		t.Fatalf("worst seek-to-segment %v; the p95 target is <1s", worst)
	}
}

func TestHLSAuthAndSignature(t *testing.T) {
	env := newTestEnv(t, nil)
	masterURL := mintHLS(t, env, map[string]string{"src": "lib/ramp.wav", "format": "opus"})

	// No key, no signature: unauthorized.
	u, err := url.Parse(masterURL)
	if err != nil {
		t.Fatal(err)
	}
	resp := keyless(t, env, u.Path+"?v="+u.Query().Get("v"))
	wantEnvelope(t, resp, http.StatusUnauthorized, waxerr.CodeUnauthorized)

	// Tampered descriptor: the signature covers v wholly.
	q := u.Query()
	tampered := hlsSwapBitrate(t, q.Get("v"))
	q.Set("v", tampered)
	resp = keyless(t, env, u.Path+"?"+q.Encode())
	wantEnvelope(t, resp, http.StatusForbidden, waxerr.CodeSignatureInvalid)

	// Unknown parameter rejected even when signed over.
	resp = env.get(t, masterURL+"&bogus=1", nil)
	wantEnvelope(t, resp, http.StatusBadRequest, waxerr.CodeInvalidRequest)

	// The raw-parameter master form needs an API key.
	resp = keyless(t, env, "/hls/master.m3u8?src=lib%2Framp.wav&format=opus")
	wantEnvelope(t, resp, http.StatusUnauthorized, waxerr.CodeUnauthorized)
	resp = env.get(t, "/hls/master.m3u8?src=lib%2Framp.wav&format=opus", nil)
	if body := readBody(t, resp); resp.StatusCode != 200 || !bytes.Contains(body, []byte("media.m3u8?")) || !bytes.Contains(body, []byte("v=")) {
		t.Fatalf("keyed raw master = %d: %s", resp.StatusCode, body)
	}

	// A per-variant URL refuses a ladder descriptor.
	ladder := hlsDescriptorFor(t, env, map[string]string{"src": "lib/ramp.wav", "format": "opus", "bitrates": "64,96"})
	resp = env.get(t, "/hls/media.m3u8?v="+ladder, nil)
	wantEnvelope(t, resp, http.StatusBadRequest, waxerr.CodeInvalidRequest)
}

// hlsDescriptorFor mints a master URL and returns its raw v= value.
func hlsDescriptorFor(t *testing.T, env *testEnv, params map[string]string) string {
	t.Helper()
	u, err := url.Parse(mintHLS(t, env, params))
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("v")
}

// hlsSwapBitrate decodes a descriptor, flips a field, and re-encodes:
// a valid but unsigned descriptor.
func hlsSwapBitrate(t *testing.T, v string) string {
	t.Helper()
	// The internal descriptor type is not importable here; a plain JSON
	// byte swap keeps the test at arm's length like a real attacker.
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		t.Fatal(err)
	}
	swapped := bytes.Replace(raw, []byte(`"format":"opus"`), []byte(`"format":"flac"`), 1)
	if bytes.Equal(swapped, raw) {
		t.Fatal("nothing swapped; fixture drifted")
	}
	return base64.RawURLEncoding.EncodeToString(swapped)
}

func TestHLSSourceChanged(t *testing.T) {
	env := newTestEnv(t, nil)
	masterURL := mintHLS(t, env, map[string]string{"src": "lib/ramp.wav", "format": "opus"})
	resp := keyless(t, env, masterURL)
	master := readBody(t, resp)
	var mediaRef string
	for _, l := range playlistLines(master) {
		if !strings.HasPrefix(l, "#") {
			mediaRef = l
		}
	}

	// Rewrite the source: identity (size+mtime) changes.
	path := filepath.Join(env.root, "ramp.wav")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, 0), 0o644); err != nil {
		t.Fatal(err)
	}

	resp = keyless(t, env, masterURL)
	wantEnvelope(t, resp, http.StatusGone, waxerr.CodeSourceChanged)
	resp = keyless(t, env, "/hls/"+mediaRef)
	wantEnvelope(t, resp, http.StatusGone, waxerr.CodeSourceChanged)
}

func TestHLSSegmentBounds(t *testing.T) {
	env := newTestEnv(t, nil)
	desc := hlsDescriptorFor(t, env, map[string]string{"src": "lib/ramp.wav", "format": "opus"})
	resp := env.get(t, "/hls/seg/99.m4s?v="+desc, nil)
	wantEnvelope(t, resp, http.StatusNotFound, waxerr.CodeNotFound)
	resp = env.get(t, "/hls/seg/nope?v="+desc, nil)
	wantEnvelope(t, resp, http.StatusNotFound, waxerr.CodeNotFound)
}

func TestHLSLossless(t *testing.T) {
	env := newTestEnv(t, nil)
	// FLAC rung: playlist plus first segment round-trips.
	desc := hlsDescriptorFor(t, env, map[string]string{"src": "lib/ramp.wav", "format": "flac"})
	resp := env.get(t, "/hls/media.m3u8?v="+desc, nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("flac media = %d: %s", resp.StatusCode, body)
	}
	_, segURIs, _ := mediaURIs(t, body)
	resp = env.get(t, "/hls/"+segURIs[0], nil)
	if seg := readBody(t, resp); resp.StatusCode != 200 || len(seg) == 0 {
		t.Fatalf("flac segment = %d", resp.StatusCode)
	}

	// bitrate on a lossless format is refused at mint time.
	c, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Sign(t.Context(), client.SignRequest{
		Path:   "/hls/master.m3u8",
		Params: map[string]string{"src": "lib/ramp.wav", "format": "flac", "bitrate": "128"},
	})
	if waxerr.CodeOf(err) != waxerr.CodeUnsupportedFormat {
		t.Fatalf("err %v, want unsupported-format", err)
	}
}

// TestHLSUnknownLengthMeasures drives the forced-measure path: an MP3
// with no Xing/LAME tag probes with unknown length, so the media
// playlist must trigger the frame-index walk and still promise an exact
// segment count (fetching the last listed segment succeeds; one past it
// is 404).
func TestHLSUnknownLengthMeasures(t *testing.T) {
	env := newTestEnv(t, nil)

	// Build the tagless MP3: transcode the ramp to MP3, then strip the
	// leading Xing/Info metadata frame.
	mp3Bytes := engineReference(t, filepath.Join(env.root, "ramp.wav"), waxflow.TranscodeOptions{Format: "mp3"})
	hdr, err := mp3.ParseHeader(mp3Bytes)
	if err != nil {
		t.Fatal(err)
	}
	tagless := mp3Bytes[hdr.Size():]
	if err := os.WriteFile(filepath.Join(env.root, "tagless.mp3"), tagless, 0o644); err != nil {
		t.Fatal(err)
	}

	// Sanity: the engine really cannot know the length from headers.
	info, err := waxflow.New().Probe(container.BytesSource(tagless), "mp3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.Default().Samples >= 0 {
		t.Fatalf("fixture has a known length (%d); the measure path is untested", info.Default().Samples)
	}

	desc := hlsDescriptorFor(t, env, map[string]string{"src": "lib/tagless.mp3", "format": "opus", "segDur": "1"})
	resp := env.get(t, "/hls/media.m3u8?v="+desc, nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("media = %d: %s", resp.StatusCode, body)
	}
	_, segURIs, extinf := mediaURIs(t, body)
	if len(segURIs) < 4 {
		t.Fatalf("%d segments for a ~4 s source at 1 s segments", len(segURIs))
	}
	var sum float64
	for _, d := range extinf {
		sum += d
	}
	if sum < 3.9 || sum > 4.3 {
		t.Fatalf("EXTINF sum %.3f, want about 4 s (plus codec delays)", sum)
	}

	// The playlist's promise holds at the tail.
	resp = env.get(t, "/hls/"+segURIs[len(segURIs)-1], nil)
	if readBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("last listed segment = %d", resp.StatusCode)
	}
	resp = env.get(t, fmt.Sprintf("/hls/seg/%d.m4s?v=%s", len(segURIs), desc), nil)
	wantEnvelope(t, resp, http.StatusNotFound, waxerr.CodeNotFound)
}

// TestHLSConcurrentFetch pulls a variant's segments from several
// goroutines at once: one worker serves them all, every response is
// complete, and nothing races (the suite runs under -race in CI).
func TestHLSConcurrentFetch(t *testing.T) {
	env := newTestEnv(t, nil)
	desc := hlsDescriptorFor(t, env, map[string]string{"src": "lib/ramp.wav", "format": "opus", "segDur": "1"})
	resp := env.get(t, "/hls/media.m3u8?v="+desc, nil)
	_, segURIs, _ := mediaURIs(t, readBody(t, resp))

	errs := make(chan error, len(segURIs))
	for _, uri := range segURIs {
		go func(uri string) {
			resp := env.get(t, "/hls/"+uri, nil)
			body := readBody(t, resp)
			if resp.StatusCode != 200 {
				errs <- fmt.Errorf("%s = %d: %s", uri, resp.StatusCode, body)
				return
			}
			if len(body) < 8 || string(body[4:8]) != "styp" {
				errs <- fmt.Errorf("%s: not a segment", uri)
				return
			}
			errs <- nil
		}(uri)
	}
	for range segURIs {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	if got := int(env.srv.Metrics().SessionsHLS.Load()); got > len(segURIs) {
		t.Fatalf("%d workers for %d in-order-ish segments", got, len(segURIs))
	}
}
