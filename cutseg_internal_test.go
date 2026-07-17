package waxflow

import (
	"testing"

	"github.com/colespringer/waxflow/container"
)

// seekableGridDemuxer is gridDemuxer plus SeekSample: it lands on the packet
// boundary at or before the target, exactly as a real container's seek does for
// an all-sync-point codec (Opus, AAC-LC), which is what the cut allowlist is.
type seekableGridDemuxer struct{ gridDemuxer }

func (d *seekableGridDemuxer) SeekSample(track int, sample int64) (int64, error) {
	if sample < 0 {
		sample = 0
	}
	k := sample / d.dur // the packet boundary at or before the target
	if k > int64(d.n) {
		k = int64(d.n)
	}
	d.i = int(k)
	d.pos = k * d.dur
	return d.pos, nil
}

// TestCutSeekMapsOutputTargetToSource is the coordinate-mapping proof, the risk
// this whole workstream turns on. A seek addresses the cut's own contiguous
// output timeline; the view must map that back through the kept window to a
// source decode position, seek the inner demuxer there, and resume the retiming
// so the next packet a restarted worker reads is byte-identical to a continuous
// run's.
//
// The span [20480, 61440) on a 1024 grid drops the first twenty packets. The head
// backs off by one frame of pre-roll (aac.EncoderDelay = 1024), so window 0 begins
// at source packet 19 (sd = snapDown(20480-1024) = 19*1024 = 19456). Every source
// packet carries its own ordinal as its payload, so a mid-stream seek's landing is
// checkable by sight.
func TestCutSeekMapsOutputTargetToSource(t *testing.T) {
	track := aacTrack(0, 96000)
	spans := []Span{{20480, 61440}}
	const grid = 1024

	view, err := cutSeekable(&seekableGridDemuxer{gridDemuxer{n: 94, dur: grid}}, track, spans, grid)
	if err != nil {
		t.Fatal(err)
	}
	sk, ok := view.(container.Seeker)
	if !ok {
		t.Fatal("cutSeekable did not return a Seeker")
	}

	for _, tc := range []struct {
		name       string
		outTarget  int64
		wantLanded int64
		wantOrd    byte // the source packet the next read must yield
	}{
		// Segment 0's own start, reached through the seek: output 0 maps to the
		// window's start, which is source packet 19 (the backed-off head), retimed
		// to output 0.
		{"output 0 maps to the window head", 0, 0, 19},
		// A mid-stream boundary: output 10240 maps to source 19456+10240 = 29696 =
		// packet 29, a grid boundary, so the inner seek lands exactly and the
		// returned output position is the target unchanged.
		{"mid-stream boundary lands exactly", 10240, 10240, 29},
	} {
		t.Run(tc.name, func(t *testing.T) {
			landed, err := sk.SeekSample(track.ID, tc.outTarget)
			if err != nil {
				t.Fatal(err)
			}
			if landed != tc.wantLanded {
				t.Fatalf("seek to output %d landed at output %d, want %d", tc.outTarget, landed, tc.wantLanded)
			}
			if landed > tc.outTarget {
				t.Fatalf("landed output %d is past the target %d; seekPackets would reject it", landed, tc.outTarget)
			}
			var pkt container.Packet
			if err := view.ReadPacket(&pkt); err != nil {
				t.Fatalf("read after seek: %v", err)
			}
			if pkt.Data[0] != tc.wantOrd {
				t.Errorf("first packet after seek is source packet %d, want %d", pkt.Data[0], tc.wantOrd)
			}
			// The retimed PTS must continue from the landing, so a restarted worker's
			// segmenter lays the same decode times a continuous run's does.
			if pkt.PTS != landed {
				t.Errorf("first packet after seek has PTS %d, want the landed output %d", pkt.PTS, landed)
			}
		})
	}
}

// TestCutSeekMapsMultiWindow pins the coordinate mapping for a cut of more than
// one span, the path the single-window HTTP surface never exercises but the public
// CutSegments/PlanCutSegments []Span API reaches. Two things appear only with a
// second window: the outStart accumulation that carries a seek into window i>0,
// and the pre-window clamp interacting with a non-zero outStart.
//
// The spans mirror TestCutRetimesPacketsContiguously: window 0 is source packets
// 0..19 (output [0, 20480)) and window 1 is source packets 39..59 (output [20480,
// 41984)), window 1's head backed off by one frame of pre-roll to source 39.
func TestCutSeekMapsMultiWindow(t *testing.T) {
	track := aacTrack(0, 96000)
	spans := []Span{{0, 20480}, {40960, 61440}}
	const grid = 1024

	// An exact landing into window 1: output 25600 is five packets past the
	// window-1 boundary at output 20480, so it maps to source packet 44 and the
	// sync-point seek lands there exactly.
	t.Run("exact landing into window 1", func(t *testing.T) {
		view, err := cutSeekable(&seekableGridDemuxer{gridDemuxer{n: 94, dur: grid}}, track, spans, grid)
		if err != nil {
			t.Fatal(err)
		}
		landed, err := view.(container.Seeker).SeekSample(track.ID, 25600)
		if err != nil {
			t.Fatal(err)
		}
		if landed != 25600 {
			t.Fatalf("landed at output %d, want 25600", landed)
		}
		var pkt container.Packet
		if err := view.ReadPacket(&pkt); err != nil {
			t.Fatal(err)
		}
		if pkt.Data[0] != 44 || pkt.PTS != 25600 {
			t.Errorf("first packet is source %d at PTS %d, want source 44 at PTS 25600", pkt.Data[0], pkt.PTS)
		}
	})

	// A coarse landing into window 1: the demuxer lands at 0, well before window
	// 1's own start, so the output offset must clamp to window 1's output start
	// (20480, a non-zero outStart) rather than go negative. The walk then skips
	// forward to window 1's head.
	t.Run("coarse landing clamps at window 1 start", func(t *testing.T) {
		view, err := cutSeekable(&coarseSeekDemuxer{gridDemuxer{n: 94, dur: grid}}, track, spans, grid)
		if err != nil {
			t.Fatal(err)
		}
		landed, err := view.(container.Seeker).SeekSample(track.ID, 25600)
		if err != nil {
			t.Fatal(err)
		}
		if landed != 20480 {
			t.Fatalf("coarse landing reported output %d, want window 1's start 20480 (a non-zero outStart clamp)", landed)
		}
		if landed > 25600 {
			t.Fatalf("landed %d past the target 25600", landed)
		}
		var pkt container.Packet
		if err := view.ReadPacket(&pkt); err != nil {
			t.Fatal(err)
		}
		if pkt.Data[0] != 39 || pkt.PTS != 20480 {
			t.Errorf("first packet is source %d at PTS %d, want window 1's head source 39 at PTS 20480", pkt.Data[0], pkt.PTS)
		}
	})
}

// coarseSeekDemuxer is gridDemuxer whose seek always lands at the very start,
// which is what a page-granular container (Ogg) routinely does: the returned
// landing is well before the target, even before the cut window's own start.
type coarseSeekDemuxer struct{ gridDemuxer }

func (d *coarseSeekDemuxer) SeekSample(track int, sample int64) (int64, error) {
	d.i, d.pos = 0, 0
	return 0, nil
}

// TestCutSeekClampsAPreWindowLanding pins the fix for the coarse-seek case: when
// the inner seek lands before the window start, the output offset must clamp to
// the window's own output start rather than go negative, so the walk resumes at
// the head and its own skip carries it to the boundary. A negative seed
// desynced the cursor from the output position and silently dropped a window's
// worth of audio from every restarted segment.
func TestCutSeekClampsAPreWindowLanding(t *testing.T) {
	track := aacTrack(0, 96000)
	spans := []Span{{20480, 61440}} // window 0 begins at source 19456 (the backed-off head)
	const grid = 1024

	view, err := cutSeekable(&coarseSeekDemuxer{gridDemuxer{n: 94, dur: grid}}, track, spans, grid)
	if err != nil {
		t.Fatal(err)
	}
	// Seek to a mid-stream output boundary the coarse demuxer cannot reach.
	landed, err := view.(container.Seeker).SeekSample(track.ID, 10240)
	if err != nil {
		t.Fatal(err)
	}
	// The landing is clamped to the window's output start (0 here), never negative,
	// and is at or before the target so seekPackets accepts it.
	if landed != 0 {
		t.Fatalf("pre-window landing reported output %d, want the clamped 0", landed)
	}
	// The next packet is the window's own head (source packet 19), retimed to
	// output 0; the caller's skip loop then walks forward to output 10240.
	var pkt container.Packet
	if err := view.ReadPacket(&pkt); err != nil {
		t.Fatal(err)
	}
	if pkt.Data[0] != 19 || pkt.PTS != 0 {
		t.Errorf("after a clamped seek the first packet is source %d at PTS %d, want source 19 at PTS 0",
			pkt.Data[0], pkt.PTS)
	}
}
