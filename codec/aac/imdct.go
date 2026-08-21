package aac

import "math"

// The MDCT pair, both directions through one DCT-IV of length N/2, which is
// itself one complex FFT of length N/4.
//
// The transforms are
//
//	y[n] = (2/N) Σ_{k<N/2} X[k] cos((2π/N)(n+n0)(k+1/2)),  n0 = (N/2+1)/2
//	X[k] = 2 Σ_{n<N}     x[n] cos((2π/N)(n+n0)(k+1/2))
//
// and the reduction is the standard one. Writing M = N/2 and
// θ(a,b) = (π/M)(a+1/2)(b+1/2), the kernel is symmetric about a = -1/2 and
// antisymmetric about a = M-1/2, which is what both directions exploit:
//
//   - INVERSE. Substituting n = M/2 + h leaves θ(h,k) plus π(k+1/2), turning
//     every cosine into ±sine; the result is a DST-IV of (-1)^k X[k], which is
//     the DCT-IV of X reversed. So the middle half is y[M/2+h] = -(2/N)·C[M-1-h]
//     with C = DCT-IV(X), and the outer quarters are never computed at all --
//     the same two symmetries give y[M/2-1-q] = -y[M/2+q] and
//     y[3M/2+r] = y[3M/2-1-r].
//
//   - FORWARD. The same symmetries fold the N input samples onto M, which is
//     the time-domain aliasing the window pairs cancel, and what is left is
//     X[k] = 2·DCT-IV(u)[k]. See fold.
//
// The DCT-IV of length M is one M/2-point complex FFT between two rotations by
// exp(-2πi(j+1/8)/N), pairing in[2j] with in[M-1-2j]: the two phases differ by
// π(2k+1/2), so one complex output carries C[2k] in its real part and
// C[M-1-2k] in its imaginary part.
//
// What this replaced was a full N-point complex FFT in each direction -- and
// the inverse fed it a spectrum zero-padded to N, so half of that transform
// was multiplying zeroes. imdct_test.go and mdct_encode_test.go score both
// directions against the O(N²) sums, which is what makes this checkable
// independently of the transform it replaced. codec/wma/mdct.go and
// codec/vorbis/mdct.go carry the same factorization.

// imdctPlan holds the rotation and FFT twiddles for one transform size. It is
// read-only after construction, so the package-level plans are shared safely
// across concurrent sessions; the FFT scratch lives on the stack of each call.
type imdctPlan struct {
	n  int // the time-domain length
	m  int // N/4, the FFT length
	n2 int // N/2, the DCT-IV length and the coefficient count
	// rotRe/rotIm is exp(-2πi(j + 1/8)/N), the rotation the DCT-IV applies
	// before and after its FFT. One table serves both passes: they index it by
	// the FFT's input and output position, which run over the same N/4 values.
	rotRe, rotIm []float64
	twRe, twIm   []float64 // forward-FFT twiddles, one per (stage, k)
	invScale     float64   // 2/N, the inverse transform's output scale
}

var (
	planLong  = newIMDCTPlan(2048)
	planShort = newIMDCTPlan(256)
)

func newIMDCTPlan(n int) *imdctPlan {
	p := &imdctPlan{n: n, m: n / 4, n2: n / 2, invScale: 2.0 / float64(n)}
	p.rotRe = make([]float64, p.m)
	p.rotIm = make([]float64, p.m)
	for j := 0; j < p.m; j++ {
		a := 2 * math.Pi * (float64(j) + 0.125) / float64(n)
		p.rotRe[j], p.rotIm[j] = math.Cos(a), -math.Sin(a)
	}
	// Forward-FFT twiddles, exp(-i·2π·k/length) per stage.
	for length := 2; length <= p.m; length <<= 1 {
		for k := 0; k < length/2; k++ {
			a := 2 * math.Pi * float64(k) / float64(length)
			p.twRe = append(p.twRe, math.Cos(a))
			p.twIm = append(p.twIm, -math.Sin(a))
		}
	}
	return p
}

// dctIV computes C[k] = Σ_{j<M} in[j]·cos((π/M)(k+1/2)(j+1/2)) for k < M,
// M = N/2, through one M/2-point complex FFT. cr and ci are caller scratch of
// length M/2.
func (p *imdctPlan) dctIV(in, out, cr, ci []float64) {
	m, n2 := p.m, p.n2
	for j := 0; j < m; j++ {
		x1, x2 := in[2*j], in[n2-1-2*j]
		c, s := p.rotRe[j], p.rotIm[j]
		cr[j] = x1*c - x2*s
		ci[j] = x2*c + x1*s
	}
	p.fft(cr, ci)
	for k := 0; k < m; k++ {
		re, im := cr[k], ci[k]
		c, s := p.rotRe[k], p.rotIm[k]
		out[2*k] = re*c - im*s
		out[n2-1-2*k] = -(im*c + re*s)
	}
}

// imdct computes the plan's inverse MDCT of spec (length n/2) into out
// (length n). The scratch is stack-sized per transform length: the plans are
// shared package singletons, so plan-owned scratch would race across
// concurrent sessions, and one 2048-sized buffer would zero 24 KiB for each of
// the eight 256-point short transforms a transient frame runs per channel.
func (p *imdctPlan) imdct(spec, out []float64) {
	if p.n <= 256 {
		var cBuf, reBuf, imBuf [128]float64
		p.runInverse(spec, out, cBuf[:p.n2], reBuf[:p.m], imBuf[:p.m])
		return
	}
	var cBuf, reBuf, imBuf [1024]float64
	p.runInverse(spec, out, cBuf[:p.n2], reBuf[:p.m], imBuf[:p.m])
}

func (p *imdctPlan) runInverse(spec, out, c, cr, ci []float64) {
	n2, half := p.n2, p.n/4
	p.dctIV(spec, c, cr, ci)
	// The middle half, reversed and scaled; then the outer quarters from the
	// transform's own symmetries, which read only from the middle.
	for h := 0; h < n2; h++ {
		out[half+h] = -p.invScale * c[n2-1-h]
	}
	for q := 0; q < half; q++ {
		out[half-1-q] = -out[half+q]
	}
	for r := 0; r < half; r++ {
		out[3*half+r] = out[3*half-1-r]
	}
}

// fold reduces the N windowed time samples in x onto the M = N/2 values whose
// DCT-IV is the forward MDCT. It is the time-domain aliasing the Princen-
// Bradley window pairs cancel on overlap-add, written out: the kernel is
// symmetric about index -1/2 and antisymmetric about M-1/2, so every input
// position outside [0, M) lands back inside it with a sign.
func (p *imdctPlan) fold(x, u []float64) {
	n2, half := p.n2, p.n/4
	for j := 0; j < half; j++ {
		u[j] = -x[3*half-1-j] - x[j+3*half]
	}
	for j := half; j < n2; j++ {
		u[j] = x[j-half] - x[3*half-1-j]
	}
}

// fft computes the in-place, unnormalized FORWARD FFT (exp(-i) kernel) of
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
