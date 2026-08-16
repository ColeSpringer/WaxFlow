package aac

// SBR per-element decoding state and frame orchestration (ISO/IEC 14496-3
// 4.6.18): each SCE or CPE with SBR carries its own header, derived band
// tables, and channel histories. A frame runs core decode into staging,
// parses the following fill element's SBR payload, then per channel: QMF
// analysis of the core, high-frequency generation and adjustment into the
// Y buffer, and 64-band synthesis of low band plus Y into 2048 output
// samples. A frame with no usable payload conceals: the high band stays
// zero and the output keeps its shape.

const (
	sbrSlots    = 32 // QMF slots per frame (16 time slots * RATE 2)
	sbrTHFAdj   = 2  // adjuster lead-in slots
	sbrBufSlots = 40 // slots + 8 history slots (T_HFGEN)
)

// sbrChanState is one channel's persistent SBR state.
type sbrChanState struct {
	frame sbrFrameData

	ana qmfAnalyzer32
	syn qmfSynthesizer64

	// Subband buffers, [slot][band], slot 0 aligned sbrTHFGen slots before
	// the current frame; shifted by sbrSlots each frame.
	xlowRe, xlowIm   [sbrBufSlots][32]float32
	xhighRe, xhighIm [sbrBufSlots][64]float32
	yRe, yIm         [sbrBufSlots][64]float32

	// Delta-coding anchors and chirp memory across frames.
	prevEnv   [64]int32
	prevRes   int
	prevNoise [5]int32
	prevInvf  [5]int
	bw        [5]float64

	// Envelope-gain smoothing FIFO, rows indexed by absolute slot plus the
	// smoothing lead, and the previous frame's last envelope border that
	// anchors the cross-frame carry.
	gTemp, qTemp [44][64]float64
	prevEnvEnd   int

	idxNoise int
	idxSine  int

	sIndexPrev [64]bool // sine active in the last envelope, per m
	prevLaEnd  bool     // previous frame's sinusoid ramp ended at its last border
}

// clear wipes everything a seek invalidates: resetTables' envelope state
// plus the QMF banks, the subband histories, and the tone indices. The
// parsed header and tables survive on the element.
func (st *sbrChanState) clear() {
	st.resetTables()
	st.ana.reset()
	st.syn.reset()
	st.xlowRe = [sbrBufSlots][32]float32{}
	st.xlowIm = [sbrBufSlots][32]float32{}
	st.xhighRe = [sbrBufSlots][64]float32{}
	st.xhighIm = [sbrBufSlots][64]float32{}
	st.idxNoise = 0
	st.idxSine = 0
}

// sbrElement is one SCE's or CPE's SBR decoder.
type sbrElement struct {
	chans   int
	sbrRate int // the extension (output) rate the 64-band bank runs at

	hdr     sbrHeader
	haveHdr bool
	tbl     sbrFreqTables
	haveTbl bool

	coupled bool // this frame's channel pair coupling
	dataOK  bool // this frame carries a usable payload

	kxPrev, mPrev int
	pendingReset  bool

	ch [2]sbrChanState
}

func newSBRElement(chans, sbrRate int) *sbrElement {
	return &sbrElement{chans: chans, sbrRate: sbrRate, kxPrev: 64}
}

// applyHeader installs a newly received sbr_header. A change in any
// table-shaping field re-derives the band tables and resets the envelope
// state (the time-continuous QMF banks keep running); gain-side fields
// update in place, except limiterBands, which re-derives the limiter
// table without a reset (ffmpeg's rule, and fLim is derived state).
func (el *sbrElement) applyHeader(h sbrHeader) {
	if el.haveHdr && el.haveTbl && h.freqFieldsEqual(el.hdr) {
		if h.limiterBands != el.hdr.limiterBands {
			// Same band structure, new limiter split: rebuild the tables in
			// place. The freq fields agree, so everything but fLim/nL comes
			// out identical and no envelope state is invalidated.
			if tbl, err := deriveFreqTables(h, el.sbrRate); err == nil {
				el.tbl = tbl
			}
		}
		el.hdr = h
		return
	}
	tbl, err := deriveFreqTables(h, el.sbrRate)
	if err != nil {
		// An unusable header mutes the tool entirely. The stream's data is
		// now coded against the header we could not derive, so keeping the
		// old tables would parse it against the wrong band counts; conceal
		// until a usable header arrives.
		el.haveHdr = false
		el.haveTbl = false
		return
	}
	el.hdr = h
	el.haveHdr = true
	el.tbl = tbl
	el.haveTbl = true
	el.pendingReset = true
	for c := range el.ch {
		el.ch[c].resetTables()
	}
}

// resetTables clears the state a band-table change invalidates, before
// this frame's payload decodes against it: delta anchors, chirp memory,
// smoothing history, sine continuation, and the Y overlap.
func (st *sbrChanState) resetTables() {
	st.yRe = [sbrBufSlots][64]float32{}
	st.yIm = [sbrBufSlots][64]float32{}
	st.prevEnv = [64]int32{}
	st.prevRes = 0
	st.prevNoise = [5]int32{}
	st.prevInvf = [5]int{}
	st.bw = [5]float64{}
	st.gTemp = [44][64]float64{}
	st.qTemp = [44][64]float64{}
	st.prevEnvEnd = 0
	st.sIndexPrev = [64]bool{}
	st.prevLaEnd = false
}

// beginFrame clears the per-frame payload markers before the element's
// core data and fill elements are parsed.
func (el *sbrElement) beginFrame() {
	el.dataOK = false
	el.coupled = false
}

// process runs one frame for every channel: in holds each channel's 1024
// core samples, out receives 2048 at the extension rate.
func (el *sbrElement) process(in, out [][]float32) {
	for c := 0; c < el.chans; c++ {
		st := &el.ch[c]
		st.shiftBuffers()
		for slot := range sbrSlots {
			base := 8 + slot
			st.ana.analyze(in[c][32*slot:32*slot+32], st.xlowRe[base][:], st.xlowIm[base][:])
		}
	}
	kxCur, mCur := el.kxPrev, el.mPrev
	var lTemp [2]int
	if el.dataOK && el.haveTbl {
		kxCur, mCur = el.tbl.kx, el.tbl.m
		var deq sbrDequant
		dequantElement(el, &deq)
		for c := 0; c < el.chans; c++ {
			st := &el.ch[c]
			// Below lTemp the band split stays the previous frame's: the
			// slots its last envelope reached into this frame keep its
			// crossover. Captured before adjustment advances the border.
			if !el.pendingReset {
				lTemp[c] = max(st.prevEnvEnd-sbrSlots, 0)
			}
			hfGenerate(st, &el.tbl)
			hfAdjust(st, &el.tbl, el.hdr, &deq.ch[c], el.pendingReset)
		}
		el.pendingReset = false
	} else if el.haveTbl {
		// A concealed frame breaks the envelope continuity the smoothing
		// FIFO and the crossover splice describe, so the next usable frame
		// restarts as a reset does. Pending resets stay pending: clearing
		// one here would let a header change be consumed by a frame that
		// never applied it.
		el.pendingReset = true
	}
	for c := 0; c < el.chans; c++ {
		st := &el.ch[c]
		var xr, xi [64]float32
		for l := range sbrSlots {
			kx, m := kxCur, mCur
			if l < lTemp[c] {
				kx, m = el.kxPrev, el.mPrev
			}
			kx = min(kx, 64)
			m = min(m, 64-kx)
			src := l + sbrTHFAdj
			clear(xr[:])
			clear(xi[:])
			for k := 0; k < kx && k < 32; k++ {
				xr[k] = st.xlowRe[src][k]
				xi[k] = st.xlowIm[src][k]
			}
			for k := kx; k < kx+m; k++ {
				xr[k] = st.yRe[src][k]
				xi[k] = st.yIm[src][k]
			}
			st.syn.synthesize(xr[:], xi[:], out[c][64*l:64*l+64])
		}
	}
	el.kxPrev, el.mPrev = kxCur, mCur
}

// shiftBuffers carries the analysis and adjuster tails across the frame
// boundary: the last 8 slots become the new history, and the Y slots past
// the frame (envelopes may extend into it) slide down likewise.
func (st *sbrChanState) shiftBuffers() {
	for i := 0; i < sbrBufSlots-sbrSlots; i++ {
		st.xlowRe[i] = st.xlowRe[i+sbrSlots]
		st.xlowIm[i] = st.xlowIm[i+sbrSlots]
		st.yRe[i] = st.yRe[i+sbrSlots]
		st.yIm[i] = st.yIm[i+sbrSlots]
	}
	for i := sbrBufSlots - sbrSlots; i < sbrBufSlots; i++ {
		st.yRe[i] = [64]float32{}
		st.yIm[i] = [64]float32{}
	}
}

// resetForSeek clears channel histories after a container seek, keeping
// the parsed header and tables: headers repeat only every half second or
// so, and dropping them would mute the high band until the next one. The
// reset stays pending so the first decoded frame seeds its smoothing FIFO
// and sinusoid state fresh instead of reading the rows just zeroed.
func (el *sbrElement) resetForSeek() {
	for c := range el.ch {
		el.ch[c].clear()
	}
	el.kxPrev, el.mPrev = 64, 0
	el.dataOK = false
	el.pendingReset = true
}
