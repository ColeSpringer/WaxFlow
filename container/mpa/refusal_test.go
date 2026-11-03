package mpa_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mpa"
	"github.com/colespringer/waxflow/waxerr"
)

// TestAStrictRefusalIsKept: a strict walk that refuses a file refuses it
// again on the next Walk, never settling the length, on each arm the run can
// end on: trailing junk, a truncated final frame, and a Xing count the run
// falls short of.
func TestAStrictRefusalIsKept(t *testing.T) {
	raw := mp3Fixture(t, "sine-cbr128.mp3")
	offs := frameOffsets(t, raw)
	for _, tc := range []struct {
		name string
		file []byte
	}{
		{"trailing junk", append(append([]byte(nil), raw...), bytes.Repeat([]byte{0x11}, 16)...)},
		{"truncated final frame", raw[:len(raw)-100]},
		{"xing shortfall", raw[:offs[len(offs)-2]]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := mpa.NewDemuxer(container.BytesSource(tc.file), &mpa.DemuxerOptions{Strict: true})
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
		})
	}
}
