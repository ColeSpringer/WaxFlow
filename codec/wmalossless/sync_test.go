package wmalossless_test

import (
	"testing"

	"github.com/colespringer/waxflow/codec/wmalossless"
)

// TestPacketIsSyncReadsTheHeaderFlag pins the bit position against the same
// header parse the decoder uses, so the two cannot drift.
func TestPacketIsSyncReadsTheHeaderFlag(t *testing.T) {
	cfg, err := wmalossless.ParseConfig(stereoConfig())
	if err != nil {
		t.Fatal(err)
	}
	for seq := 0; seq < 16; seq++ {
		for _, want := range []bool{false, true} {
			var w bitWriter
			w.put(uint32(seq), 4)
			if want {
				w.put(1, 1)
			} else {
				w.put(0, 1)
			}
			w.put(0, 1)
			w.put(0, cfg.FrameSizeBits())
			pkt := make([]byte, cfg.BlockAlign)
			copy(pkt, w.buf)
			if got := wmalossless.PacketIsSync(pkt); got != want {
				t.Fatalf("seq %d flag %v: PacketIsSync = %v", seq, want, got)
			}
			_, seek, _, _ := wmalossless.PacketHeader(pkt, cfg.FrameSizeBits())
			if seek != want {
				t.Fatalf("seq %d: the header parse disagrees with PacketIsSync", seq)
			}
		}
	}
	if wmalossless.PacketIsSync(nil) {
		t.Error("an empty packet is not a start point")
	}
}
