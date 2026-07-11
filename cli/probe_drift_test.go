package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/source"
)

// TestProbeJSONMatchesHTTP pins the no-drift contract between `waxflow
// probe --json` and GET /probe: both build server.ProbeInfo through
// their own probeMetadata adapters, and this test keeps the two
// byte-identical over a metadata-rich source (tags, chapters, art
// signaling all exercised by the m4b fixture).
func TestProbeJSONMatchesHTTP(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "testdata", "chapters.m4b"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	path := filepath.Join(root, "chapters.m4b")
	if err := os.WriteFile(path, fixture, 0o644); err != nil {
		t.Fatal(err)
	}

	roots, err := source.OpenRoots([]source.Root{{Name: "lib", Path: root}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	srv, err := server.New(server.Config{
		CacheDir: t.TempDir(),
		Version:  "test",
		Resolver: roots,
		Meta:     label.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	hs := httptest.NewServer(srv)
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/probe?src=lib%2Fchapters.m4b")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	httpBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /probe = %d: %s", resp.StatusCode, httpBody)
	}

	code, cliOut, stderr := run(t, "probe", "--json", path)
	if code != 0 {
		t.Fatalf("probe --json exit = %d; stderr: %s", code, stderr)
	}

	if got, want := strings.TrimSpace(cliOut), strings.TrimSpace(string(httpBody)); got != want {
		t.Errorf("CLI probe --json drifted from GET /probe:\n cli: %s\nhttp: %s", got, want)
	}
	// The fixture must actually exercise the metadata surface, or this
	// test pins nothing (it carries tags and chapters; no art).
	for _, want := range []string{`"tags"`, `"chapters"`} {
		if !strings.Contains(cliOut, want) {
			t.Errorf("probe output missing %s; fixture no longer metadata-rich?\n%s", want, cliOut)
		}
	}
}
