package opus

// SILK NLSF decoding and NLSF->LPC conversion, ported from libopus
// silk/NLSF_decode.c, NLSF_unpack.c, NLSF_stabilize.c, NLSF2A.c, LPC_fit.c,
// bwexpander.c, bwexpander_32.c, and LPC_inv_pred_gain.c.

// nlsf2AOrdering16/10 reorder cos(LSF) for the polynomial recurrence (NLSF2A.c).
var nlsf2AOrdering16 = [16]uint8{0, 15, 8, 7, 4, 11, 12, 3, 2, 13, 10, 5, 6, 9, 14, 1}
var nlsf2AOrdering10 = [10]uint8{0, 9, 6, 3, 4, 5, 8, 1, 2, 7}

const nlsfQuantLevelAdj = 0.1 // NLSF_QUANT_LEVEL_ADJ

// silkNLSFResidualDequant reconstructs the NLSF residual (Q10) from the
// second-stage indices and backward predictor (silk_NLSF_residual_dequant).
func silkNLSFResidualDequant(xQ10 []int16, indices []int8, predCoefQ8 []uint8, quantStepSizeQ16 int32, order int) {
	outQ10 := int32(0)
	for i := order - 1; i >= 0; i-- {
		predQ10 := silkRSHIFT(silkSMULBB(outQ10, int32(predCoefQ8[i])), 8)
		outQ10 = silkLSHIFT(int32(indices[i]), 10)
		if outQ10 > 0 {
			outQ10 -= silkFixConst(nlsfQuantLevelAdj, 10)
		} else if outQ10 < 0 {
			outQ10 += silkFixConst(nlsfQuantLevelAdj, 10)
		}
		outQ10 = silkSMLAWB(predQ10, outQ10, quantStepSizeQ16)
		xQ10[i] = int16(outQ10)
	}
}

// silkNLSFUnpack unpacks the entropy-table indices and backward predictor for
// a first-stage codebook entry (silk_NLSF_unpack).
func silkNLSFUnpack(ecIx []int16, predQ8 []uint8, cb *nlsfCB, cb1Index int) {
	sel := cb.ecSel[cb1Index*cb.order/2:]
	for i := 0; i < cb.order; i += 2 {
		entry := sel[i/2]
		ecIx[i] = int16(silkSMULBB(int32(entry>>1)&7, 2*nlsfQuantMaxAmp+1))
		predQ8[i] = cb.predQ8[i+int(entry&1)*(cb.order-1)]
		ecIx[i+1] = int16(silkSMULBB(int32(entry>>5)&7, 2*nlsfQuantMaxAmp+1))
		predQ8[i+1] = cb.predQ8[i+int((entry>>4)&1)*(cb.order-1)+1]
	}
}

// silkNLSFDecode decodes a quantized NLSF vector (Q15) from the codebook path
// (silk_NLSF_decode).
func silkNLSFDecode(pNLSFQ15 []int16, nlsfIndices []int8, cb *nlsfCB) {
	var predQ8 [silkMaxLPCOrder]uint8
	var ecIx [silkMaxLPCOrder]int16
	var resQ10 [silkMaxLPCOrder]int16
	silkNLSFUnpack(ecIx[:], predQ8[:], cb, int(nlsfIndices[0]))
	silkNLSFResidualDequant(resQ10[:], nlsfIndices[1:], predQ8[:], cb.quantStepSizeQ16, cb.order)
	cbElem := cb.cb1NLSFQ8[int(nlsfIndices[0])*cb.order:]
	cbWght := cb.cb1WghtQ9[int(nlsfIndices[0])*cb.order:]
	for i := 0; i < cb.order; i++ {
		tmp := silkDIV32_16(silkLSHIFT(int32(resQ10[i]), 14), int32(cbWght[i])) + silkLSHIFT(int32(cbElem[i]), 7)
		pNLSFQ15[i] = int16(silkLIMIT(tmp, 0, 32767))
	}
	silkNLSFStabilize(pNLSFQ15, cb.deltaMinQ15, cb.order)
}

// silkNLSFStabilize enforces the minimum NLSF spacing (silk_NLSF_stabilize).
func silkNLSFStabilize(nlsfQ15 []int16, ndeltaMinQ15 []int16, L int) {
	const maxLoops = 20
	var I, loops int
	for loops = 0; loops < maxLoops; loops++ {
		minDiff := int32(nlsfQ15[0]) - int32(ndeltaMinQ15[0])
		I = 0
		for i := 1; i <= L-1; i++ {
			diff := int32(nlsfQ15[i]) - (int32(nlsfQ15[i-1]) + int32(ndeltaMinQ15[i]))
			if diff < minDiff {
				minDiff = diff
				I = i
			}
		}
		diff := int32(1<<15) - (int32(nlsfQ15[L-1]) + int32(ndeltaMinQ15[L]))
		if diff < minDiff {
			minDiff = diff
			I = L
		}
		if minDiff >= 0 {
			return
		}
		if I == 0 {
			nlsfQ15[0] = ndeltaMinQ15[0]
		} else if I == L {
			nlsfQ15[L-1] = int16(int32(1<<15) - int32(ndeltaMinQ15[L]))
		} else {
			minCenter := int32(0)
			for k := 0; k < I; k++ {
				minCenter += int32(ndeltaMinQ15[k])
			}
			minCenter += silkRSHIFT(int32(ndeltaMinQ15[I]), 1)
			maxCenter := int32(1 << 15)
			for k := L; k > I; k-- {
				maxCenter -= int32(ndeltaMinQ15[k])
			}
			maxCenter -= silkRSHIFT(int32(ndeltaMinQ15[I]), 1)
			center := silkLIMIT(silkRSHIFTROUND(int32(nlsfQ15[I-1])+int32(nlsfQ15[I]), 1), minCenter, maxCenter)
			nlsfQ15[I-1] = int16(center - silkRSHIFT(int32(ndeltaMinQ15[I]), 1))
			nlsfQ15[I] = int16(int32(nlsfQ15[I-1]) + int32(ndeltaMinQ15[I]))
		}
	}
	// Fallback: sort ascending and enforce spacing (rarely reached).
	for i := 1; i < L; i++ {
		for j := i; j > 0 && nlsfQ15[j] < nlsfQ15[j-1]; j-- {
			nlsfQ15[j], nlsfQ15[j-1] = nlsfQ15[j-1], nlsfQ15[j]
		}
	}
	nlsfQ15[0] = int16(silkMaxInt(int32(nlsfQ15[0]), int32(ndeltaMinQ15[0])))
	for i := 1; i < L; i++ {
		nlsfQ15[i] = int16(silkMaxInt(int32(nlsfQ15[i]), silkADDSAT32(int32(nlsfQ15[i-1]), int32(ndeltaMinQ15[i]))))
	}
	nlsfQ15[L-1] = int16(silkMinInt(int32(nlsfQ15[L-1]), int32(1<<15)-int32(ndeltaMinQ15[L])))
	for i := L - 2; i >= 0; i-- {
		nlsfQ15[i] = int16(silkMinInt(int32(nlsfQ15[i]), int32(nlsfQ15[i+1])-int32(ndeltaMinQ15[i+1])))
	}
}

const nlsf2AQA = 16

// silkNLSF2AFindPoly builds one interleaved polynomial (silk_NLSF2A_find_poly).
func silkNLSF2AFindPoly(out, cLSF []int32, dd int) {
	out[0] = silkLSHIFT(1, nlsf2AQA)
	out[1] = -cLSF[0]
	for k := 1; k < dd; k++ {
		ftmp := cLSF[2*k]
		out[k+1] = silkLSHIFT(out[k-1], 1) - int32(silkRSHIFTROUND64(silkSMULL(ftmp, out[k]), nlsf2AQA))
		for n := k; n > 1; n-- {
			out[n] += out[n-2] - int32(silkRSHIFTROUND64(silkSMULL(ftmp, out[n-1]), nlsf2AQA))
		}
		out[1] -= ftmp
	}
}

// silkNLSF2A converts NLSFs (Q15) to LPC coefficients (Q12) (silk_NLSF2A).
func silkNLSF2A(aQ12 []int16, nlsf []int16, d int) {
	var ordering []uint8
	if d == 16 {
		ordering = nlsf2AOrdering16[:]
	} else {
		ordering = nlsf2AOrdering10[:]
	}
	var cosLSFQA [silkMaxLPCOrder]int32
	for k := 0; k < d; k++ {
		fInt := silkRSHIFT(int32(nlsf[k]), 15-7)
		fFrac := int32(nlsf[k]) - silkLSHIFT(fInt, 15-7)
		cosVal := int32(silk_LSFCosTab_FIX_Q12[fInt])
		delta := int32(silk_LSFCosTab_FIX_Q12[fInt+1]) - cosVal
		cosLSFQA[ordering[k]] = silkRSHIFTROUND(silkLSHIFT(cosVal, 8)+delta*fFrac, 20-nlsf2AQA)
	}
	dd := d >> 1
	var P, Q [silkMaxLPCOrder/2 + 1]int32
	silkNLSF2AFindPoly(P[:], cosLSFQA[0:], dd)
	silkNLSF2AFindPoly(Q[:], cosLSFQA[1:], dd)
	var a32QA1 [silkMaxLPCOrder]int32
	for k := 0; k < dd; k++ {
		Ptmp := P[k+1] + P[k]
		Qtmp := Q[k+1] - Q[k]
		a32QA1[k] = -Qtmp - Ptmp
		a32QA1[d-k-1] = Qtmp - Ptmp
	}
	silkLPCFit(aQ12, a32QA1[:], 12, nlsf2AQA+1, d)
	for i := 0; silkLPCInversePredGain(aQ12, d) == 0 && i < maxLPCStabilizeIters; i++ {
		silkBWExpander32(a32QA1[:], d, int32(65536-silkLSHIFT(2, i)))
		for k := 0; k < d; k++ {
			aQ12[k] = int16(silkRSHIFTROUND(a32QA1[k], nlsf2AQA+1-12))
		}
	}
}

// silkLPCFit scales the QA+1 LPC coefficients down to Q12 with saturation
// (silk_LPC_fit).
func silkLPCFit(aQOut []int16, aQIn []int32, qOut, qIn, d int) {
	var i, idx int
	for i = 0; i < 10; i++ {
		maxabs := int32(0)
		for k := 0; k < d; k++ {
			absval := silkAbs32(aQIn[k])
			if absval > maxabs {
				maxabs = absval
				idx = k
			}
		}
		maxabs = silkRSHIFTROUND(maxabs, qIn-qOut)
		if maxabs > silkInt16Max {
			maxabs = silkMinInt(maxabs, 163838)
			chirpQ16 := silkFixConst(0.999, 16) - silkDIV32(silkLSHIFT(maxabs-silkInt16Max, 14),
				silkRSHIFT(maxabs*int32(idx+1), 2))
			silkBWExpander32(aQIn, d, chirpQ16)
		} else {
			break
		}
	}
	if i == 10 {
		for k := 0; k < d; k++ {
			aQOut[k] = int16(silkSAT16(silkRSHIFTROUND(aQIn[k], qIn-qOut)))
			aQIn[k] = silkLSHIFT(int32(aQOut[k]), qIn-qOut)
		}
	} else {
		for k := 0; k < d; k++ {
			aQOut[k] = int16(silkRSHIFTROUND(aQIn[k], qIn-qOut))
		}
	}
}

// silkBWExpander32 applies chirp (bandwidth expansion) to Q-domain int32 LPC
// coefficients (silk_bwexpander_32).
func silkBWExpander32(ar []int32, d int, chirpQ16 int32) {
	chirpMinusOne := chirpQ16 - 65536
	for i := 0; i < d-1; i++ {
		ar[i] = silkSMULWW(chirpQ16, ar[i])
		chirpQ16 += silkRSHIFTROUND(chirpQ16*chirpMinusOne, 16)
	}
	ar[d-1] = silkSMULWW(chirpQ16, ar[d-1])
}

// silkBWExpander applies chirp to Q12 int16 LPC coefficients (silk_bwexpander).
func silkBWExpander(ar []int16, d int, chirpQ16 int32) {
	chirpMinusOne := chirpQ16 - 65536
	for i := 0; i < d-1; i++ {
		ar[i] = int16(silkRSHIFTROUND(chirpQ16*int32(ar[i]), 16))
		chirpQ16 += silkRSHIFTROUND(chirpQ16*chirpMinusOne, 16)
	}
	ar[d-1] = int16(silkRSHIFTROUND(chirpQ16*int32(ar[d-1]), 16))
}

const invPredGainQA = 24

// silkLPCInversePredGain returns the inverse prediction gain (Q30), or 0 if the
// filter is unstable (silk_LPC_inverse_pred_gain_c).
func silkLPCInversePredGain(aQ12 []int16, order int) int32 {
	var atmpQA [silkMaxLPCOrder]int32
	dcResp := int32(0)
	for k := 0; k < order; k++ {
		dcResp += int32(aQ12[k])
		atmpQA[k] = silkLSHIFT32(int32(aQ12[k]), invPredGainQA-12)
	}
	if dcResp >= 4096 {
		return 0
	}
	return lpcInversePredGainQA(atmpQA[:], order)
}

func mul32FracQ(a, b int32, q int) int32 {
	return int32(silkRSHIFTROUND64(silkSMULL(a, b), q))
}

func lpcInversePredGainQA(aQA []int32, order int) int32 {
	aLimit := silkFixConst(0.99975, invPredGainQA)
	invGainQ30 := silkFixConst(1, 30)
	var k int
	for k = order - 1; k > 0; k-- {
		if aQA[k] > aLimit || aQA[k] < -aLimit {
			return 0
		}
		rcQ31 := -silkLSHIFT(aQA[k], 31-invPredGainQA)
		rcMult1Q30 := silkFixConst(1, 30) - silkSMMUL(rcQ31, rcQ31)
		invGainQ30 = silkLSHIFT(silkSMMUL(invGainQ30, rcMult1Q30), 2)
		if invGainQ30 < silkFixConst(1.0/10000.0, 30) { // 1/MAX_PREDICTION_POWER_GAIN
			return 0
		}
		mult2Q := 32 - int(silkCLZ32(silkAbs32(rcMult1Q30)))
		rcMult2 := silkINVERSE32varQ(rcMult1Q30, mult2Q+30)
		for n := 0; n < (k+1)>>1; n++ {
			tmp1 := aQA[n]
			tmp2 := aQA[k-n-1]
			t := silkRSHIFTROUND64(silkSMULL(silkSUBSAT32(tmp1, mul32FracQ(tmp2, rcQ31, 31)), rcMult2), mult2Q)
			if t > silkInt32Max || t < silkInt32Min {
				return 0
			}
			aQA[n] = int32(t)
			t = silkRSHIFTROUND64(silkSMULL(silkSUBSAT32(tmp2, mul32FracQ(tmp1, rcQ31, 31)), rcMult2), mult2Q)
			if t > silkInt32Max || t < silkInt32Min {
				return 0
			}
			aQA[k-n-1] = int32(t)
		}
	}
	if aQA[k] > aLimit || aQA[k] < -aLimit {
		return 0
	}
	rcQ31 := -silkLSHIFT(aQA[0], 31-invPredGainQA)
	rcMult1Q30 := silkFixConst(1, 30) - silkSMMUL(rcQ31, rcQ31)
	invGainQ30 = silkLSHIFT(silkSMMUL(invGainQ30, rcMult1Q30), 2)
	if invGainQ30 < silkFixConst(1.0/10000.0, 30) {
		return 0
	}
	return invGainQ30
}
