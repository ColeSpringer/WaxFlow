package opus

// SILK pitch analysis, ported from libopus silk/float/pitch_analysis_core_FLP.c
// plus the fixed-point decimators it uses (silk/resampler_down2.c,
// silk/resampler_down2_3.c). The cross-correlation kernel reuses pitchXcorr
// from the CELT encoder port (celt/pitch.c is shared in the reference too).

// Pitch estimator constants (silk/pitch_est_defines.h).
const (
	peMaxFsKHz         = 16
	peSubfrLengthMS    = 5
	peLTPMemLengthMS   = 4 * peSubfrLengthMS
	peMaxFrameLengthMS = peLTPMemLengthMS + peMaxNBSubfr*peSubfrLengthMS
	peMaxFrameLength   = peMaxFrameLengthMS * peMaxFsKHz
	peMaxLag           = peMaxLagMS * peMaxFsKHz
	peDSrchLength      = 24
	peNBStage3Lags     = 5
	peNBCbksStage2     = 3
	peNBCbksStage2Ext  = 11
	peNBCbksStage3Max  = 34
	peNBCbksStage310ms = 12
	peNBCbksStage210ms = 3
	peShortlagBias     = 0.2
	pePrevlagBias      = 0.2
	peFlatcontourBias  = 0.05

	silkPEMinComplex = 0
	silkPEMidComplex = 1
	silkPEMaxComplex = 2

	pitchScratchSize = 22

	resamplerMaxBatchSizeIn = resamplerMaxBatchMS * 48
)

// silkResamplerDown2 downsamples by 2 with allpass sections
// (silk_resampler_down2). S is the 2-element Q10 state.
func silkResamplerDown2(S []int32, out, in []int16, inLen int) {
	len2 := inLen >> 1
	for k := 0; k < len2; k++ {
		in32 := silkLSHIFT(int32(in[2*k]), 10)
		Y := in32 - S[0]
		X := silkSMLAWB(Y, Y, silk_resampler_down2_1)
		out32 := S[0] + X
		S[0] = in32 + X

		in32 = silkLSHIFT(int32(in[2*k+1]), 10)
		Y = in32 - S[1]
		X = silkSMULWB(Y, silk_resampler_down2_0)
		out32 += S[1]
		out32 += X
		S[1] = in32 + X

		out[k] = int16(silkSAT16(silkRSHIFTROUND(out32, 11)))
	}
}

// Allpass coefficients for the 2x downsampler (silk/resampler_rom.h).
const (
	silk_resampler_down2_0 = 9872
	silk_resampler_down2_1 = 39809 - 65536
)

// silkResamplerDown23 downsamples by 2/3, low quality
// (silk_resampler_down2_3). S is the 6-element state.
func silkResamplerDown23(S []int32, out, in []int16, inLen int) {
	const orderFIR = 4
	buf := make([]int32, resamplerMaxBatchSizeIn+orderFIR)
	copy(buf[:orderFIR], S[:orderFIR])
	outPos, inPos := 0, 0
	nSamplesIn := 0
	for {
		nSamplesIn = inLen
		if nSamplesIn > resamplerMaxBatchSizeIn {
			nSamplesIn = resamplerMaxBatchSizeIn
		}
		silkResamplerAR2(S[orderFIR:], buf[orderFIR:], in[inPos:], silk_Resampler_2_3_COEFS_LQ, nSamplesIn)
		bufPtr := 0
		counter := nSamplesIn
		for counter > 2 {
			resQ6 := silkSMULWB(buf[bufPtr+0], int32(silk_Resampler_2_3_COEFS_LQ[2]))
			resQ6 = silkSMLAWB(resQ6, buf[bufPtr+1], int32(silk_Resampler_2_3_COEFS_LQ[3]))
			resQ6 = silkSMLAWB(resQ6, buf[bufPtr+2], int32(silk_Resampler_2_3_COEFS_LQ[5]))
			resQ6 = silkSMLAWB(resQ6, buf[bufPtr+3], int32(silk_Resampler_2_3_COEFS_LQ[4]))
			out[outPos] = int16(silkSAT16(silkRSHIFTROUND(resQ6, 6)))
			outPos++

			resQ6 = silkSMULWB(buf[bufPtr+1], int32(silk_Resampler_2_3_COEFS_LQ[4]))
			resQ6 = silkSMLAWB(resQ6, buf[bufPtr+2], int32(silk_Resampler_2_3_COEFS_LQ[5]))
			resQ6 = silkSMLAWB(resQ6, buf[bufPtr+3], int32(silk_Resampler_2_3_COEFS_LQ[3]))
			resQ6 = silkSMLAWB(resQ6, buf[bufPtr+4], int32(silk_Resampler_2_3_COEFS_LQ[2]))
			out[outPos] = int16(silkSAT16(silkRSHIFTROUND(resQ6, 6)))
			outPos++

			bufPtr += 3
			counter -= 3
		}
		inPos += nSamplesIn
		inLen -= nSamplesIn
		if inLen > 0 {
			copy(buf[:orderFIR], buf[nSamplesIn:])
		} else {
			break
		}
	}
	copy(S[:orderFIR], buf[nSamplesIn:])
}

// silkPitchAnalysisCore is the three-stage pitch analyser
// (silk_pitch_analysis_core_FLP). Returns false for voiced (a lag was
// found), true for unvoiced, matching the reference's 0/1.
func silkPitchAnalysisCore(frame []float32, pitchOut []int, lagIndex *int, contourIndex *int8,
	ltpCorr *float32, prevLag int, searchThres1, searchThres2 float32,
	fsKHz, complexity, nbSubfr int) bool {

	var frame8kHz [peMaxFrameLengthMS * 8]float32
	var frame4kHz [peMaxFrameLengthMS * 4]float32
	var frame8FIX [peMaxFrameLengthMS * 8]int16
	var frame4FIX [peMaxFrameLengthMS * 4]int16
	var filtState [6]int32

	frameLength := (peLTPMemLengthMS + nbSubfr*peSubfrLengthMS) * fsKHz
	frameLength4kHz := (peLTPMemLengthMS + nbSubfr*peSubfrLengthMS) * 4
	frameLength8kHz := (peLTPMemLengthMS + nbSubfr*peSubfrLengthMS) * 8
	sfLength := peSubfrLengthMS * fsKHz
	sfLength4kHz := peSubfrLengthMS * 4
	sfLength8kHz := peSubfrLengthMS * 8
	minLag := peMinLagMS * fsKHz
	minLag4kHz := peMinLagMS * 4
	minLag8kHz := peMinLagMS * 8
	maxLag := peMaxLagMS*fsKHz - 1
	maxLag4kHz := peMaxLagMS * 4
	maxLag8kHz := peMaxLagMS*8 - 1

	// Resample to 8 kHz.
	switch fsKHz {
	case 16:
		var frame16FIX [16 * peMaxFrameLengthMS]int16
		silkFloat2ShortArray(frame16FIX[:], frame, frameLength)
		silkResamplerDown2(filtState[:2], frame8FIX[:], frame16FIX[:], frameLength)
		silkShort2FloatArray(frame8kHz[:], frame8FIX[:], frameLength8kHz)
	case 12:
		var frame12FIX [12 * peMaxFrameLengthMS]int16
		silkFloat2ShortArray(frame12FIX[:], frame, frameLength)
		silkResamplerDown23(filtState[:6], frame8FIX[:], frame12FIX[:], frameLength)
		silkShort2FloatArray(frame8kHz[:], frame8FIX[:], frameLength8kHz)
	default:
		silkFloat2ShortArray(frame8FIX[:], frame, frameLength8kHz)
	}

	// Decimate again to 4 kHz.
	filtState[0], filtState[1] = 0, 0
	silkResamplerDown2(filtState[:2], frame4FIX[:], frame8FIX[:], frameLength8kHz)
	silkShort2FloatArray(frame4kHz[:], frame4FIX[:], frameLength4kHz)

	// Low-pass filter. The reference applies the int16 saturating-add macro
	// to float operands: the first operand is truncated to int, the sum
	// saturates, and the result truncates back through an int16 cast.
	for i := frameLength4kHz - 1; i > 0; i-- {
		sum := float64(int32(frame4kHz[i])) + float64(frame4kHz[i-1])
		if sum > silkInt16Max {
			sum = silkInt16Max
		} else if sum < silkInt16Min {
			sum = silkInt16Min
		}
		frame4kHz[i] = float32(int16(sum))
	}

	// FIRST STAGE: operating in 4 kHz.
	var C [peMaxNBSubfr][peMaxLag>>1 + 5]float32
	var xcorr [peMaxLagMS*4 - peMinLagMS*4 + 1]float32
	targetOff := silkLSHIFT32(int32(sfLength4kHz), 2)
	for k := 0; k < nbSubfr>>1; k++ {
		target := frame4kHz[targetOff:]
		basisOff := int(targetOff) - minLag4kHz
		pitchXcorr(target, frame4kHz[int(targetOff)-maxLag4kHz:], xcorr[:], sfLength8kHz, maxLag4kHz-minLag4kHz+1)

		crossCorr := float64(xcorr[maxLag4kHz-minLag4kHz])
		normalizer := silkEnergyFLP(target, sfLength8kHz) +
			silkEnergyFLP(frame4kHz[basisOff:], sfLength8kHz) +
			float64(sfLength8kHz)*4000.0
		C[0][minLag4kHz] += float32(2 * crossCorr / normalizer)

		for d := minLag4kHz + 1; d <= maxLag4kHz; d++ {
			basisOff--
			crossCorr = float64(xcorr[maxLag4kHz-d])
			normalizer += float64(frame4kHz[basisOff])*float64(frame4kHz[basisOff]) -
				float64(frame4kHz[basisOff+sfLength8kHz])*float64(frame4kHz[basisOff+sfLength8kHz])
			C[0][d] += float32(2 * crossCorr / normalizer)
		}
		targetOff += int32(sfLength8kHz)
	}

	// Short-lag bias.
	for i := maxLag4kHz; i >= minLag4kHz; i-- {
		C[0][i] -= C[0][i] * float32(i) / 4096.0
	}

	// Sort; keep length_d_srch best candidates.
	lengthDSrch := 4 + 2*complexity
	var dSrch [peDSrchLength]int
	silkInsertionSortDecreasingFLP(C[0][minLag4kHz:], dSrch[:], maxLag4kHz-minLag4kHz+1, lengthDSrch)

	Cmax := C[0][minLag4kHz]
	if Cmax < 0.2 {
		for i := 0; i < nbSubfr; i++ {
			pitchOut[i] = 0
		}
		*ltpCorr = 0
		*lagIndex = 0
		*contourIndex = 0
		return true
	}

	threshold := searchThres1 * Cmax
	for i := 0; i < lengthDSrch; i++ {
		if C[0][minLag4kHz+i] > threshold {
			dSrch[i] = (dSrch[i] + minLag4kHz) << 1
		} else {
			lengthDSrch = i
			break
		}
	}

	var dComp [peMaxLag>>1 + 5]int16
	for i := 0; i < lengthDSrch; i++ {
		dComp[dSrch[i]] = 1
	}

	// Convolution.
	for i := maxLag8kHz + 3; i >= minLag8kHz; i-- {
		dComp[i] += dComp[i-1] + dComp[i-2]
	}
	lengthDSrch = 0
	for i := minLag8kHz; i < maxLag8kHz+1; i++ {
		if dComp[i+1] > 0 {
			dSrch[lengthDSrch] = i
			lengthDSrch++
		}
	}
	for i := maxLag8kHz + 3; i >= minLag8kHz; i-- {
		dComp[i] += dComp[i-1] + dComp[i-2] + dComp[i-3]
	}
	lengthDComp := 0
	for i := minLag8kHz; i < maxLag8kHz+4; i++ {
		if dComp[i] > 0 {
			dComp[lengthDComp] = int16(i - 2)
			lengthDComp++
		}
	}

	// SECOND STAGE: operating at 8 kHz on high-correlation lag sections.
	for k := range C {
		for d := range C[k] {
			C[k][d] = 0
		}
	}
	var target []float32
	var targetBase int
	if fsKHz == 8 {
		target = frame
		targetBase = peLTPMemLengthMS * 8
	} else {
		target = frame8kHz[:]
		targetBase = peLTPMemLengthMS * 8
	}
	for k := 0; k < nbSubfr; k++ {
		energyTmp := silkEnergyFLP(target[targetBase:], sfLength8kHz) + 1.0
		for j := 0; j < lengthDComp; j++ {
			d := int(dComp[j])
			basis := target[targetBase-d:]
			crossCorr := silkInnerProductFLP(basis, target[targetBase:], sfLength8kHz)
			if crossCorr > 0.0 {
				energy := silkEnergyFLP(basis, sfLength8kHz)
				C[k][d] = float32(2 * crossCorr / (energy + energyTmp))
			} else {
				C[k][d] = 0.0
			}
		}
		targetBase += sfLength8kHz
	}

	CCmax := float32(0.0)
	CCmaxB := float32(-1000.0)
	CBimax := 0
	lag := -1
	var prevLagLog2 float32
	if prevLag > 0 {
		if fsKHz == 12 {
			prevLag = int(silkLSHIFT(int32(prevLag), 1)) / 3
		} else if fsKHz == 16 {
			prevLag >>= 1
		}
		prevLagLog2 = silkLog2F(float64(prevLag))
	}

	// Stage 2 codebook.
	var lagCB [][]int8
	var cbkSize, nbCbkSearch int
	if nbSubfr == peMaxNBSubfr {
		cbkSize = peNBCbksStage2Ext
		lagCB = silk_CB_lags_stage2
		if fsKHz == 8 && complexity > silkPEMinComplex {
			nbCbkSearch = peNBCbksStage2Ext
		} else {
			nbCbkSearch = peNBCbksStage2
		}
	} else {
		cbkSize = peNBCbksStage210ms
		lagCB = silk_CB_lags_stage2_10_ms
		nbCbkSearch = peNBCbksStage210ms
	}
	_ = cbkSize

	var CC [peNBCbksStage2Ext]float32
	for k := 0; k < lengthDSrch; k++ {
		d := dSrch[k]
		for j := 0; j < nbCbkSearch; j++ {
			CC[j] = 0.0
			for i := 0; i < nbSubfr; i++ {
				CC[j] += C[i][d+int(lagCB[i][j])]
			}
		}
		CCmaxNew := float32(-1000.0)
		CBimaxNew := 0
		for i := 0; i < nbCbkSearch; i++ {
			if CC[i] > CCmaxNew {
				CCmaxNew = CC[i]
				CBimaxNew = i
			}
		}
		lagLog2 := silkLog2F(float64(d))
		CCmaxNewB := CCmaxNew - peShortlagBias*float32(nbSubfr)*lagLog2
		if prevLag > 0 {
			deltaLagLog2Sqr := lagLog2 - prevLagLog2
			deltaLagLog2Sqr *= deltaLagLog2Sqr
			CCmaxNewB -= pePrevlagBias * float32(nbSubfr) * (*ltpCorr) * deltaLagLog2Sqr / (deltaLagLog2Sqr + 0.5)
		}
		if CCmaxNewB > CCmaxB && CCmaxNew > float32(nbSubfr)*searchThres2 {
			CCmaxB = CCmaxNewB
			CCmax = CCmaxNew
			lag = d
			CBimax = CBimaxNew
		}
	}

	if lag == -1 {
		for i := 0; i < nbSubfr; i++ {
			pitchOut[i] = 0
		}
		*ltpCorr = 0
		*lagIndex = 0
		*contourIndex = 0
		return true
	}

	*ltpCorr = CCmax / float32(nbSubfr)

	if fsKHz > 8 {
		// THIRD STAGE: search in the original signal.
		if fsKHz == 12 {
			lag = int(silkRSHIFTROUND(silkSMULBB(int32(lag), 3), 1))
		} else {
			lag = silkLSHIFTint(lag, 1)
		}
		lag = int(silkLIMIT(int32(lag), int32(minLag), int32(maxLag)))
		startLag := silkMaxIntGo(lag-2, minLag)
		endLag := silkMinIntGo(lag+2, maxLag)
		lagNew := lag
		CBimax = 0
		CCmax = -1000.0

		var energiesSt3, crossCorrSt3 [peMaxNBSubfr][peNBCbksStage3Max][peNBStage3Lags]float32
		silkPAnaCalcCorrSt3(&crossCorrSt3, frame, startLag, sfLength, nbSubfr, complexity)
		silkPAnaCalcEnergySt3(&energiesSt3, frame, startLag, sfLength, nbSubfr, complexity)

		lagCounter := 0
		contourBias := peFlatcontourBias / float32(lag)

		var lagCB3 [][]int8
		if nbSubfr == peMaxNBSubfr {
			nbCbkSearch = int(silk_nb_cbk_searchs_stage3[complexity])
			lagCB3 = silk_CB_lags_stage3
		} else {
			nbCbkSearch = peNBCbksStage310ms
			lagCB3 = silk_CB_lags_stage3_10_ms
		}

		energyTmp := silkEnergyFLP(frame[peLTPMemLengthMS*fsKHz:], nbSubfr*sfLength) + 1.0
		for d := startLag; d <= endLag; d++ {
			for j := 0; j < nbCbkSearch; j++ {
				crossCorr := 0.0
				energy := energyTmp
				for k := 0; k < nbSubfr; k++ {
					crossCorr += float64(crossCorrSt3[k][j][lagCounter])
					energy += float64(energiesSt3[k][j][lagCounter])
				}
				var CCmaxNew float32
				if crossCorr > 0.0 {
					CCmaxNew = float32(2 * crossCorr / energy)
					CCmaxNew *= 1.0 - contourBias*float32(j)
				}
				if CCmaxNew > CCmax && d+int(silk_CB_lags_stage3[0][j]) <= maxLag {
					CCmax = CCmaxNew
					lagNew = d
					CBimax = j
				}
			}
			lagCounter++
		}

		for k := 0; k < nbSubfr; k++ {
			pitchOut[k] = lagNew + int(lagCB3[k][CBimax])
			pitchOut[k] = int(silkLIMIT(int32(pitchOut[k]), int32(minLag), int32(peMaxLagMS*fsKHz)))
		}
		*lagIndex = lagNew - minLag
		*contourIndex = int8(CBimax)
	} else {
		for k := 0; k < nbSubfr; k++ {
			pitchOut[k] = lag + int(lagCB[k][CBimax])
			pitchOut[k] = int(silkLIMIT(int32(pitchOut[k]), int32(minLag8kHz), int32(peMaxLagMS*8)))
		}
		*lagIndex = lag - minLag8kHz
		*contourIndex = int8(CBimax)
	}
	return false
}

func silkLSHIFTint(a, s int) int { return a << uint(s) }
func silkMaxIntGo(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func silkMinIntGo(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// silkPAnaCalcCorrSt3 calculates the stage-3 correlations
// (silk_P_Ana_calc_corr_st3).
func silkPAnaCalcCorrSt3(crossCorrSt3 *[peMaxNBSubfr][peNBCbksStage3Max][peNBStage3Lags]float32,
	frame []float32, startLag, sfLength, nbSubfr, complexity int) {

	var lagRange [][]int8
	var lagCB [][]int8
	var nbCbkSearch int
	if nbSubfr == peMaxNBSubfr {
		lagRange = silk_Lag_range_stage3[complexity]
		lagCB = silk_CB_lags_stage3
		nbCbkSearch = int(silk_nb_cbk_searchs_stage3[complexity])
	} else {
		lagRange = silk_Lag_range_stage3_10_ms
		lagCB = silk_CB_lags_stage3_10_ms
		nbCbkSearch = peNBCbksStage310ms
	}

	var scratchMem [pitchScratchSize]float32
	var xcorr [pitchScratchSize]float32
	targetOff := silkLSHIFT32(int32(sfLength), 2)
	for k := 0; k < nbSubfr; k++ {
		lagCounter := 0
		lagLow := int(lagRange[k][0])
		lagHigh := int(lagRange[k][1])
		pitchXcorr(frame[targetOff:], frame[int(targetOff)-startLag-lagHigh:], xcorr[:], sfLength, lagHigh-lagLow+1)
		for j := lagLow; j <= lagHigh; j++ {
			scratchMem[lagCounter] = xcorr[lagHigh-j]
			lagCounter++
		}

		delta := int(lagRange[k][0])
		for i := 0; i < nbCbkSearch; i++ {
			idx := int(lagCB[k][i]) - delta
			for j := 0; j < peNBStage3Lags; j++ {
				crossCorrSt3[k][i][j] = scratchMem[idx+j]
			}
		}
		targetOff += int32(sfLength)
	}
}

// silkPAnaCalcEnergySt3 calculates the stage-3 energies recursively
// (silk_P_Ana_calc_energy_st3).
func silkPAnaCalcEnergySt3(energiesSt3 *[peMaxNBSubfr][peNBCbksStage3Max][peNBStage3Lags]float32,
	frame []float32, startLag, sfLength, nbSubfr, complexity int) {

	var lagRange [][]int8
	var lagCB [][]int8
	var nbCbkSearch int
	if nbSubfr == peMaxNBSubfr {
		lagRange = silk_Lag_range_stage3[complexity]
		lagCB = silk_CB_lags_stage3
		nbCbkSearch = int(silk_nb_cbk_searchs_stage3[complexity])
	} else {
		lagRange = silk_Lag_range_stage3_10_ms
		lagCB = silk_CB_lags_stage3_10_ms
		nbCbkSearch = peNBCbksStage310ms
	}

	var scratchMem [pitchScratchSize]float32
	targetOff := silkLSHIFT32(int32(sfLength), 2)
	for k := 0; k < nbSubfr; k++ {
		lagCounter := 0
		basisOff := int(targetOff) - (startLag + int(lagRange[k][0]))
		energy := silkEnergyFLP(frame[basisOff:], sfLength) + 1e-3
		scratchMem[lagCounter] = float32(energy)
		lagCounter++
		lagDiff := int(lagRange[k][1]) - int(lagRange[k][0]) + 1
		for i := 1; i < lagDiff; i++ {
			energy -= float64(frame[basisOff+sfLength-i]) * float64(frame[basisOff+sfLength-i])
			energy += float64(frame[basisOff-i]) * float64(frame[basisOff-i])
			scratchMem[lagCounter] = float32(energy)
			lagCounter++
		}

		delta := int(lagRange[k][0])
		for i := 0; i < nbCbkSearch; i++ {
			idx := int(lagCB[k][i]) - delta
			for j := 0; j < peNBStage3Lags; j++ {
				energiesSt3[k][i][j] = scratchMem[idx+j]
			}
		}
		targetOff += int32(sfLength)
	}
}
