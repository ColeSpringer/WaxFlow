package server_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// capsRootsReload reads delivery.rootsReload off /caps.
func capsRootsReload(t *testing.T, env *testEnv) bool {
	t.Helper()
	body := readBody(t, env.get(t, "/caps", nil))
	var c struct {
		Delivery struct {
			RootsReload bool `json:"rootsReload"`
		} `json:"delivery"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("caps body %s: %v", body, err)
	}
	return c.Delivery.RootsReload
}

// TestRootsReloadEndpoint drives the endpoint over a test-controlled
// *source.Roots and a desired-set the test flips: a source under a
// not-yet-known root 404s, a reload adds the root, and the same source then
// streams, with no restart. It also pins the auth gate, the invalid-config
// 400, and that /caps advertises the wired capability.
func TestRootsReloadEndpoint(t *testing.T) {
	// dirB holds a real source; the live roots do not know "B" yet.
	dirB := t.TempDir()
	flac, err := os.ReadFile("../testdata/sine-s16.flac")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "track.flac"), flac, 0o644); err != nil {
		t.Fatal(err)
	}

	roots, err := source.OpenRoots(nil, 0) // starts empty
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	desired := []source.Root(nil) // the config the closure reconciles to

	env := newTestEnv(t, func(c *server.Config) {
		c.Resolver = roots
		c.ReloadRoots = func() (source.ReloadResult, error) {
			mu.Lock()
			d := desired
			mu.Unlock()
			return roots.Reload(d, 0)
		}
	})
	t.Cleanup(func() { roots.Close() })

	// Before the reload the root is unknown: 404.
	resp := env.get(t, "/stream?src=B/track.flac", nil)
	wantEnvelope(t, resp, http.StatusNotFound, waxerr.CodeNotFound)

	// The capability is advertised because a closure is wired.
	if !capsRootsReload(t, env) {
		t.Fatal("caps delivery.rootsReload = false, want true when ReloadRoots is wired")
	}

	// A reload with no key is refused by requireKey.
	resp = env.req(t, http.MethodPost, "/roots/reload", map[string]string{"X-API-Key": ""})
	wantEnvelope(t, resp, http.StatusUnauthorized, waxerr.CodeUnauthorized)

	// Flip the desired set to add B, then reload.
	mu.Lock()
	desired = []source.Root{{Name: "B", Path: dirB}}
	mu.Unlock()
	resp = env.req(t, http.MethodPost, "/roots/reload", nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reload = %d, want 200 (body: %s)", resp.StatusCode, body)
	}
	var delta server.RootsReloadResponse
	if err := json.Unmarshal(body, &delta); err != nil {
		t.Fatalf("reload body %s: %v", body, err)
	}
	if delta.SchemaVersion != 1 ||
		len(delta.Added) != 1 || delta.Added[0] != "B" ||
		len(delta.Removed) != 0 || len(delta.Changed) != 0 ||
		len(delta.Roots) != 1 || delta.Roots[0] != "B" {
		t.Fatalf("reload delta = %+v, want only B added", delta)
	}

	// The same source now streams.
	resp = env.get(t, "/stream?src=B/track.flac", nil)
	if b := readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("post-reload stream = %d, want 200 (body: %s)", resp.StatusCode, b)
	}

	// A reload of an invalid desired set surfaces synchronously as 400.
	mu.Lock()
	desired = []source.Root{{Name: "B", Path: dirB}, {Name: "B", Path: dirB}} // duplicate name
	mu.Unlock()
	resp = env.req(t, http.MethodPost, "/roots/reload", nil)
	wantEnvelope(t, resp, http.StatusBadRequest, waxerr.CodeInvalidRequest)
	// The failed reload left B streaming (nothing swapped).
	resp = env.get(t, "/stream?src=B/track.flac", nil)
	if b := readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("stream after a rejected reload = %d, want 200 (body: %s)", resp.StatusCode, b)
	}
}

// TestRootsReloadDisabled pins the gated-capability contract: with no
// ReloadRoots wired the route is absent (404) and /caps reports the
// capability off, so a client detects support at probe time.
func TestRootsReloadDisabled(t *testing.T) {
	env := newTestEnv(t, nil)

	resp := env.req(t, http.MethodPost, "/roots/reload", nil)
	wantEnvelope(t, resp, http.StatusNotFound, waxerr.CodeNotFound)

	if capsRootsReload(t, env) {
		t.Fatal("caps delivery.rootsReload = true, want false when ReloadRoots is nil")
	}
}
