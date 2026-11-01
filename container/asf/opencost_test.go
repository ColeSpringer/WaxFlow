package asf_test

import (
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestOpenReadsTheHeaderObject pins what opening an ASF file costs: the
// Header Object, the Data Object's header and the probes for an index behind
// the packets, in exact reads, whatever the packet run's length. The files
// here run past two windows so a window-sized read would show.
func TestOpenReadsTheHeaderObject(t *testing.T) {
	var costs [2][2]int64
	for i, n := range []int{600, 1200} {
		b := newBuilder()
		data := make([]byte, 400)
		for j := range n {
			b.packet(uint32(j*20), byte(j), 0, uint32(len(data)), uint32(j*20), data)
		}
		raw := b.build()
		if len(raw) < 2*srcwin.Chunk {
			t.Fatalf("%d packets wrote %d bytes, want over two windows", n, len(raw))
		}
		src := &testutil.CountingSource{Src: container.BytesSource(raw)}
		if _, err := asf.NewDemuxer(src, nil); err != nil {
			t.Fatal(err)
		}
		t.Logf("%d packets (%d bytes): %d bytes in %d reads", n, len(raw), src.Bytes, src.Reads)
		if src.Bytes > 1024 {
			t.Errorf("opening a %d-packet file read %d bytes, want the header object and under 1 KiB more", n, src.Bytes)
		}
		costs[i] = [2]int64{int64(src.Reads), src.Bytes}
	}
	if costs[0] != costs[1] {
		t.Errorf("opening 600 packets cost %v (reads, bytes) and 1200 cost %v; the open must not scale with the packets", costs[0], costs[1])
	}
}

// TestACountedOpenLeavesTheIndexUnread: a WMA v1/v2 track lands by its frame
// counts and never asks the Simple Index, so its open reads nothing behind
// the packets, where an index costs a read at the far end of the file.
func TestACountedOpenLeavesTheIndexUnread(t *testing.T) {
	raw := appendSimpleIndex(readFixture(t, windowsFixture), 32, 1_000_0000)
	hdr := int64(le.Uint64(raw[16:]))
	dataEnd := hdr + int64(le.Uint64(raw[hdr+16:]))
	if dataEnd >= int64(len(raw)) {
		t.Fatal("nothing follows the packets")
	}
	src := &testutil.CountingSource{Src: container.BytesSource(raw), Log: true}
	if _, err := asf.NewDemuxer(src, nil); err != nil {
		t.Fatal(err)
	}
	for _, sp := range src.Spans {
		if sp[1] > dataEnd {
			t.Errorf("the open read [%d, %d), past the packets' end at %d", sp[0], sp[1], dataEnd)
		}
	}
}
