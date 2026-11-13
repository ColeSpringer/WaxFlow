package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow/waxerr"
)

func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "waxflow.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadPrecedence(t *testing.T) {
	tests := []struct {
		name     string
		file     string // empty = no file
		env      map[string]string
		wantAddr string
	}{
		{"defaults", "", nil, DefaultAddr},
		{"file over default", `{"addr":"127.0.0.1:5000"}`, nil, "127.0.0.1:5000"},
		{"env over file", `{"addr":"127.0.0.1:5000"}`, map[string]string{"WAXFLOW_ADDR": "127.0.0.1:6000"}, "127.0.0.1:6000"},
		{"env over default", "", map[string]string{"WAXFLOW_ADDR": "127.0.0.1:6000"}, "127.0.0.1:6000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := ""
			if tt.file != "" {
				path = writeConfig(t, tt.file)
			}
			cfg, err := Load(path, envMap(tt.env))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.ResolvedAddr(); got != tt.wantAddr {
				t.Errorf("ResolvedAddr() = %q, want %q", got, tt.wantAddr)
			}
		})
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name     string
		path     func(t *testing.T) string
		env      map[string]string
		wantCode waxerr.Code
	}{
		{
			"unknown field rejected",
			func(t *testing.T) string { return writeConfig(t, `{"addr":"127.0.0.1:5000","nope":true}`) },
			nil,
			waxerr.CodeInvalidRequest,
		},
		{
			"trailing data rejected",
			func(t *testing.T) string { return writeConfig(t, `{"addr":"127.0.0.1:5000"} {}`) },
			nil,
			waxerr.CodeInvalidRequest,
		},
		{
			"malformed json",
			func(t *testing.T) string { return writeConfig(t, `{`) },
			nil,
			waxerr.CodeInvalidRequest,
		},
		{
			"missing file",
			func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.json") },
			nil,
			waxerr.CodeNotFound,
		},
		{
			"bad addr from env",
			func(t *testing.T) string { return "" },
			map[string]string{"WAXFLOW_ADDR": "no-port"},
			waxerr.CodeInvalidRequest,
		},
		{
			"bad log level",
			func(t *testing.T) string { return "" },
			map[string]string{"WAXFLOW_LOG_LEVEL": "loud"},
			waxerr.CodeInvalidRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(tt.path(t), envMap(tt.env))
			if err == nil {
				t.Fatal("Load should fail")
			}
			e, ok := errors.AsType[*waxerr.Error](err)
			if !ok {
				t.Fatalf("error should be *waxerr.Error, got %T: %v", err, err)
			}
			if e.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", e.Code, tt.wantCode)
			}
		})
	}
}

func TestSlogLevel(t *testing.T) {
	tests := []struct {
		in   string
		want slog.Level
	}{
		{"", slog.LevelInfo},
		{"debug", slog.LevelDebug},
		{"INFO", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
	}
	for _, tt := range tests {
		t.Run("level "+tt.in, func(t *testing.T) {
			got, err := Config{LogLevel: tt.in}.SlogLevel()
			if err != nil {
				t.Fatalf("SlogLevel: %v", err)
			}
			if got != tt.want {
				t.Errorf("SlogLevel() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestZeroValueUsable(t *testing.T) {
	var cfg Config
	if err := cfg.Validate(); err != nil {
		t.Errorf("zero Config must validate, got %v", err)
	}
	if cfg.ResolvedAddr() != DefaultAddr {
		t.Errorf("zero Config addr = %q, want %q", cfg.ResolvedAddr(), DefaultAddr)
	}
	if got := cfg.ResolvedSourceMaxBytes(); got != DefaultSourceMaxBytes {
		t.Errorf("ResolvedSourceMaxBytes() = %d", got)
	}
	if got := cfg.ResolvedCacheMaxBytes(); got != DefaultCacheMaxBytes {
		t.Errorf("ResolvedCacheMaxBytes() = %d", got)
	}
	if age, err := cfg.ResolvedCacheMaxAge(); err != nil || age != 0 {
		t.Errorf("ResolvedCacheMaxAge() = %v, %v", age, err)
	}
	if got := cfg.ResolvedLiveSlots(); got < 1 {
		t.Errorf("ResolvedLiveSlots() = %d", got)
	}
	if got := cfg.ResolvedJobSlots(); got != DefaultJobSlots {
		t.Errorf("ResolvedJobSlots() = %d", got)
	}
	if got := cfg.ResolvedDefaultGain(); got != "track" {
		t.Errorf("ResolvedDefaultGain() = %q", got)
	}
	if got := cfg.ResolvedPaceBurst(); got != DefaultPaceBurst {
		t.Errorf("ResolvedPaceBurst() = %v", got)
	}
	if got := cfg.ResolvedPaceFactor(); got != DefaultPaceFactor {
		t.Errorf("ResolvedPaceFactor() = %v", got)
	}
	if got, err := cfg.ResolvedLiveIdleGrace(); err != nil || got != 5*time.Second {
		t.Errorf("ResolvedLiveIdleGrace() = %v, %v, want 5s", got, err)
	}
	if got, err := cfg.ResolvedHLSIdleGrace(); err != nil || got != time.Minute {
		t.Errorf("ResolvedHLSIdleGrace() = %v, %v, want 1m", got, err)
	}
}

func TestServiceFieldsFromJSONAndEnv(t *testing.T) {
	path := writeConfig(t, `{
		"roots": [{"name":"lib","path":"/music"}],
		"apiKeys": ["k1"],
		"cacheMaxAge": "720h",
		"paceFactor": 0,
		"liveIdleGrace": "0"
	}`)
	cfg, err := Load(path, envMap(map[string]string{
		"WAXFLOW_ROOTS":           "a=/x, b=/y",
		"WAXFLOW_API_KEYS":        "k2, k3",
		"WAXFLOW_ALLOWED_ORIGINS": "https://deck.example",
		"WAXFLOW_LIVE_SLOTS":      "3",
		"WAXFLOW_DEMO":            "true",
		"WAXFLOW_CACHE_MAX_BYTES": "1024",
		"WAXFLOW_CATALOG_DB":      "/catalog/waxbin.db",
		"WAXFLOW_HLS_IDLE_GRACE":  "90s",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Roots) != 2 || cfg.Roots[0] != (Root{"a", "/x"}) || cfg.Roots[1] != (Root{"b", "/y"}) {
		t.Errorf("env roots override = %+v", cfg.Roots)
	}
	if len(cfg.APIKeys) != 2 || cfg.APIKeys[0] != "k2" {
		t.Errorf("env apiKeys override = %v", cfg.APIKeys)
	}
	if len(cfg.AllowedOrigins) != 1 || cfg.AllowedOrigins[0] != "https://deck.example" {
		t.Errorf("allowedOrigins = %v", cfg.AllowedOrigins)
	}
	if cfg.LiveSlots != 3 || !cfg.Demo || cfg.CacheMaxBytes != 1024 {
		t.Errorf("scalar envs: liveSlots=%d demo=%v cacheMaxBytes=%d", cfg.LiveSlots, cfg.Demo, cfg.CacheMaxBytes)
	}
	if cfg.CatalogDB != "/catalog/waxbin.db" {
		t.Errorf("catalogDB = %q", cfg.CatalogDB)
	}
	if age, err := cfg.ResolvedCacheMaxAge(); err != nil || age != 720*time.Hour {
		t.Errorf("cacheMaxAge = %v, %v", age, err)
	}
	// paceFactor: an explicit JSON 0 disables pacing, distinct from absent.
	if got := cfg.ResolvedPaceFactor(); got != 0 {
		t.Errorf("explicit paceFactor 0 resolved to %v", got)
	}
	// An explicit "0" grace never cancels, distinct from absent.
	if got, err := cfg.ResolvedLiveIdleGrace(); err != nil || got != 0 {
		t.Errorf("liveIdleGrace \"0\" resolved to %v, %v", got, err)
	}
	if got, err := cfg.ResolvedHLSIdleGrace(); err != nil || got != 90*time.Second {
		t.Errorf("WAXFLOW_HLS_IDLE_GRACE resolved to %v, %v", got, err)
	}
}

func TestEmptyEnvDoesNotOverride(t *testing.T) {
	// A set-but-empty variable counts as unset: compose blocks and
	// sourced env files export empty strings freely, and an empty
	// WAXFLOW_API_KEYS must never silently wipe file-configured keys
	// (that would disable auth with no error).
	path := writeConfig(t, `{"apiKeys":["k1"],"addr":"127.0.0.1:5000"}`)
	cfg, err := Load(path, envMap(map[string]string{
		"WAXFLOW_API_KEYS": "",
		"WAXFLOW_ADDR":     "  ",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0] != "k1" {
		t.Fatalf("empty env wiped file keys: %v", cfg.APIKeys)
	}
	if cfg.Addr != "127.0.0.1:5000" {
		t.Fatalf("blank env overrode addr: %q", cfg.Addr)
	}
}

func TestServiceFieldValidation(t *testing.T) {
	bad := []Config{
		{Roots: []Root{{Name: "a/b", Path: "/x"}}},
		{Roots: []Root{{Name: "a", Path: ""}}},
		{SourceMaxBytes: -1},
		{LiveSlots: -1},
		{CacheMaxAge: "yesterday"},
		{TLSCert: "cert.pem"}, // key missing
		{DebugAddr: "no-port"},
		{PaceBurstSeconds: -3},
		{ResampleProfile: "ultra"},
		{LiveIdleGrace: "soon"},
		{HLSIdleGrace: "-1s"},
		{UploadTTL: "-1h"},
		{PIDLookupURL: "deck/pid"},
		{PIDLookupURL: "http://deck/pid?x=1"},
		{PIDLookupKey: "k"},
		{PIDLookupURL: "http://deck/pid", CatalogDB: "/x/waxbin.db"},
		{PIDLookupURL: "http://deck/pid", PIDLookupKey: "bad key"},
		{PIDLookupURL: "http://deck/pid", PIDLookupKey: "k\n"},
		{DefaultGain: "loud"},
		{DefaultGain: "NaN"},
		{SigningSecret: "a:zz"},      // not hex
		{SigningSecret: "one,two"},   // a list of literals
		{SigningSecret: "a:00,a:01"}, // one id twice
		{SigningSecret: ":00"},       // no id
		{APIKeys: []string{"k", ""}},
		{APIKeys: []string{" k"}}, // a header's value is trimmed
		{APIKeys: []string{"k\t"}},
		{APIKeys: []string{" "}},
		{MetricsKey: "m "}, // presented through the same header
		{MetricsKey: "\tm"},
	}
	for i, cfg := range bad {
		if err := cfg.Validate(); err == nil {
			t.Errorf("case %d validated: %+v", i, cfg)
		} else if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
			t.Errorf("case %d code = %s", i, waxerr.CodeOf(err))
		}
	}
	neg := -1.0
	if err := (Config{PaceFactor: &neg}).Validate(); err == nil {
		t.Error("negative paceFactor validated")
	}
}

// TestPIDLookupFromJSONAndEnv loads the two pid lookup fields from the file
// and lets the environment override them, like every other setting.
func TestPIDLookupFromJSONAndEnv(t *testing.T) {
	path := writeConfig(t, `{"pidLookupURL": "http://file:4420/pid", "pidLookupKey": "fk"}`)
	cfg, err := Load(path, envMap(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PIDLookupURL != "http://file:4420/pid" || cfg.PIDLookupKey != "fk" {
		t.Errorf("file values = %q, %q", cfg.PIDLookupURL, cfg.PIDLookupKey)
	}
	cfg, err = Load(path, envMap(map[string]string{
		"WAXFLOW_PID_LOOKUP_URL": "http://deck:4420/engine/pid",
		"WAXFLOW_PID_LOOKUP_KEY": "k",
	}))
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	if cfg.PIDLookupURL != "http://deck:4420/engine/pid" || cfg.PIDLookupKey != "k" {
		t.Errorf("env overrides = %q, %q", cfg.PIDLookupURL, cfg.PIDLookupKey)
	}
}

// TestPIDLookupValidates pins what a pid source configuration may look like:
// a lookup URL alone or with a key, or a catalogDB alone, never both sources.
func TestPIDLookupValidates(t *testing.T) {
	for i, cfg := range []Config{
		{PIDLookupURL: "http://deck/pid"},
		{PIDLookupURL: "https://u:p@deck/", PIDLookupKey: "k"},
		{PIDLookupURL: "http://deck/pid", PIDLookupKey: "Abc-._~+/="},
		{CatalogDB: "/x/waxbin.db"},
	} {
		if err := cfg.Validate(); err != nil {
			t.Errorf("case %d refused: %v", i, err)
		}
	}
}

// TestEnvVarsNamesEveryKnob: the exported list is the table, so a test suite
// clearing the ambient environment clears every setting Load reads.
func TestEnvVarsNamesEveryKnob(t *testing.T) {
	names := EnvVars()
	if len(names) != len(envVars) {
		t.Fatalf("EnvVars() lists %d names, the table has %d", len(names), len(envVars))
	}
	for _, want := range []string{"WAXFLOW_ADDR", "WAXFLOW_PID_LOOKUP_URL", "WAXFLOW_PID_LOOKUP_KEY"} {
		if !slices.Contains(names, want) {
			t.Errorf("EnvVars() lacks %s", want)
		}
	}
}

// TestLoadRefusesAFileThatIsNotUTF8: a JSON decoder reads a stray byte as
// U+FFFD, so a root's path would name another folder. The environment
// carries the bytes as they are.
func TestLoadRefusesAFileThatIsNotUTF8(t *testing.T) {
	path := writeConfig(t, "{\"roots\":[{\"name\":\"lib\",\"path\":\"/music/caf\xe9\"}]}")
	cfg, err := Load(path, func(string) (string, bool) { return "", false })
	if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Fatalf("Load = %+v, %v; want invalid-request", cfg.Roots, err)
	}
	if !strings.Contains(err.Error(), "UTF-8") || !strings.Contains(err.Error(), "pct:") {
		t.Errorf("the refusal does not state the rule and the form that satisfies it: %v", err)
	}

	cfg, err = Load("", func(name string) (string, bool) {
		if name == "WAXFLOW_ROOTS" {
			return "lib=/music/caf\xe9", true
		}
		return "", false
	})
	if err != nil || len(cfg.Roots) != 1 || cfg.Roots[0].Path != "/music/caf\xe9" {
		t.Fatalf("WAXFLOW_ROOTS gave %+v, %v; want the path's own bytes", cfg.Roots, err)
	}
}

// TestRootPathTakesTextForm: a folder's path is bytes like a file's name,
// and neither JSON nor a compose file can hold one that is not UTF-8.
func TestRootPathTakesTextForm(t *testing.T) {
	const want = "/music/caf\xe9"
	path := writeConfig(t, `{"roots":[{"name":"lib","path":"pct:/music/caf%E9"},{"name":"hits","path":"/music/100% hits"}]}`)
	cfg, err := Load(path, envMap(nil))
	if err != nil || len(cfg.Roots) != 2 || cfg.Roots[0].Path != want || cfg.Roots[1].Path != "/music/100% hits" {
		t.Fatalf("the file gave %+q, %v", cfg.Roots, err)
	}
	cfg, err = Load("", envMap(map[string]string{"WAXFLOW_ROOTS": "lib=pct:/music/caf%E9"}))
	if err != nil || len(cfg.Roots) != 1 || cfg.Roots[0].Path != want {
		t.Fatalf("WAXFLOW_ROOTS gave %+q, %v", cfg.Roots, err)
	}
	_, err = Load("", envMap(map[string]string{"WAXFLOW_ROOTS": "lib=pct:/music/100%"}))
	if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest || !strings.Contains(err.Error(), "lib") {
		t.Fatalf("a malformed escape gave %v, want invalid-request naming the root", err)
	}
}

// TestEveryPathTakesTextForm: the refusal of a file that is not UTF-8 tells
// the operator to write pct:, so every path the file can hold has to read
// it, or the daemon would make a folder named "pct:/srv/caf%E9".
func TestEveryPathTakesTextForm(t *testing.T) {
	const want = "/srv/caf\xe9"
	path := writeConfig(t, `{"dataDir":"pct:/srv/caf%E9","cacheDir":"pct:/srv/caf%E9","scratchDir":"pct:/srv/caf%E9",
		"tlsCert":"pct:/srv/caf%E9","tlsKey":"pct:/srv/caf%E9","catalogDB":"pct:/srv/caf%E9"}`)
	cfg, err := Load(path, envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{"dataDir": cfg.DataDir, "cacheDir": cfg.CacheDir, "scratchDir": cfg.ScratchDir,
		"tlsCert": cfg.TLSCert, "tlsKey": cfg.TLSKey, "catalogDB": cfg.CatalogDB} {
		if got != want {
			t.Errorf("%s = %q, want the path's own bytes %q", name, got, want)
		}
	}
	cfg, err = Load("", envMap(map[string]string{"WAXFLOW_CACHE_DIR": "pct:/srv/caf%E9"}))
	if err != nil || cfg.CacheDir != want {
		t.Fatalf("WAXFLOW_CACHE_DIR gave %q, %v", cfg.CacheDir, err)
	}
	_, err = Load("", envMap(map[string]string{"WAXFLOW_CACHE_DIR": "pct:/srv/100%"}))
	if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest || !strings.Contains(err.Error(), "cacheDir") {
		t.Fatalf("a malformed escape gave %v, want invalid-request naming the field", err)
	}
}

// TestLoadRefusesAnEscapedLoneSurrogate: \udce9 is ASCII, so the file is
// valid UTF-8, and a decoder still reads it as U+FFFD.
func TestLoadRefusesAnEscapedLoneSurrogate(t *testing.T) {
	path := writeConfig(t, `{"roots":[{"name":"lib","path":"/music/caf\udce9"}]}`)
	cfg, err := Load(path, envMap(nil))
	if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest || !strings.Contains(err.Error(), "pct:") {
		t.Fatalf("Load = %+q, %v; want invalid-request naming the pct: form", cfg.Roots, err)
	}
}

// TestEveryStringFieldIsAPathOrNot guards the two above: a string field
// added to Config is either read from text form or listed here as holding
// no path, so a new directory setting cannot miss the decode.
func TestEveryStringFieldIsAPathOrNot(t *testing.T) {
	notPaths := map[string]bool{
		"addr": true, "logLevel": true, "pidLookupURL": true, "pidLookupKey": true, "metricsKey": true,
		"signingSecret": true, "cacheMaxAge": true, "uploadTTL": true, "defaultGain": true,
		"resampleProfile": true, "debugAddr": true, "liveIdleGrace": true, "hlsIdleGrace": true,
	}
	var cfg Config
	paths := map[*string]bool{}
	for _, f := range pathFields(&cfg) {
		paths[f.value] = true
	}
	v := reflect.ValueOf(&cfg).Elem()
	for i := range v.NumField() {
		field := v.Type().Field(i)
		if field.Type.Kind() != reflect.String {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		isPath := paths[v.Field(i).Addr().Interface().(*string)]
		if isPath == notPaths[name] {
			t.Errorf("%s (%s): path = %v, listed as not a path = %v; it is one or the other", field.Name, name, isPath, notPaths[name])
		}
	}
}

// TestValidateAcceptsWhatTheDaemonStartsWith: the three settings Validate
// reads the syntax of still take every spelling the daemon does.
func TestValidateAcceptsWhatTheDaemonStartsWith(t *testing.T) {
	for i, cfg := range []Config{
		{DefaultGain: "album"},
		{DefaultGain: "-3dB"},
		{DefaultGain: "OFF"},
		{SigningSecret: "a literal secret"},
		{SigningSecret: "a:00,b:01"},
		{APIKeys: []string{"k", "another", "with a space inside"}},
		{MetricsKey: "a metrics key"},
	} {
		if err := cfg.Validate(); err != nil {
			t.Errorf("case %d (%+v) refused: %v", i, cfg, err)
		}
	}
}

// TestValidateNamesTheField: a refusal says which key to fix, and never
// quotes a secret.
func TestValidateNamesTheField(t *testing.T) {
	for field, cfg := range map[string]Config{
		"defaultGain":   {DefaultGain: "loud"},
		"signingSecret": {SigningSecret: "kid:5ec2e7,kid:5ec2e8"},
		"apiKeys":       {APIKeys: []string{""}},
	} {
		err := cfg.Validate()
		if err == nil {
			t.Errorf("%s: validated", field)
			continue
		}
		if !strings.Contains(err.Error(), field) {
			t.Errorf("%s: the refusal does not name the key: %v", field, err)
		}
		if strings.Contains(err.Error(), "5ec2e") {
			t.Errorf("%s: the refusal quotes the secret: %v", field, err)
		}
	}
	// A literal secret with a colon in it reads as a malformed kid:hex
	// entry, whose "key id" is the head of the secret.
	err := Config{SigningSecret: "hunter2:pa55word"}.Validate()
	if err == nil || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "pa55word") {
		t.Errorf("a literal secret with a colon: %v; want a refusal that quotes none of it", err)
	}
}

// TestCheckDaemon: the two rules only a daemon start has. Validate accepts
// every one of these configurations, so a local command runs under them.
func TestCheckDaemon(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"the default address, keyless", Config{}, true},
		{"loopback, keyless", Config{Addr: "127.0.0.1:4418"}, true},
		{"wildcard, keyless", Config{Addr: "0.0.0.0:4418"}, false},
		{"empty host, keyless", Config{Addr: ":4418"}, false},
		{"a LAN address, keyless", Config{Addr: "192.168.1.5:4418"}, false},
		{"wildcard with a key", Config{Addr: "0.0.0.0:4418", APIKeys: []string{"k"}}, true},
		{"wildcard, keyless by choice", Config{Addr: "0.0.0.0:4418", AllowUnauthenticated: true}, true},
		{"a loopback debug listener", Config{DebugAddr: "localhost:6060"}, true},
		{"a wildcard debug listener", Config{DebugAddr: "0.0.0.0:6060"}, false},
		{"a wildcard debug listener behind a key", Config{DebugAddr: ":6060", APIKeys: []string{"k"}}, false},
	} {
		err := tc.cfg.CheckDaemon()
		if (err == nil) != tc.ok {
			t.Errorf("%s: CheckDaemon = %v, want ok %v", tc.name, err, tc.ok)
		}
		if err != nil && waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
			t.Errorf("%s: code = %s, want invalid-request", tc.name, waxerr.CodeOf(err))
		}
		if err := tc.cfg.Validate(); err != nil {
			t.Errorf("%s: Validate = %v; a daemon-only rule must not refuse a local command", tc.name, err)
		}
	}
}

func TestLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"":                 true, // the default address
		"127.0.0.1:4418":   true,
		"127.0.0.2:1":      true,
		"localhost:4418":   true,
		"LocalHost:4418":   true,
		"[::1]:4418":       true,
		"0.0.0.0:4418":     false,
		":4418":            false, // an empty host is every interface
		"[::]:4418":        false,
		"192.168.1.5:4418": false,
		"example.com:4418": false,
		"no-port":          false,
	} {
		if got := LoopbackAddr(addr); got != want {
			t.Errorf("LoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}
