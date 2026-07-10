package opus

// SILK encoder support DSP, ported from libopus silk/VAD.c, sigm_Q15.c,
// ana_filt_bank_1.c, biquad_alt.c, LP_variable_cutoff.c, and
// HP_variable_cutoff.c, plus the silk/tuning_parameters.h constants the
// encoder analysis chain consumes.

// Encoder tuning parameters (silk/tuning_parameters.h). The float-valued ones
// feed the FLP analysis chain directly.
const (
	bitreservoirDecayTimeMS = 500

	findPitchWhiteNoiseFraction = 1e-3
	findPitchBandwidthExpansion = 0.99

	findLPCCondFac = 1e-5

	maxSumLogGainDB = 250.0
	ltpCorrInvMax   = 0.03

	variableHPSmthCoef1    = 0.1
	variableHPSmthCoef2    = 0.015
	variableHPMaxDeltaFreq = 0.4
	variableHPMinCutoffHz  = 60
	variableHPMaxCutoffHz  = 100

	speechActivityDTXThres  = 0.05
	lbrrSpeechActivityThres = 0.3

	bgSNRDecrDB     = 2.0
	harmSNRIncrDB   = 2.0
	sparseSNRIncrDB = 2.0

	energyVariationThresholdQntOffset = 0.6

	warpingMultiplier                   = 0.015
	shapeWhiteNoiseFraction             = 3e-5
	bandwidthExpansion                  = 0.94
	harmonicShaping                     = 0.3
	highRateOrLowQualityHarmonicShaping = 0.2
	hpNoiseCoef                         = 0.25
	harmHPNoiseCoef                     = 0.35
	inputTilt                           = 0.05
	highRateInputTilt                   = 0.1
	lowFreqShaping                      = 4.0
	lowQualityLowFreqShapingDecr        = 0.5
	subfrSmthCoef                       = 0.4

	lambdaOffset           = 1.2
	lambdaSpeechAct        = -0.2
	lambdaDelayedDecisions = -0.05
	lambdaInputQuality     = -0.1
	lambdaCodingQuality    = -0.2
	lambdaQuantOffset      = 0.8

	reduceBitrate10MSBPS      = 2200
	maxBandwidthSwitchDelayMS = 5000
)

// VAD constants (silk/define.h).
const (
	vadNBands              = 4
	vadInternalSubfrsLog2  = 2
	vadInternalSubfrs      = 1 << vadInternalSubfrsLog2
	vadNoiseLevelSmoothQ16 = 1024
	vadNoiseLevelsBias     = 50
	vadNegativeOffsetQ5    = 128
	vadSNRFactorQ16        = 45000
	vadSNRSmoothCoefQ18    = 4096

	nbSpeechFramesBeforeDTX = 10
	maxConsecutiveDTX       = 20
)

// silk_ADD_POS_SAT32: saturating add for positive operands.
func silkADDPOSSAT32(a, b int32) int32 {
	if (uint32(a)+uint32(b))&0x80000000 != 0 {
		return silkInt32Max
	}
	return a + b
}

// sigmoid lookup tables (silk/sigm_Q15.c).
var sigmLUTSlopeQ10 = [6]int32{237, 153, 73, 30, 12, 7}
var sigmLUTPosQ15 = [6]int32{16384, 23955, 28861, 31213, 32178, 32548}
var sigmLUTNegQ15 = [6]int32{16384, 8812, 3906, 1554, 589, 219}

// silkSigmQ15 approximates 32767/(1+exp(-inQ5/32)) (silk_sigm_Q15).
func silkSigmQ15(inQ5 int32) int32 {
	if inQ5 < 0 {
		inQ5 = -inQ5
		if inQ5 >= 6*32 {
			return 0
		}
		ind := inQ5 >> 5
		return sigmLUTNegQ15[ind] - silkSMULBB(sigmLUTSlopeQ10[ind], inQ5&0x1F)
	}
	if inQ5 >= 6*32 {
		return 32767
	}
	ind := inQ5 >> 5
	return sigmLUTPosQ15[ind] + silkSMULBB(sigmLUTSlopeQ10[ind], inQ5&0x1F)
}

// First-order allpass coefficients for the 2-band split (silk/ana_filt_bank_1.c).
const (
	aFB120 = 5394 << 1
	aFB121 = -24290
)

// silkAnaFiltBank1 splits in into two decimated bands using first-order
// allpass sections (silk_ana_filt_bank_1). State and internals are Q10.
func silkAnaFiltBank1(in []int16, S []int32, outL, outH []int16, N int) {
	N2 := N >> 1
	for k := 0; k < N2; k++ {
		in32 := silkLSHIFT(int32(in[2*k]), 10)
		Y := in32 - S[0]
		X := silkSMLAWB(Y, Y, aFB121)
		out1 := S[0] + X
		S[0] = in32 + X

		in32 = silkLSHIFT(int32(in[2*k+1]), 10)
		Y = in32 - S[1]
		X = silkSMULWB(Y, aFB120)
		out2 := S[1] + X
		S[1] = in32 + X

		outL[k] = int16(silkSAT16(silkRSHIFTROUND(out2+out1, 11)))
		outH[k] = int16(silkSAT16(silkRSHIFTROUND(out2-out1, 11)))
	}
}

// silkVADState is the SILK VAD state (silk_VAD_state).
type silkVADState struct {
	AnaState       [2]int32
	AnaState1      [2]int32
	AnaState2      [2]int32
	XnrgSubfr      [vadNBands]int32
	NrgRatioSmthQ8 [vadNBands]int32
	HPstate        int16
	NL             [vadNBands]int32
	invNL          [vadNBands]int32
	NoiseLevelBias [vadNBands]int32
	counter        int32
}

// init resets the VAD state (silk_VAD_Init).
func (v *silkVADState) init() {
	*v = silkVADState{}
	for b := 0; b < vadNBands; b++ {
		bias := silkDIV32_16(vadNoiseLevelsBias, int32(b+1))
		if bias < 1 {
			bias = 1
		}
		v.NoiseLevelBias[b] = bias
		v.NL[b] = 100 * v.NoiseLevelBias[b]
		v.invNL[b] = silkDIV32(silkInt32Max, v.NL[b])
		v.NrgRatioSmthQ8[b] = 100 * 256
	}
	v.counter = 15
}

// tiltWeights are the per-band weights of the tilt measure (silk/VAD.c).
var vadTiltWeights = [vadNBands]int32{30000, 6000, -12000, -12000}

// vadResult carries silk_VAD_GetSA_Q8's outputs into the encoder state.
type vadResult struct {
	speechActivityQ8     int32
	inputTiltQ15         int32
	inputQualityBandsQ15 [vadNBands]int32
}

// getSAQ8 measures the speech activity level of one frame
// (silk_VAD_GetSA_Q8_c). pIn holds frameLength samples at fsKHz.
func (v *silkVADState) getSAQ8(pIn []int16, frameLength, fsKHz int) vadResult {
	var res vadResult
	decimatedFramelength1 := frameLength >> 1
	decimatedFramelength2 := frameLength >> 2
	decimatedFramelength := frameLength >> 3

	var xOffset [vadNBands]int
	xOffset[0] = 0
	xOffset[1] = decimatedFramelength + decimatedFramelength2
	xOffset[2] = xOffset[1] + decimatedFramelength
	xOffset[3] = xOffset[2] + decimatedFramelength2
	X := make([]int16, xOffset[3]+decimatedFramelength1)

	// Split 0-8 kHz into 0-4/4-8, then 0-4 into 0-2/2-4, then 0-2 into 0-1/1-2.
	silkAnaFiltBank1(pIn, v.AnaState[:], X, X[xOffset[3]:], frameLength)
	silkAnaFiltBank1(X, v.AnaState1[:], X, X[xOffset[2]:], decimatedFramelength1)
	silkAnaFiltBank1(X, v.AnaState2[:], X, X[xOffset[1]:], decimatedFramelength2)

	// HP filter on the lowest band (differentiator).
	X[decimatedFramelength-1] >>= 1
	HPstateTmp := X[decimatedFramelength-1]
	for i := decimatedFramelength - 1; i > 0; i-- {
		X[i-1] >>= 1
		X[i] -= X[i-1]
	}
	X[0] -= v.HPstate
	v.HPstate = HPstateTmp

	// Energy in each band.
	var Xnrg [vadNBands]int32
	for b := 0; b < vadNBands; b++ {
		m := vadNBands - b
		if m > vadNBands-1 {
			m = vadNBands - 1
		}
		decLen := frameLength >> uint(m)
		decSubframeLength := decLen >> vadInternalSubfrsLog2
		decSubframeOffset := 0
		Xnrg[b] = v.XnrgSubfr[b]
		var sumSquared int32
		for s := 0; s < vadInternalSubfrs; s++ {
			sumSquared = 0
			for i := 0; i < decSubframeLength; i++ {
				xTmp := int32(X[xOffset[b]+i+decSubframeOffset]) >> 3
				sumSquared = silkSMLABB(sumSquared, xTmp, xTmp)
			}
			if s < vadInternalSubfrs-1 {
				Xnrg[b] = silkADDPOSSAT32(Xnrg[b], sumSquared)
			} else {
				// Look-ahead subframe.
				Xnrg[b] = silkADDPOSSAT32(Xnrg[b], sumSquared>>1)
			}
			decSubframeOffset += decSubframeLength
		}
		v.XnrgSubfr[b] = sumSquared
	}

	v.getNoiseLevels(Xnrg[:])

	// Signal-plus-noise to noise ratio estimation.
	var sumSquared, inputTilt int32
	var nrgToNoiseRatioQ8 [vadNBands]int32
	for b := 0; b < vadNBands; b++ {
		speechNrg := Xnrg[b] - v.NL[b]
		if speechNrg > 0 {
			if Xnrg[b]&int32(-8388608) == 0 { // 0xFF800000
				nrgToNoiseRatioQ8[b] = silkDIV32(silkLSHIFT(Xnrg[b], 8), v.NL[b]+1)
			} else {
				nrgToNoiseRatioQ8[b] = silkDIV32(Xnrg[b], (v.NL[b]>>8)+1)
			}
			SNRQ7 := silkLin2Log(nrgToNoiseRatioQ8[b]) - 8*128
			sumSquared = silkSMLABB(sumSquared, SNRQ7, SNRQ7)
			if speechNrg < 1<<20 {
				SNRQ7 = silkSMULWB(silkLSHIFT(silkSQRTAPPROX(speechNrg), 6), SNRQ7)
			}
			inputTilt = silkSMLAWB(inputTilt, vadTiltWeights[b], SNRQ7)
		} else {
			nrgToNoiseRatioQ8[b] = 256
		}
	}

	sumSquared = silkDIV32_16(sumSquared, vadNBands)
	pSNRdBQ7 := 3 * silkSQRTAPPROX(sumSquared)

	SAQ15 := silkSigmQ15(silkSMULWB(vadSNRFactorQ16, pSNRdBQ7) - vadNegativeOffsetQ5)
	res.inputTiltQ15 = silkLSHIFT(silkSigmQ15(inputTilt)-16384, 1)

	// Scale the sigmoid output based on power levels.
	var speechNrg int32
	for b := 0; b < vadNBands; b++ {
		speechNrg += int32(b+1) * ((Xnrg[b] - v.NL[b]) >> 4)
	}
	if frameLength == 20*fsKHz {
		speechNrg >>= 1
	}
	if speechNrg <= 0 {
		SAQ15 >>= 1
	} else if speechNrg < 16384 {
		speechNrg = silkLSHIFT32(speechNrg, 16)
		speechNrg = silkSQRTAPPROX(speechNrg)
		SAQ15 = silkSMULWB(32768+speechNrg, SAQ15)
	}

	res.speechActivityQ8 = silkMinInt(SAQ15>>7, 255)

	// Energy level and SNR estimation per band.
	smoothCoefQ16 := silkSMULWB(vadSNRSmoothCoefQ18, silkSMULWB(SAQ15, SAQ15))
	if frameLength == 10*fsKHz {
		smoothCoefQ16 >>= 1
	}
	for b := 0; b < vadNBands; b++ {
		v.NrgRatioSmthQ8[b] = silkSMLAWB(v.NrgRatioSmthQ8[b],
			nrgToNoiseRatioQ8[b]-v.NrgRatioSmthQ8[b], smoothCoefQ16)
		SNRQ7 := 3 * (silkLin2Log(v.NrgRatioSmthQ8[b]) - 8*128)
		res.inputQualityBandsQ15[b] = silkSigmQ15((SNRQ7 - 16*128) >> 4)
	}
	return res
}

// getNoiseLevels updates the per-band noise level estimate
// (silk_VAD_GetNoiseLevels).
func (v *silkVADState) getNoiseLevels(pX []int32) {
	var minCoef int32
	if v.counter < 1000 {
		minCoef = silkDIV32_16(silkInt16Max, (v.counter>>4)+1)
		v.counter++
	}
	for k := 0; k < vadNBands; k++ {
		nl := v.NL[k]
		nrg := silkADDPOSSAT32(pX[k], v.NoiseLevelBias[k])
		invNrg := silkDIV32(silkInt32Max, nrg)
		var coef int32
		if nrg > silkLSHIFT(nl, 3) {
			coef = vadNoiseLevelSmoothQ16 >> 3
		} else if nrg < nl {
			coef = vadNoiseLevelSmoothQ16
		} else {
			coef = silkSMULWB(silkSMULWW(invNrg, nl), vadNoiseLevelSmoothQ16<<1)
		}
		coef = silkMaxInt(coef, minCoef)
		v.invNL[k] = silkSMLAWB(v.invNL[k], invNrg-v.invNL[k], coef)
		nl = silkDIV32(silkInt32Max, v.invNL[k])
		nl = silkMinInt(nl, 0x00FFFFFF)
		v.NL[k] = nl
	}
}

// silkBiquadAltStride1 is the second-order ARMA filter in transposed direct
// form II (silk_biquad_alt_stride1). S is the 2-element Q12 state.
func silkBiquadAltStride1(in []int16, bQ28 []int32, aQ28 []int32, S []int32, out []int16, length int) {
	a0LQ28 := (-aQ28[0]) & 0x00003FFF
	a0UQ28 := silkRSHIFT(-aQ28[0], 14)
	a1LQ28 := (-aQ28[1]) & 0x00003FFF
	a1UQ28 := silkRSHIFT(-aQ28[1], 14)
	for k := 0; k < length; k++ {
		inval := int32(in[k])
		out32Q14 := silkLSHIFT(silkSMLAWB(S[0], bQ28[0], inval), 2)

		S[0] = S[1] + silkRSHIFTROUND(silkSMULWB(out32Q14, a0LQ28), 14)
		S[0] = silkSMLAWB(S[0], out32Q14, a0UQ28)
		S[0] = silkSMLAWB(S[0], bQ28[1], inval)

		S[1] = silkRSHIFTROUND(silkSMULWB(out32Q14, a1LQ28), 14)
		S[1] = silkSMLAWB(S[1], out32Q14, a1UQ28)
		S[1] = silkSMLAWB(S[1], bQ28[2], inval)

		out[k] = int16(silkSAT16(silkRSHIFT(out32Q14+(1<<14)-1, 14)))
	}
}

// Bandwidth-transition filter constants (silk/define.h).
const (
	transitionTimeMS   = 5120
	transitionNB       = 3
	transitionNA       = 2
	transitionIntNum   = 5
	transitionFrames   = transitionTimeMS / 20 // MAX_FRAME_LENGTH_MS
	transitionIntSteps = transitionFrames / (transitionIntNum - 1)
)

// silkLPState is the low-pass transition filter state (silk_LP_state).
type silkLPState struct {
	inLPState         [2]int32
	transitionFrameNo int32
	mode              int32
}

// interpolateFilterTaps interpolates between the transition filter tables
// (silk_LP_interpolate_filter_taps).
func silkLPInterpolateFilterTaps(bQ28 []int32, aQ28 []int32, ind int, facQ16 int32) {
	if ind < transitionIntNum-1 {
		if facQ16 > 0 {
			if facQ16 < 32768 {
				for nb := 0; nb < transitionNB; nb++ {
					bQ28[nb] = silkSMLAWB(silk_Transition_LP_B_Q28[ind][nb],
						silk_Transition_LP_B_Q28[ind+1][nb]-silk_Transition_LP_B_Q28[ind][nb], facQ16)
				}
				for na := 0; na < transitionNA; na++ {
					aQ28[na] = silkSMLAWB(silk_Transition_LP_A_Q28[ind][na],
						silk_Transition_LP_A_Q28[ind+1][na]-silk_Transition_LP_A_Q28[ind][na], facQ16)
				}
			} else {
				for nb := 0; nb < transitionNB; nb++ {
					bQ28[nb] = silkSMLAWB(silk_Transition_LP_B_Q28[ind+1][nb],
						silk_Transition_LP_B_Q28[ind+1][nb]-silk_Transition_LP_B_Q28[ind][nb], facQ16-(1<<16))
				}
				for na := 0; na < transitionNA; na++ {
					aQ28[na] = silkSMLAWB(silk_Transition_LP_A_Q28[ind+1][na],
						silk_Transition_LP_A_Q28[ind+1][na]-silk_Transition_LP_A_Q28[ind][na], facQ16-(1<<16))
				}
			}
		} else {
			copy(bQ28, silk_Transition_LP_B_Q28[ind])
			copy(aQ28, silk_Transition_LP_A_Q28[ind])
		}
	} else {
		copy(bQ28, silk_Transition_LP_B_Q28[transitionIntNum-1])
		copy(aQ28, silk_Transition_LP_A_Q28[transitionIntNum-1])
	}
}

// variableCutoff low-pass filters the frame in place during bandwidth
// transitions (silk_LP_variable_cutoff). Inactive while mode == 0.
func (lp *silkLPState) variableCutoff(frame []int16, frameLength int) {
	if lp.mode == 0 {
		return
	}
	facQ16 := silkLSHIFT(transitionFrames-lp.transitionFrameNo, 16-6)
	ind := int(facQ16 >> 16)
	facQ16 -= silkLSHIFT(int32(ind), 16)

	var bQ28 [transitionNB]int32
	var aQ28 [transitionNA]int32
	silkLPInterpolateFilterTaps(bQ28[:], aQ28[:], ind, facQ16)

	lp.transitionFrameNo = silkLIMIT(lp.transitionFrameNo+lp.mode, 0, transitionFrames)

	silkBiquadAltStride1(frame, bQ28[:], aQ28[:], lp.inLPState[:], frame, frameLength)
}

// silkHPVariableCutoff adapts the high-pass cutoff frequency from pitch lag
// statistics (silk_HP_variable_cutoff) and returns the updated
// variable_HP_smth1_Q15 smoother state.
func silkHPVariableCutoff(smth1Q15 int32, prevSignalType int, prevLag, fsKHz int, qualityBand0Q15, speechActivityQ8 int32) int32 {
	if prevSignalType != typeVoiced {
		return smth1Q15
	}
	pitchFreqHzQ16 := silkDIV32_16(silkLSHIFT(int32(fsKHz*1000), 16), int32(prevLag))
	pitchFreqLogQ7 := silkLin2Log(pitchFreqHzQ16) - 16<<7

	qualityQ15 := qualityBand0Q15
	pitchFreqLogQ7 = silkSMLAWB(pitchFreqLogQ7, silkSMULWB(silkLSHIFT(-qualityQ15, 2), qualityQ15),
		pitchFreqLogQ7-(silkLin2Log(silkFixConst(variableHPMinCutoffHz, 16))-16<<7))

	deltaFreqQ7 := pitchFreqLogQ7 - silkRSHIFT(smth1Q15, 8)
	if deltaFreqQ7 < 0 {
		deltaFreqQ7 *= 3
	}
	deltaFreqQ7 = silkLIMIT(deltaFreqQ7, -silkFixConst(variableHPMaxDeltaFreq, 7), silkFixConst(variableHPMaxDeltaFreq, 7))

	smth1Q15 = silkSMLAWB(smth1Q15, silkSMULBB(speechActivityQ8, deltaFreqQ7), silkFixConst(variableHPSmthCoef1, 16))
	smth1Q15 = silkLIMIT(smth1Q15,
		silkLSHIFT(silkLin2Log(variableHPMinCutoffHz), 8),
		silkLSHIFT(silkLin2Log(variableHPMaxCutoffHz), 8))
	return smth1Q15
}
