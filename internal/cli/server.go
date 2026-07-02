package cli

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxflow/internal/config"
	"github.com/colespringer/waxflow/waxerr"
)

// The skeleton daemon serves only GET /ping. The real server package
// (auth, signing, streaming, cache, admission) will replace the handler.
// The daemon lifecycle here (config resolution, slog to stdout, graceful
// SIGTERM drain) is already the permanent shape.

// envelope is the JSON error body shared server<->client across the family.
type envelope struct {
	Error         string      `json:"error"`
	Code          waxerr.Code `json:"code"`
	SchemaVersion int         `json:"schemaVersion"`
}

func newServerCmd(version string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the WaxFlow daemon (skeleton: liveness only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig(cmd)
			if err != nil {
				return err
			}
			// Daemon convention: logs to stdout.
			logger, err := newLogger(cmd.OutOrStdout(), cfg)
			if err != nil {
				return err
			}
			ln, err := net.Listen("tcp", cfg.ResolvedAddr())
			if err != nil {
				return waxerr.Wrap(waxerr.CodeInternal, "listen", err)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return serve(ctx, ln, logger, version)
		},
	}
	cmd.Flags().String("addr", "", "listen address host:port (default "+config.DefaultAddr+")")
	return cmd
}

func newServeMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "schemaVersion": 1})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, envelope{
			Error:         "no such endpoint (this skeleton daemon serves /ping only)",
			Code:          waxerr.CodeNotFound,
			SchemaVersion: 1,
		})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Encode errors mean the client went away; nothing useful to do.
	_ = json.NewEncoder(w).Encode(v)
}

// serve runs the daemon on ln until ctx is canceled, then drains gracefully.
// The caller must eventually cancel ctx (the shutdown goroutine waits on it).
func serve(ctx context.Context, ln net.Listener, logger *slog.Logger, version string) error {
	srv := &http.Server{
		Handler:           newServeMux(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       90 * time.Second,
		// WriteTimeout deliberately unset: long-lived streams will refresh
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
		"addr", ln.Addr().String(), "version", version, "skeleton", "/ping only")
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return waxerr.Wrap(waxerr.CodeInternal, "serve", err)
	}
	if err := <-shutdownErr; err != nil {
		return waxerr.Wrap(waxerr.CodeInternal, "shutdown", err)
	}
	logger.Info("waxflow daemon stopped")
	return nil
}
