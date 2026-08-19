package aac

import "math"

// Encoder-side 64-band QMF analysis at the SBR (output) rate (ISO/IEC
// 14496-3 4.6.18.4 at M = 64): the encoder measures envelope energies,
// noise floors, and tonality in the same subband domain the decoder's
// adjuster works in. The bank is the exact reconstruction partner of
// qmfSynthesizer64 (sbr_encqmf_test pins the pair's SNR and delay), which
// is what puts the measured energies on the decoder's scale: a subband
// sample of magnitude g here synthesizes to a time signal of magnitude g
// there. Same restarting per-block modulation as the decoder banks, at the
// spec's 64-band phase (2n-1, where the decimated 32-band form has 2n-1/2):
//
//	W(k) = sum_n u(n) * e^(+j*pi/128*(k+1/2)*(2n-1)),  n in [0,128)
//
// split as e^(-j*pi*(2k+1)/256) * conj(FFT128(u(n)*e^(-j*pi*n/128))).
var (
	qmfAnaPre64  [128][2]float32 // e^(-j*pi*n/128), conjugated pre-twiddle
	qmfAnaPost64 [64][2]float32  // e^(-j*pi*(2k+1)/256), applied to conj(FFT)
)

func init() {
	for n := range 128 {
		s, c := math.Sincos(math.Pi * float64(n) / 128)
		qmfAnaPre64[n] = [2]float32{float32(c), float32(-s)}
	}
	for k := range 64 {
		s, c := math.Sincos(-math.Pi * float64(2*k+1) / 256)
		qmfAnaPost64[k] = [2]float32{float32(c), float32(s)}
	}
}

// The analysis window is the plain full prototype c(n), which is exactly
// the synthesis fold's qmfWin64 (unlike the decimated 32-band bank there
// is no doubling; the reconstruction test proves a doubled window comes
// back at twice the input), so the table is shared rather than restated.

// qmfEncPairDelay is the analysis64+synthesis64 chain delay in samples,
// pinned by the reconstruction test. The v2 front end discards exactly
// this many synthesized samples so its downmix realigns with the input
// timeline and v1's delay contract carries over unchanged.
const qmfEncPairDelay = 577

// qmfAnalyzer64 is the encoder's 64-band analysis bank. The delay line
// keeps x(0) as the newest sample.
type qmfAnalyzer64 struct {
	x [640]float32
}

// analyze consumes 64 time samples and produces the slot's 64 complex
// subband samples.
func (a *qmfAnalyzer64) analyze(in []float32, outRe, outIm []float32) {
	copy(a.x[64:], a.x[:576])
	for n := range 64 {
		a.x[n] = in[63-n]
	}
	var u [128]float32
	for n := range u {
		u[n] = a.x[n]*qmfWin64[n] + a.x[n+128]*qmfWin64[n+128] +
			a.x[n+256]*qmfWin64[n+256] + a.x[n+384]*qmfWin64[n+384] +
			a.x[n+512]*qmfWin64[n+512]
	}
	var sr, si, fr, fi [128]float32
	for n := range u {
		sr[n] = u[n] * qmfAnaPre64[n][0]
		si[n] = u[n] * qmfAnaPre64[n][1]
	}
	qmfPlan128.Transform(fr[:], fi[:], sr[:], si[:])
	for k := range 64 {
		c, d := qmfAnaPost64[k][0], qmfAnaPost64[k][1]
		outRe[k] = fr[k]*c + fi[k]*d
		outIm[k] = fr[k]*d - fi[k]*c
	}
}
