// Package config loads WaxFlow configuration with the Wax-family
// precedence: flag > WAXFLOW_* environment variable > JSON config file >
// built-in default. Flag overrides are applied by the CLI layer after Load;
// this package resolves the other three.
//
// Config structs are zero-value-usable: an empty value means "use the
// default", resolved at the point of use, so injected configs never need
// pre-population.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"strings"

	"github.com/colespringer/waxflow/waxerr"
)

// DefaultAddr is the daemon's default listen address. Port 4418 is
// WaxFlow's assigned port in the Wax family (WaxSeal owns 4416/4417).
const DefaultAddr = "127.0.0.1:4418"

// Config holds the daemon/CLI configuration. Fields grow as features
// land; every field is documented in the README configuration table.
// JSON keys are camelCase; the matching environment variable is WAXFLOW_
// plus the upper-snake key (addr -> WAXFLOW_ADDR, logLevel ->
// WAXFLOW_LOG_LEVEL).
type Config struct {
	// Addr is the listen address for `waxflow server` and the target for
	// `waxflow ping`. Empty means DefaultAddr.
	Addr string `json:"addr"`

	// LogLevel is one of debug, info, warn, error (case-insensitive).
	// Empty means info.
	LogLevel string `json:"logLevel"`
}

// envVars maps environment variable names onto Config fields. One table so
// Load and documentation cannot drift.
var envVars = []struct {
	name  string
	field func(*Config) *string
}{
	{"WAXFLOW_ADDR", func(c *Config) *string { return &c.Addr }},
	{"WAXFLOW_LOG_LEVEL", func(c *Config) *string { return &c.LogLevel }},
}

// Load resolves configuration from the JSON file at path overlaid with
// WAXFLOW_* environment variables (looked up via lookupEnv, typically
// os.LookupEnv). An empty path skips the file layer; a non-empty path must
// exist. Unknown JSON keys are rejected (strict decode). Errors carry
// waxerr.CodeInvalidRequest.
func Load(path string, lookupEnv func(string) (string, bool)) (Config, error) {
	var cfg Config

	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			code := waxerr.CodeInvalidRequest
			if errors.Is(err, fs.ErrNotExist) {
				code = waxerr.CodeNotFound
			}
			return cfg, waxerr.Wrap(code, "config file", err)
		}
		defer f.Close()
		dec := json.NewDecoder(f)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return cfg, waxerr.Wrap(waxerr.CodeInvalidRequest, fmt.Sprintf("config file %s", path), err)
		}
		// A second value in the stream is as malformed as a bad key.
		if dec.More() {
			return cfg, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("config file %s: trailing data after JSON object", path))
		}
	}

	for _, ev := range envVars {
		if v, ok := lookupEnv(ev.name); ok {
			*ev.field(&cfg) = v
		}
	}

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate checks field syntax without touching the network or filesystem.
func (c Config) Validate() error {
	if c.Addr != "" {
		if _, _, err := net.SplitHostPort(c.Addr); err != nil {
			return waxerr.Wrap(waxerr.CodeInvalidRequest, "addr must be host:port", err)
		}
	}
	if _, err := c.SlogLevel(); err != nil {
		return err
	}
	return nil
}

// ResolvedAddr returns Addr or DefaultAddr when unset.
func (c Config) ResolvedAddr() string {
	if c.Addr == "" {
		return DefaultAddr
	}
	return c.Addr
}

// SlogLevel parses LogLevel; the zero value resolves to slog.LevelInfo.
func (c Config) SlogLevel() (slog.Level, error) {
	if c.LogLevel == "" {
		return slog.LevelInfo, nil
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.TrimSpace(c.LogLevel))); err != nil {
		return 0, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("logLevel %q: want debug, info, warn, or error", c.LogLevel))
	}
	return lvl, nil
}
