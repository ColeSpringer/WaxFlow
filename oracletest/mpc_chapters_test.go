package oracletest

// The Musepack chapter agreement cell, the .mpc twin of the WMA one. Two
// readers speak for an .mpc source's chapters since waxlabel v1.6.2:
// container/mpc, which serves the probe and any embedder with no mapper,
// and the tag library behind cli/label, whose answer wins on the
// mapper-wired route. Both read the same CT run, which chapters.mpc holds
// out of start order with one untitled entry, so the cell also pins that
// both readers sort and title it the same way.

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

func TestMusepackChaptersAgreeAcrossReaders(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "container", "mpc", "testdata", "chapters.mpc"))
	if err != nil {
		t.Fatal(err)
	}
	probe, err := waxflow.New().Probe(container.BytesSource(raw), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(probe.Chapters) != 4 {
		t.Fatalf("the container read %d chapters, want 4: %+v", len(probe.Chapters), probe.Chapters)
	}
	info, err := label.New().Read(t.Context(), container.BytesSource(raw), "mpc", meta.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(info.Chapters, probe.Chapters) {
		t.Errorf("the tag library read %+v, the container %+v", info.Chapters, probe.Chapters)
	}
}
