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
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/waxerr"
)

// DefaultAddr is the daemon's default listen address. Port 4418 is
// WaxFlow's assigned port in the Wax family (WaxSeal owns 4416/4417).
const DefaultAddr = "127.0.0.1:4418"

// Defaults resolved by the accessor methods below. The server package
// normalizes its zero-value Config through these same constants, so a
// direct embedder and a CLI-configured daemon cannot drift.
const (
	DefaultSourceMaxBytes = 4 << 30
	DefaultCacheMaxBytes  = 10 << 30
	DefaultJobSlots       = 2
	DefaultGainMode       = "track"
	DefaultPaceBurst      = 30 * time.Second
	DefaultPaceFactor     = 2.0
)

// DefaultLiveSlots is the live admission pool default: one slot per CPU,
// minus one core left for delivery and the OS.
func DefaultLiveSlots() int { return max(1, runtime.NumCPU()-1) }

// Root names one library directory; references address it as
// "<name>/<relative/path>".
type Root struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Config holds the daemon/CLI configuration. Every field is documented in
// the README configuration table. JSON keys are camelCase; the matching
// environment variable is WAXFLOW_ plus the upper-snake key (addr ->
// WAXFLOW_ADDR, logLevel -> WAXFLOW_LOG_LEVEL).
type Config struct {
	// Addr is the listen address for `waxflow server` and the target for
	// `waxflow ping`. Empty means DefaultAddr.
	Addr string `json:"addr"`

	// LogLevel is one of debug, info, warn, error (case-insensitive).
	// Empty means info.
	LogLevel string `json:"logLevel"`

	// Roots are the named library roots, each opened via os.Root. The env
	// form is comma-separated name=path pairs.
	Roots []Root `json:"roots"`

	// APIKeys are the control-API keys. With none configured, a
	// non-loopback Addr refuses to start unless AllowUnauthenticated is
	// explicit (the fail-closed rule). The env form is comma-separated.
	APIKeys []string `json:"apiKeys"`

	// AllowUnauthenticated opts into running keyless on a non-loopback
	// address. Never a default.
	AllowUnauthenticated bool `json:"allowUnauthenticated"`

	// SourceMaxBytes caps each resolved source file; 0 means 4 GiB.
	SourceMaxBytes int64 `json:"sourceMaxBytes"`

	// MetricsKey unlocks GET /metrics on keyed daemons (any API key works
	// too).
	MetricsKey string `json:"metricsKey"`

	// SigningSecret is the HMAC key material for signed URLs, in
	// sign.ParseKeys syntax: a literal secret, or comma-separated kid:hex
	// entries (first mints). Empty auto-generates a secret persisted under
	// DataDir with mode 0600.
	SigningSecret string `json:"signingSecret"`

	// AllowedOrigins is the CORS allowlist for playback endpoints. The env
	// form is comma-separated.
	AllowedOrigins []string `json:"allowedOrigins"`

	// DataDir holds daemon state (the persisted signing secret). Empty
	// means the platform user config dir plus /waxflow.
	DataDir string `json:"dataDir"`

	// CacheDir holds the transcode cache. Empty means the platform user
	// cache dir plus /waxflow.
	CacheDir string `json:"cacheDir"`

	// CacheMaxBytes bounds the transcode cache; 0 means 10 GiB.
	CacheMaxBytes int64 `json:"cacheMaxBytes"`

	// CacheMaxAge evicts entries idle longer than this Go duration string
	// ("720h"). Empty means no age limit.
	CacheMaxAge string `json:"cacheMaxAge"`

	// LiveSlots and JobSlots size the admission pools; 0 means
	// max(1, NumCPU-1) and 2 respectively.
	LiveSlots int `json:"liveSlots"`
	JobSlots  int `json:"jobSlots"`

	// DefaultGain is the gain mode when a stream request has no gain=
	// parameter: off, track, album, or a +/-dB number. Empty means track.
	DefaultGain string `json:"defaultGain"`

	// ResampleProfile selects resampler quality, hq or fast. Empty means
	// hq.
	ResampleProfile string `json:"resampleProfile"`

	// TLSCert and TLSKey enable native TLS when both are set (ADR-0007).
	TLSCert string `json:"tlsCert"`
	TLSKey  string `json:"tlsKey"`

	// DebugAddr enables the loopback-only pprof listener when set.
	DebugAddr string `json:"debugAddr"`

	// PaceBurstSeconds is how much audio read-behind delivery bursts
	// before pacing engages; 0 means 30.
	PaceBurstSeconds float64 `json:"paceBurstSeconds"`

	// PaceFactor caps read-behind delivery at this multiple of realtime
	// after the burst. Absent means 2.0; an explicit 0 disables pacing
	// (the pointer distinguishes the two).
	PaceFactor *float64 `json:"paceFactor"`

	// Demo serves the browser test page at GET /demo (dev mode only).
	Demo bool `json:"demo"`
}

// envVars maps environment variable names onto Config fields. One table so
// Load and documentation cannot drift.
var envVars = []struct {
	name string
	set  func(*Config, string) error
}{
	{"WAXFLOW_ADDR", func(c *Config, v string) error { c.Addr = v; return nil }},
	{"WAXFLOW_LOG_LEVEL", func(c *Config, v string) error { c.LogLevel = v; return nil }},
	{"WAXFLOW_ROOTS", func(c *Config, v string) error { return parseRoots(v, &c.Roots) }},
	{"WAXFLOW_API_KEYS", func(c *Config, v string) error { c.APIKeys = splitList(v); return nil }},
	{"WAXFLOW_ALLOW_UNAUTHENTICATED", func(c *Config, v string) error { return parseBool(v, &c.AllowUnauthenticated) }},
	{"WAXFLOW_SOURCE_MAX_BYTES", func(c *Config, v string) error { return parseInt64(v, &c.SourceMaxBytes) }},
	{"WAXFLOW_METRICS_KEY", func(c *Config, v string) error { c.MetricsKey = v; return nil }},
	{"WAXFLOW_SIGNING_SECRET", func(c *Config, v string) error { c.SigningSecret = v; return nil }},
	{"WAXFLOW_ALLOWED_ORIGINS", func(c *Config, v string) error { c.AllowedOrigins = splitList(v); return nil }},
	{"WAXFLOW_DATA_DIR", func(c *Config, v string) error { c.DataDir = v; return nil }},
	{"WAXFLOW_CACHE_DIR", func(c *Config, v string) error { c.CacheDir = v; return nil }},
	{"WAXFLOW_CACHE_MAX_BYTES", func(c *Config, v string) error { return parseInt64(v, &c.CacheMaxBytes) }},
	{"WAXFLOW_CACHE_MAX_AGE", func(c *Config, v string) error { c.CacheMaxAge = v; return nil }},
	{"WAXFLOW_LIVE_SLOTS", func(c *Config, v string) error { return parseInt(v, &c.LiveSlots) }},
	{"WAXFLOW_JOB_SLOTS", func(c *Config, v string) error { return parseInt(v, &c.JobSlots) }},
	{"WAXFLOW_DEFAULT_GAIN", func(c *Config, v string) error { c.DefaultGain = v; return nil }},
	{"WAXFLOW_RESAMPLE_PROFILE", func(c *Config, v string) error { c.ResampleProfile = v; return nil }},
	{"WAXFLOW_TLS_CERT", func(c *Config, v string) error { c.TLSCert = v; return nil }},
	{"WAXFLOW_TLS_KEY", func(c *Config, v string) error { c.TLSKey = v; return nil }},
	{"WAXFLOW_DEBUG_ADDR", func(c *Config, v string) error { c.DebugAddr = v; return nil }},
	{"WAXFLOW_PACE_BURST_SECONDS", func(c *Config, v string) error { return parseFloat(v, &c.PaceBurstSeconds) }},
	{"WAXFLOW_PACE_FACTOR", func(c *Config, v string) error {
		var f float64
		if err := parseFloat(v, &f); err != nil {
			return err
		}
		c.PaceFactor = &f
		return nil
	}},
	{"WAXFLOW_DEMO", func(c *Config, v string) error { return parseBool(v, &c.Demo) }},
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
			// A set-but-empty variable counts as unset. Compose blocks and
			// sourced env files export empty strings freely; letting
			// WAXFLOW_API_KEYS="" silently wipe file-configured keys would
			// disable auth with no error, so empty never overrides.
			if strings.TrimSpace(v) == "" {
				continue
			}
			if err := ev.set(&cfg, v); err != nil {
				return cfg, waxerr.Wrap(waxerr.CodeInvalidRequest, ev.name, err)
			}
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
	if c.DebugAddr != "" {
		if _, _, err := net.SplitHostPort(c.DebugAddr); err != nil {
			return waxerr.Wrap(waxerr.CodeInvalidRequest, "debugAddr must be host:port", err)
		}
	}
	if _, err := c.SlogLevel(); err != nil {
		return err
	}
	for _, r := range c.Roots {
		if r.Name == "" || strings.ContainsAny(r.Name, "/:") {
			return waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("root name %q must be non-empty without '/' or ':'", r.Name))
		}
		if r.Path == "" {
			return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("root %q has no path", r.Name))
		}
	}
	if c.SourceMaxBytes < 0 || c.CacheMaxBytes < 0 || c.LiveSlots < 0 || c.JobSlots < 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, "size and slot settings must be non-negative")
	}
	if _, err := c.ResolvedCacheMaxAge(); err != nil {
		return err
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return waxerr.New(waxerr.CodeInvalidRequest, "tlsCert and tlsKey must be set together")
	}
	if c.PaceBurstSeconds < 0 || !finite(c.PaceBurstSeconds) {
		return waxerr.New(waxerr.CodeInvalidRequest, "paceBurstSeconds must be a finite non-negative number")
	}
	if c.PaceFactor != nil && (*c.PaceFactor < 0 || !finite(*c.PaceFactor)) {
		return waxerr.New(waxerr.CodeInvalidRequest, "paceFactor must be a finite non-negative number")
	}
	if _, err := resample.ParseProfile(c.ResampleProfile); err != nil {
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

// ResolvedSourceMaxBytes returns the per-source cap, defaulted.
func (c Config) ResolvedSourceMaxBytes() int64 {
	if c.SourceMaxBytes == 0 {
		return DefaultSourceMaxBytes
	}
	return c.SourceMaxBytes
}

// ResolvedCacheMaxBytes returns the cache budget, defaulted.
func (c Config) ResolvedCacheMaxBytes() int64 {
	if c.CacheMaxBytes == 0 {
		return DefaultCacheMaxBytes
	}
	return c.CacheMaxBytes
}

// ResolvedCacheMaxAge parses CacheMaxAge; empty means 0 (no age limit).
func (c Config) ResolvedCacheMaxAge() (time.Duration, error) {
	if c.CacheMaxAge == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(c.CacheMaxAge)
	if err != nil || d < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("cacheMaxAge %q: want a non-negative Go duration like 720h", c.CacheMaxAge))
	}
	return d, nil
}

// ResolvedLiveSlots returns the live admission pool size, defaulted.
func (c Config) ResolvedLiveSlots() int {
	if c.LiveSlots == 0 {
		return DefaultLiveSlots()
	}
	return c.LiveSlots
}

// ResolvedJobSlots returns the job admission pool size, defaulted.
func (c Config) ResolvedJobSlots() int {
	if c.JobSlots == 0 {
		return DefaultJobSlots
	}
	return c.JobSlots
}

// ResolvedDefaultGain returns the default gain mode.
func (c Config) ResolvedDefaultGain() string {
	if c.DefaultGain == "" {
		return DefaultGainMode
	}
	return c.DefaultGain
}

// ResolvedResampleProfile returns the resampler profile name, resolved
// through the profile set's owner.
func (c Config) ResolvedResampleProfile() string {
	p, err := resample.ParseProfile(c.ResampleProfile)
	if err != nil {
		return string(resample.HQ) // Validate already rejected this value
	}
	return string(p)
}

// ResolvedPaceBurst returns the pacing burst window, defaulted.
func (c Config) ResolvedPaceBurst() time.Duration {
	if c.PaceBurstSeconds == 0 {
		return DefaultPaceBurst
	}
	return time.Duration(c.PaceBurstSeconds * float64(time.Second))
}

// ResolvedPaceFactor returns the realtime pacing multiple: absent means
// DefaultPaceFactor, explicit 0 disables pacing.
func (c Config) ResolvedPaceFactor() float64 {
	if c.PaceFactor == nil {
		return DefaultPaceFactor
	}
	return *c.PaceFactor
}

// ResolvedDataDir returns DataDir or the platform default.
func (c Config) ResolvedDataDir() (string, error) {
	return resolvedDir(c.DataDir, os.UserConfigDir, "dataDir")
}

// ResolvedCacheDir returns CacheDir or the platform default.
func (c Config) ResolvedCacheDir() (string, error) {
	return resolvedDir(c.CacheDir, os.UserCacheDir, "cacheDir")
}

func resolvedDir(set string, platform func() (string, error), name string) (string, error) {
	if set != "" {
		return set, nil
	}
	base, err := platform()
	if err != nil {
		return "", waxerr.Wrap(waxerr.CodeInvalidRequest,
			fmt.Sprintf("no platform default for %s; set it explicitly", name), err)
	}
	return filepath.Join(base, "waxflow"), nil
}

func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseRoots(v string, dst *[]Root) error {
	var roots []Root
	for _, entry := range splitList(v) {
		name, path, ok := strings.Cut(entry, "=")
		if !ok || name == "" || path == "" {
			return fmt.Errorf("root entry %q is not name=path", entry)
		}
		roots = append(roots, Root{Name: name, Path: path})
	}
	*dst = roots
	return nil
}

func parseBool(v string, dst *bool) error {
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return err
	}
	*dst = b
	return nil
}

func parseInt(v string, dst *int) error {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return err
	}
	*dst = n
	return nil
}

func parseInt64(v string, dst *int64) error {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return err
	}
	*dst = n
	return nil
}

func parseFloat(v string, dst *float64) error {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return err
	}
	*dst = f
	return nil
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
