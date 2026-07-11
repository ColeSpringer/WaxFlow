package cli

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/internal/config"
	"github.com/colespringer/waxflow/internal/sign"
	"github.com/colespringer/waxflow/waxerr"
	urlpkg "net/url"
)

// newSignCmd mints signed playback URLs offline: the daemon does not need
// to be running, only the signing secret and the source configuration
// (roots, catalogDB in the resolver flavor) must match its.
func newSignCmd(flavor Flavor) *cobra.Command {
	var src, format, gain string
	var rate, channels, bits int
	var t float64
	var ttl time.Duration
	var base string
	cmd := &cobra.Command{
		Use:   "sign",
		Short: "Mint a signed playback URL offline",
		Long: `Sign mints a short-lived signed /stream URL (ADR-0003) using the same
signing secret the daemon holds: signingSecret from configuration, or the
secret persisted under dataDir on the daemon's first run. The source is
resolved against the configured roots to embed its identity, so a URL
minted here dies with 410 source-changed if the file changes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig(cmd)
			if err != nil {
				return err
			}
			if src == "" {
				return waxerr.New(waxerr.CodeInvalidRequest, "--src is required")
			}

			// CLI convention: diagnostics from the resolver chain go to
			// stderr; the minted URL alone lands on stdout.
			logger, err := newLogger(cmd.ErrOrStderr(), cfg)
			if err != nil {
				return err
			}
			resolver, closeResolver, err := flavor.openResolver(cfg, logger, false)
			if err != nil {
				return err
			}
			defer closeResolver()
			f, err := resolver.Resolve(src)
			if err != nil {
				return err
			}
			defer f.Close()

			params := urlpkg.Values{"src": {src}, "id": {f.ID.String()}}
			if format != "" {
				params.Set("format", format)
			}
			if gain != "" {
				params.Set("gain", gain)
			}
			for name, v := range map[string]int{"rate": rate, "ch": channels, "bits": bits} {
				if v != 0 {
					params.Set(name, strconv.Itoa(v))
				}
			}
			if t > 0 {
				params.Set("t", strconv.FormatFloat(t, 'f', -1, 64))
			}

			if ttl <= 0 {
				// The default TTL policy needs the duration: probe
				// locally through the same engine the daemon uses.
				d := float64(-1)
				if info, err := waxflow.New().Probe(f, f.Ext, nil); err == nil {
					track := info.Default()
					d = durationSeconds(track.Samples, track.Fmt.Rate)
				}
				ttl = sign.DefaultTTLFor(d)
			}

			keys, err := resolveSigningKeys(cfg)
			if err != nil {
				return err
			}
			signer, err := sign.New(keys)
			if err != nil {
				return err
			}
			signed := signer.Sign(http.MethodGet, "/stream", params, time.Now().Add(ttl))
			if base == "" {
				base = "http://" + dialAddr(cfg.ResolvedAddr())
			}
			fmt.Fprintln(cmd.OutOrStdout(), base+"/stream?"+signed.Encode())
			return nil
		},
	}
	cmd.Flags().StringVar(&src, "src", "", "source reference <root>/<path> (required)")
	cmd.Flags().StringVar(&format, "format", "", "output format (default auto)")
	cmd.Flags().IntVar(&rate, "rate", 0, "output sample rate in Hz")
	cmd.Flags().IntVar(&channels, "channels", 0, "output channel count")
	cmd.Flags().IntVar(&bits, "bits", 0, "output bit depth: 16 or 24")
	cmd.Flags().StringVar(&gain, "gain", "", "gain mode: off, track, album, or +/-dB")
	cmd.Flags().Float64Var(&t, "t", 0, "start position in seconds")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "URL lifetime (default max(6h, 2x duration))")
	cmd.Flags().StringVar(&base, "base", "", "base URL to print (default from addr)")
	cmd.Flags().String("addr", "", "daemon address for the printed URL (default "+config.DefaultAddr+")")
	return cmd
}

// resolveSigningKeys resolves the key material exactly like the daemon:
// same resolver, same persisted file.
func resolveSigningKeys(cfg config.Config) ([]sign.Key, error) {
	dataDir, err := cfg.ResolvedDataDir()
	if err != nil {
		return nil, err
	}
	return sign.ResolveKeys(cfg.SigningSecret, dataDir)
}
