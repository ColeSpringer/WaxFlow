package mpa_test

import (
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mpa"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// TestASeekStopsWithItsContext: the index walk a seek runs to its target ends
// with the seek's context, and a seek with a live one then lands where a
// fresh demuxer lands.
func TestASeekStopsWithItsContext(t *testing.T) {
	raw := mp3Fixture(t, "sine-cbr128.mp3")
	const target = 15 * 1152
	fresh, err := mpa.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	want, err := fresh.SeekSample(t.Context(), 0, target)
	if err != nil {
		t.Fatal(err)
	}
	d, err := mpa.NewDemuxer(container.BytesSource(raw), nil)
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
