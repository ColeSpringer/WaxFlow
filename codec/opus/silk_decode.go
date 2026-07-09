package opus

// SILK per-frame decode: indices, excitation, parameters, and LTP+LPC
// synthesis, plus stereo unmixing and the packet driver. Ported from libopus
// as noted per function.

// decodeIndices reads one SILK frame's quantization indices (silk_decode_indices).
func (cs *silkChannelState) decodeIndices(dec *rangeDecoder, frameIndex int, decodeLBRR bool, condCoding int) {
	ix := &cs.indices
	var Ix int
	if decodeLBRR || cs.VADFlags[frameIndex] != 0 {
		Ix = dec.decodeICDF(silk_type_offset_VAD_iCDF, 8) + 2
	} else {
		Ix = dec.decodeICDF(silk_type_offset_no_VAD_iCDF, 8)
	}
	ix.signalType = int8(Ix >> 1)
	ix.quantOffsetType = int8(Ix & 1)

	// Gains.
	if condCoding == codeConditionally {
		ix.GainsIndices[0] = int8(dec.decodeICDF(silk_delta_gain_iCDF, 8))
	} else {
		ix.GainsIndices[0] = int8(dec.decodeICDF(silk_gain_iCDF[ix.signalType], 8) << 3)
		ix.GainsIndices[0] += int8(dec.decodeICDF(silk_uniform8_iCDF, 8))
	}
	for i := 1; i < cs.nbSubfr; i++ {
		ix.GainsIndices[i] = int8(dec.decodeICDF(silk_delta_gain_iCDF, 8))
	}

	// NLSF indices.
	cb := cs.psNLSFCB
	ix.NLSFIndices[0] = int8(dec.decodeICDF(cb.cb1ICDF[int(ix.signalType>>1)*cb.nVectors:], 8))
	var ecIx [silkMaxLPCOrder]int16
	var predQ8 [silkMaxLPCOrder]uint8
	silkNLSFUnpack(ecIx[:], predQ8[:], cb, int(ix.NLSFIndices[0]))
	for i := 0; i < cb.order; i++ {
		v := dec.decodeICDF(cb.ecICDF[ecIx[i]:], 8)
		if v == 0 {
			v -= dec.decodeICDF(silk_NLSF_EXT_iCDF, 8)
		} else if v == 2*nlsfQuantMaxAmp {
			v += dec.decodeICDF(silk_NLSF_EXT_iCDF, 8)
		}
		ix.NLSFIndices[i+1] = int8(v - nlsfQuantMaxAmp)
	}

	if cs.nbSubfr == silkMaxNBSubfr {
		ix.NLSFInterpCoefQ2 = int8(dec.decodeICDF(silk_NLSF_interpolation_factor_iCDF, 8))
	} else {
		ix.NLSFInterpCoefQ2 = 4
	}

	if ix.signalType == typeVoiced {
		decodeAbsolute := true
		if condCoding == codeConditionally && cs.ecPrevSignalType == typeVoiced {
			delta := dec.decodeICDF(silk_pitch_delta_iCDF, 8)
			if delta > 0 {
				delta -= 9
				ix.lagIndex = cs.ecPrevLagIndex + delta
				decodeAbsolute = false
			}
		}
		if decodeAbsolute {
			ix.lagIndex = dec.decodeICDF(silk_pitch_lag_iCDF, 8) * (cs.fsKHz >> 1)
			ix.lagIndex += dec.decodeICDF(cs.pitchLagLowBitsICDF, 8)
		}
		cs.ecPrevLagIndex = ix.lagIndex

		ix.contourIndex = int8(dec.decodeICDF(cs.pitchContourICDF, 8))
		ix.PERIndex = int8(dec.decodeICDF(silk_LTP_per_index_iCDF, 8))
		for k := 0; k < cs.nbSubfr; k++ {
			ix.LTPIndex[k] = int8(dec.decodeICDF(silkLTPGainICDFPtrs[ix.PERIndex], 8))
		}
		if condCoding == codeIndependently {
			ix.LTPScaleIndex = int8(dec.decodeICDF(silk_LTPscale_iCDF, 8))
		} else {
			ix.LTPScaleIndex = 0
		}
	}
	cs.ecPrevSignalType = int(ix.signalType)
	ix.Seed = int8(dec.decodeICDF(silk_uniform4_iCDF, 8))
}

// decodeParameters converts indices to parameters (silk_decode_parameters).
func (cs *silkChannelState) decodeParameters(ctrl *silkDecoderControl, condCoding int) {
	ix := &cs.indices
	silkGainsDequant(ctrl.GainsQ16[:], ix.GainsIndices[:], &cs.LastGainIndex, condCoding == codeConditionally, cs.nbSubfr)

	var nlsfQ15, nlsf0Q15 [silkMaxLPCOrder]int16
	silkNLSFDecode(nlsfQ15[:], ix.NLSFIndices[:], cs.psNLSFCB)
	silkNLSF2A(ctrl.PredCoefQ12[1][:], nlsfQ15[:], cs.LPCOrder)

	if cs.firstFrameAfterReset {
		ix.NLSFInterpCoefQ2 = 4
	}
	if ix.NLSFInterpCoefQ2 < 4 {
		for i := 0; i < cs.LPCOrder; i++ {
			nlsf0Q15[i] = cs.prevNLSFQ15[i] + int16(silkRSHIFT(int32(ix.NLSFInterpCoefQ2)*(int32(nlsfQ15[i])-int32(cs.prevNLSFQ15[i])), 2))
		}
		silkNLSF2A(ctrl.PredCoefQ12[0][:], nlsf0Q15[:], cs.LPCOrder)
	} else {
		copy(ctrl.PredCoefQ12[0][:cs.LPCOrder], ctrl.PredCoefQ12[1][:cs.LPCOrder])
	}
	copy(cs.prevNLSFQ15[:cs.LPCOrder], nlsfQ15[:cs.LPCOrder])

	if ix.signalType == typeVoiced {
		silkDecodePitch(ix.lagIndex, ix.contourIndex, ctrl.pitchL[:], cs.fsKHz, cs.nbSubfr)
		cbk := silkLTPVQPtrsQ7[ix.PERIndex]
		for k := 0; k < cs.nbSubfr; k++ {
			idx := ix.LTPIndex[k]
			for i := 0; i < silkLTPOrder; i++ {
				ctrl.LTPCoefQ14[k*silkLTPOrder+i] = int16(silkLSHIFT(int32(cbk[idx][i]), 7))
			}
		}
		ctrl.LTPScaleQ14 = int32(silk_LTPScales_table_Q14[ix.LTPScaleIndex])
	} else {
		for i := range ctrl.pitchL {
			ctrl.pitchL[i] = 0
		}
		for i := range ctrl.LTPCoefQ14 {
			ctrl.LTPCoefQ14[i] = 0
		}
		ix.PERIndex = 0
		ctrl.LTPScaleQ14 = 0
	}
}

// decodePulses reads the excitation signal (silk_decode_pulses).
func silkDecodePulses(dec *rangeDecoder, pulses []int16, signalType, quantOffsetType int8, frameLength int) {
	var sumPulses, nLshifts [maxNBShellBlocks]int
	rateLevel := dec.decodeICDF(silk_rate_levels_iCDF[signalType>>1], 8)
	iter := frameLength >> log2ShellFrame
	if iter*shellFrameLen < frameLength {
		iter++ // only for 10 ms @ 12 kHz
	}
	cdf := silk_pulses_per_block_iCDF[rateLevel]
	for i := 0; i < iter; i++ {
		nLshifts[i] = 0
		sumPulses[i] = dec.decodeICDF(cdf, 8)
		for sumPulses[i] == silkMaxPulses+1 {
			nLshifts[i]++
			row := silk_pulses_per_block_iCDF[nRateLevels-1]
			if nLshifts[i] == 10 {
				row = row[1:]
			}
			sumPulses[i] = dec.decodeICDF(row, 8)
		}
	}
	for i := 0; i < iter; i++ {
		if sumPulses[i] > 0 {
			shellDecoder(pulses[i*shellFrameLen:], dec, sumPulses[i])
		} else {
			for k := 0; k < shellFrameLen; k++ {
				pulses[i*shellFrameLen+k] = 0
			}
		}
	}
	for i := 0; i < iter; i++ {
		if nLshifts[i] > 0 {
			nLS := nLshifts[i]
			p := pulses[i*shellFrameLen:]
			for k := 0; k < shellFrameLen; k++ {
				absQ := int(p[k])
				for j := 0; j < nLS; j++ {
					absQ <<= 1
					absQ += dec.decodeICDF(silk_lsb_iCDF, 8)
				}
				p[k] = int16(absQ)
			}
			sumPulses[i] |= nLS << 5
		}
	}
	decodeSigns(dec, pulses, frameLength, signalType, quantOffsetType, sumPulses[:])
}

// shellDecoder splits a pulse count down the shell tree (silk_shell_decoder).
func shellDecoder(pulses0 []int16, dec *rangeDecoder, pulses4 int) {
	var pulses3 [2]int16
	var pulses2 [4]int16
	var pulses1 [8]int16
	decodeSplit(&pulses3[0], &pulses3[1], dec, pulses4, silk_shell_code_table3)
	decodeSplit(&pulses2[0], &pulses2[1], dec, int(pulses3[0]), silk_shell_code_table2)
	decodeSplit(&pulses1[0], &pulses1[1], dec, int(pulses2[0]), silk_shell_code_table1)
	decodeSplit(&pulses0[0], &pulses0[1], dec, int(pulses1[0]), silk_shell_code_table0)
	decodeSplit(&pulses0[2], &pulses0[3], dec, int(pulses1[1]), silk_shell_code_table0)
	decodeSplit(&pulses1[2], &pulses1[3], dec, int(pulses2[1]), silk_shell_code_table1)
	decodeSplit(&pulses0[4], &pulses0[5], dec, int(pulses1[2]), silk_shell_code_table0)
	decodeSplit(&pulses0[6], &pulses0[7], dec, int(pulses1[3]), silk_shell_code_table0)
	decodeSplit(&pulses2[2], &pulses2[3], dec, int(pulses3[1]), silk_shell_code_table2)
	decodeSplit(&pulses1[4], &pulses1[5], dec, int(pulses2[2]), silk_shell_code_table1)
	decodeSplit(&pulses0[8], &pulses0[9], dec, int(pulses1[4]), silk_shell_code_table0)
	decodeSplit(&pulses0[10], &pulses0[11], dec, int(pulses1[5]), silk_shell_code_table0)
	decodeSplit(&pulses1[6], &pulses1[7], dec, int(pulses2[3]), silk_shell_code_table1)
	decodeSplit(&pulses0[12], &pulses0[13], dec, int(pulses1[6]), silk_shell_code_table0)
	decodeSplit(&pulses0[14], &pulses0[15], dec, int(pulses1[7]), silk_shell_code_table0)
}

func decodeSplit(child1, child2 *int16, dec *rangeDecoder, p int, shellTable []uint8) {
	if p > 0 {
		*child1 = int16(dec.decodeICDF(shellTable[silk_shell_code_table_offsets[p]:], 8))
		*child2 = int16(p) - *child1
	} else {
		*child1 = 0
		*child2 = 0
	}
}

// decodeSigns attaches signs to the pulses (silk_decode_signs).
func decodeSigns(dec *rangeDecoder, pulses []int16, length int, signalType, quantOffsetType int8, sumPulses []int) {
	var icdf [2]uint8
	i := 7 * (int(quantOffsetType) + int(signalType)<<1)
	icdfPtr := silk_sign_iCDF[i:]
	nBlocks := (length + shellFrameLen/2) >> log2ShellFrame
	for i := 0; i < nBlocks; i++ {
		p := sumPulses[i]
		q := pulses[i*shellFrameLen:]
		if p > 0 {
			icdf[0] = icdfPtr[min(p&0x1F, 6)]
			for j := 0; j < shellFrameLen; j++ {
				if q[j] > 0 {
					q[j] *= int16(int(dec.decodeICDF(icdf[:], 8))<<1 - 1)
				}
			}
		}
	}
}

// decodeCore runs LTP and LPC synthesis into xq (silk_decode_core).
func (cs *silkChannelState) decodeCore(ctrl *silkDecoderControl, xq []int16, pulses []int16) {
	sLTP := make([]int16, cs.ltpMemLength)
	sLTPQ15 := make([]int32, cs.ltpMemLength+cs.frameLength)
	resQ14 := make([]int32, cs.subfrLength)
	sLPCQ14 := make([]int32, cs.subfrLength+silkMaxLPCOrder)

	offsetQ10 := int32(silk_Quantization_Offsets_Q10[cs.indices.signalType>>1][cs.indices.quantOffsetType])
	nlsfInterp := cs.indices.NLSFInterpCoefQ2 < 4

	randSeed := int32(cs.indices.Seed)
	for i := 0; i < cs.frameLength; i++ {
		randSeed = silkRAND(randSeed)
		e := silkLSHIFT(int32(pulses[i]), 14)
		if e > 0 {
			e -= quantLevelAdjQ10 << 4
		} else if e < 0 {
			e += quantLevelAdjQ10 << 4
		}
		e += offsetQ10 << 4
		if randSeed < 0 {
			e = -e
		}
		cs.excQ14[i] = e
		randSeed = silkADD32ovflw(randSeed, int32(pulses[i]))
	}

	copy(sLPCQ14[:silkMaxLPCOrder], cs.sLPCQ14Buf[:])
	pexcIdx := 0
	pxqIdx := 0
	sLTPBufIdx := cs.ltpMemLength
	lag := 0
	for k := 0; k < cs.nbSubfr; k++ {
		pres := resQ14
		aQ12 := ctrl.PredCoefQ12[k>>1][:]
		bQ14 := ctrl.LTPCoefQ14[k*silkLTPOrder:]
		gainQ10 := silkRSHIFT(ctrl.GainsQ16[k], 6)
		invGainQ31 := silkINVERSE32varQ(ctrl.GainsQ16[k], 47)

		var gainAdjQ16 int32
		if ctrl.GainsQ16[k] != cs.prevGainQ16 {
			gainAdjQ16 = silkDIV32varQ(cs.prevGainQ16, ctrl.GainsQ16[k], 16)
			for i := 0; i < silkMaxLPCOrder; i++ {
				sLPCQ14[i] = silkSMULWW(gainAdjQ16, sLPCQ14[i])
			}
		} else {
			gainAdjQ16 = int32(1) << 16
		}
		cs.prevGainQ16 = ctrl.GainsQ16[k]

		if cs.indices.signalType == typeVoiced {
			lag = ctrl.pitchL[k]
			if k == 0 || (k == 2 && nlsfInterp) {
				startIdx := cs.ltpMemLength - lag - cs.LPCOrder - silkLTPOrder/2
				if k == 2 {
					copy(cs.outBuf[cs.ltpMemLength:cs.ltpMemLength+2*cs.subfrLength], xq[:2*cs.subfrLength])
				}
				silkLPCAnalysisFilter(sLTP[startIdx:], cs.outBuf[startIdx+k*cs.subfrLength:],
					aQ12, cs.ltpMemLength-startIdx, cs.LPCOrder)
				if k == 0 {
					invGainQ31 = silkLSHIFT(silkSMULWB(invGainQ31, ctrl.LTPScaleQ14), 2)
				}
				for i := 0; i < lag+silkLTPOrder/2; i++ {
					sLTPQ15[sLTPBufIdx-i-1] = silkSMULWB(invGainQ31, int32(sLTP[cs.ltpMemLength-i-1]))
				}
			} else if gainAdjQ16 != int32(1)<<16 {
				for i := 0; i < lag+silkLTPOrder/2; i++ {
					sLTPQ15[sLTPBufIdx-i-1] = silkSMULWW(gainAdjQ16, sLTPQ15[sLTPBufIdx-i-1])
				}
			}
		}

		if cs.indices.signalType == typeVoiced {
			predLag := sLTPBufIdx - lag + silkLTPOrder/2
			for i := 0; i < cs.subfrLength; i++ {
				ltpPredQ13 := int32(2)
				ltpPredQ13 = silkSMLAWB(ltpPredQ13, sLTPQ15[predLag], int32(bQ14[0]))
				ltpPredQ13 = silkSMLAWB(ltpPredQ13, sLTPQ15[predLag-1], int32(bQ14[1]))
				ltpPredQ13 = silkSMLAWB(ltpPredQ13, sLTPQ15[predLag-2], int32(bQ14[2]))
				ltpPredQ13 = silkSMLAWB(ltpPredQ13, sLTPQ15[predLag-3], int32(bQ14[3]))
				ltpPredQ13 = silkSMLAWB(ltpPredQ13, sLTPQ15[predLag-4], int32(bQ14[4]))
				predLag++
				pres[i] = cs.excQ14[pexcIdx+i] + silkLSHIFT(ltpPredQ13, 1)
				sLTPQ15[sLTPBufIdx] = silkLSHIFT(pres[i], 1)
				sLTPBufIdx++
			}
		} else {
			pres = cs.excQ14[pexcIdx : pexcIdx+cs.subfrLength]
		}

		for i := 0; i < cs.subfrLength; i++ {
			lpcPredQ10 := silkRSHIFT(int32(cs.LPCOrder), 1)
			base := silkMaxLPCOrder + i
			lpcPredQ10 = silkSMLAWB(lpcPredQ10, sLPCQ14[base-1], int32(aQ12[0]))
			lpcPredQ10 = silkSMLAWB(lpcPredQ10, sLPCQ14[base-2], int32(aQ12[1]))
			lpcPredQ10 = silkSMLAWB(lpcPredQ10, sLPCQ14[base-3], int32(aQ12[2]))
			lpcPredQ10 = silkSMLAWB(lpcPredQ10, sLPCQ14[base-4], int32(aQ12[3]))
			lpcPredQ10 = silkSMLAWB(lpcPredQ10, sLPCQ14[base-5], int32(aQ12[4]))
			lpcPredQ10 = silkSMLAWB(lpcPredQ10, sLPCQ14[base-6], int32(aQ12[5]))
			lpcPredQ10 = silkSMLAWB(lpcPredQ10, sLPCQ14[base-7], int32(aQ12[6]))
			lpcPredQ10 = silkSMLAWB(lpcPredQ10, sLPCQ14[base-8], int32(aQ12[7]))
			lpcPredQ10 = silkSMLAWB(lpcPredQ10, sLPCQ14[base-9], int32(aQ12[8]))
			lpcPredQ10 = silkSMLAWB(lpcPredQ10, sLPCQ14[base-10], int32(aQ12[9]))
			if cs.LPCOrder == 16 {
				lpcPredQ10 = silkSMLAWB(lpcPredQ10, sLPCQ14[base-11], int32(aQ12[10]))
				lpcPredQ10 = silkSMLAWB(lpcPredQ10, sLPCQ14[base-12], int32(aQ12[11]))
				lpcPredQ10 = silkSMLAWB(lpcPredQ10, sLPCQ14[base-13], int32(aQ12[12]))
				lpcPredQ10 = silkSMLAWB(lpcPredQ10, sLPCQ14[base-14], int32(aQ12[13]))
				lpcPredQ10 = silkSMLAWB(lpcPredQ10, sLPCQ14[base-15], int32(aQ12[14]))
				lpcPredQ10 = silkSMLAWB(lpcPredQ10, sLPCQ14[base-16], int32(aQ12[15]))
			}
			sLPCQ14[base] = silkADDSAT32(pres[i], silkLSHIFTSAT32(lpcPredQ10, 4))
			xq[pxqIdx+i] = int16(silkSAT16(silkRSHIFTROUND(silkSMULWW(sLPCQ14[base], gainQ10), 8)))
		}
		copy(sLPCQ14[:silkMaxLPCOrder], sLPCQ14[cs.subfrLength:cs.subfrLength+silkMaxLPCOrder])
		pexcIdx += cs.subfrLength
		pxqIdx += cs.subfrLength
	}
	copy(cs.sLPCQ14Buf[:], sLPCQ14[:silkMaxLPCOrder])
}

// decodeFrame decodes one SILK frame into out[0:frameLength] (silk_decode_frame).
func (cs *silkChannelState) decodeFrame(dec *rangeDecoder, out []int16, condCoding int) {
	var ctrl silkDecoderControl
	pulses := make([]int16, (cs.frameLength+shellFrameLen-1) & ^(shellFrameLen-1))
	cs.decodeIndices(dec, cs.nFramesDecoded, false, condCoding)
	silkDecodePulses(dec, pulses, cs.indices.signalType, cs.indices.quantOffsetType, cs.frameLength)
	cs.decodeParameters(&ctrl, condCoding)
	cs.decodeCore(&ctrl, out, pulses)

	mvLen := cs.ltpMemLength - cs.frameLength
	copy(cs.outBuf[:mvLen], cs.outBuf[cs.frameLength:cs.frameLength+mvLen])
	copy(cs.outBuf[mvLen:mvLen+cs.frameLength], out[:cs.frameLength])
	cs.prevSignalType = int(cs.indices.signalType)
	cs.firstFrameAfterReset = false
	cs.lagPrev = ctrl.pitchL[cs.nbSubfr-1]
}
