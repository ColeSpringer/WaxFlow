package resolver

// Integration against a real WaxBin catalog: waxbin (read-write) authors
// the fixture over synthesized WAVs, our Catalog opens the same database
// read-only, and the tests drive the exact flows the flavor exists for:
// pid resolution, rename invalidation within one poll, and pid: streams
// end to end through the HTTP server.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxbin"
	binconfig "github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/waxerr"
)

// rampWAV renders a 16-bit mono ramp as a WAV file; frames vary per
// fixture so every file has a distinct audio essence (WaxBin's move
// detection matches files by essence hash).
func rampWAV(t *testing.T, frames int) []byte {
	t.Helper()
	f := audio.Format{Rate: 44100, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Int, BitDepth: 16}
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

// buildCatalog writes the named files under a fresh root, opens WaxBin
// read-write beside it, and scans. The returned library handle stays
// open (the writer WaxFlow coexists with in production).
func buildCatalog(t *testing.T, files map[string][]byte) (lib *waxbin.Library, root, db string) {
	t.Helper()
	ctx := context.Background()
	root = t.TempDir()
	db = filepath.Join(t.TempDir(), "catalog.db")
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots:  []binconfig.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("waxbin open: %v", err)
	}
	t.Cleanup(func() { lib.Close() })
	res, err := lib.Scan(ctx, waxbin.ScanRequest{})
	if err != nil {
		t.Fatalf("waxbin scan: %v", err)
	}
	if int(res.Total.AudioFiles) != len(files) || int(res.Total.ItemsCreated) != len(files) {
		t.Fatalf("scan tally = %+v, want %d audio / %d created", res.Total, len(files), len(files))
	}
	return lib, root, db
}

// itemPIDs maps each cataloged file's base name onto its item PID.
func itemPIDs(t *testing.T, lib *waxbin.Library) map[string]model.PID {
	t.Helper()
	items, err := lib.Query(context.Background(), query.New(query.EntityItems).Build())
	if err != nil {
		t.Fatalf("query items: %v", err)
	}
	out := make(map[string]model.PID, len(items))
	for _, iv := range items {
		out[filepath.Base(string(iv.Path))] = iv.PID
	}
	return out
}

func TestResolveAgainstRealCatalog(t *testing.T) {
	alpha := rampWAV(t, 8000)
	lib, _, db := buildCatalog(t, map[string][]byte{
		"alpha.wav": alpha,
		"beta.wav":  rampWAV(t, 12000),
	})
	pids := itemPIDs(t, lib)

	cat, err := Open(context.Background(), Options{DBPath: db, PollInterval: -1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cat.Close()

	f, err := cat.Resolve("pid:" + string(pids["alpha.wav"]))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer f.Close()
	if f.Ext != "wav" {
		t.Fatalf("ext = %q, want wav", f.Ext)
	}
	got := make([]byte, f.Size())
	if _, err := f.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !bytes.Equal(got, alpha) {
		t.Fatal("resolved file does not serve the cataloged bytes")
	}

	if _, err := cat.Resolve("pid:" + string(model.NewPID())); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Fatalf("unknown pid = %v, want not-found", err)
	}
}

func TestRenameInvalidatesWithinOnePoll(t *testing.T) {
	ctx := context.Background()
	alpha := rampWAV(t, 8000)
	lib, root, db := buildCatalog(t, map[string][]byte{
		"alpha.wav": alpha,
		"beta.wav":  rampWAV(t, 12000),
	})
	pid := itemPIDs(t, lib)["alpha.wav"]

	cat, err := Open(ctx, Options{DBPath: db, PollInterval: -1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cat.Close()

	f, err := cat.Resolve("pid:" + string(pid))
	if err != nil {
		t.Fatalf("warm Resolve: %v", err)
	}
	f.Close()
	oldPath, ok := cat.cached(pid)
	if !ok {
		t.Fatal("resolve did not cache the path")
	}

	// The user reorganizes the library: the file moves on disk and a
	// rescan relinks it in the catalog (same essence, new path, same
	// file PID), emitting file-update change rows.
	newPath := filepath.Join(root, "omega.wav")
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	iv, err := lib.Get(ctx, pid)
	if err != nil {
		t.Fatalf("catalog lost the item across the rename: %v", err)
	}
	if string(iv.Path) != newPath {
		t.Fatalf("catalog path = %q, want %q (relink)", iv.Path, newPath)
	}

	// One poll must drop the stale cached path.
	if err := cat.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if stale, ok := cat.cached(pid); ok {
		t.Fatalf("cached path %q survived the poll after a rename", stale)
	}

	// And the next resolve serves the new location, byte-identical.
	f, err = cat.Resolve("pid:" + string(pid))
	if err != nil {
		t.Fatalf("Resolve after rename: %v", err)
	}
	defer f.Close()
	got := make([]byte, f.Size())
	if _, err := f.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !bytes.Equal(got, alpha) {
		t.Fatal("post-rename resolve serves wrong bytes")
	}
	if path, _ := cat.cached(pid); path != newPath {
		t.Fatalf("cache after rename = %q, want %q", path, newPath)
	}
}

func TestBackgroundPollInvalidates(t *testing.T) {
	ctx := context.Background()
	lib, root, db := buildCatalog(t, map[string][]byte{
		"alpha.wav": rampWAV(t, 8000),
		"beta.wav":  rampWAV(t, 12000),
	})
	pid := itemPIDs(t, lib)["alpha.wav"]

	cat, err := Open(ctx, Options{DBPath: db, PollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cat.Close()

	f, err := cat.Resolve("pid:" + string(pid))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	oldPath, _ := cat.cached(pid)
	if err := os.Rename(oldPath, filepath.Join(root, "omega.wav")); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := cat.cached(pid); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("background poll never dropped the renamed path")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPIDStreamsE2E(t *testing.T) {
	ctx := context.Background()
	lib, root, db := buildCatalog(t, map[string][]byte{
		"alpha.wav": rampWAV(t, 8000),
		"beta.wav":  rampWAV(t, 12000),
	})
	pid := itemPIDs(t, lib)["alpha.wav"]

	cat, err := Open(ctx, Options{DBPath: db, PollInterval: -1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cat.Close()

	srv, err := server.New(server.Config{
		APIKeys:    []string{"test-key"},
		Resolver:   cat,
		PIDSources: true,
		CacheDir:   t.TempDir(),
		Version:    "test",
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	defer srv.Close()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// get reads the whole body and closes it before returning, so no
	// connection lingers to the end of the test.
	get := func(t *testing.T, path string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-API-Key", "test-key")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		return resp, body
	}

	// The flavor advertises pid sources.
	_, body := get(t, "/caps")
	var caps struct {
		Delivery struct {
			PID bool `json:"pid"`
		} `json:"delivery"`
	}
	if err := json.Unmarshal(body, &caps); err != nil || !caps.Delivery.PID {
		t.Fatalf("caps delivery.pid = %v (err %v), want true", caps.Delivery.PID, err)
	}

	// pid: probes and streams.
	if resp, _ := get(t, "/probe?src=pid:"+string(pid)); resp.StatusCode != http.StatusOK {
		t.Fatalf("probe = %d", resp.StatusCode)
	}
	resp, body := get(t, "/stream?src=pid:"+string(pid)+"&format=wav")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "audio/wav" {
		t.Fatalf("stream = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if len(body) < 44 {
		t.Fatalf("stream body = %d bytes", len(body))
	}

	// id= pins bytes for pid refs exactly like path refs: stale is 410.
	resp, body = get(t, "/stream?src=pid:"+string(pid)+"&format=wav&id=1-1")
	var envlp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &envlp); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusGone || envlp.Code != string(waxerr.CodeSourceChanged) {
		t.Fatalf("stale id = %d %q, want 410 source-changed", resp.StatusCode, envlp.Code)
	}

	// A rename changes no bytes, so a URL pinned to the true identity
	// keeps playing across it: the pid re-resolves to the new path and
	// identity (size+mtimeNS, preserved by rename) still matches.
	f, err := cat.Resolve("pid:" + string(pid))
	if err != nil {
		t.Fatal(err)
	}
	id := f.ID.String()
	f.Close()
	if err := os.Rename(filepath.Join(root, "alpha.wav"), filepath.Join(root, "omega.wav")); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := cat.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	resp, _ = get(t, "/stream?src=pid:"+string(pid)+"&format=wav&id="+id)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pinned stream after rename = %d, want 200 (renames must not kill URLs)", resp.StatusCode)
	}
}
