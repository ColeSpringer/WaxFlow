package aac

// PS stereo reconstruction (ISO/IEC 14496-3 8.6.4): the mono SBR
// spectrum, assembled over 38 QMF slots (32 plus the hybrid filter's
// six-slot lookahead), is split below QMF band 3 (band 5 in 34-band mode)
// into sub-subbands by the hybrid filterbank, decorrelated through a
// transient-ducked allpass cascade, and mixed into left and right by the
// interpolated IID/ICC (and, when present, IPD/OPD) matrices, then summed
// back to two 64-band spectra for the twin synthesis banks. Everything
// here works in the interleaved [re, im] pairs the hybrid convolutions
// want; the element deinterleaves at the synthesis boundary.

// psDec is one element's PS state: the parameter side (data) and the
// signal side (filter histories, mixing anchors, scratch).
type psDec struct {
	data psData

	// xBuf is the frame's assembled mono spectrum, 38 slots; slots 0-31
	// are rewritten in place with the left spectrum and rBuf receives the
	// right. syn is the right channel's synthesis bank (the left reuses
	// the channel state's).
	xBuf [38][64][2]float32
	rBuf [sbrSlots][64][2]float32
	syn  qmfSynthesizer64

	inBuf      [5][44][2]float32 // hybrid filter input, 6 history + 38 slots
	lHyb, rHyb [91][sbrSlots][2]float32

	delay   [91][sbrSlots + 14][2]float32
	apDelay [50][3][sbrSlots + 5][2]float32

	peakDecayNrg        [34]float32
	powerSmooth         [34]float32
	peakDecayDiffSmooth [34]float32

	// Mixing matrix anchors per parameter band: [re/im][envelope row].
	// Row 0 is the previous frame's final matrix, rows 1..numEnv this
	// frame's targets.
	h11, h12, h21, h22 [2][6][34]float32

	opdHist, ipdHist [17]int8
}

// clearSignal wipes the signal-path state a seek invalidates: filter
// histories, smoothing state, mixing anchors, and the synthesis bank. The
// parameter side survives, like the SBR header: headers repeat rarely and
// delta-time coding needs its anchors.
func (ps *psDec) clearSignal() {
	ps.inBuf = [5][44][2]float32{}
	ps.lHyb = [91][sbrSlots][2]float32{}
	ps.rHyb = [91][sbrSlots][2]float32{}
	ps.delay = [91][sbrSlots + 14][2]float32{}
	ps.apDelay = [50][3][sbrSlots + 5][2]float32{}
	ps.peakDecayNrg = [34]float32{}
	ps.powerSmooth = [34]float32{}
	ps.peakDecayDiffSmooth = [34]float32{}
	ps.h11 = [2][6][34]float32{}
	ps.h12 = [2][6][34]float32{}
	ps.h21 = [2][6][34]float32{}
	ps.h22 = [2][6][34]float32{}
	ps.opdHist = [17]int8{}
	ps.ipdHist = [17]int8{}
	ps.syn.reset()
}

// apply runs one frame's stereo reconstruction over xBuf. top is the top
// of the assembled spectrum (kx+m); the delay lines above it are cleared
// so silence up there cannot ring stale content back in.
func (ps *psDec) apply(top int) {
	is34 := 0
	if ps.data.is34 {
		is34 = 1
	}
	nr := psNrBands[is34]
	topCh := min(max(top+nr-64, 0), nr)
	for k := topCh; k < nr; k++ {
		ps.delay[k] = [sbrSlots + 14][2]float32{}
	}
	for k := topCh; k < psNrAllpass[is34]; k++ {
		ps.apDelay[k] = [3][sbrSlots + 5][2]float32{}
	}
	ps.hybridAnalysis(is34)
	ps.decorrelate(is34)
	ps.stereoProcess(is34)
	ps.hybridSynthesis(is34)
	// The band-count transition is consumed exactly once: decorrelate
	// restarted its filter state and stereoProcess remapped the anchors, so
	// latch old = new here rather than waiting for the next ps_data parse.
	// The reference re-fires the transition on every ps_data-less frame
	// until one arrives, re-clearing live decorrelator state and remapping
	// already-remapped anchors; a deliberate deviation, unobservable in the
	// differentials because a 20/34 switch followed by PS-less SBR frames
	// exists in no known encoder's output (fdk never uses 34 bands).
	ps.data.is34Old = ps.data.is34
}

// hybridAnalysis splits the lowest QMF bands into sub-subbands and lays
// the rest of the spectrum behind them in lHyb, one flat channel index k.
// The 13-tap filters are zero-phase across the frame: output slot i reads
// input slots i-6..i+6, six of history and six of lookahead, so the
// passthrough channels need no delay compensation.
func (ps *psDec) hybridAnalysis(is34 int) {
	in := &ps.inBuf
	for b := range 5 {
		for j := range 38 {
			in[b][j+6] = ps.xBuf[j][b]
		}
	}
	if is34 == 1 {
		psHybridCx(&in[0], ps.lHyb[0:12], psFilter34x12[:])
		psHybridCx(&in[1], ps.lHyb[12:20], psFilter34x8[:])
		psHybridCx(&in[2], ps.lHyb[20:24], psFilter34x4[:])
		psHybridCx(&in[3], ps.lHyb[24:28], psFilter34x4[:])
		psHybridCx(&in[4], ps.lHyb[28:32], psFilter34x4[:])
		for k := 5; k < 64; k++ {
			for n := range sbrSlots {
				ps.lHyb[27+k][n] = ps.xBuf[n][k]
			}
		}
	} else {
		psHybrid6(&in[0], ps.lHyb[0:6])
		psHybrid2(&in[1], ps.lHyb[6:8], true)
		psHybrid2(&in[2], ps.lHyb[8:10], false)
		for k := 3; k < 64; k++ {
			for n := range sbrSlots {
				ps.lHyb[7+k][n] = ps.xBuf[n][k]
			}
		}
	}
	// The last six slots become the next frame's filter history.
	for b := range 5 {
		copy(in[b][0:6], in[b][32:38])
	}
}

// psHybridCx runs a bank of complex 13-tap sub-subband filters over one
// QMF band's slot window.
func psHybridCx(in *[44][2]float32, out [][sbrSlots][2]float32, filter [][13][2]float32) {
	for i := range sbrSlots {
		for q := range filter {
			var sr, si float32
			for n := range 13 {
				xr, xi := in[i+n][0], in[i+n][1]
				fr, fi := filter[q][n][0], filter[q][n][1]
				sr += fr*xr - fi*xi
				si += fr*xi + fi*xr
			}
			out[q][i] = [2]float32{sr, si}
		}
	}
}

// psHybrid6 is the 10/20-band modes' band-0 split: the 8-way complex bank
// with its outputs reordered to ascending centre frequency and the two
// outermost pairs folded, leaving 6 channels.
func psHybrid6(in *[44][2]float32, out [][sbrSlots][2]float32) {
	var t [8][2]float32
	for i := range sbrSlots {
		for q := range t {
			var sr, si float32
			for n := range 13 {
				xr, xi := in[i+n][0], in[i+n][1]
				fr, fi := psFilter20x8[q][n][0], psFilter20x8[q][n][1]
				sr += fr*xr - fi*xi
				si += fr*xi + fi*xr
			}
			t[q] = [2]float32{sr, si}
		}
		out[0][i] = t[6]
		out[1][i] = t[7]
		out[2][i] = t[0]
		out[3][i] = t[1]
		out[4][i] = [2]float32{t[2][0] + t[5][0], t[2][1] + t[5][1]}
		out[5][i] = [2]float32{t[3][0] + t[4][0], t[3][1] + t[4][1]}
	}
}

// psHybrid2 splits one QMF band in two with the real 13-tap filter, whose
// even off-centre taps are zero: the centre tap is the in-phase part, the
// odd taps the out-of-phase, and the two channels are their sum and
// difference. reverse swaps which channel takes the sum, alternating the
// frequency order between adjacent QMF bands.
func psHybrid2(in *[44][2]float32, out [][sbrSlots][2]float32, reverse bool) {
	f := &psProtoQ2
	hi, lo := 0, 1
	if reverse {
		hi, lo = 1, 0
	}
	for i := range sbrSlots {
		reIn, imIn := f[6]*in[i+6][0], f[6]*in[i+6][1]
		var reOp, imOp float32
		for j := 1; j < 6; j += 2 {
			reOp += f[j] * (in[i+j][0] + in[i+12-j][0])
			imOp += f[j] * (in[i+j][1] + in[i+12-j][1])
		}
		out[hi][i] = [2]float32{reIn + reOp, imIn + imOp}
		out[lo][i] = [2]float32{reIn - reOp, imIn - imOp}
	}
}

// decorrelate builds the side signal in rHyb: per parameter band a
// transient ducker (smoothed power against a decaying peak), and per
// channel the fractional-delay allpass cascade, a plain 14-slot delay in
// the mid bands, and a one-slot delay at the top (8.6.4.6.4).
func (ps *psDec) decorrelate(is34 int) {
	kToI := psKToI20[:]
	if is34 == 1 {
		kToI = psKToI34[:]
	}
	// A band-count switch re-derives every channel index, so all filter
	// and smoothing state is meaningless; restart clean.
	if ps.data.is34 != ps.data.is34Old {
		ps.delay = [91][sbrSlots + 14][2]float32{}
		ps.apDelay = [50][3][sbrSlots + 5][2]float32{}
		ps.peakDecayNrg = [34]float32{}
		ps.powerSmooth = [34]float32{}
		ps.peakDecayDiffSmooth = [34]float32{}
	}

	var power [34][sbrSlots]float32
	for k := 0; k < psNrBands[is34]; k++ {
		i := kToI[k]
		for n := range sbrSlots {
			s := &ps.lHyb[k][n]
			power[i][n] += s[0]*s[0] + s[1]*s[1]
		}
	}

	var tGain [34][sbrSlots]float32
	for i := 0; i < psNrParBands[is34]; i++ {
		for n := range sbrSlots {
			decayed := float32(psPeakDecayFactor) * ps.peakDecayNrg[i]
			ps.peakDecayNrg[i] = max(decayed, power[i][n])
			ps.powerSmooth[i] += psASmooth * (power[i][n] - ps.powerSmooth[i])
			ps.peakDecayDiffSmooth[i] += psASmooth *
				(ps.peakDecayNrg[i] - power[i][n] - ps.peakDecayDiffSmooth[i])
			denom := float32(psTransientImpact) * ps.peakDecayDiffSmooth[i]
			g := float32(1)
			if denom > ps.powerSmooth[i] {
				g = ps.powerSmooth[i] / denom
			}
			tGain[i][n] = g
		}
	}

	k := 0
	for ; k < psNrAllpass[is34]; k++ {
		b := kToI[k]
		decay := 1 - psDecaySlope*float64(k-psDecayCutoff[is34])
		decay = min(max(decay, 0), 1)
		var ag [3]float32
		for m := range 3 {
			ag[m] = float32(psAllpassA[m] * decay)
		}
		d := &ps.delay[k]
		copy(d[0:14], d[sbrSlots:sbrSlots+14])
		for n := range sbrSlots {
			d[14+n] = ps.lHyb[k][n]
		}
		ap := &ps.apDelay[k]
		for m := range 3 {
			copy(ap[m][0:5], ap[m][sbrSlots:sbrSlots+5])
		}
		phi := &psPhiFract[is34][k]
		for n := range sbrSlots {
			// The direct path is the two-slot delay rotated by phi; each
			// link then applies its fractional-delay allpass.
			dr, di := d[12+n][0], d[12+n][1]
			inRe := dr*phi[0] - di*phi[1]
			inIm := dr*phi[1] + di*phi[0]
			for m := range 3 {
				lr, li := ap[m][n+2-m][0], ap[m][n+2-m][1]
				qr, qi := psQFract[is34][k][m][0], psQFract[is34][k][m][1]
				outRe := lr*qr - li*qi - ag[m]*inRe
				outIm := lr*qi + li*qr - ag[m]*inIm
				ap[m][n+5][0] = inRe + ag[m]*outRe
				ap[m][n+5][1] = inIm + ag[m]*outIm
				inRe, inIm = outRe, outIm
			}
			ps.rHyb[k][n][0] = tGain[b][n] * inRe
			ps.rHyb[k][n][1] = tGain[b][n] * inIm
		}
	}
	for ; k < psNrBands[is34]; k++ {
		b := kToI[k]
		lag := 14 // mid bands decorrelate by a long plain delay
		if k >= psShortDelay[is34] {
			lag = 1 // the top bands by a single slot
		}
		d := &ps.delay[k]
		copy(d[0:14], d[sbrSlots:sbrSlots+14])
		for n := range sbrSlots {
			d[14+n] = ps.lHyb[k][n]
		}
		for n := range sbrSlots {
			ps.rHyb[k][n][0] = tGain[b][n] * d[14-lag+n][0]
			ps.rHyb[k][n][1] = tGain[b][n] * d[14-lag+n][1]
		}
	}
}

// stereoProcess turns lHyb (the mid signal) and rHyb (its decorrelation)
// into the left and right hybrid spectra, mixing with matrices
// interpolated linearly across each envelope from the previous border's
// values (8.6.4.6.2, with 8.6.4.6.3's phase rotation when IPD/OPD ride
// along).
func (ps *psDec) stereoProcess(is34 int) {
	d := &ps.data
	nrPar := psNrParBands[is34]
	kToI := psKToI20[:]
	if is34 == 1 {
		kToI = psKToI34[:]
	}

	// Row 0 anchors on the previous frame's final matrices.
	if d.numEnvOld > 0 {
		for p := range 2 {
			ps.h11[p][0] = ps.h11[p][d.numEnvOld]
			ps.h12[p][0] = ps.h12[p][d.numEnvOld]
			ps.h21[p][0] = ps.h21[p][d.numEnvOld]
			ps.h22[p][0] = ps.h22[p][d.numEnvOld]
		}
	}

	var mapped [4][5][34]int8
	iidM := psRemapPar(&mapped[0], &d.iid, d.nrIIDPar, d.numEnv, is34, true)
	iccM := psRemapPar(&mapped[1], &d.icc, d.nrICCPar, d.numEnv, is34, true)
	ipdM, opdM := &d.ipd, &d.opd
	if d.enableIPDOPD {
		ipdM = psRemapPar(&mapped[2], &d.ipd, d.nrIPDOPDPar, d.numEnv, is34, false)
		opdM = psRemapPar(&mapped[3], &d.opd, d.nrIPDOPDPar, d.numEnv, is34, false)
	}

	// A band-count switch carries the anchors into the new band domain.
	if d.is34 != d.is34Old {
		for p := range 2 {
			for _, h := range []*[2][6][34]float32{&ps.h11, &ps.h12, &ps.h21, &ps.h22} {
				if is34 == 1 {
					psMapVal20To34(&h[p][0])
				} else {
					psMapVal34To20(&h[p][0])
				}
			}
		}
		ps.opdHist = [17]int8{}
		ps.ipdHist = [17]int8{}
	}

	mix := 0
	if d.iccMode > 2 {
		mix = 1 // ICC modes 3-5 select mixing procedure B
	}
	for e := 0; e < d.numEnv; e++ {
		for b := 0; b < nrPar; b++ {
			row := int(iidM[e][b]) + 7 + 23*d.iidQuant
			h := &psMixLUT[mix][row][iccM[e][b]]
			h11, h12, h21, h22 := h[0], h[1], h[2], h[3]
			if d.enableIPDOPD && b < psNrIPDOPDBands[is34] {
				// The phase parameters smooth over the previous two frames'
				// quantized values; the history packs those two indexes.
				opdIdx := int(ps.opdHist[b])*8 + int(opdM[e][b])
				ipdIdx := int(ps.ipdHist[b])*8 + int(ipdM[e][b])
				ps.opdHist[b] = int8(opdIdx & 0x3f)
				ps.ipdHist[b] = int8(ipdIdx & 0x3f)
				opdRe, opdIm := psPhaseSmooth[opdIdx][0], psPhaseSmooth[opdIdx][1]
				ipdRe, ipdIm := psPhaseSmooth[ipdIdx][0], psPhaseSmooth[ipdIdx][1]
				// The right column rotates by opd - ipd, the left by opd.
				adjRe := opdRe*ipdRe + opdIm*ipdIm
				adjIm := opdIm*ipdRe - opdRe*ipdIm
				ps.h11[1][e+1][b] = h11 * opdIm
				ps.h12[1][e+1][b] = h12 * adjIm
				ps.h21[1][e+1][b] = h21 * opdIm
				ps.h22[1][e+1][b] = h22 * adjIm
				h11 *= opdRe
				h12 *= adjRe
				h21 *= opdRe
				h22 *= adjRe
			}
			ps.h11[0][e+1][b] = h11
			ps.h12[0][e+1][b] = h12
			ps.h21[0][e+1][b] = h21
			ps.h22[0][e+1][b] = h22
		}
		start, stop := d.border[e], d.border[e+1]
		width := float32(1) / float32(max(stop-start, 1))
		for k := 0; k < psNrBands[is34]; k++ {
			b := kToI[k]
			var h, hs [2][4]float32
			h[0] = [4]float32{ps.h11[0][e][b], ps.h12[0][e][b], ps.h21[0][e][b], ps.h22[0][e][b]}
			if d.enableIPDOPD {
				h[1] = [4]float32{ps.h11[1][e][b], ps.h12[1][e][b], ps.h21[1][e][b], ps.h22[1][e][b]}
				// The sub-subbands with negative centre frequencies flip
				// the imaginary part (their spectrum is conjugated). The
				// interpolation target keeps its sign, following the
				// reference decoders.
				if (is34 == 1 && k >= 9 && k <= 13) || (is34 == 0 && k <= 1) {
					for j := range 4 {
						h[1][j] = -h[1][j]
					}
				}
			}
			hs[0] = [4]float32{
				(ps.h11[0][e+1][b] - h[0][0]) * width,
				(ps.h12[0][e+1][b] - h[0][1]) * width,
				(ps.h21[0][e+1][b] - h[0][2]) * width,
				(ps.h22[0][e+1][b] - h[0][3]) * width,
			}
			if d.enableIPDOPD {
				hs[1] = [4]float32{
					(ps.h11[1][e+1][b] - h[1][0]) * width,
					(ps.h12[1][e+1][b] - h[1][1]) * width,
					(ps.h21[1][e+1][b] - h[1][2]) * width,
					(ps.h22[1][e+1][b] - h[1][3]) * width,
				}
				psMixIPDOPD(ps.lHyb[k][:], ps.rHyb[k][:], &h, &hs, start, stop)
			} else {
				psMix(ps.lHyb[k][:], ps.rHyb[k][:], &h[0], &hs[0], start, stop)
			}
		}
	}
}

// psMix interpolates the real mixing matrix across (start, stop] and
// applies it: l is the mid signal going in, the left channel coming out;
// r likewise the decorrelated side and the right channel.
func psMix(l, r [][2]float32, h, hs *[4]float32, start, stop int) {
	h0, h1, h2, h3 := h[0], h[1], h[2], h[3]
	for n := start + 1; n <= stop; n++ {
		h0 += hs[0]
		h1 += hs[1]
		h2 += hs[2]
		h3 += hs[3]
		sRe, sIm := l[n][0], l[n][1]
		dRe, dIm := r[n][0], r[n][1]
		l[n][0] = h0*sRe + h2*dRe
		l[n][1] = h0*sIm + h2*dIm
		r[n][0] = h1*sRe + h3*dRe
		r[n][1] = h1*sIm + h3*dIm
	}
}

// psMixIPDOPD is psMix with the complex matrices phase rotation brings.
func psMixIPDOPD(l, r [][2]float32, h, hs *[2][4]float32, start, stop int) {
	h00, h01, h02, h03 := h[0][0], h[0][1], h[0][2], h[0][3]
	h10, h11, h12, h13 := h[1][0], h[1][1], h[1][2], h[1][3]
	for n := start + 1; n <= stop; n++ {
		h00 += hs[0][0]
		h01 += hs[0][1]
		h02 += hs[0][2]
		h03 += hs[0][3]
		h10 += hs[1][0]
		h11 += hs[1][1]
		h12 += hs[1][2]
		h13 += hs[1][3]
		sRe, sIm := l[n][0], l[n][1]
		dRe, dIm := r[n][0], r[n][1]
		l[n][0] = h00*sRe - h10*sIm + h02*dRe - h12*dIm
		l[n][1] = h00*sIm + h10*sRe + h02*dIm + h12*dRe
		r[n][0] = h01*sRe - h11*sIm + h03*dRe - h13*dIm
		r[n][1] = h01*sIm + h11*sRe + h03*dIm + h13*dRe
	}
}

// hybridSynthesis sums each channel's sub-subbands back onto their QMF
// bands: the split bands add their channels, everything else passes
// through, left into xBuf and right into rBuf.
func (ps *psDec) hybridSynthesis(is34 int) {
	for n := range sbrSlots {
		l, r := &ps.xBuf[n], &ps.rBuf[n]
		if is34 == 1 {
			for band := range 5 {
				l[band] = [2]float32{}
				r[band] = [2]float32{}
			}
			bounds := [6]int{0, 12, 20, 24, 28, 32}
			for band := range 5 {
				for c := bounds[band]; c < bounds[band+1]; c++ {
					l[band][0] += ps.lHyb[c][n][0]
					l[band][1] += ps.lHyb[c][n][1]
					r[band][0] += ps.rHyb[c][n][0]
					r[band][1] += ps.rHyb[c][n][1]
				}
			}
			for k := 5; k < 64; k++ {
				l[k] = ps.lHyb[27+k][n]
				r[k] = ps.rHyb[27+k][n]
			}
		} else {
			bounds := [4]int{0, 6, 8, 10}
			for band := range 3 {
				var lAcc, rAcc [2]float32
				for c := bounds[band]; c < bounds[band+1]; c++ {
					lAcc[0] += ps.lHyb[c][n][0]
					lAcc[1] += ps.lHyb[c][n][1]
					rAcc[0] += ps.rHyb[c][n][0]
					rAcc[1] += ps.rHyb[c][n][1]
				}
				l[band] = lAcc
				r[band] = rAcc
			}
			for k := 3; k < 64; k++ {
				l[k] = ps.lHyb[7+k][n]
				r[k] = ps.rHyb[7+k][n]
			}
		}
	}
}

// psRemapPar maps an envelope's parameters onto the processing band count
// (20, or 34 in 34-band mode) when the coded count differs; full is false
// for the phase parameters, which cover only the lower half of the bands.
func psRemapPar(dst *[5][34]int8, src *[5][34]int8, numPar, numEnv, is34 int, full bool) *[5][34]int8 {
	if is34 == 1 {
		switch numPar {
		case 20, 11:
			for e := 0; e < numEnv; e++ {
				psMapIdx20To34(&dst[e], &src[e], full)
			}
		case 10, 5:
			for e := 0; e < numEnv; e++ {
				psMapIdx10To34(&dst[e], &src[e], full)
			}
		default:
			return src
		}
		return dst
	}
	switch numPar {
	case 34, 17:
		for e := 0; e < numEnv; e++ {
			psMapIdx34To20(&dst[e], &src[e], full)
		}
	case 10, 5:
		for e := 0; e < numEnv; e++ {
			psMapIdx10To20(&dst[e], &src[e], full)
		}
	default:
		return src
	}
	return dst
}

// The index maps below are Tables 8.44/8.45's band correspondences, band
// borders averaged where several source bands merge.

func psMapIdx10To20(dst, src *[34]int8, full bool) {
	b := 4
	if full {
		b = 9
	} else {
		dst[10] = 0
	}
	for ; b >= 0; b-- {
		dst[2*b] = src[b]
		dst[2*b+1] = src[b]
	}
}

func psMapIdx34To20(dst, src *[34]int8, full bool) {
	dst[0] = (2*src[0] + src[1]) / 3
	dst[1] = (src[1] + 2*src[2]) / 3
	dst[2] = (2*src[3] + src[4]) / 3
	dst[3] = (src[4] + 2*src[5]) / 3
	dst[4] = (src[6] + src[7]) / 2
	dst[5] = (src[8] + src[9]) / 2
	dst[6] = src[10]
	dst[7] = src[11]
	dst[8] = (src[12] + src[13]) / 2
	dst[9] = (src[14] + src[15]) / 2
	dst[10] = src[16]
	if !full {
		return
	}
	dst[11] = src[17]
	dst[12] = src[18]
	dst[13] = src[19]
	dst[14] = (src[20] + src[21]) / 2
	dst[15] = (src[22] + src[23]) / 2
	dst[16] = (src[24] + src[25]) / 2
	dst[17] = (src[26] + src[27]) / 2
	dst[18] = (src[28] + src[29] + src[30] + src[31]) / 4
	dst[19] = (src[32] + src[33]) / 2
}

func psMapIdx10To34(dst, src *[34]int8, full bool) {
	if full {
		for i, s := range [34]int8{
			0, 0, 0, 1, 1, 1, 2, 2, 2, 2, 3, 3, 4, 4, 4, 4, 5, 5,
			6, 6, 7, 7, 7, 7, 8, 8, 8, 8, 9, 9, 9, 9, 9, 9,
		} {
			dst[i] = src[s]
		}
		return
	}
	for i, s := range [16]int8{0, 0, 0, 1, 1, 1, 2, 2, 2, 2, 3, 3, 4, 4, 4, 4} {
		dst[i] = src[s]
	}
	dst[16] = 0
}

func psMapIdx20To34(dst, src *[34]int8, full bool) {
	if full {
		dst[33] = src[19]
		dst[32] = src[19]
		dst[31] = src[18]
		dst[30] = src[18]
		dst[29] = src[18]
		dst[28] = src[18]
		dst[27] = src[17]
		dst[26] = src[17]
		dst[25] = src[16]
		dst[24] = src[16]
		dst[23] = src[15]
		dst[22] = src[15]
		dst[21] = src[14]
		dst[20] = src[14]
		dst[19] = src[13]
		dst[18] = src[12]
		dst[17] = src[11]
	}
	dst[16] = src[10]
	dst[15] = src[9]
	dst[14] = src[9]
	dst[13] = src[8]
	dst[12] = src[8]
	dst[11] = src[7]
	dst[10] = src[6]
	dst[9] = src[5]
	dst[8] = src[5]
	dst[7] = src[4]
	dst[6] = src[4]
	dst[5] = src[3]
	dst[4] = (src[2] + src[3]) / 2
	dst[3] = src[2]
	dst[2] = src[1]
	dst[1] = (src[0] + src[1]) / 2
	dst[0] = src[0]
}

// The value maps carry the float mixing anchors across a band-count
// switch, the same correspondences as the index maps.

func psMapVal34To20(par *[34]float32) {
	par[0] = (2*par[0] + par[1]) / 3
	par[1] = (par[1] + 2*par[2]) / 3
	par[2] = (2*par[3] + par[4]) / 3
	par[3] = (par[4] + 2*par[5]) / 3
	par[4] = (par[6] + par[7]) / 2
	par[5] = (par[8] + par[9]) / 2
	par[6] = par[10]
	par[7] = par[11]
	par[8] = (par[12] + par[13]) / 2
	par[9] = (par[14] + par[15]) / 2
	par[10] = par[16]
	par[11] = par[17]
	par[12] = par[18]
	par[13] = par[19]
	par[14] = (par[20] + par[21]) / 2
	par[15] = (par[22] + par[23]) / 2
	par[16] = (par[24] + par[25]) / 2
	par[17] = (par[26] + par[27]) / 2
	par[18] = (par[28] + par[29] + par[30] + par[31]) / 4
	par[19] = (par[32] + par[33]) / 2
}

func psMapVal20To34(par *[34]float32) {
	par[33] = par[19]
	par[32] = par[19]
	par[31] = par[18]
	par[30] = par[18]
	par[29] = par[18]
	par[28] = par[18]
	par[27] = par[17]
	par[26] = par[17]
	par[25] = par[16]
	par[24] = par[16]
	par[23] = par[15]
	par[22] = par[15]
	par[21] = par[14]
	par[20] = par[14]
	par[19] = par[13]
	par[18] = par[12]
	par[17] = par[11]
	par[16] = par[10]
	par[15] = par[9]
	par[14] = par[9]
	par[13] = par[8]
	par[12] = par[8]
	par[11] = par[7]
	par[10] = par[6]
	par[9] = par[5]
	par[8] = par[5]
	par[7] = par[4]
	par[6] = par[4]
	par[5] = par[3]
	par[4] = (par[2] + par[3]) / 2
	par[3] = par[2]
	par[2] = par[1]
	par[1] = (par[0] + par[1]) / 2
}
