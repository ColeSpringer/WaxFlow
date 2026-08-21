package vorbis

import (
	"math"
	"sync"
)

// The inverse MDCT is the same N/4-point factorization the AAC decoder uses
// (see codec/aac/imdct.go for the derivation, and codec/wma/mdct.go for a
// third instance of it): for
//
//	y[n] = scale · Σ_{k=0}^{N/2-1} X[k] cos((2π/N)(n+n0)(k+1/2)), n0 = (N/2+1)/2
//
// the middle half is y[N/4 + h] = -scale·C[N/2-1-h] with C the DCT-IV of the
// spectrum, and the outer quarters follow from the kernel's own symmetries
// without being computed. Vorbis defines the same TDAC MDCT as AAC, so the
// transform and phase match; the overall amplitude is fixed by vorbisNorm,
// calibrated so a decoded tone matches libvorbis.
//
// The FFT this runs is a quarter of the output length. The transform it
// replaced was a full N-point complex FFT fed a spectrum zero-padded to N, so
// half of that transform was multiplying zeroes. TestIMDCTMatchesDirect scores
// it against the O(N^2) sum, which is what makes it checkable independently of
// either implementation.
//
// vorbisScale is the IMDCT output scale. Unlike AAC, Vorbis's backward MDCT
// (libvorbis mdct.c) carries no 1/N normalization: the encoder's forward
// transform holds it, so the decoder is the raw inverse cosine sum. That makes
// the scale the constant 1 (the sign matches libvorbis's/ffmpeg's phase
// convention), not the AAC decoder's 2/N. Calibrated in decode_test.go.
const vorbisScale = 1.0

// imdctPlan holds read-only rotation factors, FFT twiddles, and the synthesis
// window for one block size, shared across concurrent sessions.
type imdctPlan struct {
	n  int // the time-domain length
	m  int // n/4, the FFT length
	n2 int // n/2, the DCT-IV length and the coefficient count
	// rotRe/rotIm is exp(-2*pi*i*(j + 1/8)/n), the rotation the DCT-IV applies
	// before and after its FFT; one table serves both passes.
	rotRe, rotIm []float64
	twRe, twIm   []float64 // forward-FFT twiddles, one per (stage, k)
	window       []float32 // length n/2, the rising half of the Vorbis window
}

var (
	planMu    sync.Mutex
	planCache = map[int]*imdctPlan{}
)

func getPlan(n int) *imdctPlan {
	planMu.Lock()
	defer planMu.Unlock()
	if p, ok := planCache[n]; ok {
		return p
	}
	p := newIMDCTPlan(n)
	planCache[n] = p
	return p
}

func newIMDCTPlan(n int) *imdctPlan {
	p := &imdctPlan{n: n, m: n / 4, n2: n / 2}
	p.rotRe = make([]float64, p.m)
	p.rotIm = make([]float64, p.m)
	for j := 0; j < p.m; j++ {
		a := 2 * math.Pi * (float64(j) + 0.125) / float64(n)
		p.rotRe[j], p.rotIm[j] = math.Cos(a), -math.Sin(a)
	}
	// Forward-FFT twiddles, exp(-i*2*pi*k/length) per stage.
	for length := 2; length <= p.m; length <<= 1 {
		for k := 0; k < length/2; k++ {
			a := 2 * math.Pi * float64(k) / float64(length)
			p.twRe = append(p.twRe, math.Cos(a))
			p.twIm = append(p.twIm, -math.Sin(a))
		}
	}
	// Vorbis window (spec 1.3.2): the rising half, indexed [0, n/2).
	p.window = make([]float32, n/2)
	for i := range p.window {
		s := math.Sin((float64(i) + 0.5) / float64(n) * math.Pi)
		p.window[i] = float32(math.Sin(math.Pi / 2 * s * s))
	}
	return p
}

// imdct computes the inverse MDCT of spec (length n/2) into out (length n),
// using caller scratch (cr, ci each at least length n/4). The DCT-IV lands in
// out's own middle half, so it needs no third buffer.
func (p *imdctPlan) imdct(spec []float32, out, cr, ci []float64) {
	m, n2, half := p.m, p.n2, p.n/4
	cr, ci = cr[:m], ci[:m]

	// Pre-rotation: fold the coefficients from both ends into n/4 complex
	// samples, X[2j] against X[n2-1-2j], and rotate.
	for j := 0; j < m; j++ {
		x1, x2 := float64(spec[2*j]), float64(spec[n2-1-2*j])
		c, s := p.rotRe[j], p.rotIm[j]
		cr[j] = x1*c - x2*s
		ci[j] = x2*c + x1*s
	}

	p.fft(cr, ci)

	// Post-rotation, straight into the middle half. One complex value carries
	// C[2k] in its real part and C[n2-1-2k] in its imaginary part; reversed
	// and negated, they are y[n/4 + h].
	mid := out[half : half+n2]
	for k := 0; k < m; k++ {
		re, im := cr[k], ci[k]
		c, s := p.rotRe[k], p.rotIm[k]
		mid[n2-1-2*k] = -vorbisScale * (re*c - im*s)
		mid[2*k] = vorbisScale * (im*c + re*s)
	}

	// The outer quarters, by the transform's own symmetries. Both read only
	// from the middle half, which is now complete.
	for q := 0; q < half; q++ {
		out[half-1-q] = -out[half+q]
	}
	for r := 0; r < half; r++ {
		out[3*half+r] = out[3*half-1-r]
	}
}

// fft computes the in-place, unnormalized forward FFT (exp(-i) kernel) of
// (re, im), radix-2 Cooley-Tukey with precomputed twiddles.
func (p *imdctPlan) fft(re, im []float64) {
	n := len(re)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			re[i], re[j] = re[j], re[i]
			im[i], im[j] = im[j], im[i]
		}
	}
	tw := 0
	for length := 2; length <= n; length <<= 1 {
		half := length / 2
		base := tw
		for i := 0; i < n; i += length {
			for k := 0; k < half; k++ {
				wr, wi := p.twRe[base+k], p.twIm[base+k]
				a, b := i+k, i+k+half
				vr := re[b]*wr - im[b]*wi
				vi := re[b]*wi + im[b]*wr
				re[b], im[b] = re[a]-vr, im[a]-vi
				re[a], im[a] = re[a]+vr, im[a]+vi
			}
		}
		tw += half
	}
}

// applyWindow multiplies the time-domain block buf (length n) in place by the
// Vorbis window, using neighbour block sizes to size the left and right
// overlap ramps (spec 1.3.2). ln and rn are the left- and right-neighbour
// block sizes; leftWin and rightWin are their window tables (rising halves).
func applyWindow(buf []float64, n, ln, rn int, leftWin, rightWin []float32) {
	leftBegin := n/4 - ln/4
	leftEnd := leftBegin + ln/2
	rightBegin := 3*n/4 - rn/4
	rightEnd := rightBegin + rn/2
	for i := 0; i < leftBegin; i++ {
		buf[i] = 0
	}
	for i := leftBegin; i < leftEnd; i++ {
		buf[i] *= float64(leftWin[i-leftBegin])
	}
	// [leftEnd, rightBegin): flat 1.0, unchanged.
	for i := rightBegin; i < rightEnd; i++ {
		buf[i] *= float64(rightWin[rightEnd-1-i])
	}
	for i := rightEnd; i < n; i++ {
		buf[i] = 0
	}
}
