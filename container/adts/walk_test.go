package adts

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// TestWalkFindsJunkBetweenFrames pins container.Walker on an ADTS stream:
// junk past the head is not seen at open (the index is lazy and the SBR probe
// stops at the first confirmed frames), a tolerant Walk drives the index to
// the end and leaves the finding in Warnings, Walked flips with it, and a
// strict Walk refuses with the malformed error, which is what a strict probe
// returns for the file.
//
// The finding is the junk alone, at the junk: the frame ahead of it is a
// whole frame the last header's length pointed at, and it is kept, the way
// the MP3 walker keeps it. The index's sequential step does not require the
// header behind a frame; only the resync scan confirms its candidates that
// way.
func TestWalkFindsJunkBetweenFrames(t *testing.T) {
	raw := fixture(t, "stereo.aac")
	frameLen := func(off int) int { return int(raw[off+3]&3)<<11 | int(raw[off+4])<<3 | int(raw[off+5])>>5 }
	off := 0
	for range 5 {
		off += frameLen(off)
	}
	junk := bytes.Repeat([]byte{0x11}, 16)
	damaged := append(append(append([]byte(nil), raw[:off]...), junk...), raw[off:]...)
	want := container.Warning{Offset: int64(off), Kind: container.Damage,
		Msg: fmt.Sprintf("%d unparsable bytes skipped", len(junk))}

	d, err := NewDemuxer(container.BytesSource(damaged), nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Walked() || len(d.Warnings()) != 0 {
		t.Fatalf("at open: walked = %v, warnings = %v; the head is clean and the index lazy", d.Walked(), d.Warnings())
	}
	if err := d.Walk(); err != nil {
		t.Fatalf("a tolerant Walk failed: %v", err)
	}
	if !d.Walked() {
		t.Error("not Walked after Walk")
	}
	if !slices.Contains(d.Warnings(), want) {
		t.Errorf("warnings after Walk = %v, want %v", d.Warnings(), want)
	}

	strict, err := NewDemuxer(container.BytesSource(damaged), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("strict refused at open, before the walk reached the junk: %v", err)
	}
	if err := strict.Walk(); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Errorf("strict Walk returned %v, want malformed", err)
	}

	// A clean stream walks to its end with nothing to say.
	clean, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := clean.Walk(); err != nil || !clean.Walked() || len(clean.Warnings()) != 0 {
		t.Errorf("clean stream: Walk = %v, Walked = %v, warnings = %v", err, clean.Walked(), clean.Warnings())
	}
}
