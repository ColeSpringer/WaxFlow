package testutil

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Vector is one SHA-256-pinned external conformance vector. Pinning is
// both reproducibility and supply-chain hygiene: a changed upstream file
// fails loudly instead of silently changing what the suite verifies.
type Vector struct {
	// Name is the path under testdata/vectors/ once fetched.
	Name string
	// URL is the upstream source.
	URL string
	// SHA256 is the hex digest the download must match.
	SHA256 string
}

// Vectors lists every pinned vector, fetched by `make verify-vectors`
// (CI-cached, never committed). The list grows as codec milestones land:
// the IETF FLAC suite at M2, MP3/LAME gapless fixtures at M6,
// opus_testvectors at M10. M1's fixtures are tiny and committed directly
// under testdata/, so the list starts empty.
var Vectors = []Vector{}

// VectorsDir returns the on-disk vector cache, testdata/vectors under the
// repository root (located relative to this source file).
func VectorsDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("testutil: cannot locate source file for vectors dir")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "vectors")
}

// VectorPath returns the local path of a fetched vector. Tests self-skip
// when it has not been fetched; WAXFLOW_REQUIRE_VECTORS=1 (CI jobs that
// ran `make verify-vectors` first) escalates absence to failure.
func VectorPath(t testing.TB, name string) string {
	t.Helper()
	path := filepath.Join(VectorsDir(), filepath.FromSlash(name))
	if _, err := os.Stat(path); err != nil {
		if os.Getenv("WAXFLOW_REQUIRE_VECTORS") == "1" {
			t.Fatalf("vector %s required by WAXFLOW_REQUIRE_VECTORS=1 but not fetched (run `make verify-vectors`)", name)
		}
		t.Skipf("vector %s not fetched (run `make verify-vectors`); skipping", name)
	}
	return path
}

// Fetch downloads vectors into dir, verifying each digest. Files already
// present with a matching digest are kept; mismatches are re-downloaded,
// and a mismatched download is an error. Progress goes to w.
func Fetch(w io.Writer, dir string, vectors []Vector) error {
	for _, v := range vectors {
		path := filepath.Join(dir, filepath.FromSlash(v.Name))
		if sum, err := fileSHA256(path); err == nil {
			if sum == v.SHA256 {
				fmt.Fprintf(w, "ok       %s (cached)\n", v.Name)
				continue
			}
			fmt.Fprintf(w, "refetch  %s (digest changed)\n", v.Name)
		}
		if err := fetchOne(path, v); err != nil {
			return fmt.Errorf("fetching %s: %w", v.Name, err)
		}
		fmt.Fprintf(w, "fetched  %s\n", v.Name)
	}
	fmt.Fprintf(w, "%d vector(s) verified in %s\n", len(vectors), dir)
	return nil
}

func fetchOne(path string, v Vector) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	resp, err := http.Get(v.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", v.URL, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fetch-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != v.SHA256 {
		return fmt.Errorf("digest mismatch: got %s, pinned %s", got, v.SHA256)
	}
	return os.Rename(tmp.Name(), path)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
