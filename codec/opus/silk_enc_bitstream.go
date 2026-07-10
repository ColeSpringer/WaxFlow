package opus

// SILK bitstream encoding, ported from libopus silk/encode_indices.c,
// silk/encode_pulses.c, silk/shell_coder.c (encode half), and
// silk/code_signs.c (encode half). Exact mirrors of the decode side in
// silk_decode.go: encode(x) followed by the decoder reproduces x bit-for-bit.

// encodeIndices encodes one frame's side-information parameters
// (silk_encode_indices).
func (ch *silkEncoderChannel) encodeIndices(enc *rangeEncoder, frameIndex int, encodeLBRR bool, condCoding int) {
	var psIndices *silkIndices
	if encodeLBRR {
		psIndices = &ch.indicesLBRR[frameIndex]
	} else {
		psIndices = &ch.indices
	}

	// Signal type and quantizer offset.
	typeOffset := 2*int(psIndices.signalType) + int(psIndices.quantOffsetType)
	if encodeLBRR || typeOffset >= 2 {
		enc.encodeICDF(typeOffset-2, silk_type_offset_VAD_iCDF, 8)
	} else {
		enc.encodeICDF(typeOffset, silk_type_offset_no_VAD_iCDF, 8)
	}

	// Gains: first subframe conditional or independent (MSBs then 3 LSBs).
	if condCoding == codeConditionally {
		enc.encodeICDF(int(psIndices.GainsIndices[0]), silk_delta_gain_iCDF, 8)
	} else {
		enc.encodeICDF(int(psIndices.GainsIndices[0])>>3, silk_gain_iCDF[psIndices.signalType], 8)
		enc.encodeICDF(int(psIndices.GainsIndices[0])&7, silk_uniform8_iCDF, 8)
	}
	for i := 1; i < ch.nbSubfr; i++ {
		enc.encodeICDF(int(psIndices.GainsIndices[i]), silk_delta_gain_iCDF, 8)
	}

	// NLSFs.
	enc.encodeICDF(int(psIndices.NLSFIndices[0]),
		ch.psNLSFCB.cb1ICDF[(int(psIndices.signalType)>>1)*ch.psNLSFCB.nVectors:], 8)
	var ecIx [silkMaxLPCOrder]int16
	var predQ8 [silkMaxLPCOrder]uint8
	silkNLSFUnpack(ecIx[:], predQ8[:], ch.psNLSFCB, int(psIndices.NLSFIndices[0]))
	for i := 0; i < ch.psNLSFCB.order; i++ {
		idx := int(psIndices.NLSFIndices[i+1])
		switch {
		case idx >= nlsfQuantMaxAmp:
			enc.encodeICDF(2*nlsfQuantMaxAmp, ch.psNLSFCB.ecICDF[ecIx[i]:], 8)
			enc.encodeICDF(idx-nlsfQuantMaxAmp, silk_NLSF_EXT_iCDF, 8)
		case idx <= -nlsfQuantMaxAmp:
			enc.encodeICDF(0, ch.psNLSFCB.ecICDF[ecIx[i]:], 8)
			enc.encodeICDF(-idx-nlsfQuantMaxAmp, silk_NLSF_EXT_iCDF, 8)
		default:
			enc.encodeICDF(idx+nlsfQuantMaxAmp, ch.psNLSFCB.ecICDF[ecIx[i]:], 8)
		}
	}
	if ch.nbSubfr == silkMaxNBSubfr {
		enc.encodeICDF(int(psIndices.NLSFInterpCoefQ2), silk_NLSF_interpolation_factor_iCDF, 8)
	}

	if psIndices.signalType == typeVoiced {
		// Pitch lags: absolute, or delta against the previous frame.
		encodeAbsoluteLagIndex := true
		if condCoding == codeConditionally && ch.ecPrevSignalType == typeVoiced {
			deltaLagIndex := psIndices.lagIndex - ch.ecPrevLagIndex
			if deltaLagIndex < -8 || deltaLagIndex > 11 {
				deltaLagIndex = 0
			} else {
				deltaLagIndex += 9
				encodeAbsoluteLagIndex = false
			}
			enc.encodeICDF(deltaLagIndex, silk_pitch_delta_iCDF, 8)
		}
		if encodeAbsoluteLagIndex {
			pitchHighBits := psIndices.lagIndex / (ch.fsKHz >> 1)
			pitchLowBits := psIndices.lagIndex - pitchHighBits*(ch.fsKHz>>1)
			enc.encodeICDF(pitchHighBits, silk_pitch_lag_iCDF, 8)
			enc.encodeICDF(pitchLowBits, ch.pitchLagLowBitsICDF, 8)
		}
		ch.ecPrevLagIndex = psIndices.lagIndex

		enc.encodeICDF(int(psIndices.contourIndex), ch.pitchContourICDF, 8)

		// LTP gains: period index then per-subframe codebook indices.
		enc.encodeICDF(int(psIndices.PERIndex), silk_LTP_per_index_iCDF, 8)
		for k := 0; k < ch.nbSubfr; k++ {
			enc.encodeICDF(int(psIndices.LTPIndex[k]), silkLTPGainICDFPtrs[psIndices.PERIndex], 8)
		}

		if condCoding == codeIndependently {
			enc.encodeICDF(int(psIndices.LTPScaleIndex), silk_LTPscale_iCDF, 8)
		}
	}

	ch.ecPrevSignalType = int(psIndices.signalType)

	enc.encodeICDF(int(psIndices.Seed), silk_uniform4_iCDF, 8)
}

// combineAndCheck pairwise-combines pulses and reports whether any sum
// exceeds maxPulses (combine_and_check).
func combineAndCheck(pulsesComb, pulsesIn []int, maxPulses, length int) bool {
	for k := 0; k < length; k++ {
		sum := pulsesIn[2*k] + pulsesIn[2*k+1]
		if sum > maxPulses {
			return true
		}
		pulsesComb[k] = sum
	}
	return false
}

// encodePulses encodes the quantized excitation (silk_encode_pulses).
func silkEncodePulses(enc *rangeEncoder, signalType, quantOffsetType int8, pulses []int8, frameLength int) {
	var pulsesComb [8]int

	// The 10 ms @ 12 kHz frame (120 samples) is not a whole number of shell
	// blocks; pad with zeros like the reference.
	iter := frameLength >> log2ShellFrame
	if iter*shellFrameLen < frameLength {
		iter++
		for i := frameLength; i < iter*shellFrameLen; i++ {
			pulses[i] = 0
		}
	}

	absPulses := make([]int, iter*shellFrameLen)
	for i := range absPulses {
		v := int(pulses[i])
		if v < 0 {
			v = -v
		}
		absPulses[i] = v
	}

	// Sum pulses per shell block, scaling down until each fits.
	sumPulses := make([]int, iter)
	nRshifts := make([]int, iter)
	for i := 0; i < iter; i++ {
		nRshifts[i] = 0
		blk := absPulses[i*shellFrameLen:]
		for {
			scaleDown := combineAndCheck(pulsesComb[:], blk[:shellFrameLen], int(silk_max_pulses_table[0]), 8)
			scaleDown = combineAndCheck(pulsesComb[:], pulsesComb[:], int(silk_max_pulses_table[1]), 4) || scaleDown
			scaleDown = combineAndCheck(pulsesComb[:], pulsesComb[:], int(silk_max_pulses_table[2]), 2) || scaleDown
			scaleDown = combineAndCheck(sumPulses[i:], pulsesComb[:], int(silk_max_pulses_table[3]), 1) || scaleDown
			if !scaleDown {
				break
			}
			nRshifts[i]++
			for k := 0; k < shellFrameLen; k++ {
				blk[k] >>= 1
			}
		}
	}

	// Rate level: fewest bits for the pulses-per-block info.
	rateLevelIndex := 0
	minSumBitsQ5 := int32(silkInt32Max)
	for k := 0; k < nRateLevels-1; k++ {
		nBits := silk_pulses_per_block_BITS_Q5[k]
		sumBitsQ5 := int32(silk_rate_levels_BITS_Q5[signalType>>1][k])
		for i := 0; i < iter; i++ {
			if nRshifts[i] > 0 {
				sumBitsQ5 += int32(nBits[silkMaxPulses+1])
			} else {
				sumBitsQ5 += int32(nBits[sumPulses[i]])
			}
		}
		if sumBitsQ5 < minSumBitsQ5 {
			minSumBitsQ5 = sumBitsQ5
			rateLevelIndex = k
		}
	}
	enc.encodeICDF(rateLevelIndex, silk_rate_levels_iCDF[signalType>>1], 8)

	// Sum-weighted-pulses encoding.
	cdf := silk_pulses_per_block_iCDF[rateLevelIndex]
	for i := 0; i < iter; i++ {
		if nRshifts[i] == 0 {
			enc.encodeICDF(sumPulses[i], cdf, 8)
		} else {
			enc.encodeICDF(silkMaxPulses+1, cdf, 8)
			for k := 0; k < nRshifts[i]-1; k++ {
				enc.encodeICDF(silkMaxPulses+1, silk_pulses_per_block_iCDF[nRateLevels-1], 8)
			}
			enc.encodeICDF(sumPulses[i], silk_pulses_per_block_iCDF[nRateLevels-1], 8)
		}
	}

	// Shell encoding.
	for i := 0; i < iter; i++ {
		if sumPulses[i] > 0 {
			shellEncoder(enc, absPulses[i*shellFrameLen:(i+1)*shellFrameLen])
		}
	}

	// LSB encoding.
	for i := 0; i < iter; i++ {
		if nRshifts[i] > 0 {
			blk := pulses[i*shellFrameLen:]
			nLS := nRshifts[i] - 1
			for k := 0; k < shellFrameLen; k++ {
				absQ := int32(blk[k])
				if absQ < 0 {
					absQ = -absQ
				}
				for j := nLS; j > 0; j-- {
					bit := int(absQ>>uint(j)) & 1
					enc.encodeICDF(bit, silk_lsb_iCDF, 8)
				}
				enc.encodeICDF(int(absQ)&1, silk_lsb_iCDF, 8)
			}
		}
	}

	encodeSigns(enc, pulses, frameLength, signalType, quantOffsetType, sumPulses)
}

// encodeSplit encodes one shell-tree split (encode_split).
func encodeSplit(enc *rangeEncoder, pChild1, p int, shellTable []uint8) {
	if p > 0 {
		enc.encodeICDF(pChild1, shellTable[silk_shell_code_table_offsets[p]:], 8)
	}
}

// shellEncoder encodes one shell block of 16 nonnegative pulse amplitudes
// (silk_shell_encoder).
func shellEncoder(enc *rangeEncoder, pulses0 []int) {
	var pulses1 [8]int
	var pulses2 [4]int
	var pulses3 [2]int
	var pulses4 [1]int

	for k := 0; k < 8; k++ {
		pulses1[k] = pulses0[2*k] + pulses0[2*k+1]
	}
	for k := 0; k < 4; k++ {
		pulses2[k] = pulses1[2*k] + pulses1[2*k+1]
	}
	for k := 0; k < 2; k++ {
		pulses3[k] = pulses2[2*k] + pulses2[2*k+1]
	}
	pulses4[0] = pulses3[0] + pulses3[1]

	encodeSplit(enc, pulses3[0], pulses4[0], silk_shell_code_table3)

	encodeSplit(enc, pulses2[0], pulses3[0], silk_shell_code_table2)

	encodeSplit(enc, pulses1[0], pulses2[0], silk_shell_code_table1)
	encodeSplit(enc, pulses0[0], pulses1[0], silk_shell_code_table0)
	encodeSplit(enc, pulses0[2], pulses1[1], silk_shell_code_table0)

	encodeSplit(enc, pulses1[2], pulses2[1], silk_shell_code_table1)
	encodeSplit(enc, pulses0[4], pulses1[2], silk_shell_code_table0)
	encodeSplit(enc, pulses0[6], pulses1[3], silk_shell_code_table0)

	encodeSplit(enc, pulses2[2], pulses3[1], silk_shell_code_table2)

	encodeSplit(enc, pulses1[4], pulses2[2], silk_shell_code_table1)
	encodeSplit(enc, pulses0[8], pulses1[4], silk_shell_code_table0)
	encodeSplit(enc, pulses0[10], pulses1[5], silk_shell_code_table0)

	encodeSplit(enc, pulses1[6], pulses2[3], silk_shell_code_table1)
	encodeSplit(enc, pulses0[12], pulses1[6], silk_shell_code_table0)
	encodeSplit(enc, pulses0[14], pulses1[7], silk_shell_code_table0)
}

// encodeSigns encodes the excitation signs (silk_encode_signs).
func encodeSigns(enc *rangeEncoder, pulses []int8, length int, signalType, quantOffsetType int8, sumPulses []int) {
	var icdf [2]uint8
	i := int(7 * (int32(quantOffsetType) + int32(signalType)<<1))
	icdfPtr := silk_sign_iCDF[i:]
	nBlocks := (length + shellFrameLen/2) >> log2ShellFrame
	for i := 0; i < nBlocks; i++ {
		p := sumPulses[i]
		if p > 0 {
			m := p & 0x1F
			if m > 6 {
				m = 6
			}
			icdf[0] = icdfPtr[m]
			blk := pulses[i*shellFrameLen:]
			for j := 0; j < shellFrameLen; j++ {
				if blk[j] != 0 {
					// silk_enc_map: sign bit to 0/1.
					enc.encodeICDF(int(blk[j]>>7)+1, icdf[:], 8)
				}
			}
		}
	}
}
