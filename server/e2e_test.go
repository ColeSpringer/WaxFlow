// The progressive-streaming acceptance suite: auth including the
// fail-closed rule, signature tamper/expiry/source-changed, envelope
// codes, the Range matrix across live/cached/direct-play, sample-exact
// t= seek verified by decoding response audio, 503 under load, cache
// write-through with flight dedup, disk-full degradation, and CORS.
package server_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/client"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

var update = flag.Bool("update", false, "rewrite golden response fixtures")

const (
	testKey    = "test-api-key"
	testSecret = "0123456789abcdef0123456789abcdef"
)

// rampWAV renders a 16-bit ramp signal (sample values identify positions,
// which the seek tests rely on) as a WAV file.
func rampWAV(t *testing.T, rate, channels, frames int) []byte {
	t.Helper()
	f := audio.Format{Rate: rate, Channels: channels, Layout: audio.DefaultLayout(channels), Type: audio.Int, BitDepth: 16}
	buf := testutil.Ramp(f, frames)
	defer audio.Put(buf)

	enc, err := pcm.NewEncoder(pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, f)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	mux := riff.NewMuxer(&out, nil)
	track := container.Track{Codec: codec.PCM, CodecConfig: enc.CodecConfig(), Fmt: f, Samples: int64(frames), Default: true}
	if err := mux.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error { return mux.WritePacket(container.Packet{Track: 0, Packet: p}) }
	if err := enc.Encode(buf, emit); err != nil {
		t.Fatal(err)
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := mux.End(trailer); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// testEnv is one running server over a populated library root.
type testEnv struct {
	ts    *httptest.Server
	srv   *server.Server
	root  string // library root with track.flac, sine.wav, ramp.wav
	cache string
}

func newTestEnv(t *testing.T, mutate func(*server.Config)) *testEnv {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "album"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct{ src, dst string }{
		{"../testdata/sine-s16.flac", "album/track.flac"},
		{"../testdata/sine-s16.wav", "sine.wav"},
	} {
		b, err := os.ReadFile(fixture.src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, fixture.dst), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "ramp.wav"), rampWAV(t, 48000, 2, 4*48000), 0o644); err != nil {
		t.Fatal(err)
	}

	roots, err := source.OpenRoots([]source.Root{{Name: "lib", Path: root}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	cfg := server.Config{
		Addr:        "127.0.0.1:4418",
		APIKeys:     []string{testKey},
		SigningKeys: []server.SigningKey{{ID: "1", Secret: []byte(testSecret)}},
		Resolver:    roots,
		CacheDir:    cacheDir,
		Version:     "test",
		// PaceFactor stays 0 (disabled) so tests measure logic, not sleeps.
	}
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := server.New(cfg)
	if err != nil {
		roots.Close()
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		ts.Close()
		srv.Close()
		roots.Close()
	})
	return &testEnv{ts: ts, srv: srv, root: root, cache: cacheDir}
}

// get performs a keyed request with optional extra headers.
func (e *testEnv) get(t *testing.T, path string, hdr map[string]string) *http.Response {
	t.Helper()
	return e.req(t, http.MethodGet, path, hdr)
}

func (e *testEnv) req(t *testing.T, method, path string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-Key", testKey)
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func wantEnvelope(t *testing.T, resp *http.Response, status int, code waxerr.Code) {
	t.Helper()
	body := readBody(t, resp)
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d (body: %s)", resp.StatusCode, status, body)
	}
	var env server.ErrorBody
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("not an envelope: %s", body)
	}
	if env.Code != code || env.SchemaVersion != 1 {
		t.Fatalf("envelope = %+v, want code %s", env, code)
	}
}

// decodePCM decodes a container file into one contiguous buffer.
func decodePCM(t *testing.T, raw []byte) *audio.Buffer {
	t.Helper()
	med, err := waxflow.New().OpenStream(container.BytesSource(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	f := med.Info().Default().Fmt
	total := med.Info().Default().Samples
	if total < 0 {
		total = 1 << 22
	}
	out := audio.Get(f, int(total))
	t.Cleanup(func() { audio.Put(out) })
	tmp := audio.Get(f, audio.StandardChunk)
	defer audio.Put(tmp)
	for {
		err := med.ReadChunk(tmp)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		audio.CopyFrames(out, out.N, tmp, 0, tmp.N)
		out.N += tmp.N
	}
	return out
}

// engineReference transcodes a library file exactly like a live session
// (plain non-seeking writer), for byte-for-byte comparisons.
func engineReference(t *testing.T, path string, opts waxflow.TranscodeOptions) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := waxflow.New().Transcode(t.Context(), container.BytesSource(b), "", &out, opts); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestFailClosed(t *testing.T) {
	base := server.Config{CacheDir: t.TempDir()}

	wide := base
	wide.Addr = "0.0.0.0:4418"
	if _, err := server.New(wide); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Fatalf("keyless on 0.0.0.0 must refuse to start, got %v", err)
	}

	wide.AllowUnauthenticated = true
	if srv, err := server.New(wide); err != nil {
		t.Fatalf("explicit allowUnauthenticated must start: %v", err)
	} else {
		srv.Close()
	}

	keyed := base
	keyed.Addr = "0.0.0.0:4418"
	keyed.APIKeys = []string{"k"}
	keyed.CacheDir = t.TempDir()
	if srv, err := server.New(keyed); err != nil {
		t.Fatalf("keyed non-loopback must start: %v", err)
	} else {
		srv.Close()
	}

	loop := base
	loop.Addr = "127.0.0.1:4418"
	loop.CacheDir = t.TempDir()
	if srv, err := server.New(loop); err != nil {
		t.Fatalf("keyless loopback must start: %v", err)
	} else {
		srv.Close()
	}
}

func TestAuthMatrix(t *testing.T) {
	env := newTestEnv(t, func(c *server.Config) { c.MetricsKey = "metrics-only" })

	// /ping is open.
	resp := env.get(t, "/ping", map[string]string{"X-API-Key": ""})
	if readBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("/ping without key = %d", resp.StatusCode)
	}

	// Control endpoints demand a key.
	for _, path := range []string{"/version", "/caps", "/probe?src=lib/sine.wav", "/cache/stats"} {
		resp := env.get(t, path, map[string]string{"X-API-Key": ""})
		wantEnvelope(t, resp, http.StatusUnauthorized, waxerr.CodeUnauthorized)
		resp = env.get(t, path, map[string]string{"X-API-Key": "wrong"})
		wantEnvelope(t, resp, http.StatusUnauthorized, waxerr.CodeUnauthorized)
	}

	// Bearer form works.
	resp = env.get(t, "/version", map[string]string{"X-API-Key": "", "Authorization": "Bearer " + testKey})
	if readBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("bearer auth = %d", resp.StatusCode)
	}

	// Playback needs key or sig.
	resp = env.get(t, "/stream?src=lib/sine.wav", map[string]string{"X-API-Key": ""})
	wantEnvelope(t, resp, http.StatusUnauthorized, waxerr.CodeUnauthorized)

	// Metrics: dedicated key unlocks, wrong key does not.
	resp = env.get(t, "/metrics", map[string]string{"X-API-Key": "metrics-only"})
	if readBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("metrics with metricsKey = %d", resp.StatusCode)
	}
	resp = env.get(t, "/metrics", map[string]string{"X-API-Key": "nope"})
	wantEnvelope(t, resp, http.StatusUnauthorized, waxerr.CodeUnauthorized)
}

// downCatalog stands in for the resolver flavor with an unreachable
// catalog: every resolve fails catalog-unavailable.
type downCatalog struct{}

func (downCatalog) Resolve(string) (*source.File, error) {
	return nil, waxerr.New(waxerr.CodeCatalogUnavailable, "catalog query failed")
}

func TestCatalogUnavailableRetryAfter(t *testing.T) {
	env := newTestEnv(t, func(cfg *server.Config) {
		cfg.Resolver = downCatalog{}
	})
	resp := env.get(t, "/probe?src=pid:01ARZ3NDEKTSV4RRFFQ69G5FAV", nil)
	wantEnvelope(t, resp, http.StatusServiceUnavailable, waxerr.CodeCatalogUnavailable)
	// api.md promises Retry-After on catalog-unavailable, like overloaded:
	// the condition is transient and clients should come back.
	if got := resp.Header.Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want 2", got)
	}
}

func TestGoldenResponses(t *testing.T) {
	env := newTestEnv(t, nil)
	cases := []struct {
		name, method, path string
		hdr                map[string]string
	}{
		{"ping", http.MethodGet, "/ping", nil},
		{"version", http.MethodGet, "/version", nil},
		{"caps", http.MethodGet, "/caps", nil},
		{"probe-sine-wav", http.MethodGet, "/probe?src=lib/sine.wav", nil},
		{"notfound", http.MethodGet, "/no/such/endpoint", nil},
		{"unauthorized", http.MethodPost, "/cache/gc", map[string]string{"X-API-Key": "wrong"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := env.req(t, tc.method, tc.path, tc.hdr)
			body := readBody(t, resp)
			golden := filepath.Join("testdata", "golden", tc.name+".json")
			if *update {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, body, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("missing golden (run with -update): %v", err)
			}
			if !bytes.Equal(body, want) {
				t.Errorf("response drifted from golden %s:\n got: %s\nwant: %s", golden, body, want)
			}
		})
	}
}

func TestDirectPlayFLACWithFullRange(t *testing.T) {
	env := newTestEnv(t, nil)
	orig, err := os.ReadFile(filepath.Join(env.root, "album", "track.flac"))
	if err != nil {
		t.Fatal(err)
	}

	resp := env.get(t, "/stream?src=lib/album/track.flac", nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/flac" {
		t.Fatalf("direct play = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("direct play must advertise ranges, got %q", resp.Header.Get("Accept-Ranges"))
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("direct play must send a strong ETag")
	}
	if !bytes.Equal(body, orig) {
		t.Fatal("direct play must serve the original bytes")
	}

	// Nonzero range: real 206 partial content.
	resp = env.get(t, "/stream?src=lib/album/track.flac", map[string]string{"Range": "bytes=100-199"})
	part := readBody(t, resp)
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(part, orig[100:200]) {
		t.Fatalf("range on direct play = %d, %d bytes", resp.StatusCode, len(part))
	}
	if cr := resp.Header.Get("Content-Range"); !strings.HasPrefix(cr, "bytes 100-199/") {
		t.Fatalf("Content-Range = %q", cr)
	}

	// Conditional request via the ETag.
	resp = env.get(t, "/stream?src=lib/album/track.flac", map[string]string{"If-None-Match": etag})
	if readBody(t, resp); resp.StatusCode != http.StatusNotModified {
		t.Fatalf("If-None-Match = %d, want 304", resp.StatusCode)
	}

	// HEAD serves headers with the real length and no body.
	resp = env.req(t, http.MethodHead, "/stream?src=lib/album/track.flac", nil)
	if readBody(t, resp); resp.StatusCode != 200 || resp.ContentLength != int64(len(orig)) {
		t.Fatalf("HEAD = %d, len %d, want %d", resp.StatusCode, resp.ContentLength, len(orig))
	}

	if v := env.srv.Metrics().DirectPlays.Load(); v < 2 {
		t.Fatalf("direct_play_total = %d", v)
	}
}

func TestStreamTranscodeWriteThrough(t *testing.T) {
	env := newTestEnv(t, nil)
	want := engineReference(t, filepath.Join(env.root, "album", "track.flac"), waxflow.TranscodeOptions{Format: "wav"})

	resp := env.get(t, "/stream?src=lib/album/track.flac&format=wav", nil)
	first := readBody(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/wav" {
		t.Fatalf("live transcode = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Accept-Ranges") != "none" {
		t.Fatalf("live Accept-Ranges = %q", resp.Header.Get("Accept-Ranges"))
	}
	if resp.Header.Get("X-Content-Duration") == "" || resp.Header.Get("X-Estimated-Content-Length") == "" {
		t.Fatal("live duration/size hints missing")
	}
	if !bytes.Equal(first, want) {
		t.Fatalf("live stream bytes differ from the engine reference (%d vs %d bytes)", len(first), len(want))
	}

	// The same URL now serves the completed cache entry: real length,
	// ranges, strong ETag.
	deadline := time.Now().Add(5 * time.Second)
	var cached *http.Response
	for {
		cached = env.get(t, "/stream?src=lib/album/track.flac&format=wav", nil)
		if cached.Header.Get("Accept-Ranges") == "bytes" || time.Now().After(deadline) {
			break
		}
		readBody(t, cached)
		time.Sleep(10 * time.Millisecond)
	}
	second := readBody(t, cached)
	if cached.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatal("completed entry must serve with full range support")
	}
	if cached.ContentLength != int64(len(want)) || !bytes.Equal(second, want) {
		t.Fatalf("cached response differs (len %d vs %d)", cached.ContentLength, len(want))
	}
	etag := cached.Header.Get("ETag")
	if etag == "" {
		t.Fatal("cached response needs the cache-key ETag")
	}

	// Range matrix, cached rung: real partial content.
	resp = env.get(t, "/stream?src=lib/album/track.flac&format=wav", map[string]string{"Range": "bytes=44-127"})
	part := readBody(t, resp)
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(part, want[44:128]) {
		t.Fatalf("cached range = %d, %d bytes", resp.StatusCode, len(part))
	}

	// The store owns hit/miss accounting (shared by /cache/stats and
	// /metrics): the cold request missed, the cached replays hit.
	if st := cacheStats(t, env); st.Hits < 1 || st.Misses < 1 {
		t.Fatalf("stats = %+v, want hits and misses both counted", st)
	}
}

// TestStreamTranscodeFLACWriteThrough runs the write-through matrix for
// the first compressed encoder: live FLAC bytes equal the engine
// reference, size hints stay honestly absent (VBR output has no
// projected size), and the completed cache entry serves with full
// ranges and the cache-key ETag.
func TestStreamTranscodeFLACWriteThrough(t *testing.T) {
	env := newTestEnv(t, nil)
	want := engineReference(t, filepath.Join(env.root, "sine.wav"), waxflow.TranscodeOptions{Format: "flac"})

	resp := env.get(t, "/stream?src=lib/sine.wav&format=flac", nil)
	first := readBody(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/flac" {
		t.Fatalf("live transcode = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Accept-Ranges") != "none" {
		t.Fatalf("live Accept-Ranges = %q", resp.Header.Get("Accept-Ranges"))
	}
	if resp.Header.Get("X-Content-Duration") == "" {
		t.Fatal("live duration hint missing")
	}
	if got := resp.Header.Get("X-Estimated-Content-Length"); got != "" {
		t.Fatalf("size hint %q on a VBR stream, want none (size is signal-dependent)", got)
	}
	if !bytes.Equal(first, want) {
		t.Fatalf("live stream bytes differ from the engine reference (%d vs %d bytes)", len(first), len(want))
	}

	// The same URL now serves the completed cache entry: real length,
	// ranges, strong ETag, conditional revalidation.
	deadline := time.Now().Add(5 * time.Second)
	var cached *http.Response
	for {
		cached = env.get(t, "/stream?src=lib/sine.wav&format=flac", nil)
		if cached.Header.Get("Accept-Ranges") == "bytes" || time.Now().After(deadline) {
			break
		}
		readBody(t, cached)
		time.Sleep(10 * time.Millisecond)
	}
	second := readBody(t, cached)
	if cached.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatal("completed entry must serve with full range support")
	}
	if cached.ContentLength != int64(len(want)) || !bytes.Equal(second, want) {
		t.Fatalf("cached response differs (len %d vs %d)", cached.ContentLength, len(want))
	}
	etag := cached.Header.Get("ETag")
	if etag == "" {
		t.Fatal("cached response needs the cache-key ETag")
	}

	resp = env.get(t, "/stream?src=lib/sine.wav&format=flac", map[string]string{"Range": "bytes=10-99"})
	part := readBody(t, resp)
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(part, want[10:100]) {
		t.Fatalf("cached range = %d, %d bytes", resp.StatusCode, len(part))
	}
	resp = env.get(t, "/stream?src=lib/sine.wav&format=flac", map[string]string{"If-None-Match": etag})
	if readBody(t, resp); resp.StatusCode != http.StatusNotModified {
		t.Fatalf("If-None-Match with the entry's ETag = %d, want 304", resp.StatusCode)
	}
}

// TestStreamTranscodeALAC pins the ALAC encoder and fragmented-MP4 muxer
// over /stream: a live lossless transcode (VBR, so no size hint) that
// matches the engine byte for byte and promotes to a range-served cache
// entry, exactly like the FLAC path but delivering audio/mp4.
func TestStreamTranscodeALAC(t *testing.T) {
	env := newTestEnv(t, nil)
	want := engineReference(t, filepath.Join(env.root, "sine.wav"), waxflow.TranscodeOptions{Format: "alac"})

	resp := env.get(t, "/stream?src=lib/sine.wav&format=alac", nil)
	first := readBody(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/mp4" {
		t.Fatalf("live transcode = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Accept-Ranges") != "none" {
		t.Fatalf("live Accept-Ranges = %q", resp.Header.Get("Accept-Ranges"))
	}
	if resp.Header.Get("X-Content-Duration") == "" {
		t.Fatal("live duration hint missing")
	}
	if got := resp.Header.Get("X-Estimated-Content-Length"); got != "" {
		t.Fatalf("size hint %q on a VBR stream, want none (size is signal-dependent)", got)
	}
	if !bytes.Equal(first, want) {
		t.Fatalf("live stream bytes differ from the engine reference (%d vs %d bytes)", len(first), len(want))
	}

	// The completed cache entry serves with real length, ranges, and ETag.
	deadline := time.Now().Add(5 * time.Second)
	var cached *http.Response
	for {
		cached = env.get(t, "/stream?src=lib/sine.wav&format=alac", nil)
		if cached.Header.Get("Accept-Ranges") == "bytes" || time.Now().After(deadline) {
			break
		}
		readBody(t, cached)
		time.Sleep(10 * time.Millisecond)
	}
	second := readBody(t, cached)
	if cached.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatal("completed entry must serve with full range support")
	}
	if cached.ContentLength != int64(len(want)) || !bytes.Equal(second, want) {
		t.Fatalf("cached response differs (len %d vs %d)", cached.ContentLength, len(want))
	}
	if cached.Header.Get("ETag") == "" {
		t.Fatal("cached response needs the cache-key ETag")
	}
}

// TestStreamALACRejectsBitrate confirms the lossless ALAC output refuses the
// lossy quality parameters, like any other lossless format.
func TestStreamALACRejectsBitrate(t *testing.T) {
	env := newTestEnv(t, nil)
	resp := env.get(t, "/stream?src=lib/sine.wav&format=alac&bitrate=128", nil)
	readBody(t, resp)
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("alac with bitrate = %d, want 415", resp.StatusCode)
	}
}

// TestStreamTranscodeMP3 pins the baseline MP3 encoder over /stream: a live
// CBR transcode that carries a size estimate (unlike VBR FLAC), matches the
// engine byte for byte, and promotes to a range-served cache entry.
func TestStreamTranscodeMP3(t *testing.T) {
	env := newTestEnv(t, nil)
	want := engineReference(t, filepath.Join(env.root, "sine.wav"),
		waxflow.TranscodeOptions{Format: "mp3", MP3Bitrate: 128000})

	resp := env.get(t, "/stream?src=lib/sine.wav&format=mp3&bitrate=128", nil)
	first := readBody(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("live mp3 transcode = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Accept-Ranges") != "none" {
		t.Fatalf("live Accept-Ranges = %q", resp.Header.Get("Accept-Ranges"))
	}
	if resp.Header.Get("X-Content-Duration") == "" {
		t.Fatal("live duration hint missing")
	}
	// CBR MP3 has a fixed bit rate, so the size estimate is present.
	if resp.Header.Get("X-Estimated-Content-Length") == "" {
		t.Fatal("CBR mp3 size estimate missing")
	}
	if !bytes.Equal(first, want) {
		t.Fatalf("live mp3 bytes differ from the engine reference (%d vs %d bytes)", len(first), len(want))
	}
	// The output must be a valid MP3: it starts with a frame sync.
	if len(first) < 2 || first[0] != 0xFF || first[1]&0xE0 != 0xE0 {
		t.Fatal("output does not begin with an MPEG frame sync")
	}

	deadline := time.Now().Add(5 * time.Second)
	var cached *http.Response
	for {
		cached = env.get(t, "/stream?src=lib/sine.wav&format=mp3&bitrate=128", nil)
		if cached.Header.Get("Accept-Ranges") == "bytes" || time.Now().After(deadline) {
			break
		}
		readBody(t, cached)
		time.Sleep(10 * time.Millisecond)
	}
	second := readBody(t, cached)
	if cached.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatal("completed mp3 entry must serve with full range support")
	}
	if cached.ContentLength != int64(len(want)) || !bytes.Equal(second, want) {
		t.Fatalf("cached mp3 differs (len %d vs %d)", cached.ContentLength, len(want))
	}

	// A different bitrate is a different cache entry (canonical params key on
	// the resolved bit rate), so it re-encodes rather than serving the 128k
	// bytes.
	other := readBody(t, env.get(t, "/stream?src=lib/sine.wav&format=mp3&bitrate=192", nil))
	if bytes.Equal(other, want) {
		t.Fatal("bitrate=192 served the 128k cache entry; bitrate is not in the cache key")
	}
}

// TestStreamTranscodeOpus streams a WAV source as Ogg-Opus and checks the live
// transcode matches the engine byte-for-byte and is a well-formed Ogg stream.
func TestStreamTranscodeOpus(t *testing.T) {
	env := newTestEnv(t, nil)
	want := engineReference(t, filepath.Join(env.root, "sine.wav"),
		waxflow.TranscodeOptions{Format: "opus", OpusBitrate: 96000})

	resp := env.get(t, "/stream?src=lib/sine.wav&format=opus", nil)
	first := readBody(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/ogg" {
		t.Fatalf("live opus transcode = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Accept-Ranges") != "none" {
		t.Fatalf("live Accept-Ranges = %q", resp.Header.Get("Accept-Ranges"))
	}
	if !bytes.Equal(first, want) {
		t.Fatalf("live opus bytes differ from the engine reference (%d vs %d bytes)", len(first), len(want))
	}
	// The output must be a valid Ogg stream: it starts with a capture pattern.
	if len(first) < 4 || string(first[:4]) != "OggS" {
		t.Fatal("opus output does not begin with an Ogg capture pattern")
	}
}

// TestStreamTranscodeAAC streams a WAV source as AAC in both containers:
// the default progressive fMP4 (audio/mp4) and the container=adts legacy
// opt-out (audio/aac, a raw ADTS elementary stream). The two are distinct
// cache entries (the canonical params carry the container).
func TestStreamTranscodeAAC(t *testing.T) {
	env := newTestEnv(t, nil)

	resp := env.get(t, "/stream?src=lib/sine.wav&format=aac&bitrate=128", nil)
	fmp4 := readBody(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/mp4" {
		t.Fatalf("live aac transcode = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if len(fmp4) < 8 || string(fmp4[4:8]) != "ftyp" {
		t.Fatal("aac fMP4 output does not begin with an ftyp box")
	}

	resp = env.get(t, "/stream?src=lib/sine.wav&format=aac&bitrate=128&container=adts", nil)
	adts := readBody(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/aac" {
		t.Fatalf("live adts transcode = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if len(adts) < 2 || adts[0] != 0xFF || adts[1]&0xF0 != 0xF0 {
		t.Fatal("adts output does not begin with an ADTS syncword")
	}
	if bytes.Equal(adts, fmp4) {
		t.Fatal("container=adts served the fMP4 cache entry; the container is not in the cache key")
	}

	// container= on a format without alternates, and with auto, both fail
	// up front with the invalid-request envelope.
	resp = env.get(t, "/stream?src=lib/sine.wav&format=mp3&container=adts", nil)
	if resp.StatusCode != 400 {
		t.Fatalf("container=adts on mp3 = %d, want 400", resp.StatusCode)
	}
	readBody(t, resp)
	resp = env.get(t, "/stream?src=lib/sine.wav&container=adts", nil)
	if resp.StatusCode != 400 {
		t.Fatalf("container=adts with format=auto = %d, want 400", resp.StatusCode)
	}
	readBody(t, resp)
}

// TestStreamMP3BitrateEdges checks the lossy quality-parameter edges: q=high
// clamps to the layer maximum on a low output rate instead of erroring, and an
// empty q= value is ignored rather than tripping the q/bitrate exclusion.
func TestStreamMP3BitrateEdges(t *testing.T) {
	env := newTestEnv(t, nil)

	// q=high is 192, illegal on the MPEG-2 layer (24 kHz output), so it must
	// clamp to 160 and stream rather than 415.
	resp := env.get(t, "/stream?src=lib/sine.wav&format=mp3&rate=24000&q=high", nil)
	if b := readBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("q=high on a 24 kHz output = %d (%s), want 200 (clamped)", resp.StatusCode, b)
	}
	if resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Errorf("content type %q, want audio/mpeg", resp.Header.Get("Content-Type"))
	}

	// An empty q= must be treated as absent, not as conflicting with bitrate.
	resp = env.get(t, "/stream?src=lib/sine.wav&format=mp3&q=&bitrate=128", nil)
	if readBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("empty q with bitrate = %d, want 200 (q ignored)", resp.StatusCode)
	}
}

// TestStreamFLACFormatLadder pins the decision ladder around the flac
// encoder: a compliant FLAC source under format=flac direct-plays the
// original bytes, and t>0 forces a live re-encode.
func TestStreamFLACFormatLadder(t *testing.T) {
	env := newTestEnv(t, nil)
	orig, err := os.ReadFile(filepath.Join(env.root, "album", "track.flac"))
	if err != nil {
		t.Fatal(err)
	}

	resp := env.get(t, "/stream?src=lib/album/track.flac&format=flac", nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("compliant source did not direct-play: %d, Accept-Ranges %q", resp.StatusCode, resp.Header.Get("Accept-Ranges"))
	}
	if !bytes.Equal(body, orig) {
		t.Fatal("direct play must serve the original bytes")
	}

	resp = env.get(t, "/stream?src=lib/album/track.flac&format=flac&t=0.1", nil)
	body = readBody(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("Accept-Ranges") != "none" {
		t.Fatalf("t>0 must transcode live: %d, Accept-Ranges %q", resp.StatusCode, resp.Header.Get("Accept-Ranges"))
	}
	if resp.Header.Get("Content-Type") != "audio/flac" || bytes.Equal(body, orig) {
		t.Fatal("t>0 response must be a fresh FLAC encode, not the original bytes")
	}
}

func TestRangeMatrixLive(t *testing.T) {
	env := newTestEnv(t, nil)

	// bytes=0- on a fresh (live) transcode: plain 200 full stream.
	resp := env.get(t, "/stream?src=lib/ramp.wav&format=wav&rate=24000", map[string]string{"Range": "bytes=0-"})
	if readBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("bytes=0- on live = %d, want 200", resp.StatusCode)
	}

	// A nonzero offset on an uncached key: 416 plus envelope, and no
	// pipeline spawned for it.
	before := env.srv.Metrics().SessionsLive.Load()
	resp = env.get(t, "/stream?src=lib/ramp.wav&format=wav&rate=12000", map[string]string{"Range": "bytes=100-"})
	wantEnvelope(t, resp, http.StatusRequestedRangeNotSatisfiable, waxerr.CodeInvalidRequest)
	if after := env.srv.Metrics().SessionsLive.Load(); after != before {
		t.Fatalf("refused range spawned a pipeline (%d -> %d)", before, after)
	}
}

func TestSeekSampleExact(t *testing.T) {
	env := newTestEnv(t, nil)
	srcBytes, err := os.ReadFile(filepath.Join(env.root, "ramp.wav"))
	if err != nil {
		t.Fatal(err)
	}
	src := decodePCM(t, srcBytes)

	const tSec = 1.5
	from := int(tSec * 48000)
	resp := env.get(t, "/stream?src=lib/ramp.wav&format=wav&t=1.5", nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("seek stream = %d", resp.StatusCode)
	}
	got := decodePCM(t, body)
	if got.N != src.N-from {
		t.Fatalf("seeked stream has %d frames, want %d", got.N, src.N-from)
	}
	for c := 0; c < 2; c++ {
		wantCh := src.ChanI(c)[from:]
		gotCh := got.ChanI(c)
		for i := range wantCh {
			if wantCh[i] != gotCh[i] {
				t.Fatalf("channel %d frame %d: got %d, want %d (seek not sample-exact)", c, i, gotCh[i], wantCh[i])
			}
		}
	}
}

func TestSignedURLLifecycle(t *testing.T) {
	env := newTestEnv(t, nil)
	cl, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}

	minted, err := cl.Sign(t.Context(), client.SignRequest{Params: map[string]string{
		"src": "lib/album/track.flac", "format": "wav",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if minted.Exp <= time.Now().Unix() {
		t.Fatalf("exp %d not in the future", minted.Exp)
	}

	// The signed URL plays with no API key, GET and HEAD both.
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		resp := env.req(t, method, minted.URL, map[string]string{"X-API-Key": ""})
		if readBody(t, resp); resp.StatusCode != 200 {
			t.Fatalf("%s signed URL = %d", method, resp.StatusCode)
		}
	}

	// Tampering with any parameter invalidates.
	u, err := url.Parse(minted.URL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("format", "auto")
	resp := env.get(t, u.Path+"?"+q.Encode(), map[string]string{"X-API-Key": ""})
	wantEnvelope(t, resp, http.StatusForbidden, waxerr.CodeSignatureInvalid)

	// Expired URLs (minted offline with the same secret) report expiry.
	src, _ := source.OpenRoots([]source.Root{{Name: "lib", Path: env.root}}, 0)
	defer src.Close()
	f, err := src.Resolve("lib/album/track.flac")
	if err != nil {
		t.Fatal(err)
	}
	id := f.ID.String()
	f.Close()
	expired, err := client.MintURL("1:"+hexEncode(testSecret), "/stream",
		url.Values{"src": {"lib/album/track.flac"}, "id": {id}}, time.Now().Add(-2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	resp = env.get(t, expired, map[string]string{"X-API-Key": ""})
	wantEnvelope(t, resp, http.StatusForbidden, waxerr.CodeSignatureExpired)

	// A signed URL without the identity parameter is refused.
	noID, err := client.MintURL("1:"+hexEncode(testSecret), "/stream",
		url.Values{"src": {"lib/album/track.flac"}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	resp = env.get(t, noID, map[string]string{"X-API-Key": ""})
	wantEnvelope(t, resp, http.StatusForbidden, waxerr.CodeSignatureInvalid)

	// A valid API key wins over broken signature parameters riding
	// along: the key holder is already fully trusted.
	resp = env.get(t, u.Path+"?"+q.Encode(), nil) // tampered sig, keyed request
	if readBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("keyed request with tampered sig = %d, want 200", resp.StatusCode)
	}
	resp = env.get(t, expired, nil) // expired sig, keyed request
	if readBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("keyed request with expired sig = %d, want 200", resp.StatusCode)
	}

	// Changing the file behind a valid URL yields 410 source-changed.
	path := filepath.Join(env.root, "album", "track.flac")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, 0), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	resp = env.get(t, minted.URL, map[string]string{"X-API-Key": ""})
	wantEnvelope(t, resp, http.StatusGone, waxerr.CodeSourceChanged)
}

func TestAdmission503UnderLoad(t *testing.T) {
	env := newTestEnv(t, func(c *server.Config) {
		c.LiveSlots = 1
	})

	// Occupy the only live slot. Holding it directly keeps the test
	// deterministic; driving it through a real held /transcode is flaky,
	// since that slot is freed the moment its finite body finishes writing
	// and the loopback socket buffers can swallow the whole body first.
	release, ok := env.srv.HoldLiveSlot()
	if !ok {
		t.Fatal("could not take the only live slot")
	}
	// Free the slot even if an assertion below aborts the test; release is
	// idempotent, so the explicit call in the recovery phase still stands.
	t.Cleanup(release)

	// A live transcode cannot start: 503 with Retry-After.
	resp := env.get(t, "/stream?src=lib/ramp.wav&format=wav&t=2", nil)
	if resp.Header.Get("Retry-After") != "2" {
		t.Fatalf("Retry-After = %q", resp.Header.Get("Retry-After"))
	}
	wantEnvelope(t, resp, http.StatusServiceUnavailable, waxerr.CodeOverloaded)
	if v := env.srv.Metrics().AdmissionRejects.Load(); v != 1 {
		t.Fatalf("admission_rejects = %d", v)
	}

	// Cache-served and direct-play requests bypass admission entirely.
	resp = env.get(t, "/stream?src=lib/album/track.flac", nil)
	if readBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("direct play under load = %d", resp.StatusCode)
	}

	// Releasing the held slot lets pipelines start again.
	release()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp = env.get(t, "/stream?src=lib/ramp.wav&format=wav&t=2", nil)
		if resp.StatusCode == 200 {
			readBody(t, resp)
			break
		}
		readBody(t, resp)
		if time.Now().After(deadline) {
			t.Fatal("slot never freed after release")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestDegradedCacheNeverKillsPlayback(t *testing.T) {
	env := newTestEnv(t, nil)
	want := engineReference(t, filepath.Join(env.root, "album", "track.flac"), waxflow.TranscodeOptions{Format: "wav"})

	// Break the cache volume after startup: entry creation fails, the
	// session runs ring-fed, playback still completes byte-exact.
	v1 := filepath.Join(env.cache, "v1")
	if err := os.Chmod(v1, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(v1, 0o755) })

	resp := env.get(t, "/stream?src=lib/album/track.flac&format=wav", nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 || !bytes.Equal(body, want) {
		t.Fatalf("degraded stream = %d, %d bytes (want %d)", resp.StatusCode, len(body), len(want))
	}
	if v := env.srv.Metrics().Degradations.Load(); v < 1 {
		t.Fatalf("degradations = %d", v)
	}

	// And again: every request survives, none caches.
	resp = env.get(t, "/stream?src=lib/album/track.flac&format=wav", nil)
	if body := readBody(t, resp); !bytes.Equal(body, want) {
		t.Fatal("second degraded stream mismatched")
	}
	if st := cacheStats(t, env); st.Hits != 0 {
		t.Fatalf("degraded entries must not serve as cache hits, got %d", st.Hits)
	}
}

func TestFlightDedupUnderConcurrency(t *testing.T) {
	env := newTestEnv(t, nil)
	want := engineReference(t, filepath.Join(env.root, "ramp.wav"), waxflow.TranscodeOptions{Format: "wav", Rate: 24000})

	const clients = 4
	var wg sync.WaitGroup
	bodies := make([][]byte, clients)
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/stream?src=lib/ramp.wav&format=wav&rate=24000", nil)
			req.Header.Set("X-API-Key", testKey)
			resp, err := env.ts.Client().Do(req)
			if err != nil {
				t.Errorf("client %d: %v", i, err)
				return
			}
			defer resp.Body.Close()
			bodies[i], _ = io.ReadAll(resp.Body)
		}()
	}
	wg.Wait()

	for i, b := range bodies {
		if !bytes.Equal(b, want) {
			t.Fatalf("client %d bytes differ (%d vs %d)", i, len(b), len(want))
		}
	}
	if sessions := env.srv.Metrics().SessionsLive.Load(); sessions != 1 {
		t.Fatalf("sessions_total = %d, want 1 (flight dedup)", sessions)
	}
}

func TestCORS(t *testing.T) {
	env := newTestEnv(t, func(c *server.Config) {
		c.AllowedOrigins = []string{"https://deck.example"}
	})

	resp := env.get(t, "/stream?src=lib/sine.wav", map[string]string{"Origin": "https://deck.example"})
	readBody(t, resp)
	if resp.Header.Get("Access-Control-Allow-Origin") != "https://deck.example" {
		t.Fatalf("allowed origin got %q", resp.Header.Get("Access-Control-Allow-Origin"))
	}
	resp = env.get(t, "/stream?src=lib/sine.wav", map[string]string{"Origin": "https://evil.example"})
	readBody(t, resp)
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("disallowed origin must get no CORS header")
	}

	// Preflight.
	resp = env.req(t, http.MethodOptions, "/stream", map[string]string{"Origin": "https://deck.example", "X-API-Key": ""})
	readBody(t, resp)
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Methods") == "" {
		t.Fatalf("preflight = %d, methods %q", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Methods"))
	}
}

func TestSyncTranscode(t *testing.T) {
	env := newTestEnv(t, nil)
	want := engineReference(t, filepath.Join(env.root, "ramp.wav"), waxflow.TranscodeOptions{Format: "wav", Rate: 24000, Channels: 1})

	resp := env.req(t, http.MethodPost, "/transcode?src=lib/ramp.wav&format=wav&rate=24000&ch=1", nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 || !bytes.Equal(body, want) {
		t.Fatalf("sync transcode = %d, %d bytes (want %d)", resp.StatusCode, len(body), len(want))
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="ramp.wav"`) {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if v := env.srv.Metrics().SessionsSync.Load(); v != 1 {
		t.Fatalf("sync sessions = %d", v)
	}
	// Sync one-shots never cache.
	if st := cacheStats(t, env); st.Entries != 0 {
		t.Fatalf("sync transcode cached %d entries", st.Entries)
	}
}

func TestCacheEndpointsAndEviction(t *testing.T) {
	env := newTestEnv(t, func(c *server.Config) { c.CacheMaxBytes = 1 }) // everything evicts

	resp := env.get(t, "/stream?src=lib/sine.wav&format=wav&rate=24000", nil)
	readBody(t, resp)

	if st := cacheStats(t, env); st.SchemaVersion != 1 {
		t.Fatalf("stats schemaVersion = %d", st.SchemaVersion)
	}

	resp = env.req(t, http.MethodPost, "/cache/gc", nil)
	body := readBody(t, resp)
	var gc struct {
		SchemaVersion int   `json:"schemaVersion"`
		Removed       int   `json:"removed"`
		FreedBytes    int64 `json:"freedBytes"`
	}
	if err := json.Unmarshal(body, &gc); err != nil || gc.SchemaVersion != 1 {
		t.Fatalf("gc body = %s (%v)", body, err)
	}
	if st := cacheStats(t, env); st.Bytes > 1 {
		t.Fatalf("post-gc bytes = %d, want <= 1", st.Bytes)
	}
}

func TestParameterValidation(t *testing.T) {
	env := newTestEnv(t, nil)
	cases := []struct {
		query  string
		status int
		code   waxerr.Code
	}{
		{"src=lib/sine.wav&chanels=1", 400, waxerr.CodeInvalidRequest},                   // unknown param
		{"src=lib/sine.wav&bits=8", 400, waxerr.CodeInvalidRequest},                      // outside 16|24
		{"src=lib/sine.wav&t=-3", 400, waxerr.CodeInvalidRequest},                        // negative seek
		{"src=lib/sine.wav&t=1e18", 400, waxerr.CodeInvalidRequest},                      // beyond the seek bound
		{"src=lib/sine.wav&track=-5", 400, waxerr.CodeInvalidRequest},                    // explicit negative track
		{"src=lib/sine.wav&gain=loud", 400, waxerr.CodeInvalidRequest},                   // bad gain
		{"src=lib/sine.wav&bitrate=128", 415, waxerr.CodeUnsupportedFormat},              // bitrate needs an explicit lossy format
		{"src=lib/sine.wav&format=flac&bitrate=128", 415, waxerr.CodeUnsupportedFormat},  // bitrate on lossless output
		{"src=lib/sine.wav&q=huge", 400, waxerr.CodeInvalidRequest},                      // bad q preset
		{"src=lib/sine.wav&format=mp3&q=low&bitrate=96", 400, waxerr.CodeInvalidRequest}, // q and bitrate together
		{"src=lib/sine.wav&format=aiff", 415, waxerr.CodeUnsupportedFormat},              // no streaming form
		{"src=lib/sine.wav&maxBitRate=64", 415, waxerr.CodeUnsupportedFormat},            // cap unsatisfiable
		// A cap on VBR lossless output cannot be promised, so it is
		// refused rather than silently unenforced.
		{"src=lib/sine.wav&format=flac&maxBitRate=6400", 415, waxerr.CodeUnsupportedFormat},
		{"src=upload:abc", 501, waxerr.CodeUnsupportedSource},
		{"src=pid:01ABC", 501, waxerr.CodeUnsupportedSource},
		{"src=lib/missing.wav", 404, waxerr.CodeNotFound},
		{"src=nope/x.wav", 404, waxerr.CodeNotFound},
		{"", 400, waxerr.CodeInvalidRequest}, // src required
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			resp := env.get(t, "/stream?"+tc.query, nil)
			wantEnvelope(t, resp, tc.status, tc.code)
		})
	}
}

// vanishingResolver serves a bounded number of Resolves, then reports
// the source gone: the deterministic stand-in for a file deleted between
// the handler's resolve and the pipeline's.
type vanishingResolver struct {
	inner   source.Resolver
	mu      sync.Mutex
	allowed int
}

func (v *vanishingResolver) Resolve(ref string) (*source.File, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.allowed <= 0 {
		return nil, waxerr.New(waxerr.CodeNotFound, "source: vanished between resolve and pipeline")
	}
	v.allowed--
	return v.inner.Resolve(ref)
}

func TestPipelineFastFailPropagatesRealError(t *testing.T) {
	var vr *vanishingResolver
	env := newTestEnv(t, func(c *server.Config) {
		vr = &vanishingResolver{inner: c.Resolver, allowed: 1}
		c.Resolver = vr
	})

	// The handler's resolve succeeds (the one allowed call); the
	// pipeline's re-resolve fails. The client must see the pipeline's
	// real error, not a generic retry failure, and no doomed pipeline
	// respawn loop. FLAC to WAV forces the transcode path (a wav source
	// would direct-play without a pipeline).
	resp := env.get(t, "/stream?src=lib/album/track.flac&format=wav", nil)
	wantEnvelope(t, resp, http.StatusNotFound, waxerr.CodeNotFound)
	if sessions := env.srv.Metrics().SessionsLive.Load(); sessions != 1 {
		t.Fatalf("sessions = %d, want exactly 1 (no doomed respawns)", sessions)
	}
}

func TestTranscodeEnforcesMaxBitRate(t *testing.T) {
	env := newTestEnv(t, nil)
	// The identical /stream request refuses with 415; /transcode must
	// enforce the same cap through the shared prepare path.
	resp := env.req(t, http.MethodPost, "/transcode?src=lib/ramp.wav&format=wav&maxBitRate=64", nil)
	wantEnvelope(t, resp, http.StatusUnsupportedMediaType, waxerr.CodeUnsupportedFormat)
}

func TestSignTTLBounds(t *testing.T) {
	env := newTestEnv(t, nil)
	cl, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	// Past the cap, the duration arithmetic would overflow into the past
	// and mint an already-expired URL with 200 OK.
	for _, ttl := range []int64{-5, 99_999_999_999_999} {
		_, err := cl.Sign(t.Context(), client.SignRequest{
			Params:     map[string]string{"src": "lib/sine.wav"},
			TTLSeconds: ttl,
		})
		if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
			t.Fatalf("ttlSeconds %d: %v, want invalid-request", ttl, err)
		}
	}
}

func TestKeyedIdentityPinningStillApplies(t *testing.T) {
	env := newTestEnv(t, nil)
	// Auth forgives stale signatures on keyed requests; identity pinning
	// is orthogonal and must not be forgiven: a voluntary id that no
	// longer matches means the bytes changed.
	resp := env.get(t, "/stream?src=lib/sine.wav&id=1-1", nil)
	wantEnvelope(t, resp, http.StatusGone, waxerr.CodeSourceChanged)
}

func TestOversizedBodyIs413(t *testing.T) {
	env := newTestEnv(t, nil)
	// 1 MiB + change of JSON: over the decodeJSONBody cap.
	body := `{"src":"` + strings.Repeat("a", 1<<20) + `"}`
	req, err := http.NewRequest(http.MethodPost, env.ts.URL+"/probe", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-Key", testKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	wantEnvelope(t, resp, http.StatusRequestEntityTooLarge, waxerr.CodePayloadTooLarge)
}

func TestMetricsExposition(t *testing.T) {
	env := newTestEnv(t, nil)
	readBody(t, env.get(t, "/stream?src=lib/sine.wav", nil)) // one direct play

	resp := env.get(t, "/metrics", nil)
	body := string(readBody(t, resp))
	for _, want := range []string{
		`waxflow_build_info{version="test"} 1`,
		"waxflow_direct_play_total 1",
		"waxflow_sessions_active 0",
		"waxflow_ttfb_seconds_count",
		"waxflow_cache_bytes",
		`waxflow_admission_in_use{pool="live"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func cacheStats(t *testing.T, env *testEnv) (out struct {
	SchemaVersion int    `json:"schemaVersion"`
	Entries       int    `json:"entries"`
	Bytes         int64  `json:"bytes"`
	Hits          uint64 `json:"hits"`
	Misses        uint64 `json:"misses"`
}) {
	t.Helper()
	resp := env.get(t, "/cache/stats", nil)
	body := readBody(t, resp)
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("stats body %s: %v", body, err)
	}
	return out
}

func hexEncode(s string) string {
	return fmt.Sprintf("%x", s)
}
