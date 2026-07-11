package loudness

import (
	"math"

	"github.com/colespringer/waxflow/dsp/internal/firwin"
)

// tpTaps is the tap count of each fractional interpolation phase. At
// the 4x factor that is 48 taps total, the BS.1770-4 Annex 2 scale.
const tpTaps = 12

// truePeak estimates inter-sample peaks by polyphase windowed-sinc
// interpolation per BS.1770-4 Annex 2: factor 4 below 96 kHz, factor 2
// up to 192 kHz. Above 192 kHz newTruePeak returns nil and the caller
// uses the sample grid directly, it is already dense enough. Phase 0 of
// the polyphase bank is the input sample itself, which the meter tracks
// as the sample peak, so only the fractional phases are filtered.
type truePeak struct {
	phases [][tpTaps]float64
	// hist is one double-length ring per channel: each sample is written
	// at pos and pos+tpTaps, so the last tpTaps samples always sit
	// contiguous (oldest first) starting at pos+1, and no per-sample
	// window copy is needed. The evaluation order over the window is
	// identical to a shifted buffer's, so results stay bit-identical.
	hist [][]float64
	pos  []int
}

// newTruePeak builds the interpolator for a rate, or returns nil when
// no oversampling is needed.
func newTruePeak(rate, channels int) *truePeak {
	factor := 4
	switch {
	case rate > 192000:
		return nil
	case rate >= 96000:
		factor = 2
	}
	tp := &truePeak{
		phases: make([][tpTaps]float64, factor-1),
		hist:   make([][]float64, channels),
		pos:    make([]int, channels),
	}
	for c := range tp.hist {
		tp.hist[c] = make([]float64, 2*tpTaps)
	}
	// Kaiser windowed sinc with cutoff at the original Nyquist. Beta 6
	// puts the stopband near 60 dB with the passband flat through half
	// Nyquist (about 0.03 dB), plenty for a peak detector; normalizing
	// each phase to unity DC gain pins the passband at exactly 1.
	const beta = 6.0
	const half = tpTaps / 2
	i0 := firwin.BesselI0(beta)
	for p := range tp.phases {
		frac := float64(p+1) / float64(factor)
		ph := &tp.phases[p]
		var sum float64
		for t := 0; t < tpTaps; t++ {
			// Tap t weighs sample j-half+1+t for the value at j+frac.
			x := frac - float64(t-half+1)
			u := x / half
			w := firwin.BesselI0(beta*math.Sqrt(1-u*u)) / i0
			ph[t] = firwin.Sinc(x) * w
			sum += ph[t]
		}
		for t := range ph {
			ph[t] /= sum
		}
	}
	return tp
}

// push feeds one sample of channel c and returns the largest magnitude
// among the sample itself and the fractional-phase values that become
// computable with it (the interpolation point half a window back, so
// peaks never straddle a Process boundary).
func (tp *truePeak) push(c int, x float64) float64 {
	buf := tp.hist[c]
	p := tp.pos[c]
	buf[p] = x
	buf[p+tpTaps] = x
	if p++; p == tpTaps {
		p = 0
	}
	tp.pos[c] = p
	win := buf[p : p+tpTaps]
	peak := math.Abs(x)
	for p := range tp.phases {
		ph := &tp.phases[p]
		var acc float64
		for t := 0; t < tpTaps; t++ {
			acc += ph[t] * win[t]
		}
		if a := math.Abs(acc); a > peak {
			peak = a
		}
	}
	return peak
}

// drain flushes the interpolator by pushing a full window of silence
// per channel, so evaluations that still needed future samples, and the
// ringing just past the final sample, are counted.
func (tp *truePeak) drain() float64 {
	var peak float64
	for c := range tp.hist {
		for i := 0; i < tpTaps; i++ {
			if p := tp.push(c, 0); p > peak {
				peak = p
			}
		}
	}
	return peak
}
