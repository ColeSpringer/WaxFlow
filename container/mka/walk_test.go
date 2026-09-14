package mka

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// TestWalkKeepsThePacketPosition pins container.Walker's position promise on
// the one demuxer whose walk drives the packet reader's own cursor: a Walk
// after packets were delivered leaves the next packet where it would have
// been, and Walked flips. Without the save around the walk the reader was
// left on the first cluster and the stream played from sample 0 again under
// positions that kept counting up.
func TestWalkKeepsThePacketPosition(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "seed-pcm.mka"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Walked() {
		t.Fatal("a PCM Matroska is walked at open; this cell needs a lazy one")
	}
	// One packet of the fixture's three, so the walk runs with the reader
	// inside the stream and the next packet is the second one.
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil {
		t.Fatal(err)
	}
	last := pkt.PTS + pkt.Dur
	if err := d.Walk(); err != nil {
		t.Fatal(err)
	}
	if !d.Walked() {
		t.Error("not Walked after Walk")
	}
	if err := d.ReadPacket(&pkt); err != nil {
		t.Fatal(err)
	}
	if pkt.PTS != last {
		t.Errorf("the packet after Walk starts at %d, want %d: the walk moved the reader", pkt.PTS, last)
	}
}
