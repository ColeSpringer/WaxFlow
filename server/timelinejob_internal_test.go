package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/source"
)

// TestTimelineJobGateReadsExactnessBesideTheIndexer pins the half of the job
// gate that container.Indexer alone cannot carry.
//
// The gate asks whether measuring a member costs a full scan, and it reads the
// Indexer capability as the tree's mark for that. Go's method sets are static,
// so once a WAV could carry MP3 frames the whole container advertised the mark,
// including the PCM files that are almost all of them. What separates them is
// the exactness of the length: a payload whose unit is one frame states its own
// count from its byte size, so it is exact and needs no measuring at all. This
// asserts both directions on one container, since a gate that answered the same
// way for both would be no gate.
func TestTimelineJobGateReadsExactnessBesideTheIndexer(t *testing.T) {
	root := t.TempDir()
	for _, f := range []struct{ src, dst string }{
		{filepath.Join("..", "testdata", "sine-s16.wav"), "pcm.wav"},
		{filepath.Join("..", "container", "riff", "testdata", "mp3.wav"), "mp3.wav"},
	} {
		b, err := os.ReadFile(f.src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, f.dst), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	roots, err := source.OpenRoots([]source.Root{{Name: "lib", Path: root}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	s := &Server{eng: waxflow.New(), resolver: roots}

	for _, tc := range []struct {
		ref  string
		slow bool
	}{
		{"lib/pcm.wav", false},
		{"lib/mp3.wav", true},
	} {
		f, err := s.resolver.Resolve(context.Background(), tc.ref)
		if err != nil {
			t.Fatal(err)
		}
		slow, err := s.timelineNeedsJob([]*source.File{f})
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if slow != tc.slow {
			t.Errorf("%s: needs a job = %v, want %v", tc.ref, slow, tc.slow)
		}
	}
}
