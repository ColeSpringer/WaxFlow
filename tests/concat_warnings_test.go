package waxflow_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestConcatCarriesMemberWarnings pins the timeline's half of the live
// lists: a member's damage reaches the timeline's Info under the member's
// index once the read has reached it, and nothing is claimed for a member
// not yet opened. A merge job's input-damage warnings come from this.
func TestConcatCarriesMemberWarnings(t *testing.T) {
	wav, err := os.ReadFile(repoPath("testdata", "sine-s16.wav"))
	if err != nil {
		t.Fatal(err)
	}
	mp3, err := os.ReadFile(repoPath("testdata", "sine-untagged.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	damaged := testutil.ZeroMiddle(mp3, 2048)
	e := waxflow.New()
	member := func(raw []byte, hint string) waxflow.ConcatSource {
		info, err := e.Probe(container.BytesSource(raw), hint, nil)
		if err != nil {
			t.Fatal(err)
		}
		track := info.Default()
		if track.Samples < 0 {
			// A timeline plans from declared lengths; the untagged MP3 has
			// none, so it is measured the way the daemon measures it.
			med, err := e.OpenStream(container.BytesSource(raw), hint)
			if err != nil {
				t.Fatal(err)
			}
			got := drainMedia(t, med, 40000)
			track.Samples, track.SamplesExact = int64(got.N), true
			audio.Put(got)
			med.Close()
		}
		return waxflow.ConcatSource{Track: track,
			Open: func() (format.Media, error) { return e.OpenStream(container.BytesSource(raw), hint) }}
	}
	med, err := waxflow.Concat([]waxflow.ConcatSource{member(wav, "wav"), member(damaged, "mp3")}, waxflow.ConcatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	if ws := med.Info().Warnings; len(ws) != 0 {
		t.Fatalf("warnings before any read = %v, want none", ws)
	}
	audio.Put(drainMedia(t, med, 40000))
	ws := med.Info().Warnings
	if !slices.ContainsFunc(ws, func(s string) bool {
		return strings.HasPrefix(s, "member 1: ") && strings.Contains(s, "unparsable bytes skipped")
	}) {
		t.Errorf("warnings after the read = %v, want the second member's skipped bytes under its index", ws)
	}
	if slices.ContainsFunc(ws, func(s string) bool { return strings.HasPrefix(s, "member 0: ") }) {
		t.Errorf("warnings = %v, the clean first member should contribute none", ws)
	}
}
