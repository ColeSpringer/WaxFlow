// Package server is WaxFlow's HTTP service: the progressive streaming
// surface over the engine. Stdlib mux with Go 1.22 method patterns, no
// middleware framework, no /api/v1 (a schemaVersion field in JSON bodies
// instead), and the JSON error envelope {"error","code","schemaVersion"}
// shared across the Wax family.
//
// Construction wires the pieces; behavior lives in the internal packages
// (cache, sign, admission, flight, metrics) and the engine. The package
// is transport only: it owns parameter parsing, auth, the decision ladder
// dispatch, and header semantics, nothing sample-shaped.
package server

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/internal/admission"
	"github.com/colespringer/waxflow/internal/cache"
	"github.com/colespringer/waxflow/internal/config"
	"github.com/colespringer/waxflow/internal/flight"
	"github.com/colespringer/waxflow/internal/hls"
	"github.com/colespringer/waxflow/internal/jobs"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/metrics"
	"github.com/colespringer/waxflow/internal/sign"
	"github.com/colespringer/waxflow/internal/uploads"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// writeStallTimeout kills a session that cannot complete a single write
// for this long: the slow-client defense. Refreshed per chunk via
// http.ResponseController.
const writeStallTimeout = 60 * time.Second

// SigningKey is one HMAC key for signed playback URLs; the first
// configured key mints, every key verifies (rotation).
type SigningKey struct {
	ID     string
	Secret []byte
}

// Config configures a Server. The CLI resolves config-file defaults
// before construction; zero values here mean what each field documents.
type Config struct {
	// Addr is the address the daemon will listen on, used for the
	// fail-closed check (the server itself does not listen; the caller
	// owns the listener).
	Addr string

	// APIKeys are the control-API keys. Empty plus a non-loopback Addr
	// refuses to start unless AllowUnauthenticated is set.
	APIKeys              []string
	AllowUnauthenticated bool

	// MetricsKey additionally unlocks GET /metrics without a full API key.
	MetricsKey string

	// AllowedOrigins is the CORS allowlist for playback endpoints. "*"
	// allows any origin.
	AllowedOrigins []string

	// Resolver opens source references. Nil means no roots (every source
	// resolves not-found); the resolver flavor injects its own.
	Resolver source.Resolver

	// PIDSources advertises pid:<ULID> source support in /caps. The
	// resolver flavor sets it when a catalog is wired; whether pid refs
	// actually resolve is Resolver's business, this flag only keeps the
	// capability surface honest.
	PIDSources bool

	// SigningKeys enable signed playback URLs. Empty disables sig auth
	// and POST /sign.
	SigningKeys []SigningKey

	// CacheDir is the transcode cache location (required).
	CacheDir      string
	CacheMaxBytes int64
	CacheMaxAge   time.Duration
	// RingBytes bounds degradation/one-shot rings; 0 means 4 MiB.
	RingBytes int

	// JobsDir persists the async job store (dataDir/jobs); empty
	// disables the /jobs API.
	JobsDir string

	// UploadDir is the upload spool (scratchDir/uploads); empty disables
	// /uploads and upload: references. UploadMaxBytes caps one upload
	// (0: 2 GiB), ScratchMaxBytes the whole spool (0: 8 GiB), UploadTTL
	// evicts spooled uploads after creation (0: never).
	UploadDir       string
	UploadMaxBytes  int64
	ScratchMaxBytes int64
	UploadTTL       time.Duration

	// Meta is the metadata mapper: source tag/art/chapter/lyrics reads,
	// the live minimal-tag passthrough, tag-based gain resolution, and
	// the job post-pass. The implementation is internal (waxlabel stays
	// out of the depcheck-enforced public tree), so only the waxflow CLI
	// can construct one; embedders leave it nil, which disables metadata
	// passthrough and resolves tag-based gain to 0 dB.
	Meta meta.Mapper

	// LiveSlots and JobSlots size the admission pools; 0 means
	// max(1, NumCPU-1) and 2.
	LiveSlots int
	JobSlots  int

	// DefaultGain applies when a request has no gain= parameter: off,
	// track, album, or a +/-dB number. Empty means track.
	DefaultGain string

	// ResampleProfile is hq or fast; empty means hq.
	ResampleProfile string

	// PaceBurst and PaceFactor shape read-behind delivery: burst that
	// much audio, then cap at factor x realtime. PaceBurst 0 means 30 s;
	// PaceFactor 0 disables pacing (the CLI passes its resolved default).
	PaceBurst  time.Duration
	PaceFactor float64

	// Demo serves the browser test page at GET /demo.
	Demo bool

	// Version is the build version served by /version and /metrics.
	Version string

	// Logger receives request warnings and session lifecycle notes; nil
	// discards.
	Logger *slog.Logger
}

// Server is the WaxFlow HTTP handler. Construct with New, serve via
// ServeHTTP, stop with Close.
type Server struct {
	cfg      Config
	log      *slog.Logger
	eng      *waxflow.Engine
	resolver source.Resolver
	store    *cache.Store
	pools    *admission.Pools
	signer   *sign.Signer
	met      *metrics.Metrics
	mux      *http.ServeMux

	keyHashes      [][sha256.Size]byte
	metricsKeyHash [sha256.Size]byte
	hasMetricsKey  bool
	origins        map[string]bool
	anyOrigin      bool

	defaultGain gainSpec
	profile     resample.Profile

	uploads *uploads.Store // nil: uploads disabled
	jobs    *jobs.Runner   // nil: jobs disabled

	// metaCache holds per-identity metadata reads (see readMeta).
	metaCache metaCache

	fl     flight.Group[*cache.Entry]
	hlsMgr hls.Manager

	// baseCtx bounds pipeline goroutines: pipelines outlive their
	// requests (read-behind), not the server.
	baseCtx context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	closeOnce sync.Once
	closeErr  error
}

// New validates cfg and builds a Server. The fail-closed rule is enforced
// here: keyless on a non-loopback address requires AllowUnauthenticated.
func New(cfg Config) (*Server, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if len(cfg.APIKeys) == 0 && !cfg.AllowUnauthenticated && !LoopbackAddr(cfg.Addr) {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("server: refusing to run keyless on non-loopback %q; configure apiKeys or set allowUnauthenticated", cfg.Addr))
	}
	if cfg.CacheDir == "" {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "server: cacheDir is required")
	}

	// Zero-value defaults resolve through the same constants the config
	// layer owns, so a direct embedder and a CLI-configured daemon cannot
	// drift.
	if cfg.DefaultGain == "" {
		cfg.DefaultGain = config.DefaultGainMode
	}
	if cfg.PaceBurst == 0 {
		cfg.PaceBurst = config.DefaultPaceBurst
	}
	if cfg.RingBytes == 0 {
		cfg.RingBytes = cache.DefaultRingBytes
	}
	defaultGain, err := parseGain(cfg.DefaultGain, gainSpec{mode: gainTrack})
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInvalidRequest, "server: defaultGain", err)
	}
	profile, err := resample.ParseProfile(cfg.ResampleProfile)
	if err != nil {
		return nil, err
	}

	var signer *sign.Signer
	if len(cfg.SigningKeys) > 0 {
		keys := make([]sign.Key, len(cfg.SigningKeys))
		for i, k := range cfg.SigningKeys {
			keys[i] = sign.Key{ID: k.ID, Secret: k.Secret}
		}
		if signer, err = sign.New(keys); err != nil {
			return nil, err
		}
	}

	store, err := cache.Open(cfg.CacheDir, cache.Options{
		MaxBytes:  cfg.CacheMaxBytes,
		MaxAge:    cfg.CacheMaxAge,
		RingBytes: cfg.RingBytes,
		Logger:    log,
	})
	if err != nil {
		return nil, err
	}
	// The index sidecar (plan section 10) shares the cache volume but
	// not the entry lifecycle; losing it only costs index rebuilds, so
	// an unusable idx directory downgrades to no sidecar, not a refusal
	// to start.
	engineOpts := []waxflow.Option{waxflow.WithLogger(log)}
	if idx, err := cache.OpenIdx(filepath.Join(cfg.CacheDir, "idx"), 0); err != nil {
		log.Warn("index sidecar unavailable", "err", err)
	} else {
		engineOpts = append(engineOpts, waxflow.WithIndexCache(&idxCache{idx}))
	}

	resolver := cfg.Resolver
	if resolver == nil {
		if resolver, err = source.OpenRoots(nil, 0); err != nil {
			store.Close()
			return nil, err
		}
	}

	var uploadStore *uploads.Store
	if cfg.UploadDir != "" {
		maxUp := cfg.UploadMaxBytes
		if maxUp == 0 {
			maxUp = config.DefaultUploadMaxBytes
		}
		maxTotal := cfg.ScratchMaxBytes
		if maxTotal == 0 {
			maxTotal = config.DefaultScratchMaxBytes
		}
		uploadStore, err = uploads.Open(cfg.UploadDir, uploads.Options{
			MaxBytes:      maxUp,
			MaxTotalBytes: maxTotal,
			TTL:           cfg.UploadTTL,
			Logger:        log,
		})
		if err != nil {
			store.Close()
			return nil, err
		}
		resolver = uploadResolver{next: resolver, store: uploadStore}
	}

	liveSlots := cfg.LiveSlots
	if liveSlots == 0 {
		liveSlots = config.DefaultLiveSlots()
	}
	jobSlots := cfg.JobSlots
	if jobSlots == 0 {
		jobSlots = config.DefaultJobSlots
	}

	s := &Server{
		cfg:         cfg,
		log:         log,
		eng:         waxflow.New(engineOpts...),
		resolver:    resolver,
		store:       store,
		pools:       admission.New(liveSlots),
		signer:      signer,
		met:         &metrics.Metrics{},
		defaultGain: defaultGain,
		profile:     profile,
		uploads:     uploadStore,
	}
	s.baseCtx, s.cancel = context.WithCancel(context.Background())

	if cfg.JobsDir != "" {
		s.jobs, err = jobs.Open(jobs.Config{
			Dir:      cfg.JobsDir,
			Engine:   s.eng,
			Resolver: resolver,
			Meta:     cfg.Meta,
			Pools:    s.pools,
			ResolveGain: func(gain string, info *meta.Info) (float64, error) {
				g, err := parseGain(gain, s.defaultGain)
				if err != nil {
					return 0, err
				}
				return g.resolveDB(info), nil
			},
			Slots:   jobSlots,
			Profile: profile,
			Logger:  log,
		})
		if err != nil {
			if uploadStore != nil {
				uploadStore.Close()
			}
			store.Close()
			s.cancel()
			return nil, err
		}
	}

	for _, k := range cfg.APIKeys {
		s.keyHashes = append(s.keyHashes, sha256.Sum256([]byte(k)))
	}
	if cfg.MetricsKey != "" {
		s.metricsKeyHash = sha256.Sum256([]byte(cfg.MetricsKey))
		s.hasMetricsKey = true
	}
	s.origins = make(map[string]bool, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		if o == "*" {
			s.anyOrigin = true
			continue
		}
		s.origins[o] = true
	}

	s.routes()
	return s, nil
}

func (s *Server) routes() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", s.handlePing)
	mux.HandleFunc("GET /version", s.requireKey(s.handleVersion))
	mux.HandleFunc("GET /caps", s.requireKey(s.handleCaps))
	mux.HandleFunc("GET /probe", s.requireKey(s.handleProbe))
	mux.HandleFunc("POST /probe", s.requireKey(s.handleProbe))
	mux.HandleFunc("POST /sign", s.requireKey(s.handleSign))
	mux.HandleFunc("POST /transcode", s.requireKey(s.handleTranscode))
	mux.HandleFunc("GET /stream", s.handleStream)
	mux.HandleFunc("HEAD /stream", s.handleStream)
	mux.HandleFunc("OPTIONS /stream", s.handlePreflight)
	mux.HandleFunc("GET /hls/master.m3u8", s.handleHLSMaster)
	mux.HandleFunc("GET /hls/media.m3u8", s.handleHLSMedia)
	mux.HandleFunc("GET /hls/init.mp4", s.handleHLSInit)
	mux.HandleFunc("GET /hls/seg/{seg}", s.handleHLSSegment)
	mux.HandleFunc("OPTIONS /hls/", s.handlePreflight)
	if s.uploads != nil {
		mux.HandleFunc("POST /uploads", s.requireKey(s.handleUploadCreate))
		mux.HandleFunc("DELETE /uploads/{id}", s.requireKey(s.handleUploadDelete))
	}
	if s.jobs != nil {
		mux.HandleFunc("POST /jobs", s.requireKey(s.handleJobCreate))
		mux.HandleFunc("GET /jobs", s.requireKey(s.handleJobList))
		mux.HandleFunc("GET /jobs/{id}", s.requireKey(s.handleJobGet))
		mux.HandleFunc("DELETE /jobs/{id}", s.requireKey(s.handleJobDelete))
		mux.HandleFunc("GET /jobs/{id}/events", s.handleJobEvents)
		mux.HandleFunc("GET /jobs/{id}/result", s.handleJobResult)
	}
	mux.HandleFunc("GET /art", s.handleArt)
	mux.HandleFunc("HEAD /art", s.handleArt)
	mux.HandleFunc("GET /lyrics", s.handleLyrics)
	mux.HandleFunc("HEAD /lyrics", s.handleLyrics)
	mux.HandleFunc("GET /cache/stats", s.requireKey(s.handleCacheStats))
	mux.HandleFunc("POST /cache/gc", s.requireKey(s.handleCacheGC))
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	if s.cfg.Demo {
		mux.HandleFunc("GET /demo", s.handleDemo)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.writeEnvelope(w, http.StatusNotFound, waxerr.CodeNotFound,
			fmt.Sprintf("no such endpoint: %s %s", r.Method, r.URL.Path))
	})
	s.mux = mux
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// Close stops pipeline goroutines, the job workers, the upload janitor,
// and the cache janitor. Call after the http.Server has drained. Running
// jobs stay in their persisted running state, which the next start
// requeues from zero (the restart contract). Close is idempotent: a
// restart-managing caller and a deferred cleanup may both call it.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		s.wg.Wait()
		if s.jobs != nil {
			s.jobs.Close()
		}
		if s.uploads != nil {
			s.uploads.Close()
		}
		s.closeErr = s.store.Close()
	})
	return s.closeErr
}

func (s *Server) handlePing(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "schemaVersion": 1})
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, VersionInfo{SchemaVersion: 1, Version: s.cfg.Version})
}

// LoopbackAddr reports whether addr binds only loopback: the fail-closed
// predicate, exported so the CLI's loopback-only debug listener enforces
// the identical notion of loopback.
// The empty address means the config default (loopback); an empty host
// means wildcard.
func LoopbackAddr(addr string) bool {
	if addr == "" {
		return true
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
