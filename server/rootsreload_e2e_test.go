package server_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/colespringer/waxflow/client"
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

// TestRootsReloadClient covers what only the client adds over the raw
// endpoint, which TestRootsReloadEndpoint already pins: that the response
// decodes into the typed fields at all, that a no-op's empty deltas
// survive as non-nil slices a caller can range over, and that a rejected
// reload arrives as a waxerr code rather than a status. The capability
// flag is not re-asserted here; it belongs to the endpoint tests.
func TestRootsReloadClient(t *testing.T) {
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
	desired := []source.Root(nil)

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

	cl, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}

	// A real change: B appears in Added and in the full set.
	mu.Lock()
	desired = []source.Root{{Name: "B", Path: dirB}}
	mu.Unlock()
	delta, err := cl.ReloadRoots(t.Context())
	if err != nil {
		t.Fatalf("ReloadRoots: %v", err)
	}
	if delta.SchemaVersion != 1 ||
		len(delta.Added) != 1 || delta.Added[0] != "B" ||
		len(delta.Removed) != 0 || len(delta.Changed) != 0 ||
		len(delta.Roots) != 1 || delta.Roots[0] != "B" {
		t.Fatalf("ReloadRoots delta = %+v, want only B added", delta)
	}

	// The no-op reload: every list decodes non-nil, so a caller can range
	// over all four without a nil check. That is the contract the response
	// type documents and deliberately does not normalize for.
	delta, err = cl.ReloadRoots(t.Context())
	if err != nil {
		t.Fatalf("ReloadRoots (no-op): %v", err)
	}
	for _, l := range []struct {
		name string
		v    []string
	}{
		{"added", delta.Added}, {"removed", delta.Removed}, {"changed", delta.Changed},
	} {
		if l.v == nil {
			t.Errorf("no-op reload %s = nil, want an empty array", l.name)
		}
		if len(l.v) != 0 {
			t.Errorf("no-op reload %s = %v, want empty", l.name, l.v)
		}
	}
	if len(delta.Roots) != 1 || delta.Roots[0] != "B" {
		t.Errorf("no-op reload roots = %v, want [B]", delta.Roots)
	}

	// A rejected reload arrives as the daemon's typed code, not a bare
	// status: this is the mapping a hand-rolled postJSON skips.
	mu.Lock()
	desired = []source.Root{{Name: "B", Path: dirB}, {Name: "B", Path: dirB}} // duplicate name
	mu.Unlock()
	if _, err := cl.ReloadRoots(t.Context()); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Fatalf("rejected reload = %v (code %q), want invalid-request", err, waxerr.CodeOf(err))
	}
}

// TestRootsReloadClientUnwired pins what ReloadRoots promises against a
// daemon without the route: the unregistered path falls through to the
// catch-all, which writes a real envelope, so the caller sees NotFound
// rather than the Internal a bodiless 404 would produce. A proxy that
// lost the route would produce that bodiless 404 and so a different code
// for the same situation, which is why the method's comment sends callers
// to Caps.Delivery.RootsReload instead of to this status.
// TestRootsReloadDisabled owns the capability half.
func TestRootsReloadClientUnwired(t *testing.T) {
	env := newTestEnv(t, nil)
	cl, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.ReloadRoots(t.Context()); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Fatalf("unwired ReloadRoots = %v (code %q), want not-found", err, waxerr.CodeOf(err))
	}
}
