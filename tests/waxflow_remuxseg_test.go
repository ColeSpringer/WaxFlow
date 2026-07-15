package waxflow_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
)

// collectRemuxSegments runs a segmented remux and returns the segments.
func collectRemuxSegments(t *testing.T, e *waxflow.Engine, raw []byte, hint string,
	opts waxflow.TranscodeOptions, segSamples int, start int64) []mp4.Segment {
	t.Helper()
	var segs []mp4.Segment
	_, err := e.RemuxSegments(context.Background(), container.BytesSource(raw), hint, opts,
		waxflow.SegmentedOptions{SegmentSamples: segSamples, StartSegment: start},
		func(s mp4.Segment) error {
			segs = append(segs, s)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return segs
}

// TestPacketGridReadsTheSourcesOwnGrid pins the fact the segmented rung stands
// on: an Opus stream our encoder wrote is 960 samples per packet, and the walk
// finds it without decoding anything.
func TestPacketGridReadsTheSourcesOwnGrid(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus"}, 48000)
	e := waxflow.New()
	grid, err := e.PacketGrid(container.BytesSource(src), "opus")
	if err != nil {
		t.Fatal(err)
	}
	if grid != 960 {
		t.Errorf("PacketGrid = %d, want 960 (20 ms at 48 kHz, the encoder-native frame)", grid)
	}
}

// TestRemuxSegmentsSnapToThePacketGrid is this milestone's alignment gate, and
// it pins a correction to the plan rather than the rule the plan named.
//
// The plan feared that a 60 ms-frame Opus source has no whole-packet boundary
// in a 4 s segment (192000/2880 = 66.67) and concluded such a request must fall
// to rung 3. But the segment length was never fixed at the request's ask:
// PlanSegments already snaps it to whole frames, which is precisely why a
// transcode's grid "is ours by construction". Feeding it the source's packet
// duration makes the same snap produce 67 packets of 2880, aligned, with no new
// rule. So the assertion is that the request is *served*, on a grid that
// divides, not that it is declined.
func TestRemuxSegmentsSnapToThePacketGrid(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus"}, 48000)
	e := waxflow.New()
	track := probeTrack(t, src, "opus")

	for _, tc := range []struct {
		name string
		grid int
	}{
		{"960 divides a 4s segment exactly", 960},
		// The plan's own example: 192000/2880 = 66.67, no whole-packet boundary
		// at 4 s, which the snap resolves to 67 packets rather than a refusal.
		{"2880 (60ms) divides no round segment length", 2880},
		{"5760 is Opus at its 120ms maximum", 5760},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := e.PlanRemuxSegments(track, waxflow.TranscodeOptions{Format: "opus"}, 4.0, tc.grid)
			if err != nil {
				t.Fatal(err)
			}
			if plan == nil {
				t.Fatal("PlanRemuxSegments declined a grid it can snap to")
			}
			if plan.SegmentSamples%tc.grid != 0 {
				t.Fatalf("segment of %d samples is not a whole number of %d-sample packets",
					plan.SegmentSamples, tc.grid)
			}
			if plan.SegmentSamples <= 0 {
				t.Fatalf("segment length %d", plan.SegmentSamples)
			}
		})
	}
}

// TestRemuxSegmentsDeclineVaryingPackets pins the decline that is real. A source
// whose packet durations vary has no grid to snap to, so mp4.Segmenter's
// tfdt = index * SegmentSamples would stop describing the packets it holds, and
// every later segment would carry a decode time that is a lie with no error
// anywhere. That is the silent failure; declining to rung 3 is the answer.
func TestRemuxSegmentsDeclineVaryingPackets(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus"}, 48000)
	track := probeTrack(t, src, "opus")
	e := waxflow.New()
	plan, err := e.PlanRemuxSegments(track, waxflow.TranscodeOptions{Format: "opus"}, 4.0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if plan != nil {
		t.Fatal("PlanRemuxSegments accepted a source with no uniform packet grid; its tfdt arithmetic would silently drift")
	}
}

// TestRemuxSegmentsShortFinalPacket pins the case the Opus fixtures cannot
// reach, and which shipped broken until a reviewer pointed at the seam.
//
// A stream's last packet is routinely short: FLAC flushes 1696 samples of a
// 4096-sample grid at the end of this fixture. That is not a hole in the grid,
// it is the stream ending, and it lands in the short final segment where it
// belongs. Every Opus test here passes over it blind, because Opus pads its
// final packet to full length, so a grid check that rejected the last packet
// looked correct across the entire suite while rejecting every lossless HLS
// remux at its final frame.
func TestRemuxSegmentsShortFinalPacket(t *testing.T) {
	// 100000 is 24 whole 4096-sample blocks plus 1696: a deliberately unround
	// length, so the last packet cannot be full.
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "flac"}, 100000)
	e := waxflow.New()
	opts := waxflow.TranscodeOptions{Format: "flac"}
	track := probeTrack(t, src, "flac")
	grid, err := e.PacketGrid(container.BytesSource(src), "flac")
	if err != nil {
		t.Fatal(err)
	}
	if grid != 4096 {
		t.Fatalf("PacketGrid = %d, want 4096: the fixture is not the shape this pins", grid)
	}
	plan, err := e.PlanRemuxSegments(track, opts, 1.0, grid)
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil {
		t.Fatal("PlanRemuxSegments declined a plain FLAC source")
	}
	segs := collectRemuxSegments(t, e, src, "flac", opts, plan.SegmentSamples, 0)
	if int64(len(segs)) != plan.Segments {
		t.Fatalf("the run emitted %d segments, the playlist promised %d", len(segs), plan.Segments)
	}
	// The decode durations must sum to the whole stream: the short tail is
	// carried, not dropped.
	var total int64
	for _, s := range segs {
		total += s.Samples
	}
	if total != track.Samples {
		t.Fatalf("segments carry %d samples, the source has %d", total, track.Samples)
	}
}

// TestRemuxSegmentsRestartIsByteIdentical is the segmented rung's determinism
// gate, the sibling of TestSegmentedRestartFLAC.
//
// It should be the easiest such guarantee in the tree to hold, and saying why is
// the point: priming exists to settle a resampler's window and an encoder's
// cross-frame state, and this rung has neither. The packets are the source's,
// already independently decodable, so a restarted worker's segment n is built
// from the same bytes a continuous run's was. A failure here would mean the
// segmenter's own index arithmetic drifted, which is exactly what the grid rule
// exists to prevent.
func TestRemuxSegmentsRestartIsByteIdentical(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus"}, 240000)
	e := waxflow.New()
	opts := waxflow.TranscodeOptions{Format: "opus"}
	track := probeTrack(t, src, "opus")
	grid, err := e.PacketGrid(container.BytesSource(src), "opus")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := e.PlanRemuxSegments(track, opts, 1.0, grid)
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil {
		t.Fatal("PlanRemuxSegments declined a plain Opus source")
	}

	full := collectRemuxSegments(t, e, src, "opus", opts, plan.SegmentSamples, 0)
	if len(full) < 3 {
		t.Fatalf("fixture yielded %d segments; need several to restart into", len(full))
	}
	tail := collectRemuxSegments(t, e, src, "opus", opts, plan.SegmentSamples, 1)
	if len(tail) != len(full)-1 {
		t.Fatalf("restart yielded %d segments, want %d", len(tail), len(full)-1)
	}
	for i, s := range tail {
		if s.Index != full[i+1].Index {
			t.Fatalf("restarted segment %d is numbered %d", i, s.Index)
		}
		if !bytes.Equal(s.Data, full[i+1].Data) {
			t.Fatalf("restarted segment %d differs from the continuous run's", s.Index)
		}
	}
}

// TestRemuxSegmentsCountMatchesThePlan pins the promise a VOD playlist makes:
// the plan states a segment count before a byte is produced, and the run must
// produce exactly that many, or the playlist 404s at its tail or ends early.
func TestRemuxSegmentsCountMatchesThePlan(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus"}, 240000)
	e := waxflow.New()
	opts := waxflow.TranscodeOptions{Format: "opus"}
	track := probeTrack(t, src, "opus")
	grid, err := e.PacketGrid(container.BytesSource(src), "opus")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := e.PlanRemuxSegments(track, opts, 1.0, grid)
	if err != nil {
		t.Fatal(err)
	}
	segs := collectRemuxSegments(t, e, src, "opus", opts, plan.SegmentSamples, 0)
	if int64(len(segs)) != plan.Segments {
		t.Fatalf("the run emitted %d segments, the playlist promised %d", len(segs), plan.Segments)
	}
	// The init header must describe these segments, and it must build at all.
	init, err := e.RemuxInitSegment(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(init) == 0 {
		t.Fatal("empty init segment")
	}
}
