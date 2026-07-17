package cli

import (
	"context"
	"encoding/json"
	"io"
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

func (f fakePIDResolver) Resolve(ctx context.Context, ref string) (*source.File, error) {
	if strings.HasPrefix(ref, "pid:") {
		return source.OpenLocal(ref, f.path, f.path)
	}
	return f.next.Resolve(ctx, ref)
}

func TestFlavorVersionOutput(t *testing.T) {
	var out, errOut strings.Builder
	code := ExecuteFlavor("test", []string{"version"}, &out, &errOut, Flavor{Name: "catalog"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "waxflow-catalog test ") {
		t.Errorf("output = %q, want prefix %q", out.String(), "waxflow-catalog test ")
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
	if !strings.Contains(stderr, "catalog resolver") {
		t.Errorf("stderr = %q, want a pointer at the missing catalog resolver", stderr)
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
		Name: "catalog",
		OpenResolver: func(_ context.Context, o ResolverOptions) (source.Resolver, io.Closer, error) {
			if o.Daemon {
				t.Error("sign is a one-shot command; daemon must be false")
			}
			// The rest of ResolverOptions is contract too: an
			// implementation that defaults a missing MaxBytes or Logger
			// would never notice the CLI dropping them.
			if o.MaxBytes <= 0 {
				t.Errorf("MaxBytes = %d, want the resolved source cap", o.MaxBytes)
			}
			if o.Logger == nil {
				t.Error("Logger is nil; the godoc promises it never is")
			}
			if o.Next == nil {
				t.Error("Next is nil; implementations delegate to it")
			}
			return fakePIDResolver{next: o.Next, path: path}, nil, nil
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

// TestFlavorNilResolverIsAnError pins the refusal for a broken
// out-of-tree implementation: returning no resolver and no error must
// surface as a named error, not a nil-interface panic at first use.
func TestFlavorNilResolverIsAnError(t *testing.T) {
	t.Setenv("WAXFLOW_DATA_DIR", t.TempDir())
	flavor := Flavor{
		Name: "broken",
		OpenResolver: func(context.Context, ResolverOptions) (source.Resolver, io.Closer, error) {
			return nil, nil, nil
		},
	}
	var out, errOut strings.Builder
	code := ExecuteFlavor("test", []string{"sign", "--src", "pid:01ARZ3NDEKTSV4RRFFQ69G5FAV"}, &out, &errOut, flavor)
	if code == 0 {
		t.Fatalf("exit = 0 with a nil resolver; stdout: %s", out.String())
	}
	if !strings.Contains(errOut.String(), "nil resolver") {
		t.Errorf("stderr = %q, want it to name the nil resolver", errOut.String())
	}
}

// pidReporter is a resolver that declares its pid support outright,
// the way an out-of-tree build not keyed on catalogDB must.
type pidReporter struct {
	source.Resolver
	pid bool
}

func (p pidReporter) PIDSources() bool { return p.pid }

// TestBuildServerConfigPIDSources pins what /caps advertises: the
// catalogDB inference by default, and the resolver's own answer when it
// has one.
func TestBuildServerConfigPIDSources(t *testing.T) {
	tests := []struct {
		name      string
		catalogDB string
		resolver  func(next source.Resolver) source.Resolver
		want      bool
	}{
		{
			name:     "no catalogDB, no reporter: inferred off",
			resolver: func(next source.Resolver) source.Resolver { return next },
			want:     false,
		},
		{
			name:      "catalogDB set, no reporter: inferred on",
			catalogDB: "/tmp/waxbin.db",
			resolver:  func(next source.Resolver) source.Resolver { return next },
			want:      true,
		},
		{
			// The case the inference cannot reach: pid served from
			// somewhere that is not catalogDB.
			name:     "no catalogDB, reporter says yes: advertised",
			resolver: func(next source.Resolver) source.Resolver { return pidReporter{next, true} },
			want:     true,
		},
		{
			name:      "catalogDB set, reporter says no: not advertised",
			catalogDB: "/tmp/waxbin.db",
			resolver:  func(next source.Resolver) source.Resolver { return pidReporter{next, false} },
			want:      false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("WAXFLOW_DATA_DIR", filepath.Join(dir, "data"))
			t.Setenv("WAXFLOW_CACHE_DIR", filepath.Join(dir, "cache"))
			t.Setenv("WAXFLOW_SCRATCH_DIR", filepath.Join(dir, "scratch"))
			cfg, err := config.Load("", os.LookupEnv)
			if err != nil {
				t.Fatal(err)
			}
			cfg.CatalogDB = tc.catalogDB
			logger, err := newLogger(&strings.Builder{}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			flavor := Flavor{
				Name: "catalog",
				OpenResolver: func(_ context.Context, o ResolverOptions) (source.Resolver, io.Closer, error) {
					return tc.resolver(o.Next), nil, nil
				},
			}
			srvCfg, cleanup, err := buildServerConfig(context.Background(), cfg, "test", logger, flavor)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if srvCfg.PIDSources != tc.want {
				t.Errorf("PIDSources = %v, want %v", srvCfg.PIDSources, tc.want)
			}
		})
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
