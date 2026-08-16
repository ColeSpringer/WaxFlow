package aac

import "math"

// SBR high-frequency generation and envelope adjustment (ISO/IEC 14496-3
// 4.6.18.5-7): dequantize the coded envelopes and noise floors, transpose
// low-band QMF content upward through the patch table with per-band chirped
// LPC inverse filtering, then shape the result to the coded envelopes,
// adding the noise floor and any signalled sinusoids.
//
// Energies live in this package's QMF domain, which is the spec's
// integer-PCM-scale domain divided by 32768 in amplitude: the dequant
// reference 2^6 becomes 2^(6-30), and the spec's +1 numerical guards on
// current energies become sbrEps. Gains are ratios and cancel the scale;
// only the additive noise and sine amplitudes depend on it.

// sbrEps is the spec's unit energy guard scaled into this domain (2^-30).
const sbrEps = 1.0 / (1 << 30)

// sbrGainBoostMax caps the compensation boost at +4 dB (10^0.2).
const sbrGainBoostMax = 1.584893192

// sbrChanDequant is one channel's dequantized frame data.
type sbrChanDequant struct {
	eOrig [5][64]float64 // per envelope, per band of its resolution
	qOrig [2][5]float64  // per noise envelope, per noise band
}

// sbrDequant is the element's dequantized data; coupling mixes the pair
// here so everything downstream is per-channel.
type sbrDequant struct {
	ch [2]sbrChanDequant
}

// dequantElement dequantizes every channel, resolving channel coupling
// (level on channel 0, balance on channel 1) into left/right energies.
func dequantElement(el *sbrElement, out *sbrDequant) {
	t := &el.tbl
	if el.chans == 1 || !el.coupled {
		for c := 0; c < el.chans; c++ {
			dequantPlain(&el.ch[c].frame, t, &out.ch[c])
		}
		return
	}
	fd0, fd1 := &el.ch[0].frame, &el.ch[1].frame
	a := 2.0
	pan := 24.0
	if fd0.ampRes == 1 {
		a = 1.0
		pan = 12.0
	}
	for l := 0; l < fd0.grid.numEnv; l++ {
		n := envBands(t, fd0.grid.freqRes[l])
		for b := 0; b < n; b++ {
			e0 := float64(fd0.env[l][b])
			e1 := float64(fd1.env[l][b])
			base := math.Exp2(e0/a+1) * math.Exp2(6-30)
			r := math.Exp2((pan - e1) / a)
			out.ch[0].eOrig[l][b] = base / (1 + r)
			out.ch[1].eOrig[l][b] = base * r / (1 + r)
		}
	}
	for l := 0; l < fd0.grid.numNoise; l++ {
		for b := 0; b < t.nQ; b++ {
			q0 := float64(fd0.noise[l][b])
			q1 := float64(fd1.noise[l][b])
			base := math.Exp2(7 - q0)
			r := math.Exp2(12 - q1)
			out.ch[0].qOrig[l][b] = base / (1 + r)
			out.ch[1].qOrig[l][b] = base * r / (1 + r)
		}
	}
}

// dequantPlain dequantizes an uncoupled channel: E = 2^(E/a) * 64 in spec
// scale, Q = 2^(6-Q) as a dimensionless noise-to-signal ratio.
func dequantPlain(fd *sbrFrameData, t *sbrFreqTables, out *sbrChanDequant) {
	a := 2.0
	if fd.ampRes == 1 {
		a = 1.0
	}
	for l := 0; l < fd.grid.numEnv; l++ {
		n := envBands(t, fd.grid.freqRes[l])
		for b := 0; b < n; b++ {
			out.eOrig[l][b] = math.Exp2(float64(fd.env[l][b])/a + (6 - 30))
		}
	}
	for l := 0; l < fd.grid.numNoise; l++ {
		for b := 0; b < t.nQ; b++ {
			out.qOrig[l][b] = math.Exp2(6 - float64(fd.noise[l][b]))
		}
	}
}

// hfGenerate transposes the low band into Xhigh over the frame's envelope
// span: per patch, per target band, the source band's signal plus its
// chirped second-order prediction (4.6.18.5).
func hfGenerate(st *sbrChanState, t *sbrFreqTables) {
	fd := &st.frame
	// Chirp factors per noise band, smoothed against the previous frame.
	// A transition between invf off and low in either direction takes the
	// intermediate 0.6 before smoothing.
	var bw [5]float64
	for q := 0; q < t.nQ; q++ {
		nb := sbrNewBw[fd.invf[q]&3]
		if fd.invf[q]+st.prevInvf[q] == 1 {
			nb = 0.6
		}
		if nb < st.bw[q] {
			bw[q] = 0.75*nb + 0.25*st.bw[q]
		} else {
			bw[q] = 0.90625*nb + 0.09375*st.bw[q]
		}
		if bw[q] < 0.015625 {
			bw[q] = 0
		}
		if bw[q] > 0.99609375 {
			bw[q] = 0.99609375
		}
		st.bw[q] = bw[q]
		st.prevInvf[q] = fd.invf[q]
	}
	first := fd.grid.tE[0]*2 + sbrTHFAdj
	last := fd.grid.tE[fd.grid.numEnv]*2 + sbrTHFAdj
	last = min(last, sbrBufSlots)
	for i := first; i < last; i++ {
		st.xhighRe[i] = [64]float32{}
		st.xhighIm[i] = [64]float32{}
	}
	// The LPC depends on the source band alone, and patches overlap their
	// sources, so each band's solve runs once per frame.
	var coefCache [32][4]float64
	var coefDone [32]bool
	target := t.kx
	for p := range t.patchNum {
		for i := 0; i < t.patchNum[p]; i++ {
			k := target + i
			src := t.patchStart[p] + i
			if k >= 64 || src < 0 || src >= 32 {
				continue
			}
			q := noiseBandFor(t, k)
			if !coefDone[src] {
				c0r, c0i, c1r, c1i := predictCoef(st, src)
				coefCache[src] = [4]float64{c0r, c0i, c1r, c1i}
				coefDone[src] = true
			}
			cc := &coefCache[src]
			a0r, a0i, a1r, a1i := cc[0], cc[1], cc[2], cc[3]
			b := bw[q]
			b2 := b * b
			c0r, c0i := b*a0r, b*a0i
			c1r, c1i := b2*a1r, b2*a1i
			for l := first; l < last; l++ {
				re := float64(st.xlowRe[l][src])
				im := float64(st.xlowIm[l][src])
				if b > 0 {
					r1 := float64(st.xlowRe[l-1][src])
					i1 := float64(st.xlowIm[l-1][src])
					r2 := float64(st.xlowRe[l-2][src])
					i2 := float64(st.xlowIm[l-2][src])
					re += c0r*r1 - c0i*i1 + c1r*r2 - c1i*i2
					im += c0r*i1 + c0i*r1 + c1r*i2 + c1i*r2
				}
				st.xhighRe[l][k] = float32(re)
				st.xhighIm[l][k] = float32(im)
			}
		}
		target += t.patchNum[p]
	}
}

// noiseBandFor returns the noise band containing an absolute subband.
func noiseBandFor(t *sbrFreqTables, k int) int {
	for q := t.nQ - 1; q > 0; q-- {
		if t.fNoise[q] <= k {
			return q
		}
	}
	return 0
}

// predictCoef solves the second-order covariance LPC for one low subband
// over the whole buffered slot range (4.6.18.5.3).
func predictCoef(st *sbrChanState, src int) (a0r, a0i, a1r, a1i float64) {
	var p01r, p01i, p02r, p02i, p11, p12r, p12i, p22 float64
	for l := sbrTHFAdj; l < sbrBufSlots; l++ {
		x0r, x0i := float64(st.xlowRe[l][src]), float64(st.xlowIm[l][src])
		x1r, x1i := float64(st.xlowRe[l-1][src]), float64(st.xlowIm[l-1][src])
		x2r, x2i := float64(st.xlowRe[l-2][src]), float64(st.xlowIm[l-2][src])
		p01r += x0r*x1r + x0i*x1i
		p01i += x0i*x1r - x0r*x1i
		p02r += x0r*x2r + x0i*x2i
		p02i += x0i*x2r - x0r*x2i
		p11 += x1r*x1r + x1i*x1i
		p12r += x1r*x2r + x1i*x2i
		p12i += x1i*x2r - x1r*x2i
		p22 += x2r*x2r + x2i*x2i
	}
	d := p22*p11 - (p12r*p12r+p12i*p12i)/1.000001
	if d != 0 {
		// a1 = (p01*p12 - p02*p11) / d
		a1r = (p01r*p12r - p01i*p12i - p02r*p11) / d
		a1i = (p01r*p12i + p01i*p12r - p02i*p11) / d
	}
	if p11 != 0 {
		// a0 = -(p01 + a1*conj(p12)) / p11
		a0r = -(p01r + a1r*p12r + a1i*p12i) / p11
		a0i = -(p01i + a1i*p12r - a1r*p12i) / p11
	}
	// Negated small-bound form so a NaN (a non-finite core sample can reach
	// the covariance via PNS) fails the guard instead of slipping through
	// every >= comparison and fanning out across the patch.
	if !(a0r*a0r+a0i*a0i < 16) || !(a1r*a1r+a1i*a1i < 16) {
		return 0, 0, 0, 0
	}
	return a0r, a0i, a1r, a1i
}

// sinePhase is the quarter-cycle sinusoid table the harmonic tool rotates
// through, one step per QMF slot.
var sinePhase = [4][2]float64{{1, 0}, {0, 1}, {-1, 0}, {0, -1}}

// hfAdjust shapes Xhigh to the coded envelopes and writes Y (4.6.18.7).
// Gains for every envelope are computed first, then assembly runs per
// slot: the gain FIFO smooths over the previous four slots (crossing
// envelope and frame boundaries), sinusoid-start envelopes take raw gains
// and no noise, and the noise and sine indices advance in spec order.
func hfAdjust(st *sbrChanState, t *sbrFreqTables, hdr sbrHeader, dq *sbrChanDequant, reset bool) {
	fd := &st.frame
	g := &fd.grid
	m := t.m
	limGain := sbrLimGain[hdr.limiterGains&3]
	hSL := 0
	if hdr.smoothingMode == 0 {
		hSL = 4
	}

	// Sinusoid bookkeeping: a sine rides the middle subband of its
	// high-resolution band, starts at envelope lA (immediately when the
	// frame signals none), and continues from the previous frame when it
	// was active there.
	var sineIdxBand [64]int // per m: the high-res band it is the middle of, else -1
	for i := range sineIdxBand {
		sineIdxBand[i] = -1
	}
	for i := 0; i < t.nHigh; i++ {
		mid := (t.fHigh[i]+t.fHigh[i+1])/2 - t.kx
		if mid >= 0 && mid < m {
			sineIdxBand[mid] = i
		}
	}

	// Per-envelope gain, noise, and sine vectors.
	var gainE, qME, sME [5][64]float64
	var sIndexE [5][64]bool
	var deltaE [5]float64
	var curSine [64]bool
	for l := 0; l < g.numEnv; l++ {
		// Noise gating: the sinusoid-start envelope, and envelope zero when
		// the previous frame's ramp ended exactly at the frame edge, take no
		// noise floor.
		delta := 1.0
		if l == g.lA || (l == 0 && st.prevLaEnd) {
			delta = 0
		}
		deltaE[l] = delta
		lq := 0
		if g.numNoise == 2 && g.tE[l] >= g.tQ[1] {
			lq = 1
		}

		// Per-subband mapped envelope, noise, and sine markers.
		var eMapped, qMapped [64]float64
		var sMapped [64]bool
		sIndex := &sIndexE[l]
		res := g.freqRes[l]
		bands := t.fLow
		if res == 1 {
			bands = t.fHigh
		}
		band := 0
		for mi := 0; mi < m; mi++ {
			k := t.kx + mi
			for band+1 < len(bands)-1 && bands[band+1] <= k {
				band++
			}
			eMapped[mi] = dq.eOrig[l][band]
			qMapped[mi] = dq.qOrig[lq][noiseBandFor(t, k)]
			if hb := sineIdxBand[mi]; hb >= 0 && fd.addHarmonic[hb] {
				if l >= g.lA || st.sIndexPrev[mi] {
					sIndex[mi] = true
				}
			}
		}
		// sMapped covers the whole band containing an active sine at this
		// envelope's resolution.
		for mi := 0; mi < m; mi++ {
			if !sIndex[mi] {
				continue
			}
			k := t.kx + mi
			for b := 0; b < len(bands)-1; b++ {
				if bands[b] <= k && k < bands[b+1] {
					for kk := bands[b]; kk < bands[b+1]; kk++ {
						if kk-t.kx >= 0 && kk-t.kx < m {
							sMapped[kk-t.kx] = true
						}
					}
				}
			}
		}

		// Current envelope-scale energy of the generated high band.
		var eCurr [64]float64
		slot0, slot1 := g.tE[l]*2+sbrTHFAdj, g.tE[l+1]*2+sbrTHFAdj
		slot1 = min(slot1, sbrBufSlots)
		width := float64(max(slot1-slot0, 1))
		if hdr.interpolFreq == 1 {
			for mi := 0; mi < m; mi++ {
				k := t.kx + mi
				var sum float64
				for i := slot0; i < slot1; i++ {
					sum += float64(st.xhighRe[i][k])*float64(st.xhighRe[i][k]) +
						float64(st.xhighIm[i][k])*float64(st.xhighIm[i][k])
				}
				eCurr[mi] = sum / width
			}
		} else {
			for b := 0; b < len(bands)-1; b++ {
				lo, hi := max(bands[b], t.kx), min(bands[b+1], t.kx+m)
				if lo >= hi {
					continue
				}
				var sum float64
				for k := lo; k < hi; k++ {
					for i := slot0; i < slot1; i++ {
						sum += float64(st.xhighRe[i][k])*float64(st.xhighRe[i][k]) +
							float64(st.xhighIm[i][k])*float64(st.xhighIm[i][k])
					}
				}
				avg := sum / (width * float64(hi-lo))
				for k := lo; k < hi; k++ {
					eCurr[k-t.kx] = avg
				}
			}
		}

		// Gains with the limiter, then the boost, per limiter band.
		gain, qM, sM := &gainE[l], &qME[l], &sME[l]
		for lb := 0; lb < t.nL; lb++ {
			lo, hi := t.fLim[lb]-t.kx, t.fLim[lb+1]-t.kx
			lo, hi = max(lo, 0), min(hi, m)
			if lo >= hi {
				continue
			}
			var sumOrig, sumCurr float64
			for mi := lo; mi < hi; mi++ {
				sumOrig += eMapped[mi]
				sumCurr += eCurr[mi]
			}
			gMax := limGain * math.Sqrt((sbrEps+sumOrig)/(sbrEps+sumCurr))
			gMax = min(gMax, 1e10)
			for mi := lo; mi < hi; mi++ {
				den := 1 + qMapped[mi]
				qM[mi] = math.Sqrt(eMapped[mi] * qMapped[mi] / den)
				if sIndex[mi] {
					sM[mi] = math.Sqrt(eMapped[mi] / den)
				} else {
					sM[mi] = 0
				}
				if sMapped[mi] {
					gain[mi] = math.Sqrt(eMapped[mi] * qMapped[mi] / ((sbrEps + eCurr[mi]) * den))
				} else {
					gain[mi] = math.Sqrt(eMapped[mi] / ((sbrEps + eCurr[mi]) * (1 + qMapped[mi]*delta)))
				}
				if gain[mi] > gMax {
					qM[mi] = qM[mi] * gMax / gain[mi]
					gain[mi] = gMax
				}
			}
			var denom float64
			for mi := lo; mi < hi; mi++ {
				denom += eCurr[mi] * gain[mi] * gain[mi]
				denom += sM[mi] * sM[mi]
				if sM[mi] == 0 {
					denom += delta * qM[mi] * qM[mi]
				}
			}
			boost := min(math.Sqrt((sbrEps+sumOrig)/(sbrEps+denom)), sbrGainBoostMax)
			for mi := lo; mi < hi; mi++ {
				gain[mi] *= boost
				qM[mi] *= boost
				sM[mi] *= boost
			}
		}
		curSine = *sIndex
	}

	// Gain FIFO: rows are absolute slots plus the smoothing lead. A reset
	// seeds the lead rows with the first envelope's gains; otherwise the
	// previous frame's last rows slide under the new first envelope.
	base := 2 * g.tE[0]
	if reset {
		for i := 0; i < hSL; i++ {
			st.gTemp[i+base] = gainE[0]
			st.qTemp[i+base] = qME[0]
		}
	} else if hSL > 0 {
		for i := 0; i < 4; i++ {
			if i+st.prevEnvEnd < len(st.gTemp) && i+base < len(st.gTemp) {
				st.gTemp[i+base] = st.gTemp[i+st.prevEnvEnd]
				st.qTemp[i+base] = st.qTemp[i+st.prevEnvEnd]
			}
		}
	}
	for l := 0; l < g.numEnv; l++ {
		for i := 2 * g.tE[l]; i < 2*g.tE[l+1] && hSL+i < len(st.gTemp); i++ {
			st.gTemp[hSL+i] = gainE[l]
			st.qTemp[hSL+i] = qME[l]
		}
	}

	// Assembly: Y = Xhigh*gFilt plus noise or sine, indices advancing per
	// the spec's orders (noise per subband, sine per slot).
	for l := 0; l < g.numEnv; l++ {
		gain, qM, sM := &gainE[l], &qME[l], &sME[l]
		smoothed := hSL > 0 && deltaE[l] != 0
		for i := 2 * g.tE[l]; i < 2*g.tE[l+1]; i++ {
			slot := i + sbrTHFAdj
			if slot >= sbrBufSlots || hSL+i >= len(st.gTemp) {
				break
			}
			var gFilt, qFilt [64]float64
			if smoothed {
				for mi := 0; mi < m; mi++ {
					for j := 0; j <= 4; j++ {
						gFilt[mi] += sbrSmoothWindow[j] * st.gTemp[hSL+i-j][mi]
						qFilt[mi] += sbrSmoothWindow[j] * st.qTemp[hSL+i-j][mi]
					}
				}
			} else {
				gFilt, qFilt = *gain, *qM
			}
			for mi := 0; mi < m; mi++ {
				k := t.kx + mi
				yr := float64(st.xhighRe[slot][k]) * gFilt[mi]
				yi := float64(st.xhighIm[slot][k]) * gFilt[mi]
				st.idxNoise = (st.idxNoise + 1) & 511
				if sM[mi] != 0 {
					ph := sinePhase[st.idxSine]
					sign := 1.0
					if k&1 == 1 {
						sign = -1
					}
					yr += sM[mi] * ph[0]
					yi += sM[mi] * ph[1] * sign
				} else if deltaE[l] != 0 {
					v := sbrNoiseTable[st.idxNoise]
					yr += qFilt[mi] * v[0]
					yi += qFilt[mi] * v[1]
				}
				st.yRe[slot][k] = float32(yr)
				st.yIm[slot][k] = float32(yi)
			}
			st.idxSine = (st.idxSine + 1) & 3
		}
	}

	st.sIndexPrev = curSine
	st.prevLaEnd = g.lA == g.numEnv
	st.prevEnvEnd = 2 * g.tE[g.numEnv]
}
