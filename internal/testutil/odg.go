package testutil

import "math"

// ODG-proxy: an objective difference grade approximated by a bark-band
// noise-to-mask ratio, the core measurement of PEAQ's basic model. It is a
// spectral-distortion proxy, not a calibrated PEAQ implementation, and the
// quality gates compare deltas between encoders on this identical metric
// (docs/quality-gates.md), so a consistent, monotonic ordering is what
// matters, not absolute PEAQ agreement. Higher (closer to 0) is better;
// -4 is very annoying.
//
// The measurement: both signals are downmixed to mono, aligned (the encoder
// and codec delays are found by correlation), and framed with a Hann window.
// Per frame the error spectrum is compared to a masking threshold derived
// from the reference's per-band energy, and the resulting noise-to-mask
// ratios are pooled across bands and frames, then mapped to the ODG scale.

// odgWindow and odgHop are the analysis frame and 50% hop in samples.
const (
	odgWindow = 2048
	odgHop    = 1024
)

// ODGProxy returns the ODG-proxy of a coded signal (test) against its
// reference, both interleaved with the given channel count and sample rate.
// The signals need not be pre-aligned; the encoder and codec delay are found
// by a correlation search.
func ODGProxy(ref, test []float32, rate, channels int) float64 {
	r := downmixMono(ref, channels)
	t := downmixMono(test, channels)
	if len(r) < odgWindow || len(t) < odgWindow {
		return -4 // unmeasurable (empty/short output): fail closed, not open
	}
	// Align test to ref: find the lag minimizing early-segment error.
	lag := alignLag(r, t, 3000)
	if lag > 0 {
		if lag >= len(t) {
			return -4
		}
		t = t[lag:]
	}
	n := min(len(r), len(t))
	r, t = r[:n], t[:n]

	bands := barkBands(rate, odgWindow)
	win := hann(odgWindow)

	var nmrSum float64
	var frames int
	rc := make([]complex128, odgWindow)
	tc := make([]complex128, odgWindow)
	for off := 0; off+odgWindow <= n; off += odgHop {
		for i := 0; i < odgWindow; i++ {
			rc[i] = complex(float64(r[off+i])*win[i], 0)
			tc[i] = complex(float64(t[off+i])*win[i], 0)
		}
		fft(rc)
		fft(tc)
		nmrSum += frameNMR(rc, tc, bands)
		frames++
	}
	if frames == 0 {
		return -4 // no measurable frame: fail closed
	}
	nmrDB := 10 * math.Log10(nmrSum/float64(frames)+1e-12)
	// Map mean NMR (dB) to ODG: noise well below the mask (<= -25 dB) is
	// imperceptible (0); noise well above it (>= +5 dB) is very annoying (-4).
	const lo, hi = -25.0, 5.0
	odg := -4 * (nmrDB - lo) / (hi - lo)
	return clampODG(odg)
}

// frameNMR is the mean per-band noise-to-mask ratio for one frame's spectra.
// The masking threshold is the band's own energy attenuated ~13 dB (intra-band
// masking) plus a global floor 40 dB below the loudest band, a crude stand-in
// for spreading and the threshold of hearing that keeps noise far below the
// dominant component inaudible. Both terms scale with signal level, so the
// ratio is level-invariant.
func frameNMR(ref, test []complex128, bands [][2]int) float64 {
	half := len(ref) / 2
	sig := make([]float64, len(bands))
	noise := make([]float64, len(bands))
	var peak float64
	nb := 0
	for bi, b := range bands {
		lo := b[0]
		if lo >= half {
			break
		}
		hi := min(b[1], half)
		var s, e float64
		for k := lo; k < hi; k++ {
			s += real(ref[k])*real(ref[k]) + imag(ref[k])*imag(ref[k])
			dr := real(ref[k]) - real(test[k])
			di := imag(ref[k]) - imag(test[k])
			e += dr*dr + di*di
		}
		sig[bi], noise[bi] = s, e
		if s > peak {
			peak = s
		}
		nb++
	}
	if nb == 0 || peak == 0 {
		return 0
	}
	absThresh := peak * 1e-4 // 40 dB below the loudest band
	var sum float64
	for i := 0; i < nb; i++ {
		mask := sig[i]*0.05 + absThresh
		sum += noise[i] / mask
	}
	return sum / float64(nb)
}

// barkBands returns FFT bin ranges [lo, hi) for the 24 critical bands at the
// given rate and FFT size, clamped to Nyquist.
func barkBands(rate, size int) [][2]int {
	// The 24 critical-band edges, extended past 15.5 kHz so a lowpass or
	// high-frequency noise difference at 44.1/48 kHz (Nyquist ~22 kHz) is
	// still scored rather than falling outside the last band.
	edges := []float64{
		0, 100, 200, 300, 400, 510, 630, 770, 920, 1080, 1270, 1480, 1720,
		2000, 2320, 2700, 3150, 3700, 4400, 5300, 6400, 7700, 9500, 12000, 15500,
		19000, 24000,
	}
	binHz := float64(rate) / float64(size)
	var bands [][2]int
	for i := 0; i+1 < len(edges); i++ {
		lo := int(edges[i] / binHz)
		hi := int(edges[i+1] / binHz)
		if hi <= lo {
			hi = lo + 1
		}
		bands = append(bands, [2]int{lo, hi})
	}
	return bands
}

// downmixMono averages interleaved channels to a mono signal.
func downmixMono(x []float32, channels int) []float32 {
	if channels <= 1 {
		return x
	}
	n := len(x) / channels
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		var s float32
		for c := 0; c < channels; c++ {
			s += x[i*channels+c]
		}
		out[i] = s / float32(channels)
	}
	return out
}

// alignLag finds the shift of t against r (0..maxLag) minimizing mean square
// error over an early window, recovering the encoder plus codec delay.
func alignLag(r, t []float32, maxLag int) int {
	const win = 4000
	best, bestErr := 0, math.Inf(1)
	end := min(win, len(r))
	for lag := 0; lag < maxLag && lag+end <= len(t); lag++ {
		var e float64
		for i := 0; i < end; i++ {
			d := float64(r[i]) - float64(t[lag+i])
			e += d * d
		}
		if e < bestErr {
			bestErr, best = e, lag
		}
	}
	return best
}

func hann(n int) []float64 {
	w := make([]float64, n)
	for i := range w {
		w[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1))
	}
	return w
}

func clampODG(v float64) float64 {
	if v > 0 {
		return 0
	}
	if v < -4 {
		return -4
	}
	return v
}

// fft is an in-place iterative radix-2 Cooley-Tukey FFT; len(x) must be a
// power of two. Test-only, so it lives here rather than in the public tree.
func fft(x []complex128) {
	n := len(x)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}
	for length := 2; length <= n; length <<= 1 {
		ang := -2 * math.Pi / float64(length)
		wl := complex(math.Cos(ang), math.Sin(ang))
		for i := 0; i < n; i += length {
			w := complex(1, 0)
			for k := 0; k < length/2; k++ {
				u := x[i+k]
				v := x[i+k+length/2] * w
				x[i+k] = u + v
				x[i+k+length/2] = u - v
				w *= wl
			}
		}
	}
}
