package cli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow/internal/config"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/source"
)

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut strings.Builder
	code = Execute("test", args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestVersion(t *testing.T) {
	code, out, _ := run(t, "version")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.HasPrefix(out, "waxflow test ") {
		t.Errorf("output = %q, want prefix %q", out, "waxflow test ")
	}
}

// fakePIDResolver stands in for the WaxBin catalog resolver: pid: refs
// resolve to one fixed local file, everything else delegates.
type fakePIDResolver struct {
	next source.Resolver
	path string
}

func (f fakePIDResolver) Resolve(ref string) (*source.File, error) {
	if strings.HasPrefix(ref, "pid:") {
		return source.OpenLocal(ref, f.path, f.path)
	}
	return f.next.Resolve(ref)
}

func TestFlavorVersionOutput(t *testing.T) {
	var out, errOut strings.Builder
	code := ExecuteFlavor("test", []string{"version"}, &out, &errOut, Flavor{Name: "waxbin"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "waxflow-waxbin test ") {
		t.Errorf("output = %q, want prefix %q", out.String(), "waxflow-waxbin test ")
	}
}

func TestStockBuildRefusesCatalogDB(t *testing.T) {
	// A configured catalogDB on the stock build must fail loudly: the
	// operator asked for pid: sources and this binary cannot serve them.
	t.Setenv("WAXFLOW_CATALOG_DB", "/nonexistent/waxbin.db")
	code, _, stderr := run(t, "sign", "--src", "pid:01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (invalid-request); stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "waxbin flavor") {
		t.Errorf("stderr = %q, want a pointer at the waxbin flavor", stderr)
	}
}

func TestFlavorSignsPIDSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "track.wav")
	if err := os.WriteFile(path, []byte("not audio; sign only stats it"), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAXFLOW_DATA_DIR", dataDir)
	flavor := Flavor{
		Name: "waxbin",
		OpenResolver: func(_ config.Config, next source.Resolver, _ *slog.Logger, daemon bool) (source.Resolver, io.Closer, error) {
			if daemon {
				t.Error("sign is a one-shot command; daemon must be false")
			}
			return fakePIDResolver{next: next, path: path}, nil, nil
		},
	}
	var out, errOut strings.Builder
	code := ExecuteFlavor("test", []string{"sign", "--src", "pid:01ARZ3NDEKTSV4RRFFQ69G5FAV"}, &out, &errOut, flavor)
	if code != 0 {
		t.Fatalf("exit = %d; stderr: %s", code, errOut.String())
	}
	url := out.String()
	for _, want := range []string{"src=pid%3A01ARZ3NDEKTSV4RRFFQ69G5FAV", "id=", "sig=", "exp="} {
		if !strings.Contains(url, want) {
			t.Errorf("minted URL missing %q: %s", want, url)
		}
	}
}

func TestExitCodesCommand(t *testing.T) {
	code, out, _ := run(t, "exit-codes")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"overloaded", "signature-expired", "unsupported-format", "EXIT"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestUsageErrorsExitInvalid(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"version", "--nope"}},
		{"unknown command", []string{"nope"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, errOut := run(t, tt.args...)
			if code != 2 {
				t.Errorf("exit = %d, want 2 (invalid); stderr: %s", code, errOut)
			}
		})
	}
}

// newTestHandler builds a minimal keyless server (loopback posture) for
// CLI-facing tests.
func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	srv, err := server.New(server.Config{CacheDir: t.TempDir(), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

func TestPingAgainstHandler(t *testing.T) {
	srv := httptest.NewServer(newTestHandler(t))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	code, out, errOut := run(t, "ping", "--addr", addr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, errOut)
	}
	if strings.TrimSpace(out) != "ok" {
		t.Errorf("output = %q, want \"ok\"", out)
	}
}

func TestPingUnreachable(t *testing.T) {
	// Reserve a port, then close it so nothing listens there.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	code, _, _ := run(t, "ping", "--addr", addr, "--timeout", "500ms")
	if code == 0 {
		t.Error("ping against a dead address must fail")
	}
}

func TestServeLifecycle(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cfgLogger, err := newLogger(&strings.Builder{}, config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- serve(ctx, ln, newTestHandler(t), config.Config{}, cfgLogger, "test") }()

	base := "http://" + ln.Addr().String()

	resp, err := http.Get(base + "/ping")
	if err != nil {
		t.Fatalf("GET /ping: %v", err)
	}
	var ping struct {
		Status        string `json:"status"`
		SchemaVersion int    `json:"schemaVersion"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ping); err != nil {
		t.Fatalf("decoding /ping body: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || ping.Status != "ok" || ping.SchemaVersion != 1 {
		t.Errorf("GET /ping = %d %+v, want 200 {ok 1}", resp.StatusCode, ping)
	}

	resp, err = http.Get(base + "/nope")
	if err != nil {
		t.Fatalf("GET /nope: %v", err)
	}
	var env server.ErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decoding envelope: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || string(env.Code) != "not-found" || env.SchemaVersion != 1 {
		t.Errorf("GET /nope = %d %+v, want 404 envelope with code not-found", resp.StatusCode, env)
	}

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Errorf("serve returned %v after graceful shutdown, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("serve did not shut down within 5s of cancellation")
	}
}

func TestDialAddr(t *testing.T) {
	tests := []struct{ in, want string }{
		{"127.0.0.1:4418", "127.0.0.1:4418"},
		{"0.0.0.0:4418", "127.0.0.1:4418"},
		{"[::]:4418", "127.0.0.1:4418"},
		{":4418", "127.0.0.1:4418"},
		{"example.com:4418", "example.com:4418"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := dialAddr(tt.in); got != tt.want {
				t.Errorf("dialAddr(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
