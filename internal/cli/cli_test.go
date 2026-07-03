package cli

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow/internal/config"
	"github.com/colespringer/waxflow/server"
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
