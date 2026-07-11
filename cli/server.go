package cli

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/internal/config"
	"github.com/colespringer/waxflow/internal/sign"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

func newServerCmd(version string, flavor Flavor) *cobra.Command {
	var demo bool
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the WaxFlow daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig(cmd)
			if err != nil {
				return err
			}
			if demo {
				cfg.Demo = true
			}
			// Daemon convention: logs to stdout.
			logger, err := newLogger(cmd.OutOrStdout(), cfg)
			if err != nil {
				return err
			}
			srvCfg, cleanup, err := buildServerConfig(cfg, version, logger, flavor)
			if err != nil {
				return err
			}
			defer cleanup()
			srv, err := server.New(srvCfg)
			if err != nil {
				return err
			}
			defer srv.Close()

			ln, err := net.Listen("tcp", cfg.ResolvedAddr())
			if err != nil {
				return waxerr.Wrap(waxerr.CodeInternal, "listen", err)
			}
			stopDebug, err := startDebugListener(cfg, logger)
			if err != nil {
				ln.Close()
				return err
			}
			defer stopDebug()

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return serve(ctx, ln, srv, cfg, logger, version)
		},
	}
	cmd.Flags().String("addr", "", "listen address host:port (default "+config.DefaultAddr+")")
	cmd.Flags().BoolVar(&demo, "demo", false, "serve the browser test page at /demo (dev mode)")
	return cmd
}

// buildServerConfig maps the file/env/flag configuration onto the server
// package's dependencies: the resolver chain (roots plus the flavor's
// schemes), signing keys (auto-generated into dataDir on first run), and
// resolved directories.
func buildServerConfig(cfg config.Config, version string, logger *slog.Logger, flavor Flavor) (server.Config, func(), error) {
	nop := func() {}
	dataDir, err := cfg.ResolvedDataDir()
	if err != nil {
		return server.Config{}, nop, err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return server.Config{}, nop, waxerr.Wrap(waxerr.CodeOutputUnwritable, "creating dataDir", err)
	}
	cacheDir, err := cfg.ResolvedCacheDir()
	if err != nil {
		return server.Config{}, nop, err
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return server.Config{}, nop, waxerr.Wrap(waxerr.CodeOutputUnwritable, "creating cacheDir", err)
	}

	keys, err := sign.ResolveKeys(cfg.SigningSecret, dataDir)
	if err != nil {
		return server.Config{}, nop, err
	}
	signingKeys := make([]server.SigningKey, len(keys))
	for i, k := range keys {
		signingKeys[i] = server.SigningKey{ID: k.ID, Secret: k.Secret}
	}

	resolver, closeResolver, err := flavor.openResolver(cfg, logger, true)
	if err != nil {
		return server.Config{}, nop, err
	}
	maxAge, err := cfg.ResolvedCacheMaxAge()
	if err != nil {
		closeResolver()
		return server.Config{}, nop, err
	}
	uploadTTL, err := cfg.ResolvedUploadTTL()
	if err != nil {
		closeResolver()
		return server.Config{}, nop, err
	}
	scratchDir := cfg.ResolvedScratchDir()
	if err := os.MkdirAll(scratchDir, 0o700); err != nil {
		closeResolver()
		return server.Config{}, nop, waxerr.Wrap(waxerr.CodeOutputUnwritable, "creating scratchDir", err)
	}

	return server.Config{
		Addr:                 cfg.ResolvedAddr(),
		APIKeys:              cfg.APIKeys,
		AllowUnauthenticated: cfg.AllowUnauthenticated,
		MetricsKey:           cfg.MetricsKey,
		AllowedOrigins:       cfg.AllowedOrigins,
		Resolver:             resolver,
		PIDSources:           cfg.CatalogDB != "",
		SigningKeys:          signingKeys,
		CacheDir:             cacheDir,
		CacheMaxBytes:        cfg.ResolvedCacheMaxBytes(),
		CacheMaxAge:          maxAge,
		JobsDir:              filepath.Join(dataDir, "jobs"),
		UploadDir:            filepath.Join(scratchDir, "uploads"),
		UploadMaxBytes:       cfg.ResolvedUploadMaxBytes(),
		ScratchMaxBytes:      cfg.ResolvedScratchMaxBytes(),
		UploadTTL:            uploadTTL,
		Meta:                 label.New(),
		LiveSlots:            cfg.ResolvedLiveSlots(),
		JobSlots:             cfg.ResolvedJobSlots(),
		DefaultGain:          cfg.ResolvedDefaultGain(),
		ResampleProfile:      cfg.ResolvedResampleProfile(),
		PaceBurst:            cfg.ResolvedPaceBurst(),
		PaceFactor:           cfg.ResolvedPaceFactor(),
		Demo:                 cfg.Demo,
		Version:              version,
		Logger:               logger,
	}, closeResolver, nil
}

func configRoots(cfg config.Config) []source.Root {
	roots := make([]source.Root, len(cfg.Roots))
	for i, r := range cfg.Roots {
		roots[i] = source.Root{Name: r.Name, Path: r.Path}
	}
	return roots
}

// startDebugListener serves pprof on the loopback-only debugAddr: live
// profiles from real deployments, never exposed.
func startDebugListener(cfg config.Config, logger *slog.Logger) (func(), error) {
	if cfg.DebugAddr == "" {
		return func() {}, nil
	}
	// The same loopback notion as the fail-closed rule, one predicate.
	if !server.LoopbackAddr(cfg.DebugAddr) {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "debugAddr must be loopback-only")
	}
	dmux := http.NewServeMux()
	dmux.HandleFunc("/debug/pprof/", pprof.Index)
	dmux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	dmux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	dmux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	dmux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	ln, err := net.Listen("tcp", cfg.DebugAddr)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInternal, "debug listen", err)
	}
	dsrv := &http.Server{Handler: dmux, ReadHeaderTimeout: 5 * time.Second}
	go dsrv.Serve(ln)
	logger.Info("debug listener up", "addr", ln.Addr().String())
	return func() { dsrv.Close() }, nil
}

// serve runs the daemon on ln until ctx is canceled, then drains
// gracefully. TLS engages when both tlsCert and tlsKey are configured
// (ADR-0007); otherwise document the terminating reverse proxy.
func serve(ctx context.Context, ln net.Listener, handler http.Handler, cfg config.Config, logger *slog.Logger, version string) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       90 * time.Second,
		// WriteTimeout deliberately unset: long-lived streams refresh
		// per-chunk write deadlines via http.ResponseController instead.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	shutdownErr := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownErr <- srv.Shutdown(shutCtx)
	}()
	logger.Info("waxflow daemon listening",
		"addr", ln.Addr().String(), "version", version, "tls", cfg.TLSCert != "")
	var err error
	if cfg.TLSCert != "" {
		err = srv.ServeTLS(ln, cfg.TLSCert, cfg.TLSKey)
	} else {
		err = srv.Serve(ln)
	}
	if !errors.Is(err, http.ErrServerClosed) {
		return waxerr.Wrap(waxerr.CodeInternal, "serve", err)
	}
	if err := <-shutdownErr; err != nil {
		return waxerr.Wrap(waxerr.CodeInternal, "shutdown", err)
	}
	logger.Info("waxflow daemon stopped")
	return nil
}
