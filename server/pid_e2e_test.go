package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxflow/client"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

const pidTrack = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

// pidHost is a fake lookup host answering GET /pid/{id} from refs.
type pidHost struct {
	*httptest.Server
	refs sync.Map // id -> root-relative reference
}

func newPIDHost(t *testing.T) *pidHost {
	t.Helper()
	h := &pidHost{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ref, ok := h.refs.Load(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ref":%q}`, ref)
	}))
	t.Cleanup(h.Close)
	return h
}

// pidEnv is a test daemon whose resolver asks host for pid: references, the
// way the CLI wires the stock chain when pidLookupURL is configured.
func pidEnv(t *testing.T, host *pidHost) *testEnv {
	t.Helper()
	return newTestEnv(t, func(cfg *server.Config) {
		lookup, err := source.NewPIDLookup(host.URL+"/pid", cfg.Resolver, source.PIDLookupOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cfg.Resolver = lookup
		cfg.PIDSources = true
	})
}

// TestPIDStreamsE2E plays a pid through every surface: /caps advertises it,
// /probe and a signed /stream take it, a rename the host tracks keeps the
// signed URL playing from the same cache entry, different bytes behind the
// pid die with 410, and HLS resolves it on every request.
func TestPIDStreamsE2E(t *testing.T) {
	host := newPIDHost(t)
	host.refs.Store(pidTrack, "lib/album/track.flac")
	env := pidEnv(t, host)

	var caps server.Caps
	resp := env.get(t, "/caps", nil)
	if err := json.Unmarshal(readBody(t, resp), &caps); err != nil || !caps.Delivery.PID {
		t.Fatalf("caps delivery.pid = %v (%v), want true", caps.Delivery.PID, err)
	}
	resp = env.get(t, "/probe?src=pid:"+pidTrack, nil)
	if body := readBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("probe = %d: %s", resp.StatusCode, body)
	}

	cl, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	minted, err := cl.Sign(t.Context(), client.SignRequest{Params: map[string]string{
		"src": "pid:" + pidTrack, "format": "wav",
	}})
	if err != nil {
		t.Fatal(err)
	}
	play := func() {
		t.Helper()
		resp := env.req(t, http.MethodGet, minted.URL, map[string]string{"X-API-Key": ""})
		if body := readBody(t, resp); resp.StatusCode != 200 {
			t.Fatalf("signed URL = %d: %s", resp.StatusCode, body)
		}
	}
	play()
	waitForIdle(t, env)
	before := cacheStats(t, env)

	// The host tracks the rename: the identity is unchanged and the cache
	// key is the pid, so the same entry serves.
	if err := os.Rename(filepath.Join(env.root, "album", "track.flac"), filepath.Join(env.root, "album", "moved.flac")); err != nil {
		t.Fatal(err)
	}
	host.refs.Store(pidTrack, "lib/album/moved.flac")
	play()
	after := cacheStats(t, env)
	if after.Entries != before.Entries || after.Hits != before.Hits+1 {
		t.Fatalf("cache across the rename: entries %d -> %d, hits %d -> %d; want the same entry hit once more",
			before.Entries, after.Entries, before.Hits, after.Hits)
	}

	// Different bytes behind the pid: the bytes the URL pinned are gone.
	host.refs.Store(pidTrack, "lib/sine.wav")
	resp = env.req(t, http.MethodGet, minted.URL, map[string]string{"X-API-Key": ""})
	wantEnvelope(t, resp, http.StatusGone, waxerr.CodeSourceChanged)

	host.refs.Store(pidTrack, "lib/album/moved.flac")
	masterURL := mintHLS(t, env, map[string]string{"src": "pid:" + pidTrack, "format": "aac"})
	resp = keyless(t, env, masterURL)
	master := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("master = %d: %s", resp.StatusCode, master)
	}
	mediaRefs := variantURIs(t, master)
	if len(mediaRefs) == 0 {
		t.Fatalf("master lists no variant:\n%s", master)
	}
	resp = keyless(t, env, "/hls/"+mediaRefs[0])
	media := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("media = %d: %s", resp.StatusCode, media)
	}
	initURI, segURIs, _ := mediaURIs(t, media)
	if initURI == "" || len(segURIs) == 0 {
		t.Fatalf("init %q, %d segments:\n%s", initURI, len(segURIs), media)
	}
	for _, uri := range []string{initURI, segURIs[0]} {
		resp = keyless(t, env, "/hls/"+uri)
		if body := readBody(t, resp); resp.StatusCode != 200 {
			t.Fatalf("%s = %d: %s", uri, resp.StatusCode, body)
		}
	}
}

// TestPIDHostDownIsCatalogUnavailable: a host that does not answer makes
// every pid request a 503 clients retry, like a catalog that is down.
func TestPIDHostDownIsCatalogUnavailable(t *testing.T) {
	host := newPIDHost(t)
	host.refs.Store(pidTrack, "lib/album/track.flac")
	env := pidEnv(t, host)
	host.Close()
	for _, path := range []string{"/probe?src=pid:" + pidTrack, "/stream?src=pid:" + pidTrack + "&format=wav"} {
		resp := env.get(t, path, nil)
		wantEnvelope(t, resp, http.StatusServiceUnavailable, waxerr.CodeCatalogUnavailable)
		if got := resp.Header.Get("Retry-After"); got != "2" {
			t.Fatalf("%s: Retry-After = %q, want 2", path, got)
		}
	}
}

// TestPIDHostBrokenIsInternal: a host that answers with something other
// than a root-relative reference is broken, not down, and says so.
func TestPIDHostBrokenIsInternal(t *testing.T) {
	host := newPIDHost(t)
	host.refs.Store(pidTrack, "pid:loop")
	env := pidEnv(t, host)
	resp := env.get(t, "/probe?src=pid:"+pidTrack, nil)
	wantEnvelope(t, resp, http.StatusInternalServerError, waxerr.CodeInternal)
}

// TestTimelineMembersResolveTogether: a timeline's members are each a round
// trip to the host, so a request resolves them together rather than one after
// another.
func TestTimelineMembersResolveTogether(t *testing.T) {
	var inflight, peak atomic.Int64
	host := newPIDHost(t)
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		host.Server.Config.Handler.ServeHTTP(w, r)
	})
	slowHost := httptest.NewServer(slow)
	t.Cleanup(slowHost.Close)
	const members = 6
	refs := make([]string, members)
	for i := range refs {
		id := fmt.Sprintf("%026d", i)
		host.refs.Store(id, "lib/album/track.flac")
		refs[i] = "pid:" + id
	}
	dir := filepath.Join(t.TempDir(), "timelines")
	env := newTestEnv(t, func(cfg *server.Config) {
		lookup, err := source.NewPIDLookup(slowHost.URL+"/pid", cfg.Resolver, source.PIDLookupOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cfg.Resolver = lookup
		cfg.PIDSources = true
		cfg.TimelineDir = dir
	})
	tl := mintTimeline(t, env, refs)
	peak.Store(0)
	resp := env.get(t, "/stream?tl="+tl.Tl+"&format=wav&gain=off", nil)
	if body := readBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("timeline stream = %d: %s", resp.StatusCode, body)
	}
	if p := peak.Load(); p < 2 {
		t.Fatalf("members resolved one at a time (peak %d lookups in flight for %d members)", p, members)
	}
}
