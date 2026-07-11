package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxflow/client"
	"github.com/colespringer/waxflow/internal/config"
)

// newCacheCmd groups cache operations against a running daemon. They go
// over HTTP (not the cache directory) because the daemon owns the index:
// deleting entries under a live janitor would race it.
func newCacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect or garbage-collect a running daemon's transcode cache",
	}
	cmd.PersistentFlags().String("addr", "", "daemon address host:port (default "+config.DefaultAddr+")")
	cmd.PersistentFlags().String("api-key", "", "API key (default: first configured apiKeys entry)")
	cmd.AddCommand(newCacheStatsCmd(), newCacheGCCmd())
	return cmd
}

func cacheClient(cmd *cobra.Command) (*client.Client, error) {
	cfg, err := resolveConfig(cmd)
	if err != nil {
		return nil, err
	}
	key, err := cmd.Flags().GetString("api-key")
	if err != nil {
		return nil, err
	}
	if key == "" && len(cfg.APIKeys) > 0 {
		key = cfg.APIKeys[0]
	}
	return client.New("http://"+dialAddr(cfg.ResolvedAddr()), key)
}

func newCacheStatsCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Print cache entry count, bytes, and hit counters",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := cacheClient(cmd)
			if err != nil {
				return err
			}
			st, err := cl.CacheStats(cmd.Context())
			if err != nil {
				return err
			}
			if jsonOut {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(st)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "entries: %d\nbytes:   %d\nhits:    %d\nmisses:  %d\n",
				st.Entries, st.Bytes, st.Hits, st.Misses)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print machine-readable JSON")
	return cmd
}

func newCacheGCCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Run cache eviction now",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := cacheClient(cmd)
			if err != nil {
				return err
			}
			res, err := cl.CacheGC(cmd.Context())
			if err != nil {
				return err
			}
			if jsonOut {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed: %d\nfreed:   %d bytes\n", res.Removed, res.FreedBytes)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print machine-readable JSON")
	return cmd
}
