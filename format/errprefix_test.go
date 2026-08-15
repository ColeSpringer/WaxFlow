package format

import (
	"bytes"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// Parse failures travel to end users verbatim (waxerr.Error returns the
// bare message), so the prefix each container package stamps on its
// errors is user-facing text. It must be the driver table's public
// container name, the same vocabulary /caps and Info.Container use, not
// the Go package name ("mpa", "flacn").
func TestParseErrorsUsePublicContainerNames(t *testing.T) {
	// Bytes that match no driver's magic, so the extension hint alone
	// routes them; every demuxer must then reject its own garbage.
	junk := bytes.Repeat([]byte{'z'}, 4096)
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			_, err := Probe(container.BytesSource(junk), d.exts[0], nil)
			if err == nil {
				t.Fatalf("Probe accepted %d junk bytes as %s", len(junk), d.name)
			}
			if !strings.HasPrefix(err.Error(), d.name+": ") {
				t.Fatalf("error %q does not carry the public container name %q", err, d.name)
			}
		})
	}
}
