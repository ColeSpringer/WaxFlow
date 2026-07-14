package mp3

// Encoder analysis stage: the exact inverse of the decoder's reconstruction
// (synth.go, imdct.go, granule.go) run forward. PCM enters, the polyphase
// analysis filterbank produces 32 subband signals, a per-subband forward
// MDCT turns each granule's 18 subband samples into 18 spectral lines, and
// an inverse alias butterfly pre-distorts the spectrum so the decoder's
// alias reduction recovers the MDCT output. The result is the dequantized
// spectrum xr the quantizer then rounds to integers.
//
// The forward MDCT reuses the decoder's cosine kernel (cosN36f) and hybrid
// window (imdctWinF), so the transform pair is matched by construction: any
// scale or sign is shared with imdct.go and cancels on round-trip. Only the
// polyphase analysis window is new; it is the ISO analysis window, which is
// the synthesis window (synthD) scaled by 1/32 (ISO 11172-3, D[i] = 32 C[i]).
//
// Baseline scope: long blocks only. Block switching (short windows, the
// transient path) is later work; this encoder never emits it, so the
// short-window kernels stay decode-only.

import "math"

// enwindow is the 512-tap polyphase analysis window, and anaCos the
// analysis matrixing cos((2sb+1)(k-16)pi/64). Filled at init.
var (
	enwindow [512]float64
	anaCos   [32][64]float64
)

// mdctScale is the forward-MDCT normalization that inverts the decoder's
// unnormalized IMDCT (imdctSubband sums cosN36f without a normalization
// factor): with 18 spectral lines the analysis carries the full 2/K = 1/9,
// which drives the round trip to machine precision.
const mdctScale = 1.0 / 9.0

func init() {
	for i := range enwindow {
		enwindow[i] = float64(synthD[i]) / 32
	}
	for sb := 0; sb < 32; sb++ {
		for k := 0; k < 64; k++ {
			anaCos[sb][k] = math.Cos(float64((2*sb+1)*(k-16)) * math.Pi / 64)
		}
	}
}

// analyzer holds one channel's polyphase filterbank history and the MDCT
// overlap (the previous granule's 18 subband samples per subband).
type analyzer struct {
	fifo    [512]float64 // sliding PCM window, fifo[0] newest
	overlap [32][18]float64
}

// reset clears the filterbank and MDCT history (a fresh stream or a splice).
func (a *analyzer) reset() {
	a.fifo = [512]float64{}
	a.overlap = [32][18]float64{}
}

// subband pushes 32 new PCM samples (time order) through the polyphase
// filterbank and writes 32 subband samples to out.
func (a *analyzer) subband(in []float32, out *[32]float64) {
	f := &a.fifo
	copy(f[32:], f[:512-32])
	for i := 0; i < 32; i++ {
		f[i] = float64(in[31-i])
	}
	var z [64]float64
	for i := 0; i < 64; i++ {
		s := 0.0
		for j := 0; j < 8; j++ {
			s += f[i+64*j] * enwindow[i+64*j]
		}
		z[i] = s
	}
	for sb := 0; sb < 32; sb++ {
		m := &anaCos[sb]
		s := 0.0
		for k := 0; k < 64; k++ {
			s += z[k] * m[k]
		}
		out[sb] = s
	}
}

// granuleMDCT runs the analysis for one granule: 18 filterbank slots (576
// PCM samples) followed by a per-subband forward MDCT with 50% overlap
// against the previous granule, writing 576 spectral lines to xr in the
// decoder's subband-major layout (xr[sb*18+m]). The frequency inversion the
// decoder applies before synthesis is undone here so the filterbank sees the
// right spectral orientation.
func (a *analyzer) granuleMDCT(pcm []float32, xr *[576]float32) {
	var sb [32][18]float64
	var s [32]float64
	for ss := 0; ss < 18; ss++ {
		a.subband(pcm[32*ss:32*ss+32], &s)
		for j := 0; j < 32; j++ {
			sb[j][ss] = s[j]
		}
	}

	// Undo the decoder's frequency inversion, which negates odd time
	// samples of odd subbands between the IMDCT and the synthesis
	// filterbank (imdct.go hybrid). It is a time-domain sign flip on the
	// subband samples, so it happens before the forward MDCT.
	for j := 1; j < 32; j += 2 {
		for p := 1; p < 18; p += 2 {
			sb[j][p] = -sb[j][p]
		}
	}

	win := &imdctWinF[blockNormal]
	for j := 0; j < 32; j++ {
		var z [36]float64
		for p := 0; p < 18; p++ {
			z[p] = a.overlap[j][p] * float64(win[p])
			z[p+18] = sb[j][p] * float64(win[p+18])
		}
		a.overlap[j] = sb[j]

		for m := 0; m < 18; m++ {
			acc := 0.0
			for p := 0; p < 36; p++ {
				acc += z[p] * float64(cosN36f[m][p])
			}
			xr[j*18+m] = float32(acc * mdctScale)
		}
	}

	forwardAlias(xr)
}

// forwardAlias applies the inverse of the decoder's alias-reduction
// butterflies (granule.go antialias) across all 31 long-block subband
// edges, so decoding re-aliases back to the MDCT spectrum.
func forwardAlias(xr *[576]float32) {
	for sb := 1; sb <= 31; sb++ {
		edge := 18 * sb
		for i := 0; i < 8; i++ {
			lo, hi := edge-1-i, edge+i
			a, b := float64(xr[lo]), float64(xr[hi])
			xr[lo] = float32(a*csTab[i] + b*caTab[i])
			xr[hi] = float32(b*csTab[i] - a*caTab[i])
		}
	}
}
