package cli

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow/internal/config"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// doctorEnv points every directory the doctor touches at t.TempDir so a
// test run never writes outside the sandbox.
func doctorEnv(t *testing.T) (rootDir string) {
	t.Helper()
	rootDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDir, "a.wav"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAXFLOW_ROOTS", "lib="+rootDir)
	t.Setenv("WAXFLOW_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	t.Setenv("WAXFLOW_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("WAXFLOW_SCRATCH_DIR", filepath.Join(t.TempDir(), "scratch"))
	return rootDir
}

func TestDoctorHealthy(t *testing.T) {
	doctorEnv(t)
	code, out, stderr := run(t, "doctor", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	var report struct {
		SchemaVersion int  `json:"schemaVersion"`
		Healthy       bool `json:"healthy"`
		Checks        []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decoding doctor JSON: %v\n%s", err, out)
	}
	if !report.Healthy || report.SchemaVersion != 1 {
		t.Fatalf("healthy = %v, schemaVersion = %d; want true, 1", report.Healthy, report.SchemaVersion)
	}
	status := map[string]string{}
	for _, c := range report.Checks {
		status[c.Name] = c.Status
	}
	for _, name := range []string{"config", "root:lib", "cache", "data", "scratch", "bench:opus", "bench:flac", "ffmpeg"} {
		if got := status[name]; got != "ok" {
			t.Errorf("check %s = %q, want ok", name, got)
		}
	}
	if got := status["pid"]; got != "skip" {
		t.Errorf("check pid = %q, want skip (no pid lookup or catalogDB)", got)
	}
	if got := status["tls"]; got != "skip" {
		t.Errorf("check tls = %q, want skip (no pair configured)", got)
	}
	if got := status["exposure"]; got != "ok" {
		t.Errorf("check exposure = %q, want ok (the default address is loopback)", got)
	}
}

// TestDoctorProbesThePIDLookup asks the configured lookup host for the probe
// id: a host that answers at all passes, one that refuses or is down fails.
func TestDoctorProbesThePIDLookup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		closed bool
		want   string
		detail string
	}{
		{name: "a 404 is a reachable host", status: http.StatusNotFound, want: "ok", detail: "answers lookups"},
		{name: "a 401 is a refused key", status: http.StatusUnauthorized, want: "fail", detail: "401"},
		{name: "a closed host is down", closed: true, want: "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doctorEnv(t)
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			t.Setenv("WAXFLOW_PID_LOOKUP_URL", host.URL+"/pid")
			if tc.closed {
				host.Close()
			} else {
				defer host.Close()
			}
			code, out, stderr := run(t, "doctor", "--json")
			var report struct {
				Checks []struct {
					Name, Status, Detail string
				} `json:"checks"`
			}
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatalf("decoding doctor JSON: %v\n%s\n%s", err, out, stderr)
			}
			var found bool
			for _, c := range report.Checks {
				if c.Name != "pid" {
					continue
				}
				found = true
				if c.Status != tc.want || !strings.Contains(c.Detail, tc.detail) {
					t.Errorf("check pid = %s (%s), want %s with %q", c.Status, c.Detail, tc.want, tc.detail)
				}
			}
			if !found {
				t.Fatalf("no pid check in the report:\n%s", out)
			}
			if (code != 0) != (tc.want == "fail") {
				t.Errorf("exit = %d with the pid check %s", code, tc.want)
			}
		})
	}
}

func TestDoctorFailsOnMissingRoot(t *testing.T) {
	doctorEnv(t)
	t.Setenv("WAXFLOW_ROOTS", "lib=/nonexistent/waxflow-doctor-test")
	code, out, _ := run(t, "doctor")
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero\n%s", out)
	}
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "root:lib") {
		t.Errorf("output missing FAIL root:lib line:\n%s", out)
	}
}

func TestDoctorFailsOnCatalogDBWithoutFlavor(t *testing.T) {
	doctorEnv(t)
	t.Setenv("WAXFLOW_CATALOG_DB", filepath.Join(t.TempDir(), "waxbin.db"))
	code, out, _ := run(t, "doctor")
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero\n%s", out)
	}
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "pid") {
		t.Errorf("output missing FAIL pid line:\n%s", out)
	}
	if !strings.Contains(out, "catalog resolver") {
		t.Errorf("catalog failure should point at the missing catalog resolver:\n%s", out)
	}
}

func TestDoctorWarnsOnEmptyRoot(t *testing.T) {
	doctorEnv(t)
	t.Setenv("WAXFLOW_ROOTS", "lib="+t.TempDir())
	code, out, _ := run(t, "doctor")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (warn is not fail)\n%s", code, out)
	}
	if !strings.Contains(out, "warn") || !strings.Contains(out, "empty") {
		t.Errorf("output missing warn empty-root line:\n%s", out)
	}
}

func TestDoctorBenchUsesConfiguredProfile(t *testing.T) {
	doctorEnv(t)
	t.Setenv("WAXFLOW_RESAMPLE_PROFILE", "fast")
	code, out, stderr := run(t, "doctor")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	// The plumbing cannot be observed through the ok detail (the note
	// only appears under 2x), so at minimum the bench must run and pass
	// under the fast profile; the note-text split is covered by reading
	// benchChecks with a warn on a slow box.
	if !strings.Contains(out, "bench:opus") || !strings.Contains(out, "x realtime") {
		t.Errorf("bench line missing under fast profile:\n%s", out)
	}
}

// TestDoctorProbesTheHostNotTheFlavor: the pid check asks the configured host
// itself, so a Flavor that would answer the probe cannot hide a host that is
// down.
func TestDoctorProbesTheHostNotTheFlavor(t *testing.T) {
	root := doctorEnv(t)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Setenv("WAXFLOW_PID_LOOKUP_URL", host.URL+"/pid")
	host.Close()
	flavor := Flavor{
		Name: "answers-everything",
		OpenResolver: func(_ context.Context, o ResolverOptions) (source.Resolver, io.Closer, error) {
			return fakePIDResolver{next: o.Next, path: filepath.Join(root, "a.wav")}, nil, nil
		},
	}
	var out, errOut strings.Builder
	code := ExecuteFlavor("test", []string{"doctor", "--json"}, &out, &errOut, flavor)
	var report struct {
		Checks []struct{ Name, Status string } `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out.String()), &report); err != nil {
		t.Fatalf("decoding doctor JSON: %v\n%s\n%s", err, out.String(), errOut.String())
	}
	for _, c := range report.Checks {
		if c.Name == "pid" && c.Status != "fail" {
			t.Fatalf("pid check = %s with the host down, want fail (exit %d)", c.Status, code)
		}
	}
	if code == 0 {
		t.Fatal("exit = 0 with the lookup host down")
	}
}

// healthyConfig is a configuration a daemon starts under and doctor passes,
// with every directory inside the test's sandbox.
func healthyConfig(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.wav"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return config.Config{
		Roots:      []config.Root{{Name: "lib", Path: root}},
		CacheDir:   filepath.Join(t.TempDir(), "cache"),
		DataDir:    filepath.Join(t.TempDir(), "data"),
		ScratchDir: filepath.Join(t.TempDir(), "scratch"),
	}
}

// writeConfig writes cfg as a file for --config to name.
func writeConfig(t *testing.T, cfg config.Config) string {
	t.Helper()
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "waxflow.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// daemonStartErr runs what `waxflow server` runs between reading its
// configuration and listening, with nothing validating cfg first, and
// returns the first refusal.
func daemonStartErr(t *testing.T, cfg config.Config) error {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	srvCfg, cleanup, err := buildServerConfig(t.Context(), cfg, "", "test", logger, Flavor{})
	if err != nil {
		return err
	}
	defer cleanup()
	srv, err := server.New(srvCfg)
	if err != nil {
		return err
	}
	defer srv.Close()
	stop, err := startDebugListener(cfg, logger)
	if err != nil {
		return err
	}
	stop()
	return nil
}

// configRefusals is one bad value per refusal a configuration key has.
// TestEveryConfigKeyIsClassified holds every key to having a row here or a
// reason it needs none.
var configRefusals = []struct {
	key  string // the key at fault, as the file spells it
	name string
	bad  func(*config.Config)
	// daemonOnly: a rule of the daemon's start, not of the key's syntax, so
	// a local command runs under this configuration.
	daemonOnly bool
	// start: a start-up step past the configuration refuses it as well,
	// which is what an embedder building server.Config by hand meets.
	start bool
}{
	{key: "addr", name: "no port", bad: func(c *config.Config) { c.Addr = "nope" }},
	{key: "addr", name: "keyless on every interface", bad: func(c *config.Config) { c.Addr = "0.0.0.0:4418" },
		daemonOnly: true, start: true},
	{key: "logLevel", name: "no such level", bad: func(c *config.Config) { c.LogLevel = "loud" }},
	{key: "roots", name: "a name with a slash", bad: func(c *config.Config) { c.Roots[0].Name = "a/b" }},
	{key: "catalogDB", name: "beside a lookup host", bad: func(c *config.Config) {
		c.CatalogDB, c.PIDLookupURL = "/x/waxbin.db", "http://deck/pid"
	}},
	{key: "pidLookupURL", name: "no scheme", bad: func(c *config.Config) { c.PIDLookupURL = "deck/pid" }},
	{key: "pidLookupKey", name: "no host to send it to", bad: func(c *config.Config) { c.PIDLookupKey = "k" }},
	{key: "apiKeys", name: "an empty key", bad: func(c *config.Config) { c.APIKeys = []string{"k", ""} }, start: true},
	{key: "apiKeys", name: "a key with a space at its end", bad: func(c *config.Config) { c.APIKeys = []string{"k "} }, start: true},
	{key: "sourceMaxBytes", name: "negative", bad: func(c *config.Config) { c.SourceMaxBytes = -1 }},
	{key: "metricsKey", name: "a space at its end", bad: func(c *config.Config) { c.MetricsKey = "m " }, start: true},
	{key: "signingSecret", name: "not hex", bad: func(c *config.Config) { c.SigningSecret = "a:zz" }, start: true},
	{key: "signingSecret", name: "one id twice", bad: func(c *config.Config) { c.SigningSecret = "a:00,a:01" }, start: true},
	{key: "cacheMaxBytes", name: "negative", bad: func(c *config.Config) { c.CacheMaxBytes = -1 }},
	{key: "cacheMaxAge", name: "no duration", bad: func(c *config.Config) { c.CacheMaxAge = "soon" }},
	{key: "uploadMaxBytes", name: "negative", bad: func(c *config.Config) { c.UploadMaxBytes = -1 }},
	{key: "uploadTTL", name: "no duration", bad: func(c *config.Config) { c.UploadTTL = "soon" }},
	{key: "scratchMaxBytes", name: "negative", bad: func(c *config.Config) { c.ScratchMaxBytes = -1 }},
	{key: "liveSlots", name: "negative", bad: func(c *config.Config) { c.LiveSlots = -1 }},
	{key: "jobSlots", name: "negative", bad: func(c *config.Config) { c.JobSlots = -1 }},
	{key: "defaultGain", name: "no mode and no number", bad: func(c *config.Config) { c.DefaultGain = "loud" }, start: true},
	{key: "resampleProfile", name: "no such profile", bad: func(c *config.Config) { c.ResampleProfile = "best" }},
	{key: "tlsCert", name: "without its key", bad: func(c *config.Config) { c.TLSCert = "cert.pem" }},
	{key: "tlsKey", name: "without its certificate", bad: func(c *config.Config) { c.TLSKey = "key.pem" }},
	{key: "debugAddr", name: "no port", bad: func(c *config.Config) { c.DebugAddr = "nope" }},
	{key: "debugAddr", name: "every interface", bad: func(c *config.Config) { c.DebugAddr = "0.0.0.0:6060" },
		daemonOnly: true, start: true},
	{key: "paceBurstSeconds", name: "negative", bad: func(c *config.Config) { c.PaceBurstSeconds = -1 }},
	{key: "paceFactor", name: "negative", bad: func(c *config.Config) { f := -1.0; c.PaceFactor = &f }},
	{key: "liveIdleGrace", name: "no duration", bad: func(c *config.Config) { c.LiveIdleGrace = "soon" }},
	{key: "hlsIdleGrace", name: "no duration", bad: func(c *config.Config) { c.HLSIdleGrace = "soon" }},
}

// TestDoctorRefusesWhatTheDaemonRefuses: a configuration a daemon would not
// start under fails config.Validate or config.CheckDaemon, whichever holds
// its rule, and fails doctor, with the code the start itself gives. Doctor
// used to pass a defaultGain, a signing
// secret, a keyless address and a debug address the daemon then refused.
func TestDoctorRefusesWhatTheDaemonRefuses(t *testing.T) {
	healthy := healthyConfig(t)
	if err := cmp.Or(healthy.Validate(), healthy.CheckDaemon()); err != nil {
		t.Fatalf("the healthy configuration is refused: %v", err)
	}
	if err := daemonStartErr(t, healthy); err != nil {
		t.Fatalf("a daemon does not start under the healthy configuration: %v", err)
	}
	if code, out, stderr := run(t, "doctor", "--config", writeConfig(t, healthy)); code != 0 {
		t.Fatalf("doctor fails the healthy configuration (exit %d):\n%s%s", code, out, stderr)
	}

	const want = waxerr.CodeInvalidRequest
	for _, row := range configRefusals {
		t.Run(row.key+"/"+row.name, func(t *testing.T) {
			cfg := healthyConfig(t)
			row.bad(&cfg)
			// One of the two holds each rule: a key's syntax is Validate's,
			// which every command runs, and a listener's is CheckDaemon's.
			syntax, daemon := cfg.Validate(), cfg.CheckDaemon()
			if row.daemonOnly {
				if syntax != nil {
					t.Errorf("Validate = %v; a local command must not be refused over a daemon's listener", syntax)
				}
				if waxerr.CodeOf(daemon) != want {
					t.Errorf("CheckDaemon = %v, want an %s refusal", daemon, want)
				}
			} else if waxerr.CodeOf(syntax) != want {
				t.Errorf("Validate = %v, want an %s refusal", syntax, want)
			}
			if row.start {
				if err := daemonStartErr(t, cfg); waxerr.CodeOf(err) != want {
					t.Errorf("a daemon start with nothing validating first = %v, want an %s refusal", err, want)
				}
			}
			code, out, stderr := run(t, "doctor", "--config", writeConfig(t, cfg))
			if code != want.ExitCode() {
				t.Errorf("doctor exit = %d, want %d:\n%s%s", code, want.ExitCode(), out, stderr)
			}
			if row.daemonOnly && !strings.Contains(out, "FAIL  config") {
				t.Errorf("doctor's report does not fail its config row:\n%s", out)
			}
		})
	}
}

// TestServerRefusesADaemonRuleBeforeItCreatesAnything: the two rules of a
// daemon's start are checked when the configuration is read. server.New and
// the debug listener refuse them too, but only after the data, cache and
// scratch directories and a signing secret have been made.
func TestServerRefusesADaemonRuleBeforeItCreatesAnything(t *testing.T) {
	for name, bad := range map[string]func(c *config.Config){
		"keyless on every interface": func(c *config.Config) { c.Addr = "0.0.0.0:4418" },
		"a debug listener on every interface": func(c *config.Config) {
			c.Addr, c.DebugAddr = "127.0.0.1:0", "0.0.0.0:6060"
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := healthyConfig(t)
			bad(&cfg)
			code, out, stderr := run(t, "server", "--config", writeConfig(t, cfg))
			if want := waxerr.CodeInvalidRequest.ExitCode(); code != want {
				t.Fatalf("exit = %d, want %d:\n%s%s", code, want, out, stderr)
			}
			for _, dir := range []string{cfg.DataDir, cfg.CacheDir, cfg.ScratchDir} {
				if _, err := os.Stat(dir); err == nil {
					t.Errorf("%s was created before the refusal", dir)
				}
			}
		})
	}
}

// TestEveryConfigKeyIsClassified: every configuration key has a bad value
// in configRefusals, which doctor is then held to refusing, or is listed
// here with why Validate has nothing to refuse in it.
func TestEveryConfigKeyIsClassified(t *testing.T) {
	acceptsAnyValue := map[string]string{
		"allowUnauthenticated": "a bool; false with no key on an open address is the addr row",
		"allowedOrigins":       "compared to a request's Origin as a string; one no browser sends allows nothing",
		"dataDir":              "a path, read from its text form when the file loads; doctor's data row writes to it",
		"cacheDir":             "a path, read from its text form when the file loads; doctor's cache row writes to it",
		"scratchDir":           "a path, read from its text form when the file loads; doctor's scratch row writes to it",
		"demo":                 "a bool",
	}
	refused := map[string]bool{}
	for _, row := range configRefusals {
		refused[row.key] = true
	}
	keys := map[string]bool{}
	for field := range reflect.TypeFor[config.Config]().Fields() {
		key, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		keys[key] = true
		switch {
		case refused[key] && acceptsAnyValue[key] != "":
			t.Errorf("%s has a refusal row and a reason it needs none; drop one", key)
		case !refused[key] && acceptsAnyValue[key] == "":
			t.Errorf("%s is in neither table: give configRefusals a value of it a daemon refuses, "+
				"or say here why there is none", key)
		}
	}
	for key := range refused {
		if !keys[key] {
			t.Errorf("configRefusals names %s, which is no configuration key", key)
		}
	}
	for key := range acceptsAnyValue {
		if !keys[key] {
			t.Errorf("acceptsAnyValue names %s, which is no configuration key", key)
		}
	}
}

// TestLocalCommandsRefuseABadConfigField: config.Load validates, so a
// command that loads the configuration refuses a malformed key by name,
// as it always refused a malformed addr. A probe of a plain path loads no
// configuration and runs.
func TestLocalCommandsRefuseABadConfigField(t *testing.T) {
	src := filepath.Join(t.TempDir(), "in.wav")
	hotWAV(t, src, false)
	for _, tc := range []struct {
		key string
		bad func(*config.Config)
	}{
		{"defaultGain", func(c *config.Config) { c.DefaultGain = "loud" }},
		{"signingSecret", func(c *config.Config) { c.SigningSecret = "a:zz" }},
		{"apiKeys", func(c *config.Config) { c.APIKeys = []string{""} }},
		// The guard: refused before the three above were.
		{"addr", func(c *config.Config) { c.Addr = "nope" }},
	} {
		t.Run(tc.key, func(t *testing.T) {
			cfg := healthyConfig(t)
			tc.bad(&cfg)
			path := writeConfig(t, cfg)
			for _, args := range [][]string{
				{"transcode", "--config", path, src, filepath.Join(t.TempDir(), "out.flac")},
				{"sign", "--config", path, "--src", "lib/a.wav"},
			} {
				code, _, stderr := run(t, args...)
				if want := waxerr.CodeInvalidRequest.ExitCode(); code != want {
					t.Errorf("%s: exit = %d, want %d; stderr: %s", args[0], code, want, stderr)
				}
				if !strings.Contains(stderr, tc.key) {
					t.Errorf("%s: the refusal does not name %s: %s", args[0], tc.key, stderr)
				}
			}
			if code, _, stderr := run(t, "probe", "--config", path, src); code != 0 {
				t.Errorf("probe of a plain path: exit = %d, want 0; stderr: %s", code, stderr)
			}
		})
	}
}

// doctorStatus runs doctor --json under cfg and returns its exit code and
// each check's status and detail by name.
func doctorStatus(t *testing.T, cfg config.Config) (code int, status, detail map[string]string) {
	t.Helper()
	code, out, stderr := run(t, "doctor", "--json", "--config", writeConfig(t, cfg))
	var report struct {
		Checks []struct{ Name, Status, Detail string } `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decoding doctor JSON: %v\n%s\n%s", err, out, stderr)
	}
	status, detail = map[string]string{}, map[string]string{}
	for _, c := range report.Checks {
		status[c.Name], detail[c.Name] = c.Status, c.Detail
	}
	return code, status, detail
}

// writeTLSPair writes a self-signed certificate and its key under dir.
func writeTLSPair(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "waxflow doctor test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	for path, block := range map[string]*pem.Block{
		certPath: {Type: "CERTIFICATE", Bytes: der},
		keyPath:  {Type: "EC PRIVATE KEY", Bytes: keyDER},
	} {
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return certPath, keyPath
}

// TestDoctorFailsOnAnUnloadableTLSPair: a configured pair the daemon could
// not serve with fails here, where the daemon would fail after it had
// started listening.
func TestDoctorFailsOnAnUnloadableTLSPair(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeTLSPair(t, dir)

	cfg := healthyConfig(t)
	cfg.TLSCert, cfg.TLSKey = cert, key
	if code, status, detail := doctorStatus(t, cfg); code != 0 || status["tls"] != "ok" {
		t.Fatalf("a loadable pair: exit %d, tls = %s (%s), want 0 and ok", code, status["tls"], detail["tls"])
	}

	for name, breakPair := range map[string]func(c *config.Config){
		"a certificate that is no certificate": func(c *config.Config) {
			c.TLSCert = filepath.Join(dir, "garbage.pem")
			if err := os.WriteFile(c.TLSCert, []byte("not a certificate"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"a key file that is missing": func(c *config.Config) { c.TLSKey = filepath.Join(dir, "absent.pem") },
		"a key that is another certificate's": func(c *config.Config) {
			_, c.TLSKey = writeTLSPair(t, t.TempDir())
		},
	} {
		cfg := healthyConfig(t)
		cfg.TLSCert, cfg.TLSKey = cert, key
		breakPair(&cfg)
		code, status, detail := doctorStatus(t, cfg)
		if status["tls"] != "fail" || code == 0 {
			t.Errorf("%s: exit %d, tls = %s (%s), want a failure", name, code, status["tls"], detail["tls"])
		}
	}
}

// TestDoctorWarnsOnPlainNonLoopback: an address other machines reach,
// served without TLS, is a warning (ADR-0007): a proxy may terminate TLS
// in front, which doctor cannot see.
func TestDoctorWarnsOnPlainNonLoopback(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(c *config.Config)
		want string
	}{
		{"loopback, plain", func(c *config.Config) { c.Addr = "127.0.0.1:4418" }, "ok"},
		{"every interface, plain", func(c *config.Config) { c.Addr = "0.0.0.0:4418" }, "warn"},
		{"every interface, native TLS", func(c *config.Config) {
			c.Addr = "0.0.0.0:4418"
			c.TLSCert, c.TLSKey = writeTLSPair(t, t.TempDir())
		}, "ok"},
	} {
		cfg := healthyConfig(t)
		cfg.APIKeys = []string{"k"}
		tc.set(&cfg)
		code, status, detail := doctorStatus(t, cfg)
		if status["exposure"] != tc.want {
			t.Errorf("%s: exposure = %s (%s), want %s", tc.name, status["exposure"], detail["exposure"], tc.want)
		}
		if code != 0 {
			t.Errorf("%s: exit = %d, want 0 (a warning is not a failure)", tc.name, code)
		}
	}
}
