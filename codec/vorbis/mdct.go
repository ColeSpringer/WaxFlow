package vorbis

import (
	"math"
	"sync"
)

// The inverse MDCT is the same fast, single-FFT factorization the AAC decoder
// uses (cross-checked there against the O(N^2) transform): for
//
//	y[n] = (2/N) Σ_{k=0}^{N/2-1} X[k] cos((2π/N)(n+n0)(k+1/2)), n0 = (N/2+1)/2
//
// the sum becomes y[n] = (2/N) Re{B·P[n]·FFT(X·A)[n]}. Vorbis defines the same
// TDAC MDCT, so the transform and phase match; the overall amplitude is fixed
// by vorbisNorm, calibrated so a decoded tone matches libvorbis.
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
	n          int
	aRe, aIm   []float64
	bpRe, bpIm []float64
	twRe, twIm []float64
	window     []float32 // length n/2, the rising half of the Vorbis window
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
	p := &imdctPlan{n: n}
	n0 := (float64(n)/2 + 1) / 2
	p.aRe = make([]float64, n/2)
	p.aIm = make([]float64, n/2)
	for k := 0; k < n/2; k++ {
		a := 2 * math.Pi * n0 * float64(k) / float64(n)
		p.aRe[k], p.aIm[k] = math.Cos(a), math.Sin(a)
	}
	scale := vorbisScale
	bAng := math.Pi * n0 / float64(n)
	bRe, bIm := math.Cos(bAng), math.Sin(bAng)
	p.bpRe = make([]float64, n)
	p.bpIm = make([]float64, n)
	for m := 0; m < n; m++ {
		a := math.Pi * float64(m) / float64(n)
		pRe, pIm := math.Cos(a), math.Sin(a)
		p.bpRe[m] = scale * (bRe*pRe - bIm*pIm)
		p.bpIm[m] = scale * (bRe*pIm + bIm*pRe)
	}
	for length := 2; length <= n; length <<= 1 {
		for k := 0; k < length/2; k++ {
			a := 2 * math.Pi * float64(k) / float64(length)
			p.twRe = append(p.twRe, math.Cos(a))
			p.twIm = append(p.twIm, math.Sin(a))
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
// using caller scratch (cr, ci each length n).
func (p *imdctPlan) imdct(spec []float32, out, cr, ci []float64) {
	n := p.n
	cr, ci = cr[:n], ci[:n]
	for k := 0; k < n/2; k++ {
		v := float64(spec[k])
		cr[k] = v * p.aRe[k]
		ci[k] = v * p.aIm[k]
	}
	for k := n / 2; k < n; k++ {
		cr[k], ci[k] = 0, 0
	}
	p.fft(cr, ci)
	for m := 0; m < n; m++ {
		out[m] = p.bpRe[m]*cr[m] - p.bpIm[m]*ci[m]
	}
}

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
