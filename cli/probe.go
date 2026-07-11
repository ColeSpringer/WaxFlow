package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/config"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/waxerr"
)

func newProbeCmd(flavor Flavor) *cobra.Command {
	var jsonOut, strict bool
	cmd := &cobra.Command{
		Use:   "probe <file>",
		Short: "Identify an audio file and print its stream parameters",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src, hint, cleanup, err := openSourceRef(cmd, flavor, args[0], nil, nil)
			if err != nil {
				return err
			}
			defer cleanup()
			info, err := waxflow.New().Probe(src, hint, &waxflow.ProbeOptions{Strict: strict})
			if err != nil {
				return err
			}
			// Metadata is best-effort: a source the mapper cannot read
			// still probes (m carries the warning, or stays nil).
			m, _ := label.New().Read(cmd.Context(), src, hint, meta.ReadOptions{})
			if jsonOut {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(server.ProbeJSON(info, probeMetadata(m)))
			}
			printProbe(cmd, info, m)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print machine-readable JSON (schemaVersion'd)")
	cmd.Flags().BoolVar(&strict, "strict", false, "treat tolerated input damage as errors")
	return cmd
}

// openSourceRef opens a probe/transcode input and returns it with its
// extension hint. A plain argument is a local file path; a
// source-scheme argument (pid:<ULID>, upload:<id>) resolves through the
// same chain the daemon uses, so the resolver flavor probes and
// transcodes straight from catalog references. Only those two literal
// schemes get resolver treatment; everything else stays a filesystem
// path (Windows drive letters also contain a colon).
//
// cfg and logger are needed only for scheme-shaped args. Callers that
// already resolved them (transcode) pass them in; nil resolves them on
// demand, so a plain-path probe never requires a loadable configuration.
func openSourceRef(cmd *cobra.Command, flavor Flavor, arg string, cfg *config.Config, logger *slog.Logger) (container.Source, string, func(), error) {
	if !strings.HasPrefix(arg, "pid:") && !strings.HasPrefix(arg, "upload:") {
		src, cleanup, err := openSource(arg)
		return src, extHint(arg), cleanup, err
	}
	if cfg == nil {
		resolved, err := resolveConfig(cmd)
		if err != nil {
			return nil, "", nil, err
		}
		cfg = &resolved
	}
	if logger == nil {
		var err error
		if logger, err = newLogger(cmd.ErrOrStderr(), *cfg); err != nil { // CLI logs to stderr
			return nil, "", nil, err
		}
	}
	resolver, closeResolver, err := flavor.openResolver(*cfg, logger, false)
	if err != nil {
		return nil, "", nil, err
	}
	f, err := resolver.Resolve(cmd.Context(), arg)
	if err != nil {
		closeResolver()
		return nil, "", nil, err
	}
	return f, f.Ext, func() { f.Close(); closeResolver() }, nil
}

// openSource opens a local file as a container.Source. The caller must
// invoke cleanup when done.
func openSource(path string) (container.Source, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		code := waxerr.CodeSourceUnreadable
		if errors.Is(err, fs.ErrNotExist) {
			code = waxerr.CodeNotFound
		}
		return nil, nil, waxerr.Wrap(code, "opening input", err)
	}
	src, err := container.FileSource(f)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return src, func() { f.Close() }, nil
}

// extHint extracts the extension hint for the format sniffer.
func extHint(path string) string {
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
}

// The probe JSON shape lives in the server package (server.ProbeInfo):
// the CLI and GET /probe serve the identical contract by construction.

// probeMetadata summarizes a metadata read for server.ProbeJSON,
// mirroring the server's own unexported adapter (the public signature
// must not carry the internal meta type). TestProbeJSONMatchesHTTP pins
// the two outputs byte-equal.
func probeMetadata(m *meta.Info) *server.ProbeMetadata {
	if m == nil {
		return nil
	}
	return &server.ProbeMetadata{
		Tags:      m.TagSummary(),
		Chapters:  m.Chapters,
		HasArt:    m.HasPictures,
		HasLyrics: m.HasLyrics(),
	}
}

// durationSeconds converts samples to seconds at the presentation
// boundary; positions stay integer samples everywhere else (ADR-0006).
func durationSeconds(samples int64, rate int) float64 {
	return server.DurationSeconds(samples, rate)
}

func printProbe(cmd *cobra.Command, info *format.Info, m *meta.Info) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "container: %s\n", info.Container)
	for _, t := range info.Tracks {
		fmt.Fprintf(w, "track %d:   %s %s", t.ID, t.Codec, t.Fmt)
		if t.Fmt.Layout != 0 {
			fmt.Fprintf(w, " [%s]", t.Fmt.Layout)
		}
		fmt.Fprintln(w)
		if t.Samples >= 0 {
			fmt.Fprintf(w, "samples:   %d (%.3fs)\n", t.Samples, durationSeconds(t.Samples, t.Fmt.Rate))
		} else {
			fmt.Fprintln(w, "samples:   unknown")
		}
	}
	if m != nil {
		for _, key := range []string{"TITLE", "ARTIST", "ALBUM"} {
			if vs := m.Tags[key]; len(vs) > 0 {
				fmt.Fprintf(w, "%-10s %s\n", strings.ToLower(key)+":", strings.Join(vs, "; "))
			}
		}
		if len(m.Chapters) > 0 {
			fmt.Fprintf(w, "chapters:  %d\n", len(m.Chapters))
		}
		if m.HasPictures {
			fmt.Fprintln(w, "art:       embedded")
		}
	}
	for _, warn := range info.Warnings {
		fmt.Fprintf(w, "warning:   %s\n", warn)
	}
}
