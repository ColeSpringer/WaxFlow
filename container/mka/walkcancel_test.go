package mka

import (
	"context"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// seekableMidPadding is BuildMidPadding written with Cues, so a seek takes a
// bounded walk.
func seekableMidPadding(t *testing.T) MidPadding {
	t.Helper()
	return buildMidPadding(t, MidPadding{
		Blocks: 500, BlockDur: 480, Mid: 100, MidPad: 48, EndPad: 96, Seekable: true,
	})
}

// pauseWalk opens p's file over a counting source and cancels a Walk on its
// checks-th step, returning the paused demuxer and its source.
func pauseWalk(t *testing.T, p MidPadding, checks int) (*Demuxer, *testutil.CountingSource) {
	t.Helper()
	src := &testutil.CountingSource{Src: container.BytesSource(p.File)}
	d, err := NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Walk(testutil.CancelAfterChecks(t, checks)); waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("Walk = %v, want canceled", err)
	}
	if d.walkFrames == 0 || d.walkFrames >= p.Blocks {
		t.Fatalf("the walk had counted %d of %d blocks when canceled, want a pause partway through", d.walkFrames, p.Blocks)
	}
	return d, src
}

// TestWalkPausesWithItsContext is container.Walker's cancel contract on the
// cluster walk (testutil.WalkPauses), over a file with a trim inside the run
// and one at its end.
func TestWalkPausesWithItsContext(t *testing.T) {
	p := BuildMidPadding(t)
	testutil.WalkPauses(t, p.File, 40, func(src container.Source) (testutil.Walkable, error) {
		return NewDemuxer(src, nil)
	})
}

// TestWalkPauseLeavesTheReader: a Walk canceled mid-read leaves the packet
// reader on the next packet, as a finished one does.
func TestWalkPauseLeavesTheReader(t *testing.T) {
	p := BuildMidPadding(t)
	d, err := NewDemuxer(container.BytesSource(p.File), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	for range 3 {
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Walk(testutil.CancelAfterChecks(t, 40)); waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("Walk = %v, want canceled", err)
	}
	if err := d.ReadPacket(&pkt); err != nil || pkt.PTS != p.PTS(3) {
		t.Errorf("the packet after a canceled Walk is at %d (%v), want the fourth at %d", pkt.PTS, err, p.PTS(3))
	}
	if err := d.Walk(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := d.ReadPacket(&pkt); err != nil || pkt.PTS != p.PTS(4) {
		t.Errorf("the packet after the resumed Walk is at %d (%v), want the fifth at %d", pkt.PTS, err, p.PTS(4))
	}
}

// TestASeekResumesAPausedWalk: the cursor a canceled walk left is the one a
// seek's bounded walk continues from. A seek the index already covers costs
// no read; one past the pause counts on from it, lands where a fresh demuxer
// lands and takes the Cues bound; the full walk after it settles what a fresh
// one does.
func TestASeekResumesAPausedWalk(t *testing.T) {
	p := seekableMidPadding(t)
	full, err := NewDemuxer(container.BytesSource(p.File), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := full.Walk(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := full.Tracks()[0]
	freshSrc := &testutil.CountingSource{Src: container.BytesSource(p.File)}
	fresh, err := NewDemuxer(freshSrc, nil)
	if err != nil {
		t.Fatal(err)
	}
	opened := freshSrc.Reads
	if _, err := fresh.SeekSample(context.Background(), 0, 0); err != nil {
		t.Fatal(err)
	}
	cuesCost := freshSrc.Reads - opened // the lazy Cues parse, which every first seek pays

	d, src := pauseWalk(t, p, 40)
	reads := src.Reads
	if landed, err := d.SeekSample(context.Background(), 0, 0); err != nil || landed != 0 {
		t.Fatalf("seek to 0 = %d, %v", landed, err)
	}
	if n := src.Reads - reads; n != cuesCost {
		t.Errorf("a seek to 0 after a paused walk read %d times, a fresh demuxer's %d: the index already covered it", n, cuesCost)
	}

	target := p.PTS(200)
	wantLand, err := fresh.SeekSample(context.Background(), 0, target)
	if err != nil {
		t.Fatal(err)
	}
	landed, err := d.SeekSample(context.Background(), 0, target)
	if err != nil || landed != wantLand {
		t.Errorf("seek to %d after a paused walk landed at %d (%v), a fresh demuxer at %d", target, landed, err, wantLand)
	}
	if d.walked {
		t.Error("the seek ran the full walk; the Cues bound was not taken after a pause")
	}
	if err := d.Walk(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := d.Tracks()[0]; got.Samples != want.Samples || got.Padding != want.Padding || got.MidPadding != want.MidPadding {
		t.Errorf("after the pause and the seek, the walk settled %+v, a fresh walk %+v", got, want)
	}
}

// TestASeekStopsWithItsContext: the full walk a seek runs on a file without
// Cues ends with the seek's context, and a seek with a live one then lands
// where a fresh demuxer lands.
func TestASeekStopsWithItsContext(t *testing.T) {
	p := BuildMidPadding(t)
	fresh, err := NewDemuxer(container.BytesSource(p.File), nil)
	if err != nil {
		t.Fatal(err)
	}
	target := p.PTS(400)
	want, err := fresh.SeekSample(t.Context(), 0, target)
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDemuxer(container.BytesSource(p.File), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SeekSample(testutil.CancelAfterChecks(t, 3), 0, target); waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("a seek whose context ended returned %v, want canceled", err)
	}
	if d.walked {
		t.Error("the walk ran to its end after its context ended")
	}
	if got, err := d.SeekSample(t.Context(), 0, target); err != nil || got != want {
		t.Errorf("the seek after the cancel landed at %d (%v), a fresh demuxer at %d", got, err, want)
	}
}
