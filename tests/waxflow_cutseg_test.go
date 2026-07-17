package waxflow_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/format"
)

// collectCutSegments runs a segmented cut and returns the segments it emitted.
func collectCutSegments(t *testing.T, e *waxflow.Engine, raw []byte, hint string,
	opts waxflow.TranscodeOptions, spans []waxflow.Span, grid int, samples int64, segSamples int, start int64) []mp4.Segment {
	t.Helper()
	var segs []mp4.Segment
	_, err := e.CutSegments(context.Background(), container.BytesSource(raw), hint, opts, spans, grid, samples,
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

// planCutSegments plans a segmented Opus cut of a fixture, failing on a decline so
// a test that means to exercise the rung cannot silently measure nothing.
func planCutSegments(t *testing.T, e *waxflow.Engine, src []byte, opts waxflow.TranscodeOptions,
	spans []waxflow.Span, segSeconds float64) (*waxflow.CutSegmentPlan, int) {
	t.Helper()
	track := probeTrack(t, src, "opus")
	grid, err := e.PacketGrid(container.BytesSource(src), "opus")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := e.PlanCutSegments(track, opts, spans, grid, segSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil {
		t.Fatal("PlanCutSegments declined a plain Opus cut it must serve")
	}
	return plan, grid
}

// TestPlanCutSegmentsMapsDeclinesAndErrors pins the segmented cut's entry to the
// ladder: a request this rung cannot serve is a (nil, nil) decline the caller
// falls through on, and a request no rung can serve is an error rung 3 hits
// identically. It is the segmented mirror of TestPlanCutMapsCodesOntoTheLadder.
func TestPlanCutSegmentsMapsDeclinesAndErrors(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus"}, 96000)
	e := waxflow.New()
	track := probeTrack(t, src, "opus")
	grid, err := e.PacketGrid(container.BytesSource(src), "opus")
	if err != nil {
		t.Fatal(err)
	}

	// A servable cut plans, carries the cut version, and threads the grid and the
	// source length the run must reproduce.
	plan, err := e.PlanCutSegments(track, waxflow.TranscodeOptions{Format: "opus"}, []waxflow.Span{{From: 0, To: 48000}}, grid, 1.0)
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil {
		t.Fatal("PlanCutSegments declined a plain Opus cut it can serve")
	}
	if plan.Grid != grid || plan.SourceSamples != track.Samples {
		t.Errorf("plan threads Grid=%d SourceSamples=%d, want %d and %d", plan.Grid, plan.SourceSamples, grid, track.Samples)
	}
	if len(plan.Landed) != 1 {
		t.Errorf("Landed = %v, want one span for one", plan.Landed)
	}
	var haveCut bool
	for _, v := range plan.Versions {
		if v == waxflow.CutVersion {
			haveCut = true
		}
	}
	if !haveCut {
		t.Errorf("Versions = %v, want %s among them", plan.Versions, waxflow.CutVersion)
	}

	// A codec mismatch declines (nil, nil): a FLAC output cannot hold Opus packets,
	// so PlanRemux inside declines and the caller transcodes. This is the
	// reachability fact: the cut needs a source-matching format.
	plan, err = e.PlanCutSegments(track, waxflow.TranscodeOptions{Format: "flac"}, []waxflow.Span{{From: 0, To: 48000}}, grid, 1.0)
	if err != nil || plan != nil {
		t.Errorf("PlanCutSegments(format mismatch) = (%v, %v), want (nil, nil): a decline is not an error", plan, err)
	}

	// A zero-length span errors: rung 3 refuses it identically, so it is not a
	// decline the caller should swallow into a re-encode.
	if _, err := e.PlanCutSegments(track, waxflow.TranscodeOptions{Format: "opus"}, []waxflow.Span{{From: 100, To: 100}}, grid, 1.0); err == nil {
		t.Error("PlanCutSegments(zero-length span) returned no error")
	}
}

// TestCutSegmentsRestartIsByteIdentical is the segmented cut's determinism gate
// and the proof its coordinate mapping is right, the sibling of
// TestRemuxSegmentsRestartIsByteIdentical.
//
// A worker restarted at a mid-stream segment must reproduce a continuous run's
// segment bytes for bytes. For a cut this is the whole risk: the restart seeks the
// cut's output timeline, which the seekable view has to map back to a source decode
// position without injecting pre-roll. A drift of one packet, or a pre-roll backed
// off into a mid-stream segment, shows up here as a byte diff.
func TestCutSegmentsRestartIsByteIdentical(t *testing.T) {
	// 240000 frames is 5 s at 48 kHz; the Opus fixture inherits it, and the grid is
	// 960 (20 ms). Keep [48000, 192000): drop the first and last second, on whole
	// packet boundaries.
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus"}, 240000)
	e := waxflow.New()
	opts := waxflow.TranscodeOptions{Format: "opus"}
	spans := []waxflow.Span{{From: 48000, To: 192000}}
	plan, grid := planCutSegments(t, e, src, opts, spans, 1.0)

	full := collectCutSegments(t, e, src, "opus", opts, spans, grid, plan.SourceSamples, plan.SegmentSamples, 0)
	if int64(len(full)) != plan.Segments {
		t.Fatalf("the run emitted %d segments, the plan promised %d", len(full), plan.Segments)
	}
	if len(full) < 3 {
		t.Fatalf("the cut yielded %d segments; need several to restart into", len(full))
	}
	tail := collectCutSegments(t, e, src, "opus", opts, spans, grid, plan.SourceSamples, plan.SegmentSamples, 1)
	if len(tail) != len(full)-1 {
		t.Fatalf("restart yielded %d segments, want %d", len(tail), len(full)-1)
	}
	for i, s := range tail {
		if s.Index != full[i+1].Index {
			t.Fatalf("restarted cut segment %d is numbered %d", i, s.Index)
		}
		if !bytes.Equal(s.Data, full[i+1].Data) {
			t.Fatalf("restarted cut segment %d differs from the continuous run's: the seek mapped the wrong source position or injected pre-roll", s.Index)
		}
	}
}

// TestCutSegmentsAreTheSourcesOwnPackets is the "packets moved, not re-encoded"
// proof at the segment layer, and it also pins the init header. The whole
// presentation (init plus segments, concatenated) must demux back to a
// byte-identical run of the source's own Opus packets, which no re-encode could
// produce, and the init's edit list must carry the cut's synthesized delay rather
// than the source's own.
func TestCutSegmentsAreTheSourcesOwnPackets(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus"}, 240000)
	e := waxflow.New()
	opts := waxflow.TranscodeOptions{Format: "opus"}
	// A From past 0 forces a synthesized delay the source never had: the head backs
	// off by the 3840-sample pre-roll and the snap slop becomes the trim.
	spans := []waxflow.Span{{From: 48000, To: 192000}}
	plan, grid := planCutSegments(t, e, src, opts, spans, 1.0)

	srcTrack := probeTrack(t, src, "opus")
	if plan.Delay == srcTrack.Delay {
		t.Fatalf("the cut synthesized the same delay the source had (%d); this span cannot pin the init", plan.Delay)
	}

	init, err := e.CutInitSegment(plan)
	if err != nil {
		t.Fatal(err)
	}
	whole := append([]byte(nil), init...)
	for _, s := range collectCutSegments(t, e, src, "opus", opts, spans, grid, plan.SourceSamples, plan.SegmentSamples, 0) {
		whole = append(whole, s.Data...)
	}

	// The init's edit list carries the cut's delay, read back off the container.
	_, info, err := format.OpenDemuxer(container.BytesSource(whole), "mp4", nil)
	if err != nil {
		t.Fatalf("the cut presentation does not demux: %v", err)
	}
	if got := info.Default().Delay; got != plan.Delay {
		t.Errorf("the cut fMP4 carries Delay %d, want the cut's synthesized %d (not the source's %d)",
			got, plan.Delay, srcTrack.Delay)
	}

	// The payloads must be the source's own packets, in order, byte for byte. The
	// head backs off by the pre-roll, so the run starts a few packets before sample
	// 48000; those are still source packets (the delay trims them on playback), so a
	// walk that finds each emitted packet ahead in the source, in order, is the
	// check. A prefix comparison would pass on a cut that dropped only the tail.
	want := payloads(t, src, "opus")
	got := payloads(t, whole, "mp4")
	if len(got) == 0 {
		t.Fatal("the cut presentation holds no packets")
	}
	if len(got) >= len(want) {
		t.Fatalf("the cut holds %d of the source's %d packets; nothing was dropped", len(got), len(want))
	}
	var si int
	for gi, p := range got {
		for si < len(want) && !bytes.Equal(want[si], p) {
			si++
		}
		if si == len(want) {
			t.Fatalf("cut packet %d is not any source packet's bytes: this is re-encoded audio, not moved packets", gi)
		}
		si++
	}
	t.Logf("cut %d source packets to %d across segments, all byte-identical", len(want), len(got))
}
