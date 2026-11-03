package adts_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/adts"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// TestWalkStopsWithItsContext is container.Walker's cancel contract on the
// ADTS frame index (testutil.WalkPauses).
func TestWalkStopsWithItsContext(t *testing.T) {
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	raw, _ := encodeADTS(t, f, museSrc(44100*24, 2, 44100))
	testutil.WalkPauses(t, raw, 2, func(src container.Source) (testutil.Walkable, error) {
		return adts.NewDemuxer(src, nil)
	})
}

// TestASeekStopsWithItsContext: the index walk a seek runs to its target ends
// with the seek's context, and a seek with a live one then lands where a
// fresh demuxer lands.
func TestASeekStopsWithItsContext(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "stereo.aac"))
	if err != nil {
		t.Fatal(err)
	}
	const target = 8 * 1024
	fresh, err := adts.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	want, err := fresh.SeekSample(t.Context(), 0, target)
	if err != nil {
		t.Fatal(err)
	}
	d, err := adts.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SeekSample(testutil.CancelAfterChecks(t, 2), 0, target); waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("a seek whose context ended returned %v, want canceled", err)
	}
	if got, err := d.SeekSample(t.Context(), 0, target); err != nil || got != want {
		t.Errorf("the seek after the cancel landed at %d (%v), a fresh demuxer at %d", got, err, want)
	}
}
