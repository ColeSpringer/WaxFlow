package aac

// Forward MDCT for the encoder, the exact inverse of imdct.go's transform:
//
//	X[k] = 2 Σ_{n=0}^{N-1} x[n] cos((2π/N)(n+n0)(k+1/2)), k < N/2, n0 = (N/2+1)/2
//
// The forward scale of 2 complements the decoder's (2/N)-scaled inverse:
// with the Princen-Bradley window pairs in tables.go the pair then
// reconstructs perfectly under overlap-add (mdct_encode_test.go pins the
// round trip; unit forward scale reconstructs at exactly half).
//
// It runs through the same N/4-point DCT-IV the inverse uses, over the N/2
// values imdctPlan.fold reduces the block to. Both directions therefore share
// one rotation table, one FFT twiddle table and one kernel; see imdct.go for
// the derivation.
type mdctPlan struct {
	inv *imdctPlan // the shared fold, DCT-IV and FFT
}

var (
	mdctLong  = newMDCTPlan(planLong)
	mdctShort = newMDCTPlan(planShort)
)

func newMDCTPlan(inv *imdctPlan) *mdctPlan { return &mdctPlan{inv: inv} }

// mdct computes spec[k] (length n/2) from the windowed block x (length n).
// The scratch is stack-sized per transform length, for the reason imdct gives.
func (p *mdctPlan) mdct(x, spec []float64) {
	inv := p.inv
	if inv.n <= 256 {
		var uBuf, reBuf, imBuf [128]float64
		p.run(x, spec, uBuf[:inv.n2], reBuf[:inv.m], imBuf[:inv.m])
		return
	}
	var uBuf, reBuf, imBuf [1024]float64
	p.run(x, spec, uBuf[:inv.n2], reBuf[:inv.m], imBuf[:inv.m])
}

func (p *mdctPlan) run(x, spec, u, cr, ci []float64) {
	inv := p.inv
	inv.fold(x, u)
	inv.dctIV(u, spec, cr, ci)
	for k := range spec {
		spec[k] *= 2
	}
}

// windowedLong fills w2048 with the analysis-windowed block for a long
// window sequence (sine shape; the encoder writes window_shape 0). The
// left half of LONG_STOP and the right half of LONG_START carry the
// short-window tapers at the offsets the decoder's longWindowApply uses.
func windowedLong(t *[2048]float64, seq int, out *[2048]float64) {
	wl := &longWindow[shapeSine]
	ws := &shortWindow[shapeSine]
	if seq == longStop {
		for n := 0; n < 448; n++ {
			out[n] = 0
		}
		for n := 0; n < 128; n++ {
			out[448+n] = t[448+n] * ws[n]
		}
		for n := 576; n < 1024; n++ {
			out[n] = t[n]
		}
	} else {
		for n := 0; n < 1024; n++ {
			out[n] = t[n] * wl[n]
		}
	}
	if seq == longStart {
		for n := 1024; n < 1472; n++ {
			out[n] = t[n]
		}
		for n := 0; n < 128; n++ {
			out[1472+n] = t[1472+n] * ws[128+n]
		}
		for n := 1600; n < 2048; n++ {
			out[n] = 0
		}
	} else {
		for n := 1024; n < 2048; n++ {
			out[n] = t[n] * wl[n]
		}
	}
}

// mdctFrame transforms one 2048-sample time block into the 1024-line
// spectrum for the window sequence: one long transform, or eight short
// transforms at 128-sample hops starting at offset 448 (the decoder's
// shortFilterbank layout), window-major in spec.
func mdctFrame(t *[2048]float64, seq int, spec *[1024]float64) {
	if seq != eightShort {
		var w [2048]float64
		windowedLong(t, seq, &w)
		mdctLong.mdct(w[:], spec[:1024])
		return
	}
	ws := &shortWindow[shapeSine]
	var w [256]float64
	for i := 0; i < 8; i++ {
		off := 448 + i*128
		for n := 0; n < 256; n++ {
			w[n] = t[off+n] * ws[n]
		}
		mdctShort.mdct(w[:], spec[i*128:i*128+128])
	}
}
