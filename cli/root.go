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
	"github.com/colespringer/waxflow/internal/meta"
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
	// resolver's last use. Return a plain nil on error, having cleaned up
	// what was opened: a Closer returned alongside an error is closed
	// rather than leaked, so handing back one already closed is a double
	// close. A typed-nil pointer in a non-nil interface is not a nil
	// Closer and will be called. An implementation that is present but
	// unconfigured owns the refusal for its own schemes: it returns a
	// resolver that answers them with a message naming what is missing,
	// rather than passing them down to opts.Next and the stock
	// unsupported-source error.
	//
	// An implementation must not start background goroutines when
	// opts.Daemon is false: the command resolves one reference and
	// tears down, so nothing outlives it.
	//
	// The resolver it returns may implement PIDSourceReporter to say what
	// /caps advertises, and ReloadableResolver to be reconciled by a root
	// reload. Only the returned resolver is inspected for either, so a
	// resolver that wraps another must implement and forward the ones the
	// inner resolver needs.
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

// ReloadableResolver, when implemented by the resolver an OpenResolver
// returns, is reconciled by a root reload (POST /roots/reload) alongside the
// library roots: it receives the re-read source cap, so a build that caps
// its own sources -- a catalog resolver enforcing the cap on the pid: files
// it opens itself -- stays in step with a changed sourceMaxBytes instead of
// holding the value it snapshotted at open. This is what makes a reload's
// "byte-for-byte as a restart" promise hold for a Flavor build, not only the
// stock one.
//
// Optional. A resolver that only delegates to ResolverOptions.Next needs
// nothing: Next is the reconciled roots, which enforce the live cap on their
// own sources. maxBytes is the resolved per-source cap, defaulted exactly as
// ResolverOptions.MaxBytes was.
//
// It is called with no library lock held and concurrently with in-flight
// Resolve calls, so an implementation must synchronize the field it updates
// (an atomic, say). Only the resolver OpenResolver returned is checked, as
// with PIDSourceReporter: a resolver that wraps another must implement this
// and forward when the inner one caps its own sources.
type ReloadableResolver interface {
	ReloadSourceMaxBytes(maxBytes int64)
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
	// roots enforce on theirs. It is the value at open: a Flavor that caps
	// its own sources on it (rather than delegating to Next, whose roots a
	// reload reconciles in place) should implement ReloadableResolver to
	// stay in step with a runtime sourceMaxBytes change; without that, the
	// copied value is a restart-only snapshot.
	MaxBytes int64

	// Next serves every reference the implementation does not claim:
	// the configured library roots. Implementations delegate rather
	// than answer not-found.
	//
	// Next is live-mutable: a root reload (POST /roots/reload) mutates
	// the roots it points at in place, so an implementation must delegate
	// through the held Next reference and never snapshot its resolvable
	// set, or a runtime-added root would be invisible to this build's
	// resolver until restart.
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

// usageArgs types an Args validator's error as a usage mistake, the way
// SetFlagErrorFunc does for flags. cobra has no hook for Args, so an
// unwrapped one exits 1 (internal) instead of 2.
func usageArgs(v cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := v(cmd, args); err != nil {
			return waxerr.Wrap(waxerr.CodeInvalidRequest, "usage", err)
		}
		return nil
	}
}

// openResolver builds the source-resolution chain every ref-taking
// command shares: the configured roots, wrapped by the Flavor's schemes
// when present. The returned close func tears the whole chain down. It is
// the thin wrapper for the commands (sign, probe, doctor) that do not need
// the inner roots handle; server calls openResolverWithRoots.
func (f Flavor) openResolver(ctx context.Context, cfg config.Config, logger *slog.Logger, daemon bool) (source.Resolver, func(), error) {
	resolver, _, cleanup, err := f.openResolverWithRoots(ctx, cfg, logger, daemon)
	return resolver, cleanup, err
}

// logClose closes c and reports a failure rather than dropping it. Every
// close in the resolver chain is diagnostic, never fatal: on the rollback
// paths the construction error being returned is the real story, and a
// Warn cannot mask it. A nil interface is a no-op, which is what an absent
// optional closer looks like; a typed-nil pointer inside a non-nil
// interface still calls through, the same exposure as the closer != nil
// guards this replaces, and Flavor.OpenResolver documents the contract.
func logClose(logger *slog.Logger, what string, c io.Closer) {
	if c == nil {
		return
	}
	if err := c.Close(); err != nil {
		logger.Warn(what+" close failed", "err", err)
	}
}

// openResolverWithRoots is openResolver plus the inner *source.Roots it
// opens regardless of flavor, so the server can hand that same live handle
// to a root-reload closure (the whole chain delegates to it by pointer). A
// configured catalogDB with no resolver to serve it is refused loudly: the
// operator asked for pid: sources and this build cannot deliver them.
func (f Flavor) openResolverWithRoots(ctx context.Context, cfg config.Config, logger *slog.Logger, daemon bool) (source.Resolver, *source.Roots, func(), error) {
	roots, err := source.OpenRoots(configRoots(cfg), cfg.ResolvedSourceMaxBytes())
	if err != nil {
		return nil, nil, nil, err
	}
	if f.OpenResolver == nil {
		if cfg.CatalogDB != "" {
			logClose(logger, "roots", roots)
			return nil, nil, nil, waxerr.New(waxerr.CodeInvalidRequest,
				"catalogDB is set but this build has no catalog resolver; pid: sources need a build that injects one")
		}
		return roots, roots, func() { logClose(logger, "roots", roots) }, nil
	}
	resolver, closer, err := f.OpenResolver(ctx, ResolverOptions{
		CatalogDB: cfg.CatalogDB,
		MaxBytes:  cfg.ResolvedSourceMaxBytes(),
		Next:      roots,
		Logger:    logger,
		Daemon:    daemon,
	})
	if err != nil {
		// A failing implementation should not hand back a live handle, but
		// the seam is out of tree: close what it returned rather than leak
		// it for the rest of the command. doctor is the case that shows,
		// since it keeps running its remaining checks after this one fails.
		logClose(logger, "resolver", closer)
		logClose(logger, "roots", roots)
		return nil, nil, nil, err
	}
	if resolver == nil {
		// Out-of-tree implementations reach here, so a broken one gets a
		// named error rather than a nil-interface panic at the first
		// Resolve. The closer is a third-party handle (a catalog DB), the
		// one close in this chain that fails in practice, so this rollback
		// reports it the same way the teardown closure does.
		logClose(logger, "resolver", closer)
		logClose(logger, "roots", roots)
		return nil, nil, nil, waxerr.New(waxerr.CodeInternal,
			"cli: Flavor.OpenResolver returned a nil resolver and no error")
	}
	return resolver, roots, func() {
		logClose(logger, "resolver", closer)
		logClose(logger, "roots", roots)
	}, nil
}

// resolveConfigPath resolves the config file path from the --config flag,
// falling back to WAXFLOW_CONFIG. resolveConfig reads it to load the
// config; server reads it to build the reload closure over the same file.
func resolveConfigPath(cmd *cobra.Command) (string, error) {
	path, err := cmd.Flags().GetString("config")
	if err != nil {
		return "", err
	}
	if path == "" {
		path = os.Getenv("WAXFLOW_CONFIG")
	}
	return path, nil
}

// resolveConfig applies the family precedence. config.Load resolves
// env > file > default; flag overrides land here, last.
func resolveConfig(cmd *cobra.Command) (config.Config, error) {
	path, err := resolveConfigPath(cmd)
	if err != nil {
		return config.Config{}, err
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

// logMetaRead reports a metadata read through the logger --log-level
// configures, so warnings can be quieted without --no-tags dropping the
// metadata too. Notes are source lint (see meta.Info.Notes) and sit at Debug.
func logMetaRead(logger *slog.Logger, info *meta.Info) {
	if info == nil {
		return
	}
	for _, w := range info.Warnings {
		logger.Warn("metadata", "warning", w)
	}
	for _, n := range info.Notes {
		logger.Debug("metadata", "note", n)
	}
}
