package mpc_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mpc"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// TestASeekStopsWithItsContext: the state scan a cold seek runs to its target
// (SV7's from the last checkpoint, SV8's over the noise generator when the
// stream declares noise substitution) ends with the seek's context, and a
// seek with a live one then lands where a fresh demuxer lands.
func TestASeekStopsWithItsContext(t *testing.T) {
	for _, name := range []string{"seek-sv7.mpc", "seek-pns.mpc"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := mpc.NewDemuxer(container.BytesSource(raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			target := fresh.Tracks()[0].Samples * 3 / 4
			want, err := fresh.SeekSample(t.Context(), 0, target)
			if err != nil {
				t.Fatal(err)
			}
			d, err := mpc.NewDemuxer(container.BytesSource(raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.SeekSample(testutil.CancelAfterChecks(t, 2), 0, target); waxerr.CodeOf(err) != waxerr.CodeCanceled {
				t.Fatalf("a seek whose context ended returned %v, want canceled", err)
			}
			if got, err := d.SeekSample(t.Context(), 0, target); err != nil || got != want {
				t.Errorf("the seek after the cancel landed at %d (%v), a fresh demuxer at %d", got, err, want)
			}
		})
	}
}
