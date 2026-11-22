package cli

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/waxerr"
)

// configFaults is each way a config file fails to load, as the bytes it
// holds; nil is a file that is not there. value is a setting's value the
// fault quotes to the daemon's own readers, where there is one; the config
// file's path is quoted otherwise.
var configFaults = []struct {
	name  string
	body  []byte
	code  waxerr.Code // what the loader answers
	value string
	// wire is the reload's message, where it is pinned; a missing file's is
	// the OS's own words (missingFileText).
	wire string
}{
	{name: "malformed JSON", body: []byte(`{"roots": [`), code: waxerr.CodeInvalidRequest},
	{name: "trailing data", body: []byte(`{} {}`), code: waxerr.CodeInvalidRequest},
	{name: "a byte that is not UTF-8", body: []byte("{\"addr\": \"\xff\"}"), code: waxerr.CodeInvalidRequest},
	{name: "missing", code: waxerr.CodeNotFound},
	{name: "a pid lookup URL that is a bare path", body: []byte(`{"pidLookupURL": "/var/lib/waxflow/pids"}`),
		code: waxerr.CodeInvalidRequest, value: "/var/lib/waxflow/pids"},
	{name: "a pid lookup URL that is a file URL", body: []byte(`{"pidLookupURL": "file:///var/lib/waxflow/pids"}`),
		code: waxerr.CodeInvalidRequest, value: "file:///var/lib/waxflow/pids"},
	{name: "a pid lookup URL with no host", body: []byte(`{"pidLookupURL": "http:///var/lib/waxflow/pids"}`),
		code: waxerr.CodeInvalidRequest, value: "http:///var/lib/waxflow/pids"},
}

// configSpellings is the config file named absolute and relative to the
// working directory, which each run moves into dir.
func configSpellings(t *testing.T, dir string) map[string]string {
	t.Helper()
	testutil.Chdir(t, dir)
	return map[string]string{
		"absolute": filepath.Join(dir, "waxflow.json"),
		"relative": "waxflow.json",
	}
}

// breakConfig leaves the fault's bytes at path, or no file.
func breakConfig(t *testing.T, path string, body []byte) {
	t.Helper()
	if body == nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// missingFileText is the OS's words for opening a file that is not there,
// which Linux and Windows spell differently.
func missingFileText(t *testing.T) string {
	t.Helper()
	_, err := os.Open(filepath.Join(t.TempDir(), "missing.json"))
	pe, ok := errors.AsType[*fs.PathError](err)
	if !ok {
		t.Fatalf("opening a missing file: %v, want a *fs.PathError", err)
	}
	return pe.Err.Error()
}

// holdsPart reports the first part of path that text holds: the path as
// given, as absolute, its folder or its file name.
func holdsPart(t *testing.T, text, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{path, abs, filepath.Dir(abs), filepath.Base(path)} {
		if strings.Contains(text, part) {
			return part
		}
	}
	return ""
}

// A reload refused over the config file answers what it answered, a 400,
// and names no part of the file or a path a setting holds; the daemon's log
// line names them.
func TestAFailedReloadNamesNoConfigPath(t *testing.T) {
	unsetenv(t, "WAXFLOW_ROOTS")
	unsetenv(t, "WAXFLOW_CATALOG_DB")
	for _, fault := range configFaults {
		t.Run(fault.name, func(t *testing.T) {
			for spelling, path := range configSpellings(t, t.TempDir()) {
				t.Run(spelling, func(t *testing.T) {
					cfg := healthyConfig(t)
					b, err := json.Marshal(cfg)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, b, 0o600); err != nil {
						t.Fatal(err)
					}
					var log strings.Builder
					logger, err := newLogger(&log, cfg)
					if err != nil {
						t.Fatal(err)
					}
					srvCfg, cleanup, err := buildServerConfig(t.Context(), cfg, path, "test", logger, Flavor{})
					if err != nil {
						t.Fatal(err)
					}
					defer cleanup()
					srv, err := server.New(srvCfg)
					if err != nil {
						t.Fatal(err)
					}
					defer srv.Close()

					breakConfig(t, path, fault.body)
					rec := httptest.NewRecorder()
					srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/roots/reload", nil))
					body := rec.Body.String()
					var env struct{ Code waxerr.Code }
					if err := json.Unmarshal([]byte(body), &env); err != nil {
						t.Fatalf("reload body %q: %v", body, err)
					}
					if rec.Code != http.StatusBadRequest || env.Code != waxerr.CodeInvalidRequest {
						t.Errorf("reload = %d %s, want 400 %s", rec.Code, env.Code, waxerr.CodeInvalidRequest)
					}
					if part := holdsPart(t, body, path); part != "" {
						t.Errorf("the reload's body %q names %q of the config file", body, part)
					}
					if fault.value != "" && strings.Contains(body, "/var/lib/waxflow") {
						t.Errorf("the reload's body %q quotes the setting %s", body, fault.value)
					}
					var said struct{ Error string }
					json.Unmarshal([]byte(body), &said)
					wire := fault.wire
					if fault.body == nil {
						wire = "reload: config file: open: " + missingFileText(t)
					}
					if wire != "" && said.Error != wire {
						t.Errorf("the reload says %q, want %q", said.Error, wire)
					}
					named := path
					if fault.value != "" {
						named = fault.value
					}
					if line := log.String(); !strings.Contains(line, "roots reload failed") || !strings.Contains(line, named) {
						t.Errorf("the daemon's log does not name %s:\n%s", named, line)
					}
				})
			}
		})
	}
}

// Every command that loads the configuration names the file it could not
// load, or the setting's value, to its own user, with the code it had.
func TestTheCLINamesTheConfigFileItCannotLoad(t *testing.T) {
	for _, fault := range configFaults {
		t.Run(fault.name, func(t *testing.T) {
			for spelling, path := range configSpellings(t, t.TempDir()) {
				t.Run(spelling, func(t *testing.T) {
					breakConfig(t, path, fault.body)
					code, out, stderr := run(t, "doctor", "--config", path)
					if code != fault.code.ExitCode() {
						t.Errorf("doctor exit = %d, want %d:\n%s%s", code, fault.code.ExitCode(), out, stderr)
					}
					named := path
					if fault.value != "" {
						named = fault.value
					}
					if !strings.Contains(stderr, named) {
						t.Errorf("doctor's error does not name %s:\n%s", named, stderr)
					}
				})
			}
		})
	}
}
