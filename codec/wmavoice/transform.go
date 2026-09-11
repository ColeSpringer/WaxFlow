//go:build !wmavoicetablesgen

package wmavoice

import (
	"math"
	"sync"

	"github.com/colespringer/waxflow/dsp/fft"
)

// The four transforms the denoise filter runs through, to the definitions of
// note 10.5. These are not "a DFT of the right size": each one's normalisation
// and sign is what decides whether the postfilter agrees with the reference at
// all, so each is stated as a closed form there and checked against it here by
// TestTransformsMatchTheirClosedForms.
//
// The two 128-point transforms ride on dsp/fft; the two 64-point ones are
// odd-kernel (63 and 65 point) sums that no power-of-two plan covers, so they
// are matrix products against tables built once. They accumulate in float64
// and narrow, which puts them nearer the exact value than the reference's own
// float32 FFT path rather than further from it.

const (
	// dftLen is the transform length the denoise filter works at.
	dftLen = 128
	// dftBins is the packed half-spectrum: 65 complex values interleaved into
	// 130 floats at (2k, 2k+1).
	dftBins  = dftLen/2 + 1
	dftCoefs = 2 * dftBins
	// trigLen is the 64-point DCT-I and DST-I length.
	trigLen = 64
	// phaseLen is the length of the phase table step 5 indexes.
	phaseLen = 511
	// phaseMid is the index that stands for zero phase.
	phaseMid = 255
)

// fft128 is the shared plan. A Plan is immutable and safe for concurrent use.
// Each decoder takes a pointer to it, and to the four tables below, at
// construction, so the hot path never touches a once.
var fft128 = sync.OnceValue(func() *fft.Plan { return fft.NewPlan(dftLen) })

// transforms holds one decoder's transform scratch and its pointers to the
// shared tables. Nothing here is state: it is buffers, held so the postfilter
// never allocates.
type transforms struct {
	plan         *fft.Plan
	dct, dst     *[trigLen][trigLen]float64
	trig         *[trigLen + 1][2]float64
	phase        *[phaseLen][2]float32
	re, im       [dftLen]float32
	outRe, outIm [dftLen]float32
	ext          [2*trigLen + 2]float64 // the DST-I input's antisymmetric extension
}

func newTransforms() *transforms {
	return &transforms{plan: fft128(), dct: dctMatrix(), dst: dstMatrix(), trig: h64Trig(), phase: phaseTable()}
}

// forwardDFT computes the unnormalised forward real DFT of 128 samples into a
// packed half-spectrum: X[k] = sum_t x[t] exp(-2*pi*i*k*t/128), k = 0..64.
//
// X[0] and X[64] are real for a real input, and they are forced so rather than
// left at the transform's own rounding, because step 5 relies on the Nyquist
// bin's imaginary part being exactly zero.
func (t *transforms) forwardDFT(dst *[dftCoefs]float32, src *[dftLen]float32) {
	copy(t.re[:], src[:])
	clear(t.im[:])
	t.plan.Transform(t.outRe[:], t.outIm[:], t.re[:], t.im[:])
	for k := range dftBins {
		dst[2*k] = t.outRe[k]
		dst[2*k+1] = t.outIm[k]
	}
	dst[1] = 0
	dst[dftCoefs-1] = 0
}

// inverseDFT computes the unnormalised inverse real DFT, so a round trip
// through the pair multiplies by 128:
//
//	y[t] = X[0].re + (-1)^t X[64].re
//	       + 2 * sum over k in 1..63 of (X[k].re cos(2 pi k t/128) - X[k].im sin(...))
//
// It runs on the forward plan: for a Hermitian spectrum the inverse is the
// forward transform of the conjugate, whose real part is the answer.
func (t *transforms) inverseDFT(dst *[dftLen]float32, src *[dftCoefs]float32) {
	for k := range dftBins {
		t.re[k] = src[2*k]
		t.im[k] = -src[2*k+1]
	}
	for k := dftBins; k < dftLen; k++ {
		t.re[k] = src[2*(dftLen-k)]
		t.im[k] = src[2*(dftLen-k)+1]
	}
	t.plan.Transform(t.outRe[:], t.outIm[:], t.re[:], t.im[:])
	copy(dst[:], t.outRe[:])
}

// dctI is the 64-point DCT-I at scale 1/64:
//
//	y[k] = (1/64) * (x[0] + (-1)^k x[63] + 2 * sum over t in 1..62 of x[t] cos(pi t k/63))
func (t *transforms) dctI(dst, src *[trigLen]float32) {
	m := t.dct
	for k := range trigLen {
		row := &m[k]
		var acc float64
		for t := range trigLen {
			acc += row[t] * float64(src[t])
		}
		dst[k] = float32(acc)
	}
}

// dstI is the 64-point DST-I at scale 1/64:
//
//	y[k] = (2/64) * sum over t in 0..63 of x[t] sin(pi (t+1)(k+1)/65)
func (t *transforms) dstI(dst, src *[trigLen]float32) {
	m := t.dst
	for k := range trigLen {
		row := &m[k]
		var acc float64
		for t := range trigLen {
			acc += row[t] * float64(src[t])
		}
		dst[k] = float32(acc)
	}
}

// phaseRef65 is the value note 10.5 calls h64: what the reference finds at
// index 64 of a 64-point DST-I's output.
//
// It is NOT the 65th phase surviving the transform, which is the reading with
// a defensible meaning. It is the imaginary part of bin 64 of the unscaled
// 65-point complex sub-FFT the reference's transform library builds its
// 130-point real DFT on, written past the 64 outputs the DST-I declares. This
// decoder reproduces it deliberately: the note measures the choice at max
// 6.5e-06 against the reference for this reading and max 6.4e-03 for the
// natural one, and a gate three orders of magnitude looser than the oracle's
// own floor hides real bugs. Both are audibly identical, at about -65 dB.
//
// See docs/notes/wma-voice-bitstream.md section 10.5, which states the closed
// form below and measures both readings.
func (t *transforms) phaseRef65(src *[trigLen]float32) float32 {
	// The antisymmetric 130-point extension of the DST-I's input.
	e := &t.ext
	clear(e[:])
	for i := 1; i <= trigLen; i++ {
		e[i] = -float64(src[i-1])
		e[2*trigLen+2-i] = float64(src[i-1])
	}
	sc := t.trig
	var acc float64
	for n := range trigLen + 1 {
		acc += e[2*n]*sc[n][0] + e[2*n+1]*sc[n][1]
	}
	return float32(acc)
}

// The tables the three sums above run against, built once.

var dctMatrix = sync.OnceValue(func() *[trigLen][trigLen]float64 {
	var m [trigLen][trigLen]float64
	for k := range trigLen {
		m[k][0] = 1.0 / trigLen
		m[k][trigLen-1] = 1.0 / trigLen
		if k&1 != 0 {
			m[k][trigLen-1] = -1.0 / trigLen
		}
		for t := 1; t < trigLen-1; t++ {
			m[k][t] = 2 * math.Cos(math.Pi*float64(t)*float64(k)/(trigLen-1)) / trigLen
		}
	}
	return &m
})

var dstMatrix = sync.OnceValue(func() *[trigLen][trigLen]float64 {
	var m [trigLen][trigLen]float64
	for k := range trigLen {
		for t := range trigLen {
			m[k][t] = 2 * math.Sin(math.Pi*float64(t+1)*float64(k+1)/(trigLen+1)) / trigLen
		}
	}
	return &m
})

// h64Trig holds sin and cos of 2*pi*n/65 for the sum phaseRef65 evaluates.
var h64Trig = sync.OnceValue(func() *[trigLen + 1][2]float64 {
	var t [trigLen + 1][2]float64
	for n := range trigLen + 1 {
		a := 2 * math.Pi * float64(n) / (trigLen + 1)
		t[n][0], t[n][1] = math.Sin(a), math.Cos(a)
	}
	return &t
})

// phaseTable is the cos/sin pair step 5 indexes, traversing a phase in
// (-pi/2, pi/2) with a half-sample offset, so index phaseMid is very nearly
// zero phase rather than exactly it.
var phaseTable = sync.OnceValue(func() *[phaseLen][2]float32 {
	var t [phaseLen][2]float32
	for idx := range phaseLen {
		m := idx - phaseMid
		a := (math.Abs(float64(m)) + 0.5) * math.Pi / 512
		t[idx][0] = float32(math.Cos(a))
		s := float32(math.Sin(a))
		if m < 0 {
			s = -s
		}
		t[idx][1] = s
	}
	return &t
})

// phaseIndex maps a phase, truncated to an integer, onto the table. A NaN is
// pinned to zero phase first: int(NaN) is the most negative value on amd64 and
// zero on arm64, which would put the same packet at opposite ends of the table
// on the two. Nothing in reach produces one; the pin is for determinism.
func phaseIndex(v float32) int {
	if math.IsNaN(float64(v)) {
		return phaseMid
	}
	return phaseMid + clip(int(v), -phaseMid, phaseMid)
}
