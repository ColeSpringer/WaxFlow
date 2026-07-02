package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

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
}
