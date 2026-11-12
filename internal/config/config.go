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
	"bytes"
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
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// DefaultAddr is the daemon's default listen address. Port 4418 is
// WaxFlow's assigned port in the Wax family (WaxSeal owns 4416/4417).
const DefaultAddr = "127.0.0.1:4418"

// Defaults resolved by the accessor methods below. The server package
// normalizes its zero-value Config through these same constants, so a
// direct embedder and a CLI-configured daemon cannot drift.
const (
	DefaultSourceMaxBytes  = 4 << 30
	DefaultCacheMaxBytes   = 10 << 30
	DefaultJobSlots        = 2
	DefaultGainMode        = "track"
	DefaultPaceBurst       = 30 * time.Second
	DefaultPaceFactor      = 2.0
	DefaultUploadMaxBytes  = 2 << 30
	DefaultUploadTTL       = time.Hour
	DefaultScratchMaxBytes = 8 << 30
	DefaultLiveIdleGrace   = 5 * time.Second
	DefaultHLSIdleGrace    = time.Minute
)

// DefaultLiveSlots is the live admission pool default: one slot per CPU,
// minus one core left for delivery and the OS.
func DefaultLiveSlots() int { return max(1, runtime.NumCPU()-1) }

// Root names one library directory; references address it as
// "<name>/<relative/path>". Load reads Path, like every path in Config, from
// text form (source.Ref), so what it holds is the folder's own bytes.
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

	// CatalogDB is the path of a WaxBin catalog database, opened
	// read-only to resolve pid:<id> source references in a build that
	// injects a catalog resolver through cli.Flavor (the stock build
	// resolves them through PIDLookupURL instead). It exists so such a
	// build reads its catalog path from the family's normal precedence
	// chain (flag > env > JSON file) instead of reinventing configuration;
	// cli.ResolverOptions.CatalogDB is what carries it across the seam.
	//
	// A build without such a resolver refuses a configured CatalogDB
	// loudly, rather than silently ignoring a capability the operator
	// asked for, everywhere configuration is resolved: the server, sign,
	// and any one-shot given a pid: or upload: ref. One-shots on plain
	// paths never read configuration at all (a broken config file must
	// not fail probing a local file), so they neither honor nor refuse
	// it. Exclusive with PIDLookupURL.
	CatalogDB string `json:"catalogDB"`

	// PIDLookupURL is the host endpoint that maps pid:<id> source references
	// to root-relative ones: GET <url>/<id> answers {"ref": "<root>/<path>"}
	// (source.PIDLookup). Read at start; exclusive with CatalogDB.
	PIDLookupURL string `json:"pidLookupURL"`

	// PIDLookupKey is sent as a bearer token with every pid lookup, so it
	// holds only what that header carries (source.CheckPIDLookupKey).
	PIDLookupKey string `json:"pidLookupKey"`

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

	// ScratchDir holds the upload spool. Empty means the platform temp
	// directory plus /waxflow (the hardened container mounts a tmpfs
	// there).
	ScratchDir string `json:"scratchDir"`

	// UploadMaxBytes caps one spooled upload; 0 means 2 GiB.
	UploadMaxBytes int64 `json:"uploadMaxBytes"`

	// UploadTTL evicts spooled uploads this long after creation, as a Go
	// duration string ("1h"). Empty means the 1 hour default; "0" never
	// expires.
	UploadTTL string `json:"uploadTTL"`

	// ScratchMaxBytes caps the aggregate spool directory (UploadMaxBytes
	// only caps one upload); 0 means 8 GiB.
	ScratchMaxBytes int64 `json:"scratchMaxBytes"`

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

	// LiveIdleGrace stops a /stream encode once every client reading it has
	// been gone this long, as a Go duration string. Empty means 5s; "0"
	// lets it run on into the cache.
	LiveIdleGrace string `json:"liveIdleGrace"`

	// HLSIdleGrace stops an HLS variant's encode once nobody has fetched a
	// segment of it for this long, as a Go duration string. Empty means 60s;
	// "0" lets it run to the end of the stream.
	HLSIdleGrace string `json:"hlsIdleGrace"`

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
	{"WAXFLOW_CATALOG_DB", func(c *Config, v string) error { c.CatalogDB = v; return nil }},
	{"WAXFLOW_PID_LOOKUP_URL", func(c *Config, v string) error { c.PIDLookupURL = v; return nil }},
	{"WAXFLOW_PID_LOOKUP_KEY", func(c *Config, v string) error { c.PIDLookupKey = v; return nil }},
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
	{"WAXFLOW_SCRATCH_DIR", func(c *Config, v string) error { c.ScratchDir = v; return nil }},
	{"WAXFLOW_UPLOAD_MAX_BYTES", func(c *Config, v string) error { return parseInt64(v, &c.UploadMaxBytes) }},
	{"WAXFLOW_UPLOAD_TTL", func(c *Config, v string) error { c.UploadTTL = v; return nil }},
	{"WAXFLOW_SCRATCH_MAX_BYTES", func(c *Config, v string) error { return parseInt64(v, &c.ScratchMaxBytes) }},
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
	{"WAXFLOW_LIVE_IDLE_GRACE", func(c *Config, v string) error { c.LiveIdleGrace = v; return nil }},
	{"WAXFLOW_HLS_IDLE_GRACE", func(c *Config, v string) error { c.HLSIdleGrace = v; return nil }},
	{"WAXFLOW_DEMO", func(c *Config, v string) error { return parseBool(v, &c.Demo) }},
}

// EnvVars lists every environment variable Load reads, in table order, so a
// test suite can clear the ambient environment before setting its own.
func EnvVars() []string {
	names := make([]string, len(envVars))
	for i, ev := range envVars {
		names[i] = ev.name
	}
	return names
}

// Load resolves configuration from the JSON file at path overlaid with
// WAXFLOW_* environment variables (looked up via lookupEnv, typically
// os.LookupEnv). An empty path skips the file layer; a non-empty path must
// exist. Unknown JSON keys are rejected (strict decode). Errors carry
// waxerr.CodeInvalidRequest.
func Load(path string, lookupEnv func(string) (string, bool)) (Config, error) {
	var cfg Config

	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			code := waxerr.CodeInvalidRequest
			if errors.Is(err, fs.ErrNotExist) {
				code = waxerr.CodeNotFound
			}
			return cfg, waxerr.Wrap(code, "config file", err)
		}
		// Refused rather than decoded: the decoder reads a stray byte, or
		// half a surrogate pair, as U+FFFD, and a path would name another
		// folder, a secret another key.
		if err := source.CheckJSON(raw); err != nil {
			return cfg, waxerr.Wrap(waxerr.CodeInvalidRequest, fmt.Sprintf("config file %s", path), err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
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

	// Every path is read from text form, as a reference is, so a file or a
	// compose block that holds only UTF-8 can name a folder that is not.
	for _, f := range pathFields(&cfg) {
		path, err := source.ParseRef(*f.value)
		if err != nil {
			return cfg, waxerr.Wrap(waxerr.CodeInvalidRequest, f.name, err)
		}
		*f.value = string(path)
	}

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// pathField is one setting that names a place on disk.
type pathField struct {
	name  string
	value *string
}

// pathFields lists every path in c: the settings Load reads from text form.
// TestEveryStringFieldIsAPathOrNot holds a new string field to being listed
// here or named there as no path.
func pathFields(c *Config) []pathField {
	fields := []pathField{
		{"catalogDB", &c.CatalogDB}, {"dataDir", &c.DataDir}, {"cacheDir", &c.CacheDir},
		{"scratchDir", &c.ScratchDir}, {"tlsCert", &c.TLSCert}, {"tlsKey", &c.TLSKey},
	}
	for i := range c.Roots {
		fields = append(fields, pathField{fmt.Sprintf("root %q", c.Roots[i].Name), &c.Roots[i].Path})
	}
	return fields
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
	if c.SourceMaxBytes < 0 || c.CacheMaxBytes < 0 || c.LiveSlots < 0 || c.JobSlots < 0 ||
		c.UploadMaxBytes < 0 || c.ScratchMaxBytes < 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, "size and slot settings must be non-negative")
	}
	if _, err := c.ResolvedCacheMaxAge(); err != nil {
		return err
	}
	if _, err := c.ResolvedUploadTTL(); err != nil {
		return err
	}
	if _, err := c.ResolvedLiveIdleGrace(); err != nil {
		return err
	}
	if _, err := c.ResolvedHLSIdleGrace(); err != nil {
		return err
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return waxerr.New(waxerr.CodeInvalidRequest, "tlsCert and tlsKey must be set together")
	}
	if c.PIDLookupURL != "" {
		if _, err := source.ParsePIDLookupURL(c.PIDLookupURL); err != nil {
			return waxerr.Wrap(waxerr.CodeInvalidRequest, "pidLookupURL", err)
		}
		if c.CatalogDB != "" {
			return waxerr.New(waxerr.CodeInvalidRequest, "pid: sources come from one place: set pidLookupURL or catalogDB, not both")
		}
		if err := source.CheckPIDLookupKey(c.PIDLookupKey); err != nil {
			return waxerr.Wrap(waxerr.CodeInvalidRequest, "pidLookupKey", err)
		}
	} else if c.PIDLookupKey != "" {
		return waxerr.New(waxerr.CodeInvalidRequest, "pidLookupKey needs pidLookupURL")
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
	return parseDuration("cacheMaxAge", c.CacheMaxAge, 0, "720h")
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

// ResolvedUploadMaxBytes returns the per-upload cap, defaulted.
func (c Config) ResolvedUploadMaxBytes() int64 {
	if c.UploadMaxBytes == 0 {
		return DefaultUploadMaxBytes
	}
	return c.UploadMaxBytes
}

// ResolvedScratchMaxBytes returns the aggregate spool cap, defaulted.
func (c Config) ResolvedScratchMaxBytes() int64 {
	if c.ScratchMaxBytes == 0 {
		return DefaultScratchMaxBytes
	}
	return c.ScratchMaxBytes
}

// ResolvedUploadTTL parses UploadTTL; empty means the 1 hour default,
// an explicit "0" disables expiry.
func (c Config) ResolvedUploadTTL() (time.Duration, error) {
	return parseDuration("uploadTTL", c.UploadTTL, DefaultUploadTTL, "1h (0 never expires)")
}

// ResolvedLiveIdleGrace parses LiveIdleGrace; empty means the 5 s default,
// an explicit "0" never stops an unread encode.
func (c Config) ResolvedLiveIdleGrace() (time.Duration, error) {
	return parseDuration("liveIdleGrace", c.LiveIdleGrace, DefaultLiveIdleGrace, "5s (0 never stops)")
}

// ResolvedHLSIdleGrace parses HLSIdleGrace; empty means the 60 s default,
// an explicit "0" never stops an unfetched variant.
func (c Config) ResolvedHLSIdleGrace() (time.Duration, error) {
	return parseDuration("hlsIdleGrace", c.HLSIdleGrace, DefaultHLSIdleGrace, "60s (0 never stops)")
}

// parseDuration reads a duration setting: empty means def, and anything
// unparsable or negative is refused with a hint such as "1h (0 never
// expires)".
func parseDuration(key, v string, def time.Duration, hint string) (time.Duration, error) {
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("%s %q: want a non-negative Go duration like %s", key, v, hint))
	}
	return d, nil
}

// ResolvedScratchDir returns ScratchDir or the platform temp default.
func (c Config) ResolvedScratchDir() string {
	if c.ScratchDir != "" {
		return c.ScratchDir
	}
	return filepath.Join(os.TempDir(), "waxflow")
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
