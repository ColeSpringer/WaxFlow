package opus

import (
	"math"

	"github.com/colespringer/waxflow/dsp/fft"
)

// The CELT inverse MDCT. This is a clean-room port of libopus mdct.c
// (clt_mdct_backward) and the CELT window from modes.c,
// implementing the transform RFC 6716 section 4.3.7 specifies. CELT factors the
// inverse MDCT into a pre-rotation, an N/4-point complex DFT, a post-rotation,
// and a windowed time-domain aliasing (TDAC) fold.
//
// libopus runs the DFT with a mixed-radix float32 FFT (kiss_fft); so do we,
// through dsp/fft. The rotation and windowing passes stay in float64 (they
// are O(n) against the transform's O(n log n), and keeping their arithmetic
// unchanged confines this revision's numeric delta to the DFT itself), so
// the transform's phase and normalization match the reference exactly and
// its precision matches the reference float build's.

// mdctPlan holds the read-only rotation table and DFT plan for one MDCT
// length. Plans are keyed by the full time-domain length n (n/2 frequency
// bins, n/4-point DFT) and shared across sessions.
type mdctPlan struct {
	n  int       // full MDCT length in time samples
	n2 int       // n/2, the number of frequency bins
	n4 int       // n/4, the DFT length
	tr []float64 // trig[j] = cos(2π(j+0.125)/n), length n/2
	fp *fft.Plan // the n/4-point complex DFT
}

// mdctPlanCache lazily builds and reuses MDCT plans per transform size; the
// encoder and decoder each embed one.
type mdctPlanCache struct {
	mplans map[int]*mdctPlan
}

func (c *mdctPlanCache) planFor(n int) *mdctPlan {
	if p, ok := c.mplans[n]; ok {
		return p
	}
	if c.mplans == nil {
		c.mplans = map[int]*mdctPlan{}
	}
	p := newMDCTPlan(n)
	c.mplans[n] = p
	return p
}

func newMDCTPlan(n int) *mdctPlan {
	p := &mdctPlan{n: n, n2: n / 2, n4: n / 4}
	p.tr = make([]float64, p.n2)
	for j := range p.tr {
		p.tr[j] = math.Cos(2 * math.Pi * (float64(j) + 0.125) / float64(n))
	}
	p.fp = fft.NewPlan(p.n4)
	return p
}

// celtWindow computes CELT's overlap window (libopus modes.c): the rising half
// w[i] = sin(π/2 · sin²(π/2 · (i+0.5)/overlap)), i in [0, overlap). The window
// is symmetric; the falling half is w[overlap-1-i].
func celtWindow(overlap int) []float64 {
	w := make([]float64, overlap)
	for i := range w {
		s := math.Sin(0.5 * math.Pi * (float64(i) + 0.5) / float64(overlap))
		w[i] = math.Sin(0.5 * math.Pi * s * s)
	}
	return w
}

// mdctScratch is caller-owned working memory for backward and forward, sized to
// the largest DFT length (n/4) either direction will use, so the hot path never
// allocates. fr/fi hold the pre-rotated input; gr/gi hold the DFT output.
type mdctScratch struct {
	fr, fi, gr, gi []float32
}

func newMDCTScratch(maxN4 int) *mdctScratch {
	return &mdctScratch{
		fr: make([]float32, maxN4),
		fi: make([]float32, maxN4),
		gr: make([]float32, maxN4),
		gi: make([]float32, maxN4),
	}
}

// backward computes the inverse MDCT of the n2 frequency coefficients read from
// in at the given stride (CELT interleaves B short-block spectra, so a block's
// coefficients are in[0], in[stride], in[2·stride], …). It writes the windowed
// time-domain block into out, overlap-adding into out's leading `overlap`
// samples exactly as libopus does in place: out[0:overlap] must hold the prior
// block's tail on entry, and out must have room through overlap/2 + n2.
func (p *mdctPlan) backward(in []float32, stride int, out []float32, window []float64, overlap int, s *mdctScratch) {
	n2, n4 := p.n2, p.n4
	tr := p.tr
	fr, fi := s.fr[:n4], s.fi[:n4]
	gr, gi := s.gr[:n4], s.gi[:n4]

	// Pre-rotation: fold the coefficients from both ends into n4 complex
	// samples. Real and imaginary parts are swapped throughout because the DFT
	// runs forward where the transform wants an inverse (libopus does the same).
	for i := 0; i < n4; i++ {
		x1 := float64(in[(2*i)*stride])
		x2 := float64(in[(n2-1-2*i)*stride])
		yr := x2*tr[i] + x1*tr[n4+i]
		yi := x1*tr[i] - x2*tr[n4+i]
		fr[i] = float32(yi)
		fi[i] = float32(yr)
	}

	// N/4-point forward DFT: G[k] = Σ f[j]·e^(-2πi kj/n4).
	p.fp.Transform(gr, gi, fr, fi)

	// Post-rotation and de-shuffle into out[overlap/2 : overlap/2 + n2].
	mid := overlap / 2
	half := (n4 + 1) >> 1
	for i := 0; i < half; i++ {
		// low end
		re := float64(gi[i])
		im := float64(gr[i])
		t0 := tr[i]
		t1 := tr[n4+i]
		yr0 := re*t0 + im*t1
		yi0 := re*t1 - im*t0
		// high end
		re2 := float64(gi[n4-1-i])
		im2 := float64(gr[n4-1-i])
		t0b := tr[n4-1-i]
		t1b := tr[n2-1-i]
		yr1 := re2*t0b + im2*t1b
		yi1 := re2*t1b - im2*t0b
		out[mid+2*i] = float32(yr0)
		out[mid+n2-1-2*i] = float32(yi0)
		out[mid+n2-2-2*i] = float32(yr1)
		out[mid+2*i+1] = float32(yi1)
	}

	// TDAC: window and mirror the leading `overlap` samples, folding in the
	// prior block's tail already present in out[0:overlap].
	for i := 0; i < overlap/2; i++ {
		x1 := float64(out[overlap-1-i])
		x2 := float64(out[i])
		w1 := window[i]
		w2 := window[overlap-1-i]
		out[i] = float32(x2*w2 - x1*w1)
		out[overlap-1-i] = float32(x2*w1 + x1*w2)
	}
}

// forward computes the forward MDCT of the block at `in` (n2 coded samples plus
// the `overlap` window tail: in must hold n2+overlap samples), writing the n2
// frequency coefficients into out at the given stride. It is a clean-room port
// of libopus clt_mdct_forward (float build, FFT scale 1/n4) and the exact
// analysis companion of backward: decode(backward) inverts encode(forward), so
// the quantized band energies match the reference. The pre-rotation and DFT
// reuse the same trig table and forward-DFT direction backward uses.
func (p *mdctPlan) forward(in []float32, out []float32, stride int, window []float64, overlap int, s *mdctScratch) {
	n2, n4 := p.n2, p.n4
	tr := p.tr
	fr, fi := s.fr[:n4], s.fi[:n4]
	gr, gi := s.gr[:n4], s.gi[:n4]
	scale := 1.0 / float64(n4)
	o2 := overlap / 2
	half1 := (overlap + 3) >> 2

	// Window, shuffle, fold the n2+overlap input into n4 complex samples, fused
	// with the pre-rotation (libopus folds then pre-rotates in two passes; the
	// arithmetic is identical done together).
	for i := 0; i < n4; i++ {
		xp1 := o2 + 2*i
		xp2 := n2 - 1 + o2 - 2*i
		var re, im float64
		switch {
		case i < half1:
			wp1 := o2 + 2*i
			wp2 := o2 - 1 - 2*i
			re = float64(in[xp1+n2])*window[wp2] + float64(in[xp2])*window[wp1]
			im = float64(in[xp1])*window[wp1] - float64(in[xp2-n2])*window[wp2]
		case i < n4-half1:
			re = float64(in[xp2])
			im = float64(in[xp1])
		default:
			k := i - (n4 - half1)
			wp1 := 2 * k
			wp2 := overlap - 1 - 2*k
			re = -float64(in[xp1-n2])*window[wp1] + float64(in[xp2])*window[wp2]
			im = float64(in[xp1])*window[wp2] + float64(in[xp2+n2])*window[wp1]
		}
		t0 := tr[i]
		t1 := tr[n4+i]
		fr[i] = float32((re*t0 - im*t1) * scale)
		fi[i] = float32((im*t0 + re*t1) * scale)
	}

	// N/4-point forward DFT (same direction as backward).
	p.fp.Transform(gr, gi, fr, fi)

	// Post-rotation, de-shuffled into out at the block stride.
	for i := 0; i < n4; i++ {
		t0 := tr[i]
		t1 := tr[n4+i]
		yr := float64(gi[i])*t1 - float64(gr[i])*t0
		yi := float64(gr[i])*t1 + float64(gi[i])*t0
		out[stride*(2*i)] = float32(yr)
		out[stride*(n2-1-2*i)] = float32(yi)
	}
}
