package aac

import "math"

// PS parameter extraction and the stereo-to-mono downmix, the encoder
// half of ISO/IEC 14496-3 8.6.4. The encoder analyzes both input
// channels in the decoder's 64-band QMF domain, measures IID and ICC per
// stereo parameter band (through the decoder's own hybrid filters below
// QMF band 3, so the measured sub-bands are exactly the ones the decoder
// mixes), and folds the pair into a phase-aligned active mono downmix
// whose synthesis feeds the v1 chain: an aligned rotation of the right
// channel prevents anti-phase cancellation, and a per-band gain restores
// the pair's energy, so the decoder's IID split (which conserves 2*e_m)
// reconstructs the original band levels. The quantized parameters ride
// as ps_data inside the SBR extension of every access unit.

// psEncBands is the coded stereo parameter band count: 20 bands with the
// default quantizers (iid_mode and icc_mode 1), fdk's own operating
// shape. 34-band mode and IPD/OPD stay unwritten.
const psEncBands = 20

// psEncRing is the L/R analysis ring in slots. The live span (newest
// pushed minus oldest read at payload-build time) measured 108 slots at
// ring 128, not the ~85 a naive tally of the 44-slot window plus the
// 9-slot QMF pair offset suggests: frontEnd pushes a WHOLE input chunk
// (up to 32 slots) into the rings before drainDM feeds any of its mono
// output to the core, so a full chunk's slots ride on top of the
// pipeline offset. The core's deferred window decision holds each AU one
// block past its window, another 32 slots of span (~140; at 128 the
// overwrite surfaced as chunk-dependent AU bytes). The span still rests
// on two chops staying at 2048 (Encode's min(2*frameLen, ...) input
// step and drainDM's identical feed step), and the readers' ringGuard
// panics on an overrun instead of reading a wrapped slot.
const psEncRing = 192

// Downmix smoothing constants: the leaky per-band accumulators weight
// louder slots more and settle in a few slots (750 slots/s), fast enough
// to track program changes and slow enough that the rotation cannot
// scramble uncorrelated content slot to slot.
const (
	psDMLeak = 0.88
	// The rotation target is FULL phase alignment whenever smoothed
	// coherence clears this floor, identity below it. Full, not blended:
	// aligned coherent content sums constructively whatever its phase,
	// so the active gain stays near 1, where a partial rotation of
	// anti-phase content leaves part of the cancellation for the gain to
	// paper over (the original half-blend targeted the ZERO vector at
	// coherence 0.5 over an anti-phase pair, exactly the cancelling
	// half-sum this front end exists to prevent). The floor only decides
	// where uncorrelated bands stop chasing noise; leaky-smoothed white
	// noise measures about 0.25, so the floor sits under it and the slew
	// below is what keeps the wandering harmless.
	psDMCohFloor = 0.2
	// The applied rotation slews toward the target instead of jumping:
	// continuity in time by construction, with no angle-wrap seam (an
	// anti-phase pair's cross sits at +-pi and flips sign freely; a
	// vector slew passes through the flip smoothly where an angle blend
	// alternates between +w*pi and -w*pi).
	psDMRotSlew = 0.25
)

// psHuffEncTable maps a PS delta value (offset +30) to its code.
type psHuffEncTable struct {
	code [61]uint32
	bits [61]uint8
}

func (t *psHuffEncTable) write(w *bitWriter, d int32) {
	w.writeBits(uint(t.bits[d+30]), uint64(t.code[d+30]))
}

// buildPSHuffEnc walks a decode tree into its encode table, the
// buildHuffEnc pattern: deriving from the tree the parser walks (rather
// than re-running the canonical construction over the listings) means
// the two directions cannot drift.
func buildPSHuffEnc(tree [][2]int16) (enc psHuffEncTable) {
	var walk func(node int, code uint32, bits uint8)
	walk = func(node int, code uint32, bits uint8) {
		for b := range 2 {
			v := tree[node][b]
			c, n := code<<1|uint32(b), bits+1
			if v < 0 {
				enc.code[int(v)-psLeafBias+30] = c
				enc.bits[int(v)-psLeafBias+30] = n
				continue
			}
			walk(int(v), c, n)
		}
	}
	walk(0, 0, 0)
	return enc
}

// Only the delta-frequency books are written: every frame's parameter
// runs are freq-coded (see buildFrame), so the delta-time books stay
// decode-only.
var (
	psEncIIDDF = buildPSHuffEnc(psTreeIIDDF)
	psEncICCDF = buildPSHuffEnc(psTreeICCDF)
)

// psEnc is the PS side of an HE-AAC v2 encoder element.
type psEnc struct {
	ana    [2]qmfAnalyzer64
	ringRe [2][psEncRing][64]float32
	ringIm [2][psEncRing][64]float32
	pushed int64

	syn qmfSynthesizer64

	// Per-band downmix state: smoothed cross-spectrum, channel and raw
	// downmix energies, and the slewed rotation vector (raw, normalized
	// per slot on use).
	crossRe, crossIm [64]float64
	eLs, eRs, eMs    [64]float64
	rot              [64][2]float64

	// Quantized frame parameters. There are no delta-time anchors: every
	// run is freq-coded (see buildFrame), so nothing carries across
	// frames and a dropped payload needs no PS rollback.
	iid, icc [psEncBands]int32

	data bitWriter // ps_data scratch
}

// pushSlot analyzes one 64-sample slot of both channels into the ring
// and writes the slot's 64 downmixed mono samples (delayed by the QMF
// pair, which the caller trims).
func (p *psEnc) pushSlot(l, r, out []float32) {
	idx := p.pushed % psEncRing
	lRe, lIm := p.ringRe[0][idx][:], p.ringIm[0][idx][:]
	rRe, rIm := p.ringRe[1][idx][:], p.ringIm[1][idx][:]
	p.ana[0].analyze(l, lRe, lIm)
	p.ana[1].analyze(r, rRe, rIm)
	p.pushed++

	var mRe, mIm [64]float32
	for k := range 64 {
		xr, xi := float64(lRe[k]), float64(lIm[k])
		yr, yi := float64(rRe[k]), float64(rIm[k])
		el := xr*xr + xi*xi
		er := yr*yr + yi*yi
		p.crossRe[k] = psDMLeak*p.crossRe[k] + (xr*yr + xi*yi)
		p.crossIm[k] = psDMLeak*p.crossIm[k] + (xi*yr - xr*yi)
		p.eLs[k] = psDMLeak*p.eLs[k] + el
		p.eRs[k] = psDMLeak*p.eRs[k] + er

		// Rotate R toward L's phase where the pair is coherent: the slewed
		// vector tracks the full-alignment target, and a brief antipodal
		// transit (an image flipping through pi) passes near zero, where
		// the slot falls back to identity rather than a noise direction.
		tr, ti := 1.0, 0.0
		mag := math.Hypot(p.crossRe[k], p.crossIm[k])
		if den := math.Sqrt(p.eLs[k] * p.eRs[k]); den > 1e-30 && mag > psDMCohFloor*den {
			tr, ti = p.crossRe[k]/mag, p.crossIm[k]/mag
		}
		p.rot[k][0] += psDMRotSlew * (tr - p.rot[k][0])
		p.rot[k][1] += psDMRotSlew * (ti - p.rot[k][1])
		ur, ui := 1.0, 0.0
		if rm := math.Hypot(p.rot[k][0], p.rot[k][1]); rm > 1e-3 {
			ur, ui = p.rot[k][0]/rm, p.rot[k][1]/rm
		}
		sr := 0.5 * (xr + yr*ur - yi*ui)
		si := 0.5 * (xi + yr*ui + yi*ur)

		// Active gain from the smoothed energies: the decoder's mixing
		// conserves e_l + e_r = 2*e_m, so the downmix must carry half the
		// pair's energy for the reconstruction to land at the source level.
		p.eMs[k] = psDMLeak*p.eMs[k] + (sr*sr + si*si)
		g := 1.0
		if p.eMs[k] > 1e-30 {
			g = math.Sqrt((p.eLs[k] + p.eRs[k]) / (2 * p.eMs[k]))
		}
		g = math.Min(math.Max(g, 0.5), 2)
		mRe[k] = float32(g * sr)
		mIm[k] = float32(g * si)
	}
	p.syn.synthesize(mRe[:], mIm[:], out)
}

// buildFrame measures and quantizes access unit au's stereo parameters
// over the envelope window the decoder applies them to: abs slots
// [s0, s0+32), s0 = 32(au-1)-6, one envelope per frame with its border
// at the frame end (the decoder interpolates the mixing matrices from
// the previous frame's across the window).
func (p *psEnc) buildFrame(au int64) {
	s0 := 32*(au-1) - 6

	// Hybrid analysis of the three lowest QMF bands per channel, over the
	// 44-slot window (6 history, 32 frame, 6 lookahead) the zero-phase
	// filters need. Slots outside the pushed range read as silence.
	var hyb [2][10][sbrSlots][2]float32
	for c := range 2 {
		var in [3][44][2]float32
		for j := range 44 {
			abs := s0 - 6 + int64(j)
			if abs < 0 || abs >= p.pushed {
				continue
			}
			ringGuard(p.pushed, abs, psEncRing)
			idx := abs % psEncRing
			for b := range 3 {
				in[b][j] = [2]float32{p.ringRe[c][idx][b], p.ringIm[c][idx][b]}
			}
		}
		psHybrid6(&in[0], hyb[c][0:6])
		psHybrid2(&in[1], hyb[c][6:8], true)
		psHybrid2(&in[2], hyb[c][8:10], false)
	}

	// Per parameter band: channel energies and the real cross-correlation
	// (mixing procedure A's ICC), accumulated over the hybrid channels
	// (k < 10) and the plain QMF bands behind them (band k-7).
	var eL, eR, cRe [psEncBands]float64
	for k := range 71 {
		b := int(psKToI20[k])
		for n := range sbrSlots {
			var l, r [2]float32
			if k < 10 {
				l, r = hyb[0][k][n], hyb[1][k][n]
			} else {
				abs := s0 + int64(n)
				if abs < 0 || abs >= p.pushed {
					continue
				}
				ringGuard(p.pushed, abs, psEncRing)
				idx := abs % psEncRing
				band := k - 7
				l = [2]float32{p.ringRe[0][idx][band], p.ringIm[0][idx][band]}
				r = [2]float32{p.ringRe[1][idx][band], p.ringIm[1][idx][band]}
			}
			eL[b] += float64(l[0])*float64(l[0]) + float64(l[1])*float64(l[1])
			eR[b] += float64(r[0])*float64(r[0]) + float64(r[1])*float64(r[1])
			cRe[b] += float64(l[0])*float64(r[0]) + float64(l[1])*float64(r[1])
		}
	}
	for b := range psEncBands {
		p.iid[b] = quantIID(eL[b], eR[b])
		p.icc[b] = quantICC(cRe[b], eL[b], eR[b])
	}
}

// quantIID quantizes a band's channel energy ratio onto the default IID
// quantizer: positive indexes mean a louder left, matching the decoder's
// dequantization.
func quantIID(eL, eR float64) int32 {
	if eL <= 1e-30 && eR <= 1e-30 {
		return 0
	}
	if eR <= 1e-30 {
		return 7
	}
	if eL <= 1e-30 {
		return -7
	}
	db := 10 * math.Log10(eL/eR)
	idx := int32(nearestIdx(math.Abs(db), psIIDStepsDB[:]))
	if db < 0 {
		return -idx
	}
	return idx
}

// nearestIdx returns the index of the value in vals closest to x.
func nearestIdx(x float64, vals []float64) int {
	best, bestD := 0, math.Inf(1)
	for i, v := range vals {
		if d := math.Abs(x - v); d < bestD {
			best, bestD = i, d
		}
	}
	return best
}

// quantICC quantizes a band's normalized real cross-correlation onto the
// ICC list (descending 1 to -1).
func quantICC(cRe, eL, eR float64) int32 {
	den := math.Sqrt(eL * eR)
	if den <= 1e-30 {
		return 0
	}
	rho := math.Min(math.Max(cRe/den, -1), 1)
	return int32(nearestIdx(rho, psICCRho[:]))
}

// writePSRun emits one parameter run, freq-coded: dt flag zero, then
// deltas accumulating from zero. Always freq, never time, by decision:
// the saving delta-time offers measured under 0.01 ODG at both gate
// bitrates (flat runs cost about a bit a band either way), and
// freq-only makes every frame's ps_data self-contained, so a decoder
// joining anywhere (a restarted segment worker's first served AU, a
// mid-stream ADTS tune-in) reconstructs the stereo image on its first
// frame instead of accumulating deltas against anchors it never saw.
func writePSRun(w *bitWriter, vals []int32, tab *psHuffEncTable) {
	w.writeBits(1, 0)
	prev := int32(0)
	for _, v := range vals {
		tab.write(w, v-prev)
		prev = v
	}
}

// appendPSExtension renders this frame's ps_data and writes the whole
// extended-data block carrying it (bs_extended_data through the byte
// padding, which stays under a byte so the parser's consumption rule
// cannot re-enter on it). The header rides every frame like the
// sbr_header, and with the runs freq-coded (writePSRun) the payload
// carries no cross-frame state at all.
func (p *psEnc) appendPSExtension(w *bitWriter) {
	d := &p.data
	d.reset()
	d.writeBits(1, 1) // enable_ps_header
	d.writeBits(1, 1) // enable_iid
	d.writeBits(3, 1) // iid_mode 1: 20 bands, default quantizer
	d.writeBits(1, 1) // enable_icc
	d.writeBits(3, 1) // icc_mode 1: 20 bands, mixing procedure A
	d.writeBits(1, 0) // enable_ext
	d.writeBits(1, 0) // frame_class FIX
	d.writeBits(2, 1) // bs_num_env_idx: one envelope
	writePSRun(d, p.iid[:], &psEncIIDDF)
	writePSRun(d, p.icc[:], &psEncICCDF)

	bits := d.bitLen()
	cnt := (2 + bits + 7) / 8
	w.writeBits(1, 1) // bs_extended_data
	// base 15, NOT the fill element's 14: see writeEscapedCount. The
	// crossed base once over-declared every escaped block by a byte,
	// which pushed it past the fill end and cost the whole SBR payload.
	writeEscapedCount(w, cnt, 15)
	w.writeBits(2, extensionIDPS)
	for _, b := range d.buf {
		w.writeBits(8, uint64(b))
	}
	if d.n > 0 {
		w.writeBits(d.n, d.cache&(1<<d.n-1))
	}
	w.writeBits(uint(cnt*8-2-bits), 0)
}
