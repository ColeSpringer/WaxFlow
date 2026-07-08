package aac

import "math"

// Fast inverse MDCT via a single complex FFT. The AAC IMDCT
//
//	y[n] = (2/N) Σ_{k=0}^{N/2-1} X[k] cos((2π/N)(n+n0)(k+1/2)), n0 = (N/2+1)/2
//
// factors into y[n] = (2/N) Re{ B · P[n] · Y[n] } where Y is the length-N
// inverse DFT of c[k] = X[k]·A[k] (upper half zero), A[k] = exp(i2π·n0·k/N),
// P[n] = exp(iπ·n/N), and B = exp(iπ·n0/N). This replaces the O(N²) direct
// sum with an O(N log N) FFT. imdct_test.go cross-checks it against the
// direct transform.

// imdctPlan holds the precomputed rotation factors and FFT twiddles for one
// transform size. It is read-only after construction, so the package-level
// plans are shared safely across concurrent decoder sessions; the FFT
// scratch lives on the stack of each imdct call.
type imdctPlan struct {
	n          int
	aRe, aIm   []float64 // A[k], k = 0..N/2-1
	bpRe, bpIm []float64 // (2/N)·B·P[n], n = 0..N-1 (post-FFT rotation)
	twRe, twIm []float64 // FFT twiddles, one per (stage, k)
}

var (
	planLong  = newIMDCTPlan(2048)
	planShort = newIMDCTPlan(256)
)

func newIMDCTPlan(n int) *imdctPlan {
	p := &imdctPlan{n: n}
	n0 := (float64(n)/2 + 1) / 2
	p.aRe = make([]float64, n/2)
	p.aIm = make([]float64, n/2)
	for k := 0; k < n/2; k++ {
		a := 2 * math.Pi * n0 * float64(k) / float64(n)
		p.aRe[k], p.aIm[k] = math.Cos(a), math.Sin(a)
	}
	// B·P[m] with the 2/N output scale folded in, so imdct's post-FFT loop is
	// one complex multiply per point instead of rebuilding B·P every call.
	scale := 2.0 / float64(n)
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
	// FFT twiddles for the inverse transform (exp(+i·2π·k/length) per stage).
	for length := 2; length <= n; length <<= 1 {
		for k := 0; k < length/2; k++ {
			a := 2 * math.Pi * float64(k) / float64(length)
			p.twRe = append(p.twRe, math.Cos(a))
			p.twIm = append(p.twIm, math.Sin(a))
		}
	}
	return p
}

// imdct computes the plan's inverse MDCT of spec (length n/2) into out
// (length n).
func (p *imdctPlan) imdct(spec, out []float64) {
	n := p.n
	// Stack scratch, zeroed by declaration; the upper half stays zero
	// (the transform's input is the length-N/2 spectrum zero-padded to N).
	var reBuf, imBuf [2048]float64
	cr, ci := reBuf[:n], imBuf[:n]
	for k := 0; k < n/2; k++ {
		cr[k] = spec[k] * p.aRe[k]
		ci[k] = spec[k] * p.aIm[k]
	}
	p.fft(cr, ci)
	for m := 0; m < n; m++ {
		// Re{(2/N)·B·P[m]·Y[m]}, the rotation precomputed into bpRe/bpIm.
		out[m] = p.bpRe[m]*cr[m] - p.bpIm[m]*ci[m]
	}
}

// fft computes the in-place, unnormalized inverse FFT (exp(+i) kernel) of
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
