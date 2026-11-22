package waxflow

import (
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

var update = flag.Bool("update", false, "rewrite golden files (make goldens)")

// formNames is every Container value any row takes, plus the empty default.
var formNames = []string{
	"", "mka", "webm", ContainerOgg, "adts", ContainerProgressive, ContainerFragmented,
}

// TestGoldenOutputForms pins what every (row, container) pair resolves to: the
// plan's container name and media type, the muxer that runs and its cache-key
// term, liveness, the file extension and whether the muxer writes the tags, or
// the refusal for a pair no form takes.
func TestGoldenOutputForms(t *testing.T) {
	for i := range outputs {
		row := &outputs[i]
		seen := map[string]bool{}
		for _, f := range row.forms[1:] {
			if !slices.Contains(formNames, f.name) {
				t.Errorf("%s: form %q is not in formNames, so the golden does not cover it", row.name, f.name)
			}
			if seen[f.name] {
				t.Errorf("%s: two forms are named %q; only the first is reachable", row.name, f.name)
			}
			seen[f.name] = true
		}
	}
	var out strings.Builder
	for i := range outputs {
		row := &outputs[i]
		for _, name := range formNames {
			fmt.Fprintf(&out, "%s | %q | %s | ext=%s embeds=%t\n", row.name, name,
				formFacts(row, name), OutputExt(row.name, name), OutputEmbedsTags(row.name, name))
		}
	}
	testutil.GoldenText(t, testutil.PkgFile("testdata", "golden", "forms.txt"), []byte(out.String()), *update,
		"an output form moved: check the change names these lines, then run "+
			"`go test -run 'TestGoldenOutputForms$' . -update` and review the diff.")
}

// formFacts describes the form name selects on row, or its refusal.
func formFacts(row *output, name string) string {
	f, err := row.form(name)
	if err != nil {
		return fmt.Sprintf("refused %s: %v", waxerr.CodeOf(err), err)
	}
	opts := TranscodeOptions{Format: row.name, Container: name}
	mux, err := f.mux(container.Track{Fmt: audio.Format{Rate: 44100, Channels: 2, BitDepth: 16}}, opts, runConfig{}, nil, io.Discard)
	if err != nil {
		return fmt.Sprintf("mux: %v", err)
	}
	return fmt.Sprintf("container=%s media=%s muxer=%T version=%s live=%t",
		row.containerName(f), f.mediaType, mux, f.version, f.live)
}
