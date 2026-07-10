package opus

// SILK encoder analysis chain, ported from libopus silk/float/:
// find_pitch_lags_FLP.c, find_LPC_FLP.c, find_pred_coefs_FLP.c,
// find_LTP_FLP.c, corrMatrix_FLP.c, residual_energy_FLP.c,
// LTP_analysis_filter_FLP.c, LTP_scale_ctrl_FLP.c, process_gains_FLP.c,
// noise_shape_analysis_FLP.c, and the fixed/float wrappers from
// wrappers_FLP.c.

import "math"

const (
	maxFindPitchLPCOrder = 16
	findPitchLPCWinMax   = findPitchLPCWinMS * silkMaxFsKHz
)

// silkA2NLSFFLP converts float AR coefficients to NLSFs (silk_A2NLSF_FLP).
func silkA2NLSFFLP(nlsfQ15 []int16, pAR []float32, lpcOrder int) {
	var aFixQ16 [silkMaxLPCOrder]int32
	for i := 0; i < lpcOrder; i++ {
		aFixQ16[i] = silkFloat2Int(pAR[i] * 65536.0)
	}
	silkA2NLSF(nlsfQ15, aFixQ16[:lpcOrder], lpcOrder)
}

// silkNLSF2AFLP converts NLSFs to float AR coefficients (silk_NLSF2A_FLP).
func silkNLSF2AFLP(pAR []float32, nlsfQ15 []int16, lpcOrder int) {
	var aFixQ12 [silkMaxLPCOrder]int16
	silkNLSF2A(aFixQ12[:], nlsfQ15, lpcOrder)
	for i := 0; i < lpcOrder; i++ {
		pAR[i] = float32(aFixQ12[i]) * (1.0 / 4096.0)
	}
}

// processNLSFsFLP quantizes NLSFs through the fixed-point core and returns
// float prediction coefficients (silk_process_NLSFs_FLP).
func (ch *silkEncoderChannel) processNLSFsFLP(predCoef *[2][silkMaxLPCOrder]float32, nlsfQ15 []int16, prevNLSFQ15 []int16) {
	var predCoefQ12 [2][silkMaxLPCOrder]int16
	ch.processNLSFs(&predCoefQ12, nlsfQ15, prevNLSFQ15)
	for j := 0; j < 2; j++ {
		for i := 0; i < ch.predictLPCOrder; i++ {
			predCoef[j][i] = float32(predCoefQ12[j][i]) * (1.0 / 4096.0)
		}
	}
}

// silkQuantLTPGainsFLP quantizes LTP gains through the fixed-point core
// (silk_quant_LTP_gains_FLP).
func silkQuantLTPGainsFLP(B []float32, cbkIndex []int8, periodicityIndex *int8, sumLogGainQ7 *int32,
	predGainDB *float32, XX, xX []float32, subfrLen, nbSubfr int) {

	var bQ14 [silkMaxNBSubfr * silkLTPOrder]int16
	xxQ17 := make([]int32, nbSubfr*silkLTPOrder*silkLTPOrder)
	xXQ17 := make([]int32, nbSubfr*silkLTPOrder)
	for i := range xxQ17 {
		xxQ17[i] = silkFloat2Int(XX[i] * 131072.0)
	}
	for i := range xXQ17 {
		xXQ17[i] = silkFloat2Int(xX[i] * 131072.0)
	}
	var predGainDBQ7 int32
	silkQuantLTPGains(bQ14[:], cbkIndex, periodicityIndex, sumLogGainQ7, &predGainDBQ7, xxQ17, xXQ17, subfrLen, nbSubfr)
	for i := 0; i < nbSubfr*silkLTPOrder; i++ {
		B[i] = float32(bQ14[i]) * (1.0 / 16384.0)
	}
	*predGainDB = float32(predGainDBQ7) * (1.0 / 128.0)
}

// nsqWrapper converts the float control parameters to fixed point and runs
// the noise shaping quantizer (silk_NSQ_wrapper_FLP).
func (ch *silkEncoderChannel) nsqWrapper(ctrl *silkEncoderControl, psIndices *silkIndices,
	psNSQ *silkNSQState, pulses []int8, x []float32) {

	var x16 [silkMaxFrameLen]int16
	var gainsQ16 [silkMaxNBSubfr]int32
	var predCoefQ12 [2][silkMaxLPCOrder]int16
	var ltpCoefQ14 [silkLTPOrder * silkMaxNBSubfr]int16
	var arQ13 [silkMaxNBSubfr * maxShapeLPCOrder]int16
	var lfShpQ14 [silkMaxNBSubfr]int32
	var tiltQ14 [silkMaxNBSubfr]int32
	var harmShapeGainQ14 [silkMaxNBSubfr]int32

	for i := 0; i < ch.nbSubfr; i++ {
		for j := 0; j < ch.shapingLPCOrder; j++ {
			arQ13[i*maxShapeLPCOrder+j] = int16(silkFloat2Int(ctrl.ar[i*maxShapeLPCOrder+j] * 8192.0))
		}
	}
	for i := 0; i < ch.nbSubfr; i++ {
		lfShpQ14[i] = silkLSHIFT32(silkFloat2Int(ctrl.lfARShp[i]*16384.0), 16) |
			int32(uint16(silkFloat2Int(ctrl.lfMAShp[i]*16384.0)))
		tiltQ14[i] = silkFloat2Int(ctrl.tilt[i] * 16384.0)
		harmShapeGainQ14[i] = silkFloat2Int(ctrl.harmShapeGain[i] * 16384.0)
	}
	lambdaQ10 := silkFloat2Int(ctrl.lambda * 1024.0)

	for i := 0; i < ch.nbSubfr*silkLTPOrder; i++ {
		ltpCoefQ14[i] = int16(silkFloat2Int(ctrl.ltpCoef[i] * 16384.0))
	}
	for j := 0; j < 2; j++ {
		for i := 0; i < ch.predictLPCOrder; i++ {
			predCoefQ12[j][i] = int16(silkFloat2Int(ctrl.predCoef[j][i] * 4096.0))
		}
	}
	for i := 0; i < ch.nbSubfr; i++ {
		gainsQ16[i] = silkFloat2Int(ctrl.gains[i] * 65536.0)
	}
	var ltpScaleQ14 int32
	if psIndices.signalType == typeVoiced {
		ltpScaleQ14 = int32(silk_LTPScales_table_Q14[psIndices.LTPScaleIndex])
	}

	for i := 0; i < ch.frameLength; i++ {
		x16[i] = int16(silkSAT16(silkFloat2Int(x[i])))
	}

	if ch.nStatesDelayedDecision > 1 || ch.warpingQ16 > 0 {
		ch.nsqDelDec(psNSQ, psIndices, x16[:], pulses, &predCoefQ12, ltpCoefQ14[:], arQ13[:],
			harmShapeGainQ14[:], tiltQ14[:], lfShpQ14[:], gainsQ16[:], ctrl.pitchL[:], lambdaQ10, ltpScaleQ14)
	} else {
		ch.nsq(psNSQ, psIndices, x16[:], pulses, &predCoefQ12, ltpCoefQ14[:], arQ13[:],
			harmShapeGainQ14[:], tiltQ14[:], lfShpQ14[:], gainsQ16[:], ctrl.pitchL[:], lambdaQ10, ltpScaleQ14)
	}
}

// findPitchLags runs the whitening filter and pitch estimator
// (silk_find_pitch_lags_FLP). res receives the LPC residual; x is the frame
// positioned at xBuf[ltpMemLength:].
func (ch *silkEncoderChannel) findPitchLags(ctrl *silkEncoderControl, res, xBuf []float32) {
	bufLen := ch.laPitch + ch.frameLength + ch.ltpMemLength

	var wsig [findPitchLPCWinMax]float32
	var autoCorr [maxFindPitchLPCOrder + 1]float32
	var reflCoef, A [maxFindPitchLPCOrder]float32

	// Windowed signal: sine slope, flat middle, cosine slope.
	off := bufLen - ch.pitchLPCWinLength
	silkApplySineWindowFLP(wsig[:], xBuf[off:], 1, ch.laPitch)
	shift := ch.laPitch
	copy(wsig[shift:ch.pitchLPCWinLength-ch.laPitch], xBuf[off+shift:off+ch.pitchLPCWinLength-ch.laPitch])
	shift = ch.pitchLPCWinLength - ch.laPitch
	silkApplySineWindowFLP(wsig[shift:], xBuf[off+shift:], 2, ch.laPitch)

	silkAutocorrelationFLP(autoCorr[:], wsig[:], ch.pitchLPCWinLength, ch.pitchEstimationLPCOrder+1)
	autoCorr[0] += autoCorr[0]*findPitchWhiteNoiseFraction + 1

	resNrg := silkSchurFLP(reflCoef[:], autoCorr[:], ch.pitchEstimationLPCOrder)
	den := resNrg
	if den < 1.0 {
		den = 1.0
	}
	ctrl.predGain = autoCorr[0] / den

	silkK2AFLP(A[:], reflCoef[:], ch.pitchEstimationLPCOrder)
	silkBWExpanderFLP(A[:], ch.pitchEstimationLPCOrder, findPitchBandwidthExpansion)

	silkLPCAnalysisFilterFLP(res, A[:], xBuf, bufLen, ch.pitchEstimationLPCOrder)

	if ch.indices.signalType != typeNoVoiceActivity && !ch.firstFrameAfterReset {
		thrhld := float32(0.6)
		thrhld -= 0.004 * float32(ch.pitchEstimationLPCOrder)
		thrhld -= 0.1 * float32(ch.speechActivityQ8) * (1.0 / 256.0)
		thrhld -= 0.15 * float32(ch.prevSignalType>>1)
		thrhld -= 0.1 * float32(ch.inputTiltQ15) * (1.0 / 32768.0)

		lagIndex := ch.indices.lagIndex
		voiced := !silkPitchAnalysisCore(res, ctrl.pitchL[:], &lagIndex, &ch.indices.contourIndex,
			&ch.LTPCorr, ch.prevLag, float32(ch.pitchEstimationThresholdQ16)/65536.0,
			thrhld, ch.fsKHz, ch.pitchEstimationComplexity, ch.nbSubfr)
		ch.indices.lagIndex = lagIndex
		if voiced {
			ch.indices.signalType = typeVoiced
		} else {
			ch.indices.signalType = typeUnvoiced
		}
	} else {
		for i := range ctrl.pitchL {
			ctrl.pitchL[i] = 0
		}
		ch.indices.lagIndex = 0
		ch.indices.contourIndex = 0
		ch.LTPCorr = 0
	}
}

const silkFloatMax = math.MaxFloat32

// findLPC computes the LPC coefficients and NLSF interpolation decision
// (silk_find_LPC_FLP).
func (ch *silkEncoderChannel) findLPC(nlsfQ15 []int16, x []float32, minInvGain float32) {
	subfrLength := ch.subfrLength + ch.predictLPCOrder

	var a, aTmp [silkMaxLPCOrder]float32
	var nlsf0Q15 [silkMaxLPCOrder]int16
	lpcRes := make([]float32, silkMaxFrameLen+silkMaxNBSubfr*silkMaxLPCOrder)

	ch.indices.NLSFInterpCoefQ2 = 4

	resNrg := silkBurgModifiedFLP(a[:], x, minInvGain, subfrLength, ch.nbSubfr, ch.predictLPCOrder)

	if ch.useInterpolatedNLSFs && !ch.firstFrameAfterReset && ch.nbSubfr == silkMaxNBSubfr {
		// Optimal for the last 10 ms.
		resNrg -= silkBurgModifiedFLP(aTmp[:], x[(silkMaxNBSubfr/2)*subfrLength:], minInvGain, subfrLength, silkMaxNBSubfr/2, ch.predictLPCOrder)
		silkA2NLSFFLP(nlsfQ15, aTmp[:], ch.predictLPCOrder)

		resNrg2nd := float32(silkFloatMax)
		for k := 3; k >= 0; k-- {
			silkInterpolate(nlsf0Q15[:], ch.prevNLSFqQ15[:], nlsfQ15, k, ch.predictLPCOrder)
			silkNLSF2AFLP(aTmp[:], nlsf0Q15[:], ch.predictLPCOrder)
			silkLPCAnalysisFilterFLP(lpcRes, aTmp[:], x, 2*subfrLength, ch.predictLPCOrder)
			resNrgInterp := float32(
				silkEnergyFLP(lpcRes[ch.predictLPCOrder:], subfrLength-ch.predictLPCOrder) +
					silkEnergyFLP(lpcRes[ch.predictLPCOrder+subfrLength:], subfrLength-ch.predictLPCOrder))
			if resNrgInterp < resNrg {
				resNrg = resNrgInterp
				ch.indices.NLSFInterpCoefQ2 = int8(k)
			} else if resNrgInterp > resNrg2nd {
				break
			}
			resNrg2nd = resNrgInterp
		}
	}

	if ch.indices.NLSFInterpCoefQ2 == 4 {
		silkA2NLSFFLP(nlsfQ15, a[:], ch.predictLPCOrder)
	}
}

// silkCorrVectorFLP calculates X'*t (silk_corrVector_FLP). x has L+order-1
// samples ending order-1 before the target.
func silkCorrVectorFLP(x, t []float32, L, order int, Xt []float32) {
	for lag := 0; lag < order; lag++ {
		Xt[lag] = float32(silkInnerProductFLP(x[order-1-lag:], t, L))
	}
}

// silkCorrMatrixFLP calculates X'*X (silk_corrMatrix_FLP).
func silkCorrMatrixFLP(x []float32, L, order int, XX []float32) {
	p1 := order - 1 // first sample of column 0 of X
	energy := silkEnergyFLP(x[p1:], L)
	XX[0] = float32(energy)
	for j := 1; j < order; j++ {
		energy += float64(x[p1-j])*float64(x[p1-j]) - float64(x[p1+L-j])*float64(x[p1+L-j])
		XX[j*order+j] = float32(energy)
	}
	p2 := order - 2 // first sample of column 1 of X
	for lag := 1; lag < order; lag++ {
		energy = silkInnerProductFLP(x[p1:], x[p2:], L)
		XX[lag*order] = float32(energy)
		XX[lag] = float32(energy)
		for j := 1; j < order-lag; j++ {
			energy += float64(x[p1-j])*float64(x[p2-j]) - float64(x[p1+L-j])*float64(x[p2+L-j])
			XX[(lag+j)*order+j] = float32(energy)
			XX[j*order+lag+j] = float32(energy)
		}
		p2--
	}
}

// findLTP computes the LTP correlation weights (silk_find_LTP_FLP). r is the
// pitch residual positioned at the frame start.
func silkFindLTPFLP(XX, xX []float32, r []float32, rOff int, lag []int, subfrLength, nbSubfr int) {
	for k := 0; k < nbSubfr; k++ {
		lagOff := rOff - (lag[k] + silkLTPOrder/2)
		silkCorrMatrixFLP(r[lagOff:], subfrLength, silkLTPOrder, XX[k*silkLTPOrder*silkLTPOrder:])
		silkCorrVectorFLP(r[lagOff:], r[rOff:], subfrLength, silkLTPOrder, xX[k*silkLTPOrder:])
		xx := float32(silkEnergyFLP(r[rOff:], subfrLength+silkLTPOrder))
		den := ltpCorrInvMax*0.5*(XX[k*silkLTPOrder*silkLTPOrder+0]+XX[k*silkLTPOrder*silkLTPOrder+24]) + 1.0
		if xx > den {
			den = xx
		}
		temp := 1.0 / den
		silkScaleVectorFLP(XX[k*silkLTPOrder*silkLTPOrder:], temp, silkLTPOrder*silkLTPOrder)
		silkScaleVectorFLP(xX[k*silkLTPOrder:], temp, silkLTPOrder)
		rOff += subfrLength
	}
}

// silkLTPAnalysisFilterFLP creates the LTP residual (silk_LTP_analysis_filter_FLP).
// x is positioned preLength samples before the first subframe.
func silkLTPAnalysisFilterFLP(ltpRes []float32, x []float32, xOff int, B []float32,
	pitchL []int, invGains []float32, subfrLength, nbSubfr, preLength int) {

	resOff := 0
	for k := 0; k < nbSubfr; k++ {
		lagOff := xOff - pitchL[k]
		invGain := invGains[k]
		var btmp [silkLTPOrder]float32
		copy(btmp[:], B[k*silkLTPOrder:])
		for i := 0; i < subfrLength+preLength; i++ {
			v := x[xOff+i]
			for j := 0; j < silkLTPOrder; j++ {
				v -= btmp[j] * x[lagOff+i+silkLTPOrder/2-j]
			}
			ltpRes[resOff+i] = v * invGain
		}
		resOff += subfrLength + preLength
		xOff += subfrLength
	}
}

// ltpScaleCtrl selects the LTP scaling index (silk_LTP_scale_ctrl_FLP).
func (ch *silkEncoderChannel) ltpScaleCtrl(ctrl *silkEncoderControl, condCoding int) {
	if condCoding == codeIndependently {
		roundLoss := int32(ch.packetLossPerc * ch.nFramesPerPacket)
		if ch.LBRRFlag != 0 {
			roundLoss = 2 + silkSMULBB(roundLoss, roundLoss)/100
		}
		idx := int8(0)
		if silkSMULBB(int32(ctrl.ltpredCodGain), roundLoss) > silkLog2Lin(2900-ch.snrDBQ7) {
			idx++
		}
		if silkSMULBB(int32(ctrl.ltpredCodGain), roundLoss) > silkLog2Lin(3900-ch.snrDBQ7) {
			idx++
		}
		ch.indices.LTPScaleIndex = idx
	} else {
		ch.indices.LTPScaleIndex = 0
	}
	ctrl.ltpScale = float32(silk_LTPScales_table_Q14[ch.indices.LTPScaleIndex]) / 16384.0
}

// findPredCoefs finds the LPC and LTP coefficients (silk_find_pred_coefs_FLP).
// resPitch is the pitch-analysis residual buffer; both it and xBuf are
// positioned at the frame start via their offsets.
func (ch *silkEncoderChannel) findPredCoefs(ctrl *silkEncoderControl, resPitch []float32, resPitchOff int,
	xBuf []float32, xOff int, condCoding int) {

	var invGains [silkMaxNBSubfr]float32
	var nlsfQ15 [silkMaxLPCOrder]int16
	lpcInPre := make([]float32, silkMaxNBSubfr*silkMaxLPCOrder+silkMaxFrameLen)

	for i := 0; i < ch.nbSubfr; i++ {
		invGains[i] = 1.0 / ctrl.gains[i]
	}

	if ch.indices.signalType == typeVoiced {
		var xxLTP [silkMaxNBSubfr * silkLTPOrder * silkLTPOrder]float32
		var xXLTP [silkMaxNBSubfr * silkLTPOrder]float32
		silkFindLTPFLP(xxLTP[:], xXLTP[:], resPitch, resPitchOff, ctrl.pitchL[:], ch.subfrLength, ch.nbSubfr)
		silkQuantLTPGainsFLP(ctrl.ltpCoef[:], ch.indices.LTPIndex[:], &ch.indices.PERIndex,
			&ch.sumLogGainQ7, &ctrl.ltpredCodGain, xxLTP[:], xXLTP[:], ch.subfrLength, ch.nbSubfr)
		ch.ltpScaleCtrl(ctrl, condCoding)
		silkLTPAnalysisFilterFLP(lpcInPre, xBuf, xOff-ch.predictLPCOrder, ctrl.ltpCoef[:],
			ctrl.pitchL[:], invGains[:], ch.subfrLength, ch.nbSubfr, ch.predictLPCOrder)
	} else {
		// Unvoiced: prepended subframes scaled by inverse gains.
		off := xOff - ch.predictLPCOrder
		pre := 0
		for i := 0; i < ch.nbSubfr; i++ {
			silkScaleCopyVectorFLP(lpcInPre[pre:], xBuf[off:], invGains[i], ch.subfrLength+ch.predictLPCOrder)
			pre += ch.subfrLength + ch.predictLPCOrder
			off += ch.subfrLength
		}
		for i := range ctrl.ltpCoef {
			ctrl.ltpCoef[i] = 0
		}
		ctrl.ltpredCodGain = 0
		ch.sumLogGainQ7 = 0
	}

	var minInvGain float32
	if ch.firstFrameAfterReset {
		minInvGain = 1.0 / maxPredictionPowerGainAfterReset
	} else {
		minInvGain = float32(math.Pow(2, float64(ctrl.ltpredCodGain)/3)) / maxPredictionPowerGain
		minInvGain /= 0.25 + 0.75*ctrl.codingQuality
	}

	ch.findLPC(nlsfQ15[:], lpcInPre, minInvGain)

	ch.processNLSFsFLP(&ctrl.predCoef, nlsfQ15[:], ch.prevNLSFqQ15[:])

	silkResidualEnergyFLP(ctrl.resNrg[:], lpcInPre, &ctrl.predCoef, ctrl.gains[:],
		ch.subfrLength, ch.nbSubfr, ch.predictLPCOrder)

	copy(ch.prevNLSFqQ15[:], nlsfQ15[:])
}

const maxPredictionPowerGainAfterReset = 1e2

// silkResidualEnergyFLP measures the residual energy per subframe with the
// quantized coefficients (silk_residual_energy_FLP).
func silkResidualEnergyFLP(nrgs []float32, x []float32, a *[2][silkMaxLPCOrder]float32,
	gains []float32, subfrLength, nbSubfr, lpcOrder int) {

	shift := lpcOrder + subfrLength
	lpcRes := make([]float32, (silkMaxFrameLen+silkMaxNBSubfr*silkMaxLPCOrder)/2)

	silkLPCAnalysisFilterFLP(lpcRes, a[0][:], x, 2*shift, lpcOrder)
	nrgs[0] = float32(float64(gains[0]) * float64(gains[0]) * silkEnergyFLP(lpcRes[lpcOrder:], subfrLength))
	nrgs[1] = float32(float64(gains[1]) * float64(gains[1]) * silkEnergyFLP(lpcRes[lpcOrder+shift:], subfrLength))

	if nbSubfr == silkMaxNBSubfr {
		silkLPCAnalysisFilterFLP(lpcRes, a[1][:], x[2*shift:], 2*shift, lpcOrder)
		nrgs[2] = float32(float64(gains[2]) * float64(gains[2]) * silkEnergyFLP(lpcRes[lpcOrder:], subfrLength))
		nrgs[3] = float32(float64(gains[3]) * float64(gains[3]) * silkEnergyFLP(lpcRes[lpcOrder+shift:], subfrLength))
	}
}

// processGains processes and quantizes the subframe gains
// (silk_process_gains_FLP).
func (ch *silkEncoderChannel) processGains(ctrl *silkEncoderControl, condCoding int) {
	if ch.indices.signalType == typeVoiced {
		s := 1.0 - 0.5*silkSigmoid(0.25*(ctrl.ltpredCodGain-12.0))
		for k := 0; k < ch.nbSubfr; k++ {
			ctrl.gains[k] *= s
		}
	}

	invMaxSqrVal := float32(math.Pow(2.0, 0.33*(21.0-float64(ch.snrDBQ7)*(1/128.0))) / float64(ch.subfrLength))
	for k := 0; k < ch.nbSubfr; k++ {
		gain := ctrl.gains[k]
		gain = float32(math.Sqrt(float64(gain)*float64(gain) + float64(ctrl.resNrg[k])*float64(invMaxSqrVal)))
		if gain > 32767.0 {
			gain = 32767.0
		}
		ctrl.gains[k] = gain
	}

	var pGainsQ16 [silkMaxNBSubfr]int32
	for k := 0; k < ch.nbSubfr; k++ {
		pGainsQ16[k] = silkFloat2Int(ctrl.gains[k] * 65536.0)
	}
	copy(ctrl.gainsUnqQ16[:ch.nbSubfr], pGainsQ16[:ch.nbSubfr])
	ctrl.lastGainIndexPrev = ch.sShape.lastGainIndex

	silkGainsQuant(ch.indices.GainsIndices[:], pGainsQ16[:], &ch.sShape.lastGainIndex,
		condCoding == codeConditionally, ch.nbSubfr)

	for k := 0; k < ch.nbSubfr; k++ {
		ctrl.gains[k] = float32(pGainsQ16[k]) / 65536.0
	}

	if ch.indices.signalType == typeVoiced {
		if ctrl.ltpredCodGain+float32(ch.inputTiltQ15)*(1.0/32768.0) > 1.0 {
			ch.indices.quantOffsetType = 0
		} else {
			ch.indices.quantOffsetType = 1
		}
	}

	quantOffset := float32(silk_Quantization_Offsets_Q10[ch.indices.signalType>>1][ch.indices.quantOffsetType]) / 1024.0
	ctrl.lambda = lambdaOffset +
		lambdaDelayedDecisions*float32(ch.nStatesDelayedDecision) +
		lambdaSpeechAct*float32(ch.speechActivityQ8)*(1.0/256.0) +
		lambdaInputQuality*ctrl.inputQuality +
		lambdaCodingQuality*ctrl.codingQuality +
		lambdaQuantOffset*quantOffset
}

// warpedGain computes the gain making warped coefficients zero-mean in log
// frequency (noise_shape_analysis_FLP.c warped_gain).
func warpedGain(coefs []float32, lambda float32, order int) float32 {
	lambda = -lambda
	gain := coefs[order-1]
	for i := order - 2; i >= 0; i-- {
		gain = lambda*gain + coefs[i]
	}
	return 1.0 / (1.0 - lambda*gain)
}

// warpedTrue2MonicCoefs converts to monic warped coefficients, limiting the
// maximum amplitude by bandwidth expansion (warped_true2monic_coefs).
func warpedTrue2MonicCoefs(coefs []float32, lambda, limit float32, order int) {
	for i := order - 1; i > 0; i-- {
		coefs[i-1] -= lambda * coefs[i]
	}
	gain := (1.0 - lambda*lambda) / (1.0 + lambda*coefs[0])
	for i := 0; i < order; i++ {
		coefs[i] *= gain
	}
	for iter := 0; iter < 10; iter++ {
		maxabs := float32(-1.0)
		ind := 0
		for i := 0; i < order; i++ {
			tmp := coefs[i]
			if tmp < 0 {
				tmp = -tmp
			}
			if tmp > maxabs {
				maxabs = tmp
				ind = i
			}
		}
		if maxabs <= limit {
			return
		}
		for i := 1; i < order; i++ {
			coefs[i-1] += lambda * coefs[i]
		}
		gain = 1.0 / gain
		for i := 0; i < order; i++ {
			coefs[i] *= gain
		}
		chirp := 0.99 - (0.8+0.1*float32(iter))*(maxabs-limit)/(maxabs*float32(ind+1))
		silkBWExpanderFLP(coefs, order, chirp)
		for i := order - 1; i > 0; i-- {
			coefs[i-1] -= lambda * coefs[i]
		}
		gain = (1.0 - lambda*lambda) / (1.0 + lambda*coefs[0])
		for i := 0; i < order; i++ {
			coefs[i] *= gain
		}
	}
}

// limitCoefs bounds coefficient amplitudes by bandwidth expansion (limit_coefs).
func limitCoefs(coefs []float32, limit float32, order int) {
	for iter := 0; iter < 10; iter++ {
		maxabs := float32(-1.0)
		ind := 0
		for i := 0; i < order; i++ {
			tmp := coefs[i]
			if tmp < 0 {
				tmp = -tmp
			}
			if tmp > maxabs {
				maxabs = tmp
				ind = i
			}
		}
		if maxabs <= limit {
			return
		}
		chirp := 0.99 - (0.8+0.1*float32(iter))*(maxabs-limit)/(maxabs*float32(ind+1))
		silkBWExpanderFLP(coefs, order, chirp)
	}
}

// noiseShapeAnalysis computes the noise shaping coefficients and initial
// gains (silk_noise_shape_analysis_FLP). pitchRes is the pitch residual at
// the frame start; xBuf/xOff point at the frame start (la_shape history is
// available before it).
func (ch *silkEncoderChannel) noiseShapeAnalysis(ctrl *silkEncoderControl,
	pitchRes []float32, pitchResOff int, xBuf []float32, xOff int) {

	psShapeSt := &ch.sShape
	var xWindowed [shapeLPCWinMax]float32
	var autoCorr [maxShapeLPCOrder + 1]float32
	var rc [maxShapeLPCOrder + 1]float32

	xPtr := xOff - ch.laShape

	// GAIN CONTROL.
	SNRAdjDB := float32(ch.snrDBQ7) * (1 / 128.0)
	ctrl.inputQuality = 0.5 * float32(ch.inputQualityBandsQ15[0]+ch.inputQualityBandsQ15[1]) * (1.0 / 32768.0)
	ctrl.codingQuality = silkSigmoid(0.25 * (SNRAdjDB - 20.0))

	if !ch.useCBR {
		b := 1.0 - float32(ch.speechActivityQ8)*(1.0/256.0)
		SNRAdjDB -= bgSNRDecrDB * ctrl.codingQuality * (0.5 + 0.5*ctrl.inputQuality) * b * b
	}

	if ch.indices.signalType == typeVoiced {
		SNRAdjDB += harmSNRIncrDB * ch.LTPCorr
	} else {
		SNRAdjDB += (-0.4*float32(ch.snrDBQ7)*(1/128.0) + 6.0) * (1.0 - ctrl.inputQuality)
	}

	// SPARSENESS PROCESSING.
	if ch.indices.signalType == typeVoiced {
		ch.indices.quantOffsetType = 0
	} else {
		nSamples := 2 * ch.fsKHz
		energyVariation := float32(0.0)
		logEnergyPrev := float32(0.0)
		off := pitchResOff
		nSegs := silkSubFrameMS * ch.nbSubfr / 2
		for k := 0; k < nSegs; k++ {
			nrg := float32(nSamples) + float32(silkEnergyFLP(pitchRes[off:], nSamples))
			logEnergy := silkLog2F(float64(nrg))
			if k > 0 {
				d := logEnergy - logEnergyPrev
				if d < 0 {
					d = -d
				}
				energyVariation += d
			}
			logEnergyPrev = logEnergy
			off += nSamples
		}
		if energyVariation > energyVariationThresholdQntOffset*float32(nSegs-1) {
			ch.indices.quantOffsetType = 0
		} else {
			ch.indices.quantOffsetType = 1
		}
	}

	// Bandwidth expansion control.
	strength := float32(findPitchWhiteNoiseFraction) * ctrl.predGain
	BWExp := float32(bandwidthExpansion) / (1.0 + strength*strength)

	warping := float32(ch.warpingQ16)/65536.0 + 0.01*ctrl.codingQuality

	// Compute noise shaping AR coefficients and gains.
	for k := 0; k < ch.nbSubfr; k++ {
		flatPart := ch.fsKHz * 3
		slopePart := (ch.shapeWinLength - flatPart) / 2
		silkApplySineWindowFLP(xWindowed[:], xBuf[xPtr:], 1, slopePart)
		shift := slopePart
		copy(xWindowed[shift:shift+flatPart], xBuf[xPtr+shift:])
		shift += flatPart
		silkApplySineWindowFLP(xWindowed[shift:], xBuf[xPtr+shift:], 2, slopePart)

		xPtr += ch.subfrLength

		if ch.warpingQ16 > 0 {
			silkWarpedAutocorrelationFLP(autoCorr[:], xWindowed[:], warping, ch.shapeWinLength, ch.shapingLPCOrder)
		} else {
			silkAutocorrelationFLP(autoCorr[:], xWindowed[:], ch.shapeWinLength, ch.shapingLPCOrder+1)
		}

		autoCorr[0] += autoCorr[0]*shapeWhiteNoiseFraction + 1.0

		nrg := silkSchurFLP(rc[:], autoCorr[:], ch.shapingLPCOrder)
		silkK2AFLP(ctrl.ar[k*maxShapeLPCOrder:], rc[:], ch.shapingLPCOrder)
		ctrl.gains[k] = float32(math.Sqrt(float64(nrg)))

		if ch.warpingQ16 > 0 {
			ctrl.gains[k] *= warpedGain(ctrl.ar[k*maxShapeLPCOrder:], warping, ch.shapingLPCOrder)
		}

		silkBWExpanderFLP(ctrl.ar[k*maxShapeLPCOrder:], ch.shapingLPCOrder, BWExp)

		if ch.warpingQ16 > 0 {
			warpedTrue2MonicCoefs(ctrl.ar[k*maxShapeLPCOrder:], warping, 3.999, ch.shapingLPCOrder)
		} else {
			limitCoefs(ctrl.ar[k*maxShapeLPCOrder:], 3.999, ch.shapingLPCOrder)
		}
	}

	// Gain tweaking.
	gainMult := float32(math.Pow(2.0, -0.16*float64(SNRAdjDB)))
	gainAdd := float32(math.Pow(2.0, 0.16*minQGainDB))
	for k := 0; k < ch.nbSubfr; k++ {
		ctrl.gains[k] *= gainMult
		ctrl.gains[k] += gainAdd
	}

	// Low-frequency shaping and noise tilt.
	strength = lowFreqShaping * (1.0 + lowQualityLowFreqShapingDecr*(float32(ch.inputQualityBandsQ15[0])*(1.0/32768.0)-1.0))
	strength *= float32(ch.speechActivityQ8) * (1.0 / 256.0)
	var tilt float32
	if ch.indices.signalType == typeVoiced {
		for k := 0; k < ch.nbSubfr; k++ {
			b := 0.2/float32(ch.fsKHz) + 3.0/float32(ctrl.pitchL[k])
			ctrl.lfMAShp[k] = -1.0 + b
			ctrl.lfARShp[k] = 1.0 - b - b*strength
		}
		tilt = -hpNoiseCoef - (1-hpNoiseCoef)*harmHPNoiseCoef*float32(ch.speechActivityQ8)*(1.0/256.0)
	} else {
		b := 1.3 / float32(ch.fsKHz)
		ctrl.lfMAShp[0] = -1.0 + b
		ctrl.lfARShp[0] = 1.0 - b - b*strength*0.6
		for k := 1; k < ch.nbSubfr; k++ {
			ctrl.lfMAShp[k] = ctrl.lfMAShp[0]
			ctrl.lfARShp[k] = ctrl.lfARShp[0]
		}
		tilt = -hpNoiseCoef
	}

	// Harmonic shaping control.
	var harmShapeGain float32
	if ch.indices.signalType == typeVoiced {
		harmShapeGain = harmonicShaping
		harmShapeGain += highRateOrLowQualityHarmonicShaping * (1.0 - (1.0-ctrl.codingQuality)*ctrl.inputQuality)
		harmShapeGain *= float32(math.Sqrt(float64(ch.LTPCorr)))
	}

	// Smooth over subframes.
	for k := 0; k < ch.nbSubfr; k++ {
		psShapeSt.harmShapeGainSmth += subfrSmthCoef * (harmShapeGain - psShapeSt.harmShapeGainSmth)
		ctrl.harmShapeGain[k] = psShapeSt.harmShapeGainSmth
		psShapeSt.tiltSmth += subfrSmthCoef * (tilt - psShapeSt.tiltSmth)
		ctrl.tilt[k] = psShapeSt.tiltSmth
	}
}
