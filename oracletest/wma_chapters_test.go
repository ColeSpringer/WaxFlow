package oracletest

// The WMA chapter agreement cell. Two readers speak for a .wma source's
// chapters since waxlabel v1.6.2: container/asf, which serves the probe and any
// embedder with no mapper, and the tag library behind cli/label, whose answer
// wins on the mapper-wired route. Both read the same Marker Object and must
// place it on the same timeline, or a transcode would carry one set of
// chapters and the probe report another.

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
)

func TestWMAChaptersAgreeAcrossReaders(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "container", "asf", "testdata", "chapters.wma"))
	if err != nil {
		t.Fatal(err)
	}
	probe, err := waxflow.New().Probe(container.BytesSource(raw), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(probe.Chapters) != 3 {
		t.Fatalf("the container read %d chapters, want 3: %+v", len(probe.Chapters), probe.Chapters)
	}
	info, err := label.New().Read(t.Context(), container.BytesSource(raw), "wma", meta.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(info.Chapters, probe.Chapters) {
		t.Errorf("the tag library read %+v, the container %+v", info.Chapters, probe.Chapters)
	}
}
