// The HLS acceptance suite: signed master minting, ladder masters, VOD
// media playlists with exact segment counts, init and segment delivery
// through the variant workers, seek-restart latency, regeneration
// determinism, auth/signature/identity enforcement, and the
// unknown-length measure path.
package server_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/client"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/m3u8"
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

// playlistLines returns the non-blank lines of an m3u8 body, for a tag the
// parsed playlists do not carry.
func playlistLines(body []byte) []string {
	var lines []string
	for _, l := range strings.Split(string(body), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// variantURIs reads a master playlist through m3u8.ParseMaster and returns its
// variants' URIs in order.
func variantURIs(t *testing.T, master []byte) []string {
	t.Helper()
	pl, err := m3u8.ParseMaster(string(master))
	if err != nil {
		t.Fatalf("master playlist: %v\n%s", err, master)
	}
	uris := make([]string, len(pl.Variants))
	for i, v := range pl.Variants {
		uris[i] = v.URI
	}
	return uris
}

// mediaURIs reads a media playlist through m3u8.ParseMedia: the EXT-X-MAP init
// URI, the segment URIs, and their EXTINF durations.
func mediaURIs(t *testing.T, body []byte) (initURI string, segURIs []string, extinf []float64) {
	t.Helper()
	pl, err := m3u8.ParseMedia(string(body))
	if err != nil {
		t.Fatalf("media playlist: %v\n%s", err, body)
	}
	for _, s := range pl.Segments {
		segURIs = append(segURIs, s.URI)
		extinf = append(extinf, s.Seconds)
	}
	return pl.InitURI, segURIs, extinf
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
	mediaRefs := variantURIs(t, master)
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
	// EXTINF is the presentation timeline, so the durations sum to the
	// source's true length rather than to the decode total, which overshoots
	// it by the encoder delay plus the tail rounding. The exactness is in
	// samples; the playlist prints %.5f, so the rendered sum carries up to
	// 5e-6 s of rounding per segment and is bounded by that rather than
	// demanded equal.
	var sum float64
	for _, d := range extinf {
		sum += d
	}
	if tol := float64(len(extinf)) * 5e-6; math.Abs(sum-4.0) > tol {
		t.Fatalf("EXTINF sum %.5f, want 4 s within %.5f", sum, tol)
	}
	// TARGETDURATION still covers the longest segment it lists.
	for _, d := range extinf {
		if d > 4 {
			t.Fatalf("EXTINF %.5f exceeds the declared TARGETDURATION 4", d)
		}
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

// TestHLSMediaPlaylistMatchesTheExportedPlan pins what SegmentPlan.MediaSegments
// promises a host: plan as the daemon's rung does, through the exported
// planners, and m3u8.Media renders the daemon's playlist byte for byte, given
// its URIs. The daemon renders through the same two calls, so what this pins
// is the plan (the rendering is pinned against a literal in tests/); a rung's
// counter shows the daemon served the rung the host planned, since the
// transcode fallback would serve a playlist too.
func TestHLSMediaPlaylistMatchesTheExportedPlan(t *testing.T) {
	env := newTestEnv(t, nil)
	ref := writeOpusFixture(t, env, "ramp.wav", "ramp.opus")
	e := waxflow.New()
	track := func(name, ext string) (container.Source, container.Track) {
		raw, err := os.ReadFile(filepath.Join(env.root, name))
		if err != nil {
			t.Fatal(err)
		}
		info, err := e.Probe(context.Background(), container.BytesSource(raw), ext, nil)
		if err != nil {
			t.Fatal(err)
		}
		return container.BytesSource(raw), info.Default()
	}
	_, wav := track("ramp.wav", "wav")
	opusSrc, opus := track("ramp.opus", "opus")
	grid, err := e.PacketGrid(context.Background(), opusSrc, "opus")
	if err != nil {
		t.Fatal(err)
	}
	asOpus := waxflow.TranscodeOptions{Format: "opus"}
	met := env.srv.Metrics()
	for _, tc := range []struct {
		name    string
		params  map[string]string
		counter *atomic.Uint64
		plan    func() (*waxflow.SegmentPlan, error)
	}{
		{"transcode/opus", map[string]string{"src": "lib/ramp.wav", "format": "opus", "segDur": "1"}, nil,
			func() (*waxflow.SegmentPlan, error) { return e.PlanSegments(wav, asOpus, 1) }},
		{"transcode/aac", map[string]string{"src": "lib/ramp.wav", "format": "aac", "segDur": "1"}, nil,
			func() (*waxflow.SegmentPlan, error) {
				return e.PlanSegments(wav, waxflow.TranscodeOptions{Format: "aac"}, 1)
			}},
		{"transcode/flac", map[string]string{"src": "lib/ramp.wav", "format": "flac", "segDur": "1"}, nil,
			func() (*waxflow.SegmentPlan, error) {
				return e.PlanSegments(wav, waxflow.TranscodeOptions{Format: "flac"}, 1)
			}},
		{"remux", map[string]string{"src": ref, "format": "opus", "segDur": "1"}, &met.Remuxes,
			func() (*waxflow.SegmentPlan, error) {
				rp, err := e.PlanRemuxSegments(opus, asOpus, 1, grid)
				if rp == nil {
					return nil, fmt.Errorf("the remux rung declined: %v", err)
				}
				return &rp.SegmentPlan, nil
			}},
		{"cut", map[string]string{"src": ref, "format": "opus", "segDur": "1", "from": "48000", "to": "144000"}, &met.Cuts,
			func() (*waxflow.SegmentPlan, error) {
				cp, err := e.PlanCutSegments(opus, asOpus, []waxflow.Span{{From: 48000, To: 144000}}, grid, 1)
				if cp == nil {
					return nil, fmt.Errorf("the cut rung declined: %v", err)
				}
				return &cp.SegmentPlan, nil
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc := hlsDescriptorFor(t, env, tc.params)
			resp := env.get(t, "/hls/media.m3u8?v="+desc, nil)
			served := readBody(t, resp)
			if resp.StatusCode != 200 {
				t.Fatalf("media = %d: %s", resp.StatusCode, served)
			}
			pl, err := m3u8.ParseMedia(string(served))
			if err != nil {
				t.Fatal(err)
			}
			plan, err := tc.plan()
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(pl.Segments)) != plan.Segments {
				t.Fatalf("the daemon lists %d segments, the plan %d", len(pl.Segments), plan.Segments)
			}
			segs, err := plan.MediaSegments(func(n int64) string { return pl.Segments[n].URI })
			if err != nil {
				t.Fatal(err)
			}
			got, err := m3u8.Media(pl.InitURI, segs)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(served) {
				t.Errorf("plan renders:\n%s\nthe daemon serves:\n%s", got, served)
			}
			if tc.counter == nil {
				return
			}
			before := tc.counter.Load()
			if r := env.get(t, "/hls/"+pl.Segments[0].URI, nil); readBody(t, r) == nil || r.StatusCode != 200 {
				t.Fatalf("segment 0 = %d", r.StatusCode)
			}
			if tc.counter.Load() == before {
				t.Error("the daemon served segment 0 on another rung than the one planned")
			}
		})
	}
}

// writeSilentWAV writes an 8 kHz mono 8-bit WAV of frames samples whose data
// the file system leaves sparse, for a source long enough to plan past a cap
// without paying for it on disk.
func writeSilentWAV(t *testing.T, path string, frames uint32) {
	t.Helper()
	h := []byte("RIFF\x00\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x40\x1f\x00\x00\x40\x1f\x00\x00\x01\x00\x08\x00data\x00\x00\x00\x00")
	binary.LittleEndian.PutUint32(h[4:], 36+frames)
	binary.LittleEndian.PutUint32(h[40:], frames)
	if err := os.WriteFile(path, h, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, int64(len(h))+int64(frames)); err != nil {
		t.Fatal(err)
	}
}

// TestHLSRefusesTooManySegmentsAtEveryEntry pins the playlist cap where a
// player is first handed a URL: a mint and a master refuse a variant whose
// media playlist would be refused, rather than answer 200 and leave the
// player to find out at media.m3u8. 1400 s at a one-frame Opus segDur is
// 70000 segments, past the 65536 one playlist lists.
func TestHLSRefusesTooManySegmentsAtEveryEntry(t *testing.T) {
	env := newTestEnv(t, nil)
	writeSilentWAV(t, filepath.Join(env.root, "long.wav"), 1400*8000)
	params := map[string]string{"src": "lib/long.wav", "format": "opus", "segDur": "0.02"}

	c, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Sign(t.Context(), client.SignRequest{Path: "/hls/master.m3u8", Params: params}); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Errorf("mint: %v, want %s", err, waxerr.CodeInvalidRequest)
	}
	wantEnvelope(t, env.get(t, "/hls/master.m3u8?src=lib/long.wav&format=opus&segDur=0.02", nil),
		http.StatusBadRequest, waxerr.CodeInvalidRequest)

	// The same source at 4 s segments mints, so the refusal is the count.
	params["segDur"] = "4"
	mintHLS(t, env, params)
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
	mediaRef := variantURIs(t, master)[0]
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
	mediaRef := variantURIs(t, master)[0]

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
	info, err := waxflow.New().Probe(context.Background(), container.BytesSource(tagless), "mp3", nil)
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

// TestSingleSegmentTargetDuration is the one place U4 moves a number a player
// reads directly. TARGETDURATION is a ceiling over the EXTINF values, and the
// shrink can only touch the first segment and the last; in a multi-segment
// playlist the maximum sits on an interior segment and does not move. With a
// single segment the only one is both, so the ceiling follows it down: a 4 s
// source through a delayed codec listed 4.02 s and declared 5.
func TestSingleSegmentTargetDuration(t *testing.T) {
	env := newTestEnv(t, nil)
	for _, name := range []string{"opus", "aac", "flac"} {
		t.Run(name, func(t *testing.T) {
			desc := hlsDescriptorFor(t, env, map[string]string{
				"src": "lib/ramp.wav", "format": name, "segDur": "10",
			})
			resp := env.get(t, "/hls/media.m3u8?v="+desc, nil)
			body := readBody(t, resp)
			if resp.StatusCode != 200 {
				t.Fatalf("media = %d: %s", resp.StatusCode, body)
			}
			_, segURIs, extinf := mediaURIs(t, body)
			if len(segURIs) != 1 {
				t.Fatalf("%d segments at segDur 10 on a 4 s source, want 1:\n%s", len(segURIs), body)
			}
			if math.Abs(extinf[0]-4.0) > 5e-6 {
				t.Errorf("EXTINF %.5f, want the source's 4 s", extinf[0])
			}
			// The whole line, newline included: a bare Contains also passes
			// for :40 or :45, and this test exists to prove the ceiling
			// followed the single segment down from 5 to 4.
			if !bytes.Contains(body, []byte("#EXT-X-TARGETDURATION:4\n")) {
				t.Errorf("TARGETDURATION did not follow the single segment down:\n%s", body)
			}
		})
	}
}

// TestHLSMasterAdvertisesHECopy pins the master playlist against the rung
// the media path serves: a remux-eligible HE-AAC source under format=aac
// resolves to the he-aac row (mp4a.40.5), and the master must say so.
// Before the master planned with the remux source, it advertised the
// transcode rung's mp4a.40.2 while the segments carried AOT-5 packets.
func TestHLSMasterAdvertisesHECopy(t *testing.T) {
	env := newTestEnv(t, nil)
	fixture, err := os.ReadFile(filepath.Join("..", "codec", "aac", "testdata", "fdk_he_v1.m4a"))
	if err != nil {
		t.Fatalf("reading the committed HE-AAC fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(env.root, "he.m4a"), fixture, 0o644); err != nil {
		t.Fatal(err)
	}

	masterURL := mintHLS(t, env, map[string]string{"src": "lib/he.m4a", "format": "aac"})
	resp := keyless(t, env, masterURL)
	master := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("master = %d: %s", resp.StatusCode, master)
	}
	if !bytes.Contains(master, []byte(`CODECS="mp4a.40.5"`)) {
		t.Fatalf("master does not advertise the HE-AAC copy as mp4a.40.5: %s", master)
	}
	if bytes.Contains(master, []byte(`mp4a.40.2`)) {
		t.Fatalf("master still advertises an LC re-encode for an HE copy: %s", master)
	}

	// The media playlist behind it serves the same variant.
	mediaRef := variantURIs(t, master)[0]
	resp = keyless(t, env, "/hls/"+mediaRef)
	if media := readBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("media playlist behind the HE master = %d: %s", resp.StatusCode, media)
	}
}

// TestHLSMintRefusesNonUTF8Src pins that a source name the descriptor cannot
// carry is refused at mint rather than silently rewritten. The descriptor
// travels as JSON, and json.Marshal substitutes U+FFFD for every invalid byte,
// so the round trip inside mintHLSDescriptor used to hand back a src naming a
// file that does not exist. The second half measures rather than assumes that
// the limit is the descriptor's and not the library's: /stream carries no JSON
// and serves the same file. That control needs a library holding the name,
// which only Linux does.
func TestHLSMintRefusesNonUTF8Src(t *testing.T) {
	env := newTestEnv(t, nil)
	const name = "caf\xe9.wav"
	src := url.QueryEscape("lib/" + name)

	// A file name is opaque bytes only on Linux. macOS rejects one that is not
	// valid UTF-8 (EILSEQ from the write below), and Windows names files in
	// UTF-16, where the byte becomes U+FFFD and the file created is not the
	// file asked for. The refusal needs no file of its own, since mint reads
	// the parameter before it resolves anything, so on those two it is checked
	// without the control.
	hasFile := runtime.GOOS == "linux"
	if hasFile {
		b, err := os.ReadFile(filepath.Join(env.root, "sine.wav"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(env.root, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	resp := env.get(t, "/hls/master.m3u8?src="+src, nil)
	body := readBody(t, resp)
	if resp.StatusCode != 400 {
		t.Fatalf("non-UTF-8 src minted with %d: %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("not valid UTF-8")) {
		t.Fatalf("the refusal does not state the UTF-8 rule: %s", body)
	}
	if !hasFile {
		return
	}

	resp = env.get(t, "/stream?src="+src, nil)
	if got := readBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("/stream on the same file = %d: %s; the HLS refusal is the library's, not the descriptor's", resp.StatusCode, got)
	}
}

// TestHLSMeasuresAnAdvisoryLength is the playlist's own rule. A VOD playlist
// promises an exact segment count, and for a source whose headers only
// estimate its total (ASF's ticks, a Matroska Info Duration, a WebM Opus file
// a tolerant probe did not walk) taking that estimate at its word yields a
// tail 404 or an early ENDLIST. trackFor hands a non-exact caller the headers'
// number on purpose, so the playlist asks again for a measured one.
//
// The check is that every segment the playlist lists can actually be fetched,
// which is what an over-estimate breaks, and that the last one is served
// rather than 404ing, which is what an under-estimate breaks.
func TestHLSMeasuresAnAdvisoryLength(t *testing.T) {
	env := newTestEnv(t, nil)
	masterURL := mintHLS(t, env, map[string]string{"src": "lib/seed.webm", "format": "aac"})
	master := readBody(t, keyless(t, env, masterURL))
	mediaRef := variantURIs(t, master)[0]
	resp := keyless(t, env, "/hls/"+mediaRef)
	media := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("media = %d: %s", resp.StatusCode, media)
	}
	initURI, segURIs, extinf := mediaURIs(t, media)
	if initURI == "" || len(segURIs) == 0 {
		t.Fatalf("init %q, %d segments:\n%s", initURI, len(segURIs), media)
	}

	// The two numbers this is about, taken from the surface that reports
	// them: a tolerant probe states the headers' estimate, a strict one walks
	// and measures. The fixture's Duration overstates by 8 ms, which is the
	// tail the playlist must not promise.
	declared := probeSeconds(t, env, "/probe?src=lib/seed.webm")
	measured := probeSeconds(t, env, "/probe?src=lib/seed.webm&strict=1")
	if declared == measured {
		t.Fatalf("the fixture's estimate (%v) equals its measure; this cell needs a gap", declared)
	}
	var sum float64
	for _, d := range extinf {
		sum += d
	}
	tol := float64(len(extinf)) * 5e-6 // the playlist prints %.5f
	if math.Abs(sum-measured) > tol {
		t.Fatalf("EXTINF sum %.5f, want the measured %.5f (the headers estimate %.5f)", sum, measured, declared)
	}

	for i, u := range segURIs {
		r := keyless(t, env, "/hls/"+u)
		body := readBody(t, r)
		if r.StatusCode != 200 {
			t.Fatalf("segment %d of %d = %d: the playlist promised a segment the stream cannot fill: %s",
				i, len(segURIs), r.StatusCode, body)
		}
		if len(body) == 0 {
			t.Fatalf("segment %d of %d is empty", i, len(segURIs))
		}
	}
}

// probeSeconds reads the default track's durationSeconds off a probe.
func probeSeconds(t *testing.T, env *testEnv, path string) float64 {
	t.Helper()
	resp := env.get(t, path, nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("%s = %d: %s", path, resp.StatusCode, body)
	}
	var doc struct {
		Tracks []struct {
			DurationSeconds float64 `json:"durationSeconds"`
		} `json:"tracks"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Tracks) != 1 {
		t.Fatalf("%s returned %d tracks", path, len(doc.Tracks))
	}
	return doc.Tracks[0].DurationSeconds
}
