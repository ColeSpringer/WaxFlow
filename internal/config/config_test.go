package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxflow/waxerr"
)

func noEnv(string) (string, bool) { return "", false }

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
}

func TestServiceFieldsFromJSONAndEnv(t *testing.T) {
	path := writeConfig(t, `{
		"roots": [{"name":"lib","path":"/music"}],
		"apiKeys": ["k1"],
		"cacheMaxAge": "720h",
		"paceFactor": 0
	}`)
	cfg, err := Load(path, envMap(map[string]string{
		"WAXFLOW_ROOTS":           "a=/x, b=/y",
		"WAXFLOW_API_KEYS":        "k2, k3",
		"WAXFLOW_ALLOWED_ORIGINS": "https://deck.example",
		"WAXFLOW_LIVE_SLOTS":      "3",
		"WAXFLOW_DEMO":            "true",
		"WAXFLOW_CACHE_MAX_BYTES": "1024",
		"WAXFLOW_CATALOG_DB":      "/catalog/waxbin.db",
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
