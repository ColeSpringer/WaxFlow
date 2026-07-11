package oracletest

// The test server environment, a copy of the server package's e2e
// helper (server/e2e_test.go): it uses only the public server API, and
// this module deliberately duplicates it rather than exporting a
// test-support surface from the main module (the resolver module's
// fixture helpers set the precedent).

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

var updateGoldens = flag.Bool("update", false, "rewrite golden files (make goldens)")

const (
	testKey    = "test-api-key"
	testSecret = "0123456789abcdef0123456789abcdef"
)

// rampWAV renders a 16-bit ramp signal as a WAV file.
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
