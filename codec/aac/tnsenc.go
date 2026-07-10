package aac

import "math"

// TNS analysis (ISO 14496-3 4.6.9): LPC over the upper spectrum decides
// whether temporal noise shaping pays, quantizes the reflection
// coefficients exactly as parseTNS dequantizes them, and applies the
// all-zero analysis filter the decoder's all-pole synthesis inverts.
// Long windows only: the win is largest on pitched/transient material
// inside long blocks, and tns_data_present simply stays 0 for shorts.

const (
	tnsMaxOrderLC = 12  // LC profile long-window order cap
	tnsMinGain    = 1.4 // prediction gain below this is not worth the side bits
	tnsStartHz    = 1500.0
)

// tnsEnc is one channel's TNS decision for the frame.
type tnsEnc struct {
	present bool
	order   int
	length  int // in scalefactor bands, counting down from numSwb
	coef    [tnsMaxOrderLC]int
}

// analyzeTNS decides, quantizes, and (when it pays) filters spec in
// place. numSwb/maxSfb/rateIdx describe the long-window geometry.
func analyzeTNS(spec *[1024]float64, swb []uint16, numSwb, maxSfb, rateIdx, rate int) tnsEnc {
	maxBands := tnsMaxBandsLong[rateIdx]
	// The filtered region: from the band at ~1.5 kHz up to the caps the
	// decoder applies (tns_max_bands, max_sfb).
	startSfb := 0
	lineHz := float64(rate) / 2048
	for startSfb < numSwb && float64(swb[startSfb])*lineHz < tnsStartHz {
		startSfb++
	}
	topSfb := min(min(numSwb, maxBands), maxSfb)
	start, end := int(swb[min(startSfb, topSfb)]), int(swb[topSfb])
	if end-start < 32 {
		return tnsEnc{}
	}
	region := spec[start:end]

	// Autocorrelation and Levinson-Durbin up to the LC order cap.
	var r [tnsMaxOrderLC + 1]float64
	for lag := 0; lag <= tnsMaxOrderLC; lag++ {
		s := 0.0
		for i := lag; i < len(region); i++ {
			s += region[i] * region[i-lag]
		}
		r[lag] = s
	}
	if r[0] <= 0 {
		return tnsEnc{}
	}
	var lpc [tnsMaxOrderLC + 1]float64
	var refl [tnsMaxOrderLC]float64
	lpc[0] = 1
	errE := r[0]
	order := 0
	for m := 1; m <= tnsMaxOrderLC; m++ {
		acc := r[m]
		for i := 1; i < m; i++ {
			acc += lpc[i] * r[m-i]
		}
		k := -acc / errE
		if math.Abs(k) >= 1 {
			break
		}
		var next [tnsMaxOrderLC + 1]float64
		copy(next[:], lpc[:])
		for i := 1; i < m; i++ {
			next[i] = lpc[i] + k*lpc[m-i]
		}
		next[m] = k
		lpc = next
		refl[m-1] = k
		errE *= 1 - k*k
		order = m
	}
	if order == 0 || errE <= 0 || r[0]/errE < tnsMinGain {
		return tnsEnc{}
	}
	// Trim negligible tail coefficients: they cost 4 bits each.
	for order > 1 && math.Abs(refl[order-1]) < 0.05 {
		order--
	}

	// Quantize the reflection coefficients exactly as parseTNS
	// dequantizes them (coef_res 1 -> 4-bit values, coef_compress 0),
	// then rebuild the LPC from the QUANTIZED values so encoder filter
	// and decoder filter agree.
	const resBits = 4
	iqfac := (float64(int(1)<<uint(resBits-1)) - 0.5) / (math.Pi / 2)
	iqfacM := (float64(int(1)<<uint(resBits-1)) + 0.5) / (math.Pi / 2)
	t := tnsEnc{present: true, order: order, length: numSwb - startSfb}
	var qrefl [tnsMaxOrderLC]float64
	for i := 0; i < order; i++ {
		fac := iqfac
		if refl[i] < 0 {
			fac = iqfacM
		}
		c := int(math.Round(math.Asin(refl[i]) * fac))
		if c > 7 {
			c = 7
		} else if c < -8 {
			c = -8
		}
		t.coef[i] = c
		if c < 0 {
			qrefl[i] = math.Sin(float64(c) / iqfacM)
		} else {
			qrefl[i] = math.Sin(float64(c) / iqfac)
		}
	}
	var a [tnsMaxOrderLC]float64
	reflToLPC(qrefl[:order], a[:order])

	// All-zero analysis filter y[n] = x[n] + Σ a[j]·x[n-1-j], forward
	// direction, over the same region the decoder will synthesize.
	var hist [tnsMaxOrderLC]float64
	for i := range region {
		x := region[i]
		y := x
		for j := 0; j < order && j < i; j++ {
			y += a[j] * hist[j]
		}
		for j := order - 1; j > 0; j-- {
			hist[j] = hist[j-1]
		}
		hist[0] = x
		region[i] = y
	}
	return t
}

// sideBits is the tns_data cost in bits for a long window.
func (t *tnsEnc) sideBits() int {
	if !t.present {
		return 0
	}
	// n_filt(2) + coef_res(1) + length(6) + order(5) + direction(1) +
	// compress(1) + 4 bits per coefficient.
	return 2 + 1 + 6 + 5 + 1 + 1 + 4*t.order
}

// write emits tns_data for a long window (one filter).
func (t *tnsEnc) write(w *bitWriter) {
	w.writeBits(2, 1) // n_filt = 1
	w.writeBits(1, 1) // coef_res = 1 (4-bit coefficients)
	w.writeBits(6, uint64(t.length))
	w.writeBits(5, uint64(t.order))
	w.writeBits(1, 0) // direction: forward
	w.writeBits(1, 0) // coef_compress = 0
	for i := 0; i < t.order; i++ {
		w.writeBits(4, uint64(t.coef[i]&0xF))
	}
}
