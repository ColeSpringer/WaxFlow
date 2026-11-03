package adts

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// TestAStrictRefusalIsKept: a strict walk that refuses the trailing junk
// refuses it again on the next Walk, never settling the length.
func TestAStrictRefusalIsKept(t *testing.T) {
	raw := append(fixture(t, "stereo.aac"), bytes.Repeat([]byte{0x11}, 16)...)
	d, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	first := d.Walk(t.Context())
	if !errors.Is(first, waxerr.ErrMalformedInput) {
		t.Fatalf("first Walk = %v, want malformed", first)
	}
	if d.Walked() {
		t.Error("Walked after a walk that refused")
	}
	if again := d.Walk(t.Context()); again == nil || again.Error() != first.Error() {
		t.Errorf("the second Walk = %v, want the first one's %v again", again, first)
	}
	if d.Walked() {
		t.Error("Walked after the second refused walk")
	}
	if tr := d.Tracks()[0]; tr.SamplesExact {
		t.Errorf("a refused walk settled the length at %d", tr.Samples)
	}
}

// TestADeadContextStopsNoSeekThatWalksNothing: a seek on a finished index
// walks nothing and lands whatever its context says.
func TestADeadContextStopsNoSeekThatWalksNothing(t *testing.T) {
	d, err := NewDemuxer(container.BytesSource(fixture(t, "stereo.aac")), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	want, err := d.SeekSample(context.Background(), 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := d.SeekSample(ctx, 0, 1<<40); err != nil || got != want {
		t.Errorf("a seek past the end of a finished index under an ended context = %d, %v; want %d and nil", got, err, want)
	}
}
