package adts_test

import (
	"context"
	"errors"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/adts"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// TestWalkSurfacesAReadFailure pins that a read failing in the middle of the
// walk comes back as the error, from a stream long enough that the failure
// lands past the first window: the combined frame-and-header read used to
// slice a nil view there and panic.
func TestWalkSurfacesAReadFailure(t *testing.T) {
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	raw, _ := encodeADTS(t, f, museSrc(44100*24, 2, 44100))
	if len(raw) < 2*srcwin.Chunk {
		t.Fatalf("the stream is %d bytes, want over two windows", len(raw))
	}
	// The tail still reads: the open probes it for trailers.
	src := &testutil.FailingSource{Src: container.BytesSource(raw), From: int64(len(raw)) - srcwin.Chunk/2, To: int64(len(raw)) - 4096}
	d, err := adts.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Walk(context.Background()); !errors.Is(err, waxerr.ErrSourceUnreadable) {
		t.Fatalf("Walk = %v, want the read failure as source-unreadable", err)
	}
	if d.Walked() {
		t.Error("a walk stopped by a read failure claims to be finished")
	}
}

// TestAFailedReadNeverFinishesTheWalk: a walk a failed read stopped is not
// finished, and the next walk and the next seek return the error again. The
// frames counted before the failure are never settled as the stream's length.
func TestAFailedReadNeverFinishesTheWalk(t *testing.T) {
	raw := lcFrames(2000)
	src := &testutil.FailingSource{Src: container.BytesSource(raw), From: int64(len(raw))}
	d, err := adts.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Index 1,500 frames, then read at the head: the window has left the last
	// indexed frame, so the walk's next read is that frame's header.
	ctx := context.Background()
	for _, target := range []int64{1500 * 1024, 0} {
		if _, err := d.SeekSample(ctx, 0, target); err != nil {
			t.Fatal(err)
		}
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil || src.Failed != 0 {
		t.Fatalf("ReadPacket = %v after %d failed reads, want a source that still reads", err, src.Failed)
	}
	src.From = 0 // the source goes down
	for _, call := range []string{"the first Walk", "the second Walk"} {
		err := d.Walk(ctx)
		if tr := d.Tracks()[0]; !errors.Is(err, waxerr.ErrSourceUnreadable) || d.Walked() || tr.SamplesExact {
			t.Errorf("%s = %v, Walked = %v, length %d exact %v; want the read's error and nothing settled",
				call, err, d.Walked(), tr.Samples, tr.SamplesExact)
		}
	}
	if landed, err := d.SeekSample(ctx, 0, 1900*1024); !errors.Is(err, waxerr.ErrSourceUnreadable) {
		t.Errorf("a seek past the indexed frames landed at %d with %v, want the read's error", landed, err)
	}
	// An indexed frame: the window's error from the walk is the packet's, and
	// it carries the cause.
	if _, err := d.SeekSample(ctx, 0, 1000*1024); err != nil {
		t.Fatal(err)
	}
	if err := d.ReadPacket(&pkt); !errors.Is(err, testutil.ErrSourceDown) {
		t.Errorf("reading an indexed frame returned %v, want the read's error", err)
	}
}
