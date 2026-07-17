// Package cli implements the waxflow command tree. It is deliberately thin:
// commands parse flags, resolve configuration, and delegate; behavior lives
// in the library packages.
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxflow/internal/config"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// Flavor customizes the command tree for a build that adds source
// schemes the stock binary cannot serve. The zero value is the stock
// build. A build that resolves pid:<ULID> against a WaxBin catalog
// injects one; no build in this repo does, so the seam is aimed at
// modules outside it, and examples/catalogcli is a worked example.
type Flavor struct {
	// Name tags the version output: "catalog" prints waxflow-catalog.
	// Empty means stock.
	Name string

	// OpenResolver wraps opts.Next with this build's source schemes.
	// Every ref-taking command builds its resolver through it: server,
	// sign, probe, transcode, split, and doctor.
	//
	// The returned Closer, which may be nil, is closed after the
	// resolver's last use. An implementation that is present but
	// unconfigured owns the refusal for its own schemes: it returns a
	// resolver that answers them with a message naming what is missing,
	// rather than passing them down to opts.Next and the stock
	// unsupported-source error.
	//
	// An implementation must not start background goroutines when
	// opts.Daemon is false: the command resolves one reference and
	// tears down, so nothing outlives it.
	//
	// The resolver it returns may implement PIDSourceReporter to say
	// what /caps advertises.
	OpenResolver func(ctx context.Context, opts ResolverOptions) (source.Resolver, io.Closer, error)
}

// PIDSourceReporter, when implemented by the resolver an OpenResolver
// returns, declares whether this build resolves pid:<ULID> references.
// `waxflow server` publishes the answer as delivery.pid in /caps.
//
// Implementing it is optional. Without it the CLI infers support from
// catalogDB being configured, which is right for a resolver keyed on
// catalogDB -- the documented channel, and what ResolverOptions.CatalogDB
// carries -- and wrong for one that is not. A build that resolves pid:
// from somewhere else says so here rather than leaving /caps to deny a
// capability it has.
//
// This only keeps the capability surface honest; whether a given pid
// reference resolves is the resolver's business either way.
type PIDSourceReporter interface {
	PIDSources() bool
}

// ResolverOptions carries what an OpenResolver implementation needs from
// the resolved configuration. Every field is exported or stdlib, so a
// Flavor is constructible from any module.
type ResolverOptions struct {
	// CatalogDB is the configured catalog database path, resolved
	// through the family precedence (flag > env > JSON file). Empty
	// means the operator configured none.
	CatalogDB string

	// MaxBytes caps each resolved source file, the cap the library
	// roots enforce on theirs.
	MaxBytes int64

	// Next serves every reference the implementation does not claim:
	// the configured library roots. Implementations delegate rather
	// than answer not-found.
	Next source.Resolver

	// Logger is never nil.
	Logger *slog.Logger

	// Daemon is true only under `waxflow server`, the one command whose
	// process outlives a single resolution. See OpenResolver.
	Daemon bool
}

// Execute runs the waxflow CLI with the given argument vector (excluding
// the program name) and returns the process exit code per the contract
// printed by `waxflow exit-codes`.
func Execute(version string, args []string, stdout, stderr io.Writer) int {
	return ExecuteFlavor(version, args, stdout, stderr, Flavor{})
}

// ExecuteFlavor is Execute for a build that customizes the command tree
// (see Flavor); the stock main passes the zero Flavor via Execute.
func ExecuteFlavor(version string, args []string, stdout, stderr io.Writer, flavor Flavor) int {
	root := newRootCmd(version, flavor)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.Execute()
	if err == nil {
		return 0
	}
	// cobra reports unknown subcommands with an untyped error; that is a
	// usage mistake, not an internal failure. Worst case, a wording change
	// in cobra demotes this to exit 1.
	if strings.HasPrefix(err.Error(), "unknown command") {
		err = waxerr.Wrap(waxerr.CodeInvalidRequest, "usage", err)
	}
	fmt.Fprintf(stderr, "waxflow: %v\n", err)
	return waxerr.ExitCode(err)
}

func newRootCmd(version string, flavor Flavor) *cobra.Command {
	root := &cobra.Command{
		Use:   "waxflow",
		Short: "Pure-Go audio transcoding service for the Wax family",
		Long: `WaxFlow is a self-hosted, pure-Go, on-the-fly audio transcoding service:
request -> decode -> DSP -> encode -> stream, with no ffmpeg at runtime.

Configuration precedence: flag > WAXFLOW_* environment > JSON config file
(--config or WAXFLOW_CONFIG) > built-in default.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().String("config", "", "path to JSON config file (also WAXFLOW_CONFIG)")
	root.PersistentFlags().String("log-level", "", "log level: debug|info|warn|error (default info)")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return waxerr.Wrap(waxerr.CodeInvalidRequest, "usage", err)
	})
	root.AddCommand(
		newVersionCmd(version, flavor.Name),
		newPingCmd(),
		newExitCodesCmd(),
		newServerCmd(version, flavor),
		newProbeCmd(flavor),
		newTranscodeCmd(flavor),
		newSplitCmd(flavor),
		newSignCmd(flavor),
		newCacheCmd(),
		newDoctorCmd(flavor),
	)
	return root
}

// openResolver builds the source-resolution chain every ref-taking
// command shares: the configured roots, wrapped by the Flavor's schemes
// when present. The returned close func tears the whole chain down. A
// configured catalogDB with no resolver to serve it is refused loudly:
// the operator asked for pid: sources and this build cannot deliver
// them.
func (f Flavor) openResolver(ctx context.Context, cfg config.Config, logger *slog.Logger, daemon bool) (source.Resolver, func(), error) {
	roots, err := source.OpenRoots(configRoots(cfg), cfg.ResolvedSourceMaxBytes())
	if err != nil {
		return nil, nil, err
	}
	if f.OpenResolver == nil {
		if cfg.CatalogDB != "" {
			roots.Close()
			return nil, nil, waxerr.New(waxerr.CodeInvalidRequest,
				"catalogDB is set but this build has no catalog resolver; pid: sources need a build that injects one")
		}
		return roots, func() { roots.Close() }, nil
	}
	resolver, closer, err := f.OpenResolver(ctx, ResolverOptions{
		CatalogDB: cfg.CatalogDB,
		MaxBytes:  cfg.ResolvedSourceMaxBytes(),
		Next:      roots,
		Logger:    logger,
		Daemon:    daemon,
	})
	if err != nil {
		roots.Close()
		return nil, nil, err
	}
	if resolver == nil {
		// Out-of-tree implementations reach here, so a broken one gets a
		// named error rather than a nil-interface panic at the first
		// Resolve.
		if closer != nil {
			closer.Close()
		}
		roots.Close()
		return nil, nil, waxerr.New(waxerr.CodeInternal,
			"cli: Flavor.OpenResolver returned a nil resolver and no error")
	}
	return resolver, func() {
		if closer != nil {
			closer.Close()
		}
		roots.Close()
	}, nil
}

// resolveConfig applies the family precedence. config.Load resolves
// env > file > default; flag overrides land here, last.
func resolveConfig(cmd *cobra.Command) (config.Config, error) {
	path, err := cmd.Flags().GetString("config")
	if err != nil {
		return config.Config{}, err
	}
	if path == "" {
		path = os.Getenv("WAXFLOW_CONFIG")
	}
	cfg, err := config.Load(path, os.LookupEnv)
	if err != nil {
		return cfg, err
	}
	for name, field := range map[string]*string{
		"addr":      &cfg.Addr,
		"log-level": &cfg.LogLevel,
	} {
		if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
			*field = f.Value.String()
		}
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// newLogger builds a *slog.Logger per family convention: TextHandler, level
// from config. The daemon passes stdout, CLI diagnostics pass stderr.
func newLogger(w io.Writer, cfg config.Config) (*slog.Logger, error) {
	lvl, err := cfg.SlogLevel()
	if err != nil {
		return nil, err
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})), nil
}
