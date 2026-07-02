package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/waxerr"
)

func newProbeCmd() *cobra.Command {
	var jsonOut, strict bool
	cmd := &cobra.Command{
		Use:   "probe <file>",
		Short: "Identify an audio file and print its stream parameters",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src, cleanup, err := openSource(args[0])
			if err != nil {
				return err
			}
			defer cleanup()
			info, err := waxflow.New().Probe(src, extHint(args[0]), &waxflow.ProbeOptions{Strict: strict})
			if err != nil {
				return err
			}
			if jsonOut {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(probeJSON(info))
			}
			printProbe(cmd, info)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print machine-readable JSON (schemaVersion'd)")
	cmd.Flags().BoolVar(&strict, "strict", false, "treat tolerated input damage as errors")
	return cmd
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

// probeInfoJSON is the stable JSON shape of probe results (also the shape
// the HTTP /probe endpoint will serve from M4).
type probeInfoJSON struct {
	SchemaVersion int              `json:"schemaVersion"`
	Container     string           `json:"container"`
	Tracks        []probeTrackJSON `json:"tracks"`
	Warnings      []string         `json:"warnings,omitempty"`
}

type probeTrackJSON struct {
	ID              int     `json:"id"`
	Codec           string  `json:"codec"`
	Rate            int     `json:"rate"`
	Channels        int     `json:"channels"`
	Layout          string  `json:"layout"`
	SampleType      string  `json:"sampleType"`
	BitDepth        int     `json:"bitDepth"`
	Samples         int64   `json:"samples"`
	DurationSeconds float64 `json:"durationSeconds"`
	Default         bool    `json:"default"`
}

func probeJSON(info *format.Info) probeInfoJSON {
	out := probeInfoJSON{SchemaVersion: 1, Container: info.Container, Warnings: info.Warnings}
	for _, t := range info.Tracks {
		out.Tracks = append(out.Tracks, probeTrackJSON{
			ID:              t.ID,
			Codec:           string(t.Codec),
			Rate:            t.Fmt.Rate,
			Channels:        t.Fmt.Channels,
			Layout:          t.Fmt.Layout.String(),
			SampleType:      t.Fmt.Type.String(),
			BitDepth:        t.Fmt.BitDepth,
			Samples:         t.Samples,
			DurationSeconds: durationSeconds(t.Samples, t.Fmt.Rate),
			Default:         t.Default,
		})
	}
	return out
}

// durationSeconds converts samples to seconds at the presentation
// boundary; positions stay integer samples everywhere else (ADR-0006).
func durationSeconds(samples int64, rate int) float64 {
	if samples < 0 || rate <= 0 {
		return -1
	}
	return float64(samples) / float64(rate)
}

func printProbe(cmd *cobra.Command, info *format.Info) {
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
	for _, warn := range info.Warnings {
		fmt.Fprintf(w, "warning:   %s\n", warn)
	}
}
