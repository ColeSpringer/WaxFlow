package server_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/client"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// rootsEnv starts a server over the test library plus a root "later" whose
// folder does not exist yet, with GET /roots wired. It returns the path the
// folder belongs at.
func rootsEnv(t *testing.T) (*testEnv, string) {
	t.Helper()
	later := filepath.Join(t.TempDir(), "later")
	env := newTestEnv(t, func(c *server.Config) {
		roots := c.Resolver.(*source.Roots)
		lib := roots.Status()[0]
		if _, err := roots.Reload([]source.Root{{Name: lib.Name, Path: lib.Path}, {Name: "later", Path: later}}, 0); err != nil {
			t.Fatal(err)
		}
		c.RootsStatus = roots.Status
	})
	return env, later
}

// capsRootsStatus reads delivery.rootsStatus off /caps.
func capsRootsStatus(t *testing.T, env *testEnv) bool {
	t.Helper()
	body := readBody(t, env.get(t, "/caps", nil))
	var c struct {
		Delivery struct {
			RootsStatus bool `json:"rootsStatus"`
		} `json:"delivery"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("caps body %s: %v", body, err)
	}
	return c.Delivery.RootsStatus
}

// TestRootsStatusEndpoint pins GET /roots: every configured root in order,
// an unmounted one with its reason, behind the API key, and advertised.
func TestRootsStatusEndpoint(t *testing.T) {
	env, later := rootsEnv(t)

	resp := env.get(t, "/roots", nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /roots = %d (body: %s)", resp.StatusCode, body)
	}
	var got server.RootsResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("roots body %s: %v", body, err)
	}
	if got.SchemaVersion != 1 || len(got.Roots) != 2 {
		t.Fatalf("roots = %s, want schemaVersion 1 and two roots", body)
	}
	lib, down := got.Roots[0], got.Roots[1]
	if lib.Name != "lib" || lib.Path != env.root || !lib.Mounted || lib.Error != "" || lib.Since.IsZero() {
		t.Errorf("lib = %+v, want mounted at %s", lib, env.root)
	}
	if down.Name != "later" || down.Path != later || down.Mounted || !strings.Contains(down.Error, later) || down.Since.IsZero() {
		t.Errorf("later = %+v, want unmounted at %s with its open error", down, later)
	}
	if !strings.Contains(string(body), `"error":`) || strings.Count(string(body), `"error":`) != 1 {
		t.Errorf("only the unmounted root should carry an error field: %s", body)
	}

	resp = env.req(t, http.MethodGet, "/roots", map[string]string{"X-API-Key": ""})
	wantEnvelope(t, resp, http.StatusUnauthorized, waxerr.CodeUnauthorized)
	if !capsRootsStatus(t, env) {
		t.Fatal("caps delivery.rootsStatus = false, want true when RootsStatus is wired")
	}
}

// TestRootsStatusDisabled pins the gated-capability contract GET /roots shares
// with the reload: unwired, the route is absent, /caps says so, and the gauge
// is not emitted (it would claim every root mounted).
func TestRootsStatusDisabled(t *testing.T) {
	env := newTestEnv(t, nil)
	wantEnvelope(t, env.get(t, "/roots", nil), http.StatusNotFound, waxerr.CodeNotFound)
	if capsRootsStatus(t, env) {
		t.Fatal("caps delivery.rootsStatus = true, want false when RootsStatus is nil")
	}
	if body := string(readBody(t, env.get(t, "/metrics", nil))); strings.Contains(body, "waxflow_roots_unmounted") {
		t.Fatalf("metrics emit waxflow_roots_unmounted with no roots status wired:\n%s", body)
	}
}

// TestRootUnavailableIsRetryable pins the answer for a request on an unmounted
// root, a 503 with Retry-After, and that the folder appearing is enough: the
// next request mounts it and streams, with nothing else run in between.
func TestRootUnavailableIsRetryable(t *testing.T) {
	env, later := rootsEnv(t)

	for _, path := range []string{"/stream?src=later/track.flac", "/probe?src=later/track.flac"} {
		resp := env.get(t, path, nil)
		if got := resp.Header.Get("Retry-After"); got != "2" {
			t.Errorf("%s Retry-After = %q, want 2", path, got)
		}
		wantEnvelope(t, resp, http.StatusServiceUnavailable, waxerr.CodeRootUnavailable)
	}
	if body := string(readBody(t, env.get(t, "/metrics", nil))); !strings.Contains(body, "waxflow_roots_unmounted 1\n") {
		t.Fatalf("metrics missing waxflow_roots_unmounted 1:\n%s", body)
	}

	flac, err := os.ReadFile("../testdata/sine-s16.flac")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(later, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(later, "track.flac"), flac, 0o644); err != nil {
		t.Fatal(err)
	}
	resp := env.get(t, "/stream?src=later/track.flac", nil)
	if b := readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("stream after mkdir = %d, want 200 (body: %s)", resp.StatusCode, b)
	}
	if body := string(readBody(t, env.get(t, "/metrics", nil))); !strings.Contains(body, "waxflow_roots_unmounted 0\n") {
		t.Fatalf("metrics missing waxflow_roots_unmounted 0 after the mount:\n%s", body)
	}
}

// TestRootsClient covers what the client adds over the endpoint: the typed
// decode, and the unwired daemon's not-found.
func TestRootsClient(t *testing.T) {
	env, later := rootsEnv(t)
	cl, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	got, err := cl.Roots(t.Context())
	if err != nil {
		t.Fatalf("Roots: %v", err)
	}
	if len(got.Roots) != 2 || !got.Roots[0].Mounted || got.Roots[1].Mounted || got.Roots[1].Path != later || got.Roots[1].Error == "" {
		t.Fatalf("Roots = %+v, want lib mounted and later unmounted with an error", got)
	}
	caps, err := cl.Caps(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !caps.Delivery.RootsStatus {
		t.Error("client caps RootsStatus = false, want true")
	}

	unwired, err := client.New(newTestEnv(t, nil).ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unwired.Roots(t.Context()); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Fatalf("unwired Roots = %v (code %q), want not-found", err, waxerr.CodeOf(err))
	}
}

// TestClientRootsMirrorsCoverTheWire pins the client's roots types against
// the server's, populated and zero, as the job mirrors are pinned.
func TestClientRootsMirrorsCoverTheWire(t *testing.T) {
	var roots server.RootsResponse
	n := 0
	fillNonZero(t, reflect.ValueOf(&roots).Elem(), &n)
	roundTripEqual(t, "populated roots", roots, &client.RootsResponse{})
	roundTripEqual(t, "zero root", server.RootStatus{}, &client.RootStatus{})

	var reload server.RootsReloadResponse
	n = 0
	fillNonZero(t, reflect.ValueOf(&reload).Elem(), &n)
	roundTripEqual(t, "populated reload", reload, &client.RootsReloadResponse{})
}

// TestRootsStatusNamesAnyPath: a folder's path is bytes, as a file's name
// is, and the response is JSON, so the path travels in text form.
func TestRootsStatusNamesAnyPath(t *testing.T) {
	name, _, ok := testutil.NonUTF8Name()
	if !ok {
		t.Skip("this platform's file names are all valid UTF-8")
	}
	stray := filepath.Join(t.TempDir(), name)
	env := newTestEnv(t, func(c *server.Config) {
		roots := c.Resolver.(*source.Roots)
		lib := roots.Status()[0]
		if _, err := roots.Reload([]source.Root{{Name: lib.Name, Path: lib.Path}, {Name: "stray", Path: stray}}, 0); err != nil {
			t.Fatal(err)
		}
		c.RootsStatus = roots.Status
	})
	body := readBody(t, env.get(t, "/roots", nil))
	want, err := json.Marshal(source.Ref(stray).Text())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"path":`+string(want)) {
		t.Errorf("GET /roots does not carry the path as %s:\n%s", want, body)
	}
	// The error quotes the path, and is JSON too.
	if strings.Count(string(body), strings.Trim(string(want), `"`)) != 2 || strings.Contains(string(body), `\ufffd`) {
		t.Errorf("GET /roots does not name the path in its error as %s:\n%s", want, body)
	}
	cl, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	got, err := cl.Roots(t.Context())
	if err != nil || len(got.Roots) != 2 || got.Roots[1].Path != stray {
		t.Fatalf("the client read %#v (%v), want the stray root at %q", got, err, stray)
	}
}
