package opus

// SILK stereo encoding, ported from libopus silk/stereo_LR_to_MS.c,
// stereo_find_predictor.c, stereo_quant_pred.c, stereo_encode_pred.c,
// sum_sqr_shift.c, and inner_prod_aligned.c. The decode direction
// (MS to LR) lives with the decoder.

const (
	stereoRatioSmoothCoef = 0.01
	stereoQuantTabSize    = 16
)

// silkSumSqrShift computes the energy of x and the right-shift applied to
// make it fit an int32 with headroom (silk_sum_sqr_shift).
func silkSumSqrShift(x []int16, length int) (energy int32, shift int) {
	shft := 31 - int(silkCLZ32(int32(length)))
	nrg := int32(length)
	var i int
	for i = 0; i < length-1; i += 2 {
		nrgTmp := uint32(silkSMULBB(int32(x[i]), int32(x[i])))
		nrgTmp = uint32(silkSMLABBovflw(int32(nrgTmp), int32(x[i+1]), int32(x[i+1])))
		nrg = int32(uint32(nrg) + (nrgTmp >> uint(shft)))
	}
	if i < length {
		nrgTmp := uint32(silkSMULBB(int32(x[i]), int32(x[i])))
		nrg = int32(uint32(nrg) + (nrgTmp >> uint(shft)))
	}
	shft = int(silkMaxInt(0, int32(shft)+3-silkCLZ32(nrg)))
	nrg = 0
	for i = 0; i < length-1; i += 2 {
		nrgTmp := uint32(silkSMULBB(int32(x[i]), int32(x[i])))
		nrgTmp = uint32(silkSMLABBovflw(int32(nrgTmp), int32(x[i+1]), int32(x[i+1])))
		nrg = int32(uint32(nrg) + (nrgTmp >> uint(shft)))
	}
	if i < length {
		nrgTmp := uint32(silkSMULBB(int32(x[i]), int32(x[i])))
		nrg = int32(uint32(nrg) + (nrgTmp >> uint(shft)))
	}
	return nrg, shft
}

// silkInnerProdAlignedScale computes a scaled inner product
// (silk_inner_prod_aligned_scale).
func silkInnerProdAlignedScale(v1, v2 []int16, scale, length int) int32 {
	var sum int32
	for i := 0; i < length; i++ {
		sum += silkRSHIFT(silkSMULBB(int32(v1[i]), int32(v2[i])), scale)
	}
	return sum
}

// silkStereoFindPredictor finds and smooths the least-squares predictor of y
// from x (silk_stereo_find_predictor). Returns the predictor in Q13.
func silkStereoFindPredictor(ratioQ14 *int32, x, y []int16, midResAmpQ0 []int32, length int, smoothCoefQ16 int32) int32 {
	nrgx, scale1 := silkSumSqrShift(x, length)
	nrgy, scale2 := silkSumSqrShift(y, length)
	scale := scale1
	if scale2 > scale {
		scale = scale2
	}
	scale += scale & 1
	nrgy = silkRSHIFT(nrgy, scale-scale2)
	nrgx = silkRSHIFT(nrgx, scale-scale1)
	nrgx = silkMaxInt(nrgx, 1)
	corr := silkInnerProdAlignedScale(x, y, scale, length)
	predQ13 := silkDIV32varQ(corr, nrgx, 13)
	predQ13 = silkLIMIT(predQ13, -(1 << 14), 1<<14)
	pred2Q10 := silkSMULWB(predQ13, predQ13)

	smoothCoefQ16 = silkMaxInt(smoothCoefQ16, silkAbs32(pred2Q10))

	scale >>= 1
	midResAmpQ0[0] = silkSMLAWB(midResAmpQ0[0], silkLSHIFT(silkSQRTAPPROX(nrgx), scale)-midResAmpQ0[0], smoothCoefQ16)
	nrgy -= silkLSHIFT(silkSMULWB(corr, predQ13), 3+1)
	nrgy += silkLSHIFT(silkSMULWB(nrgx, pred2Q10), 6)
	midResAmpQ0[1] = silkSMLAWB(midResAmpQ0[1], silkLSHIFT(silkSQRTAPPROX(nrgy), scale)-midResAmpQ0[1], smoothCoefQ16)

	*ratioQ14 = silkDIV32varQ(midResAmpQ0[1], silkMaxInt(midResAmpQ0[0], 1), 14)
	*ratioQ14 = silkLIMIT(*ratioQ14, 0, 32767)
	return predQ13
}

// silkStereoQuantPred quantizes the mid/side predictors (silk_stereo_quant_pred).
func silkStereoQuantPred(predQ13 []int32, ix *[2][3]int8) {
	for n := 0; n < 2; n++ {
		errMinQ13 := int32(silkInt32Max)
		quantPredQ13 := int32(0)
	search:
		for i := 0; i < stereoQuantTabSize-1; i++ {
			lowQ13 := int32(silk_stereo_pred_quant_Q13[i])
			stepQ13 := silkSMULWB(int32(silk_stereo_pred_quant_Q13[i+1])-lowQ13,
				silkFixConst(0.5/stereoQuantSubSteps, 16))
			for j := 0; j < stereoQuantSubSteps; j++ {
				lvlQ13 := silkSMLABB(lowQ13, stepQ13, int32(2*j+1))
				errQ13 := silkAbs32(predQ13[n] - lvlQ13)
				if errQ13 < errMinQ13 {
					errMinQ13 = errQ13
					quantPredQ13 = lvlQ13
					ix[n][0] = int8(i)
					ix[n][1] = int8(j)
				} else {
					// Error increasing: past the optimum.
					break search
				}
			}
		}
		ix[n][2] = ix[n][0] / 3
		ix[n][0] -= ix[n][2] * 3
		predQ13[n] = quantPredQ13
	}
	predQ13[0] -= predQ13[1]
}

// silkStereoEncodePred entropy codes the predictor indices
// (silk_stereo_encode_pred).
func silkStereoEncodePred(enc *rangeEncoder, ix *[2][3]int8) {
	n := 5*int(ix[0][2]) + int(ix[1][2])
	enc.encodeICDF(n, silk_stereo_pred_joint_iCDF, 8)
	for n := 0; n < 2; n++ {
		enc.encodeICDF(int(ix[n][0]), silk_uniform3_iCDF, 8)
		enc.encodeICDF(int(ix[n][1]), silk_uniform5_iCDF, 8)
	}
}

// silkStereoEncodeMidOnly entropy codes the mid-only flag
// (silk_stereo_encode_mid_only).
func silkStereoEncodeMidOnly(enc *rangeEncoder, midOnlyFlag int8) {
	enc.encodeICDF(int(midOnlyFlag), silk_stereo_only_code_mid_iCDF, 8)
}

// lrToMS converts a Left/Right frame to adaptive Mid/Side
// (silk_stereo_LR_to_MS). x1 and x2 are the full channel buffers including
// the two history samples at the front (the reference's &x1[-2]); on return
// x1 holds the mid signal and x2 the side residual.
func (state *stereoEncState) lrToMS(x1, x2 []int16, ix *[2][3]int8, midOnlyFlag *int8,
	midSideRatesBps []int32, totalRateBps int32, prevSpeechActQ8 int32, toMono bool, fsKHz, frameLength int) {

	side := make([]int16, frameLength+2)
	mid := x1
	// Convert to basic mid/side signals.
	for n := 0; n < frameLength+2; n++ {
		sum := int32(x1[n]) + int32(x2[n])
		diff := int32(x1[n]) - int32(x2[n])
		mid[n] = int16(silkRSHIFTROUND(sum, 1))
		side[n] = int16(silkSAT16(silkRSHIFTROUND(diff, 1)))
	}

	// Buffering.
	copy(mid[:2], state.sMid[:])
	copy(side[:2], state.sSide[:])
	copy(state.sMid[:], mid[frameLength:frameLength+2])
	copy(state.sSide[:], side[frameLength:frameLength+2])

	// LP and HP filter mid and side signals.
	lpMid := make([]int16, frameLength)
	hpMid := make([]int16, frameLength)
	for n := 0; n < frameLength; n++ {
		sum := silkRSHIFTROUND(int32(mid[n])+int32(mid[n+2])+silkLSHIFT(int32(mid[n+1]), 1), 2)
		lpMid[n] = int16(sum)
		hpMid[n] = int16(int32(mid[n+1]) - sum)
	}
	lpSide := make([]int16, frameLength)
	hpSide := make([]int16, frameLength)
	for n := 0; n < frameLength; n++ {
		sum := silkRSHIFTROUND(int32(side[n])+int32(side[n+2])+silkLSHIFT(int32(side[n+1]), 1), 2)
		lpSide[n] = int16(sum)
		hpSide[n] = int16(int32(side[n+1]) - sum)
	}

	is10msFrame := frameLength == 10*fsKHz
	var smoothCoefQ16 int32
	if is10msFrame {
		smoothCoefQ16 = silkFixConst(stereoRatioSmoothCoef/2, 16)
	} else {
		smoothCoefQ16 = silkFixConst(stereoRatioSmoothCoef, 16)
	}
	smoothCoefQ16 = silkSMULWB(silkSMULBB(prevSpeechActQ8, prevSpeechActQ8), smoothCoefQ16)

	var lpRatioQ14, hpRatioQ14 int32
	var predQ13 [2]int32
	predQ13[0] = silkStereoFindPredictor(&lpRatioQ14, lpMid, lpSide, state.midSideAmpQ0[0:2], frameLength, smoothCoefQ16)
	predQ13[1] = silkStereoFindPredictor(&hpRatioQ14, hpMid, hpSide, state.midSideAmpQ0[2:4], frameLength, smoothCoefQ16)

	fracQ16 := silkSMLABB(hpRatioQ14, lpRatioQ14, 3)
	fracQ16 = silkMinInt(fracQ16, silkFixConst(1, 16))

	if is10msFrame {
		totalRateBps -= 1200
	} else {
		totalRateBps -= 600
	}
	if totalRateBps < 1 {
		totalRateBps = 1
	}
	minMidRateBps := silkSMLABB(2000, int32(fsKHz), 600)
	frac3Q16 := 3 * fracQ16
	midSideRatesBps[0] = silkDIV32varQ(totalRateBps, silkFixConst(8+5, 16)+frac3Q16, 16+3)
	var widthQ14 int32
	if midSideRatesBps[0] < minMidRateBps {
		midSideRatesBps[0] = minMidRateBps
		midSideRatesBps[1] = totalRateBps - midSideRatesBps[0]
		widthQ14 = silkDIV32varQ(silkLSHIFT(midSideRatesBps[1], 1)-minMidRateBps,
			silkSMULWB(silkFixConst(1, 16)+frac3Q16, minMidRateBps), 14+2)
		widthQ14 = silkLIMIT(widthQ14, 0, silkFixConst(1, 14))
	} else {
		midSideRatesBps[1] = totalRateBps - midSideRatesBps[0]
		widthQ14 = silkFixConst(1, 14)
	}

	state.smthWidthQ14 = int16(silkSMLAWB(int32(state.smthWidthQ14), widthQ14-int32(state.smthWidthQ14), smoothCoefQ16))

	*midOnlyFlag = 0
	switch {
	case toMono:
		widthQ14 = 0
		predQ13[0] = 0
		predQ13[1] = 0
		silkStereoQuantPred(predQ13[:], ix)
	case state.widthPrevQ14 == 0 &&
		(8*totalRateBps < 13*minMidRateBps || silkSMULWB(fracQ16, int32(state.smthWidthQ14)) < silkFixConst(0.05, 14)):
		predQ13[0] = silkRSHIFT(silkSMULBB(int32(state.smthWidthQ14), predQ13[0]), 14)
		predQ13[1] = silkRSHIFT(silkSMULBB(int32(state.smthWidthQ14), predQ13[1]), 14)
		silkStereoQuantPred(predQ13[:], ix)
		widthQ14 = 0
		predQ13[0] = 0
		predQ13[1] = 0
		midSideRatesBps[0] = totalRateBps
		midSideRatesBps[1] = 0
		*midOnlyFlag = 1
	case state.widthPrevQ14 != 0 &&
		(8*totalRateBps < 11*minMidRateBps || silkSMULWB(fracQ16, int32(state.smthWidthQ14)) < silkFixConst(0.02, 14)):
		predQ13[0] = silkRSHIFT(silkSMULBB(int32(state.smthWidthQ14), predQ13[0]), 14)
		predQ13[1] = silkRSHIFT(silkSMULBB(int32(state.smthWidthQ14), predQ13[1]), 14)
		silkStereoQuantPred(predQ13[:], ix)
		widthQ14 = 0
		predQ13[0] = 0
		predQ13[1] = 0
	case int32(state.smthWidthQ14) > silkFixConst(0.95, 14):
		silkStereoQuantPred(predQ13[:], ix)
		widthQ14 = silkFixConst(1, 14)
	default:
		predQ13[0] = silkRSHIFT(silkSMULBB(int32(state.smthWidthQ14), predQ13[0]), 14)
		predQ13[1] = silkRSHIFT(silkSMULBB(int32(state.smthWidthQ14), predQ13[1]), 14)
		silkStereoQuantPred(predQ13[:], ix)
		widthQ14 = int32(state.smthWidthQ14)
	}

	// Keep encoding the side until the tapered output has been transmitted.
	if *midOnlyFlag == 1 {
		state.silentSideLen += int16(frameLength - stereoInterpLenMS*fsKHz)
		if state.silentSideLen < int16(laShapeMS*fsKHz) {
			*midOnlyFlag = 0
		} else {
			state.silentSideLen = 10000
		}
	} else {
		state.silentSideLen = 0
	}

	if *midOnlyFlag == 0 && midSideRatesBps[1] < 1 {
		midSideRatesBps[1] = 1
		midSideRatesBps[0] = silkMaxInt(1, totalRateBps-midSideRatesBps[1])
	}

	// Interpolate predictors and subtract prediction from side channel.
	pred0Q13 := -int32(state.predPrevQ13[0])
	pred1Q13 := -int32(state.predPrevQ13[1])
	wQ24 := silkLSHIFT(int32(state.widthPrevQ14), 10)
	denomQ16 := silkDIV32_16(1<<16, int32(stereoInterpLenMS*fsKHz))
	delta0Q13 := -silkRSHIFTROUND(silkSMULBB(predQ13[0]-int32(state.predPrevQ13[0]), denomQ16), 16)
	delta1Q13 := -silkRSHIFTROUND(silkSMULBB(predQ13[1]-int32(state.predPrevQ13[1]), denomQ16), 16)
	deltawQ24 := silkLSHIFT(silkSMULWB(widthQ14-int32(state.widthPrevQ14), denomQ16), 10)
	for n := 0; n < stereoInterpLenMS*fsKHz; n++ {
		pred0Q13 += delta0Q13
		pred1Q13 += delta1Q13
		wQ24 += deltawQ24
		sum := silkLSHIFT(int32(mid[n])+int32(mid[n+2])+silkLSHIFT(int32(mid[n+1]), 1), 9)
		sum = silkSMLAWB(silkSMULWB(wQ24, int32(side[n+1])), sum, pred0Q13)
		sum = silkSMLAWB(sum, silkLSHIFT(int32(mid[n+1]), 11), pred1Q13)
		x2[n+1] = int16(silkSAT16(silkRSHIFTROUND(sum, 8)))
	}

	pred0Q13 = -predQ13[0]
	pred1Q13 = -predQ13[1]
	wQ24 = silkLSHIFT(widthQ14, 10)
	for n := stereoInterpLenMS * fsKHz; n < frameLength; n++ {
		sum := silkLSHIFT(int32(mid[n])+int32(mid[n+2])+silkLSHIFT(int32(mid[n+1]), 1), 9)
		sum = silkSMLAWB(silkSMULWB(wQ24, int32(side[n+1])), sum, pred0Q13)
		sum = silkSMLAWB(sum, silkLSHIFT(int32(mid[n+1]), 11), pred1Q13)
		x2[n+1] = int16(silkSAT16(silkRSHIFTROUND(sum, 8)))
	}

	state.predPrevQ13[0] = int16(predQ13[0])
	state.predPrevQ13[1] = int16(predQ13[1])
	state.widthPrevQ14 = int16(widthQ14)
}
