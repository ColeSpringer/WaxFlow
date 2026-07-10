package psy

import "math"

// fftPlan is an iterative radix-2 complex FFT with precomputed
// bit-reversal and twiddle tables. The model runs one forward transform
// per block per channel; at 2048 points that is far off any encoder's
// critical path, so the portable kernel is fine here (the SIMD flavor
// milestone owns shared transform kernels).
type fftPlan struct {
	n   int
	rev []int
	cos []float64
	sin []float64
}

func newFFTPlan(n int) *fftPlan {
	p := &fftPlan{n: n, rev: make([]int, n), cos: make([]float64, n/2), sin: make([]float64, n/2)}
	bits := 0
	for 1<<bits < n {
		bits++
	}
	for i := 0; i < n; i++ {
		r := 0
		for b := 0; b < bits; b++ {
			r = r<<1 | (i>>b)&1
		}
		p.rev[i] = r
	}
	for k := 0; k < n/2; k++ {
		p.cos[k] = math.Cos(2 * math.Pi * float64(k) / float64(n))
		p.sin[k] = -math.Sin(2 * math.Pi * float64(k) / float64(n))
	}
	return p
}

// transform runs the forward DFT in place: X[k] = sum x[n] e^(-i2pikn/N).
func (p *fftPlan) transform(re, im []float64) {
	n := p.n
	for i, r := range p.rev {
		if i < r {
			re[i], re[r] = re[r], re[i]
			im[i], im[r] = im[r], im[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		half := size / 2
		step := n / size
		for base := 0; base < n; base += size {
			k := 0
			for off := base; off < base+half; off++ {
				c, s := p.cos[k], p.sin[k]
				tRe := re[off+half]*c - im[off+half]*s
				tIm := re[off+half]*s + im[off+half]*c
				re[off+half] = re[off] - tRe
				im[off+half] = im[off] - tIm
				re[off] += tRe
				im[off] += tIm
				k += step
			}
		}
	}
}
