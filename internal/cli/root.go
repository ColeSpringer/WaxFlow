// Package cli implements the waxflow command tree. It is deliberately thin:
// commands parse flags, resolve configuration, and delegate; behavior lives
// in the library packages.
package cli

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxflow/internal/config"
	"github.com/colespringer/waxflow/waxerr"
)

// Execute runs the waxflow CLI with the given argument vector (excluding
// the program name) and returns the process exit code per the contract
// printed by `waxflow exit-codes`.
func Execute(version string, args []string, stdout, stderr io.Writer) int {
	root := newRootCmd(version)
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

func newRootCmd(version string) *cobra.Command {
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
		newVersionCmd(version),
		newPingCmd(),
		newExitCodesCmd(),
		newServerCmd(version),
		newProbeCmd(),
		newTranscodeCmd(),
	)
	return root
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
