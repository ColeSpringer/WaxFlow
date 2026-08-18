package aac

import (
	"math"

	"github.com/colespringer/waxflow/dsp/fft"
)

// The SBR QMF pair (ISO/IEC 14496-3 4.6.18.4, 4.6.18.8): 32-band analysis
// of the core frame, 64-band synthesis at the doubled rate. Both use the
// spec's exact forms, verified tap-for-tap against ffmpeg: a plain
// polyphase fold and a per-block restarting phase
//
//	analysis:  W(k) = sum_n u(n) * e^(+j*pi/64*(k+1/2)*(2n-1/2))
//	synthesis: v(n) = (1/64) * Re{sum_k X(k) * e^(j*pi/128*(k+1/2)*(2n-255))}
//
// The restart, not a continuous modulation, is load-bearing: it places
// each band's alias images where the matched synthesis cancels them when
// bands are hard-replaced at the crossover. Twiddles are designed in
// float64 and rounded once, and the kernels avoid FMA, so output is a pure
// function of input on every platform. The encoder-side 64-band analysis
// (sbr_encqmf.go) shares the prototype and its post-twiddle covers this
// bank's: e^(-j*pi*(2k+1)/256) at 32 bands is that table's lower half.

var (
	qmfPlan64  = fft.NewPlan(64)
	qmfPlan128 = fft.NewPlan(128)

	qmfAnaPre32  [64][2]float32  // e^(-j*pi*n/64), conjugated pre-twiddle
	qmfSynPre64  [64][2]float32  // e^(+j*255*pi*k/128), applied to conj(X)
	qmfSynPost64 [128][2]float32 // (1/64)*e^(j*(pi*n/128 - 255*pi/256))
)

func init() {
	for n := range 64 {
		s, c := math.Sincos(math.Pi * float64(n) / 64)
		qmfAnaPre32[n] = [2]float32{float32(c), float32(-s)}
	}
	for k := range 64 {
		s, c := math.Sincos(255 * math.Pi * float64(k) / 128)
		qmfSynPre64[k] = [2]float32{float32(c), float32(s)}
	}
	for n := range 128 {
		s, c := math.Sincos(math.Pi*float64(n)/128 - 255*math.Pi/256)
		qmfSynPost64[n] = [2]float32{float32(c / 64), float32(s / 64)}
	}
}

// qmfWin32 is the doubled decimated prototype 2*c(2n) the 32-band analysis
// uses; the doubling makes the upsampling chain's low band unit-gain, so
// the whole QMF domain sits at 1/32768 of the spec's integer-scale one.
var qmfWin32 = func() (w [320]float32) {
	for n := range w {
		w[n] = float32(2 * qmfWindow[2*n])
	}
	return
}()

// qmfWin64 is the full 640-tap prototype in float32, for the synthesis
// fold.
var qmfWin64 = func() (w [640]float32) {
	for n := range w {
		w[n] = float32(qmfWindow[n])
	}
	return
}()

// qmfAnalyzer32 is the decoder-side 32-band analysis bank. The delay line
// keeps x(0) as the newest sample.
type qmfAnalyzer32 struct {
	x [320]float32
}

func (a *qmfAnalyzer32) reset() { a.x = [320]float32{} }

// analyze consumes 32 time samples and produces the slot's 32 complex
// subband samples W(k) = sum_n u(n)*e^(j*pi/64*(k+1/2)*(2n-1/2)), where u
// is the plain polyphase fold of the windowed delay line.
func (a *qmfAnalyzer32) analyze(in []float32, outRe, outIm []float32) {
	copy(a.x[32:], a.x[:288])
	for n := range 32 {
		a.x[n] = in[31-n]
	}
	var u [64]float32
	for n := range u {
		u[n] = a.x[n]*qmfWin32[n] + a.x[n+64]*qmfWin32[n+64] +
			a.x[n+128]*qmfWin32[n+128] + a.x[n+192]*qmfWin32[n+192] +
			a.x[n+256]*qmfWin32[n+256]
	}
	// The modulation splits as e^(-j*pi*(2k+1)/256) * conj(FFT64(u(n)*e^(-j*pi*n/64))).
	var sr, si, fr, fi [64]float32
	for n := range u {
		sr[n] = u[n] * qmfAnaPre32[n][0]
		si[n] = u[n] * qmfAnaPre32[n][1]
	}
	qmfPlan64.Transform(fr[:], fi[:], sr[:], si[:])
	for k := range 32 {
		c, d := qmfAnaPost64[k][0], qmfAnaPost64[k][1]
		outRe[k] = fr[k]*c + fi[k]*d
		outIm[k] = fr[k]*d - fi[k]*c
	}
}

// qmfSynthesizer64 is the 64-band synthesis bank: each slot turns 64
// complex subband samples into 64 time samples through the 1280-sample
// overlap FIFO.
type qmfSynthesizer64 struct {
	v [1280]float32
}

func (s *qmfSynthesizer64) reset() { s.v = [1280]float32{} }

// synthesize consumes one slot's 64 complex subband samples and writes 64
// time samples: v(n) = (1/64)*Re{sum_k X(k)*e^(j*pi/128*(k+1/2)*(2n-255))},
// then the plain windowed fold over the v history.
func (s *qmfSynthesizer64) synthesize(inRe, inIm []float32, out []float32) {
	var sr, si, fr, fi [128]float32
	// Pre-twiddle b(k) = X(k)*e^(-j*255*pi*k/128), zero-padded to 128; the
	// IDFT rides the forward plan via conjugation, so the source is
	// conj(b) = conj(X(k))*e^(+j*255*pi*k/128).
	for k := range 64 {
		c, d := qmfSynPre64[k][0], qmfSynPre64[k][1]
		sr[k] = inRe[k]*c + inIm[k]*d
		si[k] = inRe[k]*d - inIm[k]*c
	}
	qmfPlan128.Transform(fr[:], fi[:], sr[:], si[:])
	copy(s.v[128:], s.v[:1152])
	for n := range 128 {
		c, d := qmfSynPost64[n][0], qmfSynPost64[n][1]
		s.v[n] = fr[n]*c + fi[n]*d
	}
	for j := range 64 {
		acc := s.v[j]*qmfWin64[j] + s.v[192+j]*qmfWin64[64+j]
		acc += s.v[256+j]*qmfWin64[128+j] + s.v[448+j]*qmfWin64[192+j]
		acc += s.v[512+j]*qmfWin64[256+j] + s.v[704+j]*qmfWin64[320+j]
		acc += s.v[768+j]*qmfWin64[384+j] + s.v[960+j]*qmfWin64[448+j]
		acc += s.v[1024+j]*qmfWin64[512+j] + s.v[1216+j]*qmfWin64[576+j]
		out[j] = acc
	}
}

// synthesizeDown is the downsampled (single-rate) synthesis: one slot's
// lower 32 subbands become 32 time samples at the core rate. The spec's
// decimated bank is algebraically this bank's output taken at every
// second sample, because the downsampled prototype is the full window's
// even polyphase and every fold offset is even; running the full bank
// and decimating trades a small constant for zero extra tables and
// phase constants. Bands 32 and up are outside the decimated Nyquist and
// are cleared (in the caller's scratch) so decimation cannot alias them.
func (s *qmfSynthesizer64) synthesizeDown(inRe, inIm []float32, out []float32) {
	clear(inRe[32:64])
	clear(inIm[32:64])
	var tmp [64]float32
	s.synthesize(inRe, inIm, tmp[:])
	for j := range 32 {
		out[j] = tmp[2*j]
	}
}
