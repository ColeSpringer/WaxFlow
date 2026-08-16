package aac

import (
	"math"
	"testing"
)

// TestApplyHeaderBadDeriveConceals: a header whose band tables cannot be
// derived mutes the tool entirely. Keeping the old tables live would parse
// the new header's data against the wrong band counts.
func TestApplyHeaderBadDeriveConceals(t *testing.T) {
	el := buildTestElement(t)
	bad := el.hdr
	bad.startFreq, bad.stopFreq = 15, 0
	if _, err := deriveFreqTables(bad, 48000); err == nil {
		t.Fatal("the test header must be underivable; pick another shape")
	}
	el.applyHeader(bad)
	if el.haveTbl || el.haveHdr {
		t.Error("an underivable header left stale tables live")
	}
	good := buildTestElement(t).hdr
	el.applyHeader(good)
	if !el.haveTbl || !el.pendingReset {
		t.Error("a usable header after a bad one must re-arm the element")
	}
}

// TestApplyHeaderLimiterOnlyRederive: a header changing only limiterBands
// takes the no-reset path but still re-derives the limiter table (ffmpeg's
// rule); everything else, envelope state included, is untouched.
func TestApplyHeaderLimiterOnlyRederive(t *testing.T) {
	el := buildTestElement(t)
	el.pendingReset = false
	el.ch[0].prevEnv[0] = 42
	oldKx, oldM := el.tbl.kx, el.tbl.m
	oldLim := append([]int(nil), el.tbl.fLim[:el.tbl.nL+1]...)

	h2 := el.hdr
	h2.limiterBands = 1
	el.applyHeader(h2)
	if el.hdr.limiterBands != 1 {
		t.Fatal("header not installed")
	}
	if el.pendingReset {
		t.Error("a limiter-only change must not reset the element")
	}
	if el.ch[0].prevEnv[0] != 42 {
		t.Error("a limiter-only change cleared envelope state")
	}
	if el.tbl.kx != oldKx || el.tbl.m != oldM {
		t.Errorf("band structure moved: kx/m %d/%d -> %d/%d", oldKx, oldM, el.tbl.kx, el.tbl.m)
	}
	newLim := el.tbl.fLim[:el.tbl.nL+1]
	same := len(newLim) == len(oldLim)
	if same {
		for i := range newLim {
			same = same && newLim[i] == oldLim[i]
		}
	}
	if same {
		t.Errorf("fLim unchanged (%v) after limiterBands 2 -> 1; the limiter table was not re-derived", newLim)
	}
}

// TestDamagedPayloadRestoresAnchors: the delta-coding anchors update while
// a payload parses, so a payload rejected after its envelope run (an
// out-of-range noise run, a declared length shorter than the data) must
// roll them back or the next frame's delta-time references anchor on
// garbage.
func TestDamagedPayloadRestoresAnchors(t *testing.T) {
	el := buildTestElement(t)
	good := sceSBRFill(t, &el.tbl, sbrPayloadOpts{envStart: 60, noiseStart: 20})
	el.beginFrame()
	el.parseFill(newBitReader(good), len(good))
	if !el.dataOK {
		t.Fatal("the good payload must parse")
	}
	anchorEnv, anchorNoise, anchorRes := el.ch[0].prevEnv, el.ch[0].prevNoise, el.ch[0].prevRes

	check := func(name string, payload []byte, count int) {
		el.beginFrame()
		el.parseFill(newBitReader(payload), count)
		if el.dataOK {
			t.Fatalf("%s: damaged payload accepted", name)
		}
		if el.ch[0].prevEnv != anchorEnv || el.ch[0].prevNoise != anchorNoise || el.ch[0].prevRes != anchorRes {
			t.Errorf("%s: delta anchors poisoned by a rejected payload", name)
		}
	}
	// Valid envelope (start 120), then a noise run stepping past 30: the
	// envelope anchors are written before the noise run fails.
	bad := sceSBRFill(t, &el.tbl, sbrPayloadOpts{envStart: 120, noiseStart: 20, noiseDelta: 11})
	check("noise range", bad, len(bad))
	// The same failure by truncation: a fill whose declared length ends
	// before the data does.
	check("truncated fill", good, len(good)-2)

	// And the range rule itself: an envelope run stepping past 127 fails
	// the frame (the dequant of the clamped 254 this replaced overflowed
	// float32 into the synthesis).
	over := sceSBRFill(t, &el.tbl, sbrPayloadOpts{envStart: 120, envDelta: 30, noiseStart: 20})
	check("envelope range", over, len(over))
}

// TestConcealKeepsResetPending pins the reset lifecycle: consumed only by
// a frame that decodes data, re-armed by concealment (the FIFO and
// crossover continuity a concealed frame breaks), and armed by a seek.
func TestConcealKeepsResetPending(t *testing.T) {
	el := buildTestElement(t)
	in := make([]float32, 1024)
	out := make([]float32, 2048)
	fill := sceSBRFill(t, &el.tbl, sbrPayloadOpts{envStart: 60, noiseStart: 20})
	run := func(withFill bool) {
		el.beginFrame()
		if withFill {
			el.parseFill(newBitReader(fill), len(fill))
			if !el.dataOK {
				t.Fatal("fill rejected")
			}
		}
		el.process([][]float32{in}, [][]float32{out})
	}

	if !el.pendingReset {
		t.Fatal("a fresh header must arm a reset")
	}
	run(true)
	if el.pendingReset {
		t.Error("a decoded frame must consume the reset")
	}
	run(false)
	if !el.pendingReset {
		t.Error("a concealed frame must re-arm the reset for the resume")
	}
	run(true)
	if el.pendingReset {
		t.Error("the resume frame must consume the re-armed reset")
	}
	el.resetForSeek()
	if !el.pendingReset {
		t.Error("a seek must arm a reset")
	}
}

// TestGainSmoothingFIFO drives the smoothing FIFO directly (no committed
// stream uses bs_smoothing_mode 0, so no differential reaches it). The
// hard-coded expectations pin the tap order: the window's largest
// coefficient weights the current slot, matching ffmpeg's h_smooth, so a
// mirrored table or a reversed FIFO index fails these numbers.
func TestGainSmoothingFIFO(t *testing.T) {
	const w0 = 0.33333333333333       // h_smooth[0], the current slot's tap
	const w01 = w0 + 0.30150283239582 // plus h_smooth[1]
	el := buildTestElement(t)
	hdrOn := el.hdr
	hdrOn.smoothingMode = 0
	st := &el.ch[0]
	tbl := &el.tbl

	// Two 8-border envelopes over a constant unit high band: gains are
	// sqrt(eOrig) exactly (no noise, no limiter or boost engagement).
	frame := func(e0, e1 float64) *sbrChanDequant {
		fd := &st.frame
		*fd = sbrFrameData{}
		fd.grid = sbrGrid{frameClass: fixfix, numEnv: 2, lA: -1, numNoise: 2}
		fd.grid.tE[0], fd.grid.tE[1], fd.grid.tE[2] = 0, 8, 16
		fd.grid.tQ[0], fd.grid.tQ[1], fd.grid.tQ[2] = 0, 8, 16
		fd.ampRes = 1
		dq := &sbrChanDequant{}
		for b := 0; b < tbl.nLow; b++ {
			dq.eOrig[0][b], dq.eOrig[1][b] = e0, e1
		}
		return dq
	}
	fillX := func() {
		for l := range st.xhighRe {
			for k := range st.xhighRe[l] {
				st.xhighRe[l][k] = 1
				st.xhighIm[l][k] = 0
			}
		}
	}
	y := func(slot int) float64 { return float64(st.yRe[slot+sbrTHFAdj][tbl.kx]) }
	expect := func(name string, cases [][2]float64) {
		t.Helper()
		for _, c := range cases {
			if got := y(int(c[0])); math.Abs(got-c[1]) > 1e-5 {
				t.Errorf("%s: slot %d = %.8f, want %.8f", name, int(c[0]), got, c[1])
			}
		}
	}

	// Frame 1, reset: gain steps 1 -> 2 at slot 16. The seed rows hold the
	// first envelope's gains, so the head is flat; the step spreads over
	// the window with the current slot carrying the big tap.
	fillX()
	hfAdjust(st, tbl, hdrOn, frame(1, 4), true)
	expect("frame 1", [][2]float64{
		{0, 1}, {15, 1},
		{16, 1 + w0}, {17, 1 + w01},
		{20, 2}, {31, 2},
	})

	// Frame 2, no reset: constant gain 1. The carry seeds the lead rows
	// from frame 1's final gain-2 rows, so the boundary smooths down.
	fillX()
	hfAdjust(st, tbl, hdrOn, frame(1, 1), false)
	expect("frame 2 carry", [][2]float64{
		{0, 2 - w0}, {1, 2 - w01},
		{4, 1}, {31, 1},
	})

	// Smoothing off (the shipped streams' mode): the same step lands whole
	// at its border.
	el2 := buildTestElement(t)
	st2, tbl2 := &el2.ch[0], &el2.tbl
	st = st2
	tbl = tbl2
	fillX()
	hfAdjust(st2, tbl2, el2.hdr, frame(1, 4), true)
	expect("unsmoothed", [][2]float64{{15, 1}, {16, 2}})
}
