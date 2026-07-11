package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxflow/internal/config"
	"github.com/colespringer/waxflow/waxerr"
)

// newPingCmd probes a running daemon's GET /ping. It is the container
// HEALTHCHECK command, so it must stay fast and side-effect free.
func newPingCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "ping",
		Short: "Check liveness of a running waxflow daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig(cmd)
			if err != nil {
				return err
			}
			url := "http://" + dialAddr(cfg.ResolvedAddr()) + "/ping"
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeInvalidRequest, "building ping request", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeInternal, "daemon unreachable", err)
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			if resp.StatusCode != http.StatusOK {
				return waxerr.New(waxerr.CodeInternal,
					fmt.Sprintf("daemon unhealthy: %s from %s", resp.Status, url))
			}
			fmt.Fprintln(cmd.OutOrStdout(), "ok")
			return nil
		},
	}
	cmd.Flags().String("addr", "", "daemon address host:port (default "+config.DefaultAddr+")")
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Second, "ping timeout")
	return cmd
}

// dialAddr rewrites wildcard listen addresses to loopback so `waxflow ping`
// works against a daemon bound to 0.0.0.0 or :: (the container case, where
// the HEALTHCHECK shares WAXFLOW_ADDR with the server).
func dialAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
