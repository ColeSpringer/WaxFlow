package opus

// LPC-to-NLSF conversion, ported from libopus silk/A2NLSF.c. The exact
// inverse pair of silkNLSF2A in silk_nlsf.go: a piecewise linear
// approximation maps LSF <-> cos(LSF), so the values are not accurate NLSFs
// but the two functions invert each other accurately.

const (
	binDivStepsA2NLSF   = 3
	maxIterationsA2NLSF = 16
	lsfCosTabSz         = 128 // LSF_COS_TAB_SZ_FIX
)

// a2nlsfTransPoly transforms polynomials from cos(n*f) to cos(f)^n
// (silk_A2NLSF_trans_poly).
func a2nlsfTransPoly(p []int32, dd int) {
	for k := 2; k <= dd; k++ {
		for n := dd; n > k; n-- {
			p[n-2] -= p[n]
		}
		p[k-2] -= silkLSHIFT(p[k], 1)
	}
}

// a2nlsfEvalPoly evaluates the polynomial at x (Q12), returning Q16
// (silk_A2NLSF_eval_poly).
func a2nlsfEvalPoly(p []int32, x int32, dd int) int32 {
	y32 := p[dd]
	xQ16 := silkLSHIFT(x, 4)
	for n := dd - 1; n >= 0; n-- {
		y32 = silkSMLAWW(p[n], y32, xQ16)
	}
	return y32
}

// a2nlsfInit converts filter coefficients to the even/odd polynomial pair
// (silk_A2NLSF_init).
func a2nlsfInit(aQ16 []int32, P, Q []int32, dd int) {
	P[dd] = 1 << 16
	Q[dd] = 1 << 16
	for k := 0; k < dd; k++ {
		P[k] = -aQ16[dd-k-1] - aQ16[dd+k]
		Q[k] = -aQ16[dd-k-1] + aQ16[dd+k]
	}
	for k := dd; k > 0; k-- {
		P[k-1] -= P[k]
		Q[k-1] += Q[k]
	}
	a2nlsfTransPoly(P, dd)
	a2nlsfTransPoly(Q, dd)
}

// silkA2NLSF computes NLSFs from whitening filter coefficients, bandwidth
// expanding until all roots are found (silk_A2NLSF). aQ16 is modified when
// expansion is applied.
func silkA2NLSF(nlsf []int16, aQ16 []int32, d int) {
	var P, Q [silkMaxOrderLPC/2 + 1]int32
	PQ := [2][]int32{P[:], Q[:]}

	dd := d >> 1
	a2nlsfInit(aQ16, P[:], Q[:], dd)

	p := P[:]
	xlo := int32(silk_LSFCosTab_FIX_Q12[0])
	ylo := a2nlsfEvalPoly(p, xlo, dd)
	rootIx := 0
	if ylo < 0 {
		nlsf[0] = 0
		p = Q[:]
		ylo = a2nlsfEvalPoly(p, xlo, dd)
		rootIx = 1
	}
	k := 1
	i := 0
	thr := int32(0)
	for {
		xhi := int32(silk_LSFCosTab_FIX_Q12[k])
		yhi := a2nlsfEvalPoly(p, xhi, dd)

		if (ylo <= 0 && yhi >= thr) || (ylo >= 0 && yhi <= -thr) {
			if yhi == 0 {
				thr = 1
			} else {
				thr = 0
			}
			ffrac := int32(-256)
			for m := 0; m < binDivStepsA2NLSF; m++ {
				xmid := silkRSHIFTROUND(xlo+xhi, 1)
				ymid := a2nlsfEvalPoly(p, xmid, dd)
				if (ylo <= 0 && ymid >= 0) || (ylo >= 0 && ymid <= 0) {
					xhi = xmid
					yhi = ymid
				} else {
					xlo = xmid
					ylo = ymid
					ffrac += 128 >> uint(m)
				}
			}
			if silkAbs32(ylo) < 65536 {
				den := ylo - yhi
				nom := silkLSHIFT(ylo, 8-binDivStepsA2NLSF) + silkRSHIFT(den, 1)
				if den != 0 {
					ffrac += silkDIV32(nom, den)
				}
			} else {
				ffrac += silkDIV32(ylo, silkRSHIFT(ylo-yhi, 8-binDivStepsA2NLSF))
			}
			nlsf[rootIx] = int16(silkMinInt(silkLSHIFT(int32(k), 8)+ffrac, silkInt16Max))

			rootIx++
			if rootIx >= d {
				break
			}
			p = PQ[rootIx&1]
			xlo = int32(silk_LSFCosTab_FIX_Q12[k-1])
			ylo = silkLSHIFT(1-int32(rootIx&2), 12)
		} else {
			k++
			xlo = xhi
			ylo = yhi
			thr = 0
			if k > lsfCosTabSz {
				i++
				if i > maxIterationsA2NLSF {
					// Set NLSFs to white spectrum and exit.
					nlsf[0] = int16(silkDIV32_16(1<<15, int32(d+1)))
					for k := 1; k < d; k++ {
						nlsf[k] = nlsf[k-1] + nlsf[0]
					}
					return
				}
				silkBWExpander32(aQ16, d, 65536-silkLSHIFT(1, i))
				a2nlsfInit(aQ16, P[:], Q[:], dd)
				p = P[:]
				xlo = int32(silk_LSFCosTab_FIX_Q12[0])
				ylo = a2nlsfEvalPoly(p, xlo, dd)
				if ylo < 0 {
					nlsf[0] = 0
					p = Q[:]
					ylo = a2nlsfEvalPoly(p, xlo, dd)
					rootIx = 1
				} else {
					rootIx = 0
				}
				k = 1
			}
		}
	}
}
