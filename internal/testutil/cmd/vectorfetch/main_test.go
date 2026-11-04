package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/internal/testutil"
)

// TestPruneClearsAnOlderLayout pins that -prune runs before the fetch. CI
// restores its vector cache from an older pin list, and a file where a pin
// now needs a directory, or a directory where it needs a file, fails the
// fetch. A failed fetch saves nothing, so every later run would restore the
// same set and fail the same way until someone deleted the cache by hand.
func TestPruneClearsAnOlderLayout(t *testing.T) {
	payload := []byte("pinned bytes")
	sum := sha256.Sum256(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	for _, c := range []struct{ old, pinned string }{
		{"x/y", "x/y/z"},
		{"x/y/z", "x/y"},
	} {
		t.Run(strings.ReplaceAll(c.old+" then "+c.pinned, "/", "-"), func(t *testing.T) {
			dir := t.TempDir()
			old := filepath.Join(dir, filepath.FromSlash(c.old))
			if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(old, []byte("an older pin"), 0o644); err != nil {
				t.Fatal(err)
			}
			pins := []testutil.Vector{{Name: c.pinned, URL: srv.URL + "/v", SHA256: hex.EncodeToString(sum[:])}}

			var log strings.Builder
			if err := fetch(&log, dir, pins, pins, true); err != nil {
				t.Fatalf("fetch over the older layout: %v\n%s", err, log.String())
			}
			got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(c.pinned)))
			if err != nil || string(got) != string(payload) {
				t.Errorf("%s holds %q, %v; want the pinned bytes", c.pinned, got, err)
			}
		})
	}
}
