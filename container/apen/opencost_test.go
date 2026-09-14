package apen_test

import (
	"testing"

	"github.com/colespringer/waxflow/codec/ape"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/apen"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestOpenReadsTheHeaderAndSeekTable pins what opening an APE file costs:
// the file header, the seek table (four bytes a frame, inside the header
// read at these lengths) and the trailer probes, in exact reads. The two
// files differ only in their frame count, and the open costs the same for
// both and nowhere near a window.
func TestOpenReadsTheHeaderAndSeekTable(t *testing.T) {
	var costs [2][2]int64
	frames := [2]int{8, 16}
	for i, n := range frames {
		track, pkts := muxFrames(t, n*ape.BlocksPerFrame, int64(n*ape.BlocksPerFrame))
		raw := writeAll(t, track, pkts, nil)
		if len(raw) < 2*srcwin.Chunk {
			t.Fatalf("%d frames wrote %d bytes, want over two windows", n, len(raw))
		}
		src := &testutil.CountingSource{Src: container.BytesSource(raw)}
		if _, err := apen.NewDemuxer(src, nil); err != nil {
			t.Fatal(err)
		}
		t.Logf("%d frames (%d bytes): %d bytes in %d reads", n, len(raw), src.Bytes, src.Reads)
		if src.Bytes >= srcwin.Chunk {
			t.Errorf("opening a %d-frame file read %d bytes, want under a window", n, src.Bytes)
		}
		costs[i] = [2]int64{int64(src.Reads), src.Bytes}
	}
	if costs[0] != costs[1] {
		t.Errorf("opening %d frames cost %v (reads, bytes) and %d frames cost %v; the open must not scale with the frames",
			frames[0], costs[0], frames[1], costs[1])
	}
}
