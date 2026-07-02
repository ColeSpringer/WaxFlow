package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/waxerr"
)

func newTranscodeCmd() *cobra.Command {
	var formatName string
	var force bool
	cmd := &cobra.Command{
		Use:   "transcode <input> <output>",
		Short: "Transcode an audio file locally through the engine",
		Long: `Transcode decodes the input and writes it to the output path via the
same engine the daemon uses: decode -> encode -> mux. The output format
comes from --format or the output extension. Through M1 both ends are
PCM (WAV/AIFF in, WAV/AIFF out) and transcodes are bit-exact.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolveConfig(cmd)
			if err != nil {
				return err
			}
			logger, err := newLogger(cmd.ErrOrStderr(), cfg) // CLI logs to stderr
			if err != nil {
				return err
			}

			outFormat := formatName
			if outFormat == "" {
				// The engine's output table is the single source of truth
				// for extensions, so the CLI cannot drift from it.
				outFormat = waxflow.OutputFormatForExt(extHint(args[1]))
				if outFormat == "" {
					return waxerr.New(waxerr.CodeInvalidRequest,
						fmt.Sprintf("cannot infer output format from %q; pass --format (%s)",
							filepath.Base(args[1]), strings.Join(waxflow.OutputFormats(), ", ")))
				}
			}

			src, cleanup, err := openSource(args[0])
			if err != nil {
				return err
			}
			defer cleanup()

			// An in-place transcode would truncate the input before it is
			// ever read (and the failure path would then unlink it), so
			// refuse when both paths name the same file. os.SameFile
			// catches hard links and symlinked spellings, not just equal
			// path strings.
			if outFi, err := os.Stat(args[1]); err == nil {
				if inFi, err := os.Stat(args[0]); err == nil && os.SameFile(inFi, outFi) {
					return waxerr.New(waxerr.CodeInvalidRequest,
						"input and output are the same file; transcode to a new path")
				}
			}

			flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
			if force {
				flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
			}
			out, err := os.OpenFile(args[1], flags, 0o644)
			if err != nil {
				if errors.Is(err, os.ErrExist) {
					return waxerr.Wrap(waxerr.CodeInvalidRequest, "output exists (use --force to overwrite)", err)
				}
				return waxerr.Wrap(waxerr.CodeOutputUnwritable, "creating output", err)
			}

			e := waxflow.New(waxflow.WithLogger(logger))
			res, err := e.Transcode(cmd.Context(), src, extHint(args[0]), out, waxflow.TranscodeOptions{Format: outFormat})
			if err != nil {
				out.Close()
				// A failed transcode leaves no half-written artifact.
				os.Remove(args[1])
				return err
			}
			if err := out.Close(); err != nil {
				return waxerr.Wrap(waxerr.CodeOutputUnwritable, "closing output", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s: %s %d samples (%.3fs)\n",
				args[1], res.Format, res.Samples, durationSeconds(res.Samples, res.Format.Rate))
			return nil
		},
	}
	cmd.Flags().StringVar(&formatName, "format", "", "output format: wav or aiff (default: from output extension)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite the output if it exists")
	return cmd
}
