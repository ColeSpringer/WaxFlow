package mpa

import (
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/mpegframes"
	"github.com/colespringer/waxflow/internal/testutil"
)

// openCounted opens raw through a counting source.
func openCounted(t *testing.T, raw []byte) (*Demuxer, *testutil.CountingSource) {
	t.Helper()
	src := &testutil.CountingSource{Src: container.BytesSource(raw)}
	d, err := NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	return d, src
}

// TestOpenReadsTheHead pins what opening a bare MP3 costs: the head of the
// run (the first header, one frame, the header behind it), whatever the
// stream's length; a leading ID3v2 tag adds its own size and nothing else;
// and restoring an index sidecar adds a header per probe rather than a
// window per probe. Every bare MP3 used to pay a whole 128 KiB window at
// open, and a sidecar restore nine of them.
func TestOpenReadsTheHead(t *testing.T) {
	var costs [2][2]int64
	for i, n := range []int{2000, 4000} {
		_, src := openCounted(t, testutil.SyntheticMP3Frames(n))
		t.Logf("%d frames: %d bytes in %d reads", n, src.Bytes, src.Reads)
		if src.Bytes > 2048 {
			t.Errorf("opening %d frames read %d bytes, want the head of the run, under 2 KiB", n, src.Bytes)
		}
		costs[i] = [2]int64{int64(src.Reads), src.Bytes}
	}
	if costs[0] != costs[1] {
		t.Errorf("opening 2000 frames cost %v (reads, bytes) and 4000 cost %v; the open must not scale with the stream", costs[0], costs[1])
	}

	// An ID3v2 tag ahead of the frames: the header, then a body of padding.
	const body = 3000
	tag := append([]byte{'I', 'D', '3', 4, 0, 0, 0, 0, byte(body >> 7), byte(body & 0x7F)}, make([]byte, body)...)
	_, src := openCounted(t, append(tag, testutil.SyntheticMP3Frames(2000)...))
	t.Logf("tagged: %d bytes in %d reads", src.Bytes, src.Reads)
	if extra := src.Bytes - costs[0][1]; extra < 0 || extra > int64(len(tag)) {
		t.Errorf("a %d-byte tag added %d bytes to the open, want no more than the tag itself", len(tag), extra)
	}

	// A restored sidecar: a header per probe, exact.
	run := testutil.SyntheticMP3Frames(mpegframes.IdxMinFrames + 50)
	full, _ := openCounted(t, run)
	var pkt container.Packet
	for {
		if err := full.ReadPacket(&pkt); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	blob := full.IndexSnapshot()
	if blob == nil {
		t.Fatal("no snapshot after a full walk")
	}
	d, src := openCounted(t, run)
	opened := src.Bytes
	src.Reset()
	if !d.RestoreIndex(blob) {
		t.Fatal("the snapshot was rejected by an identical source")
	}
	t.Logf("restore: %d bytes in %d reads", src.Bytes, src.Reads)
	if src.Bytes > 2048 || src.Reads > 12 {
		t.Errorf("restoring the index read %d bytes in %d reads on top of the %d-byte open, want a header per probe", src.Bytes, src.Reads, opened)
	}
}
