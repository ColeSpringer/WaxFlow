package opus

// SILK decoder, ported from libopus silk/dec_API.c, decode_frame.c,
// decode_indices.c, decode_parameters.c, decode_core.c, decode_pulses.c,
// shell_coder.c, code_signs.c, decode_pitch.c, gain_quant.c, decoder_set_fs.c,
// stereo_decode_pred.c, stereo_MS_to_LR.c, and LPC_analysis_filter.c.
// Integer-only, matching the reference bit-for-bit.
//
// The Opus decoder drives this at the internal 8/12/16 kHz rate (NB/MB/WB) and
// the resampler brings each SILK frame up to 48 kHz. Loss concealment (PLC),
// comfort noise (CNG), and the neural OSCE enhancer are out of scope (file
// decode, not RTC), so this ports the FLAG_DECODE_NORMAL path only.

// silkIndices holds one SILK frame's decoded quantization indices.
type silkIndices struct {
	signalType       int8
	quantOffsetType  int8
	GainsIndices     [silkMaxNBSubfr]int8
	NLSFIndices      [silkMaxLPCOrder + 1]int8
	lagIndex         int
	contourIndex     int8
	PERIndex         int8
	LTPIndex         [silkMaxNBSubfr]int8
	NLSFInterpCoefQ2 int8
	Seed             int8
	LTPScaleIndex    int8
}

// silkDecoderControl holds the per-frame parameters derived from the indices.
type silkDecoderControl struct {
	pitchL      [silkMaxNBSubfr]int
	GainsQ16    [silkMaxNBSubfr]int32
	PredCoefQ12 [2][silkMaxLPCOrder]int16
	LTPCoefQ14  [silkLTPOrder * silkMaxNBSubfr]int16
	LTPScaleQ14 int32
}

// silkChannelState is one internal channel's persistent decoder state.
type silkChannelState struct {
	prevGainQ16          int32
	excQ14               [silkMaxFrameLen]int32
	sLPCQ14Buf           [silkMaxLPCOrder]int32
	outBuf               [silkMaxFrameLen + 2*silkMaxSubfrLen]int16
	lagPrev              int
	LastGainIndex        int8
	fsKHz                int
	prevSignalType       int
	frameLength          int
	subfrLength          int
	ltpMemLength         int
	nbSubfr              int
	LPCOrder             int
	prevNLSFQ15          [silkMaxLPCOrder]int16
	firstFrameAfterReset bool
	psNLSFCB             *nlsfCB

	pitchLagLowBitsICDF []uint8
	pitchContourICDF    []uint8

	ecPrevSignalType int
	ecPrevLagIndex   int

	VADFlags         [3]int
	LBRRFlag         int
	LBRRFlags        [3]int
	nFramesDecoded   int
	nFramesPerPacket int

	indices   silkIndices
	resampler silkResampler
}

// reset clears the channel state (silk_reset_decoder).
func (cs *silkChannelState) reset() {
	*cs = silkChannelState{}
	cs.firstFrameAfterReset = true
	cs.prevGainQ16 = 65536
}

// stereoState carries the SILK stereo unmixing state (stereo_dec_state).
type stereoState struct {
	predPrevQ13 [2]int32
	sMid        [2]int16
	sSide       [2]int16
}

// silkDecoder is the top-level SILK decoder for a stream (silk_decoder).
type silkDecoder struct {
	channel              [2]silkChannelState
	sStereo              stereoState
	prevDecodeOnlyMiddle int
	nChannelsAPI         int
	nChannelsInternal    int
}

func newSILKDecoder() *silkDecoder {
	d := &silkDecoder{}
	d.channel[0].reset()
	d.channel[1].reset()
	return d
}

// setFS configures a channel for a sampling frequency (silk_decoder_set_fs).
func (cs *silkChannelState) setFS(fsKHz int, fsAPIHz int) {
	cs.subfrLength = silkSubFrameMS * fsKHz
	frameLength := cs.nbSubfr * cs.subfrLength
	if cs.fsKHz != fsKHz || cs.resampler.fsInKHz*1000 != fsKHz*1000 || cs.resampler.fsOutKHz*1000 != fsAPIHz {
		cs.resampler.init(fsKHz*1000, fsAPIHz)
	}
	if cs.fsKHz != fsKHz || frameLength != cs.frameLength {
		if fsKHz == 8 {
			if cs.nbSubfr == silkMaxNBSubfr {
				cs.pitchContourICDF = silk_pitch_contour_NB_iCDF
			} else {
				cs.pitchContourICDF = silk_pitch_contour_10_ms_NB_iCDF
			}
		} else {
			if cs.nbSubfr == silkMaxNBSubfr {
				cs.pitchContourICDF = silk_pitch_contour_iCDF
			} else {
				cs.pitchContourICDF = silk_pitch_contour_10_ms_iCDF
			}
		}
		if cs.fsKHz != fsKHz {
			cs.ltpMemLength = silkLTPMemMS * fsKHz
			if fsKHz == 8 || fsKHz == 12 {
				cs.LPCOrder = silkMinLPCOrder
				cs.psNLSFCB = silkNLSFCBNBMB
			} else {
				cs.LPCOrder = silkMaxLPCOrder
				cs.psNLSFCB = silkNLSFCBWB
			}
			switch fsKHz {
			case 16:
				cs.pitchLagLowBitsICDF = silk_uniform8_iCDF
			case 12:
				cs.pitchLagLowBitsICDF = silk_uniform6_iCDF
			case 8:
				cs.pitchLagLowBitsICDF = silk_uniform4_iCDF
			}
			cs.firstFrameAfterReset = true
			cs.lagPrev = 100
			cs.LastGainIndex = 10
			cs.prevSignalType = typeNoVoiceActivity
			clear(cs.outBuf[:])
			clear(cs.sLPCQ14Buf[:])
		}
		cs.fsKHz = fsKHz
		cs.frameLength = frameLength
	}
}

// silkGainsDequant reconstructs the quantized gains (silk_gains_dequant).
func silkGainsDequant(gainQ16 []int32, ind []int8, prevInd *int8, conditional bool, nbSubfr int) {
	const offset = (minQGainDB*128)/6 + 16*128
	const invScaleQ16 = (65536 * (((maxQGainDB - minQGainDB) * 128) / 6)) / (nLevelsQGain - 1)
	for k := 0; k < nbSubfr; k++ {
		if k == 0 && !conditional {
			*prevInd = int8(silkMaxInt(int32(ind[k]), int32(*prevInd)-16))
		} else {
			indTmp := int32(ind[k]) + minDeltaGain
			dst := int32(2*maxDeltaGain - nLevelsQGain + int(*prevInd))
			if indTmp > dst {
				*prevInd = int8(int32(*prevInd) + silkLSHIFT(indTmp, 1) - dst)
			} else {
				*prevInd = int8(int32(*prevInd) + indTmp)
			}
		}
		*prevInd = int8(silkLIMIT(int32(*prevInd), 0, nLevelsQGain-1))
		gainQ16[k] = silkLog2Lin(silkMinInt(silkSMULWB(invScaleQ16, int32(*prevInd))+offset, 3967))
	}
}

// silkDecodePitch reconstructs the four subframe pitch lags (silk_decode_pitch).
func silkDecodePitch(lagIndex int, contourIndex int8, pitchLags []int, fsKHz, nbSubfr int) {
	var lagCB [][]int8
	if fsKHz == 8 {
		if nbSubfr == silkMaxNBSubfr {
			lagCB = silk_CB_lags_stage2
		} else {
			lagCB = silk_CB_lags_stage2_10_ms
		}
	} else {
		if nbSubfr == silkMaxNBSubfr {
			lagCB = silk_CB_lags_stage3
		} else {
			lagCB = silk_CB_lags_stage3_10_ms
		}
	}
	minLag := peMinLagMS * fsKHz
	maxLag := peMaxLagMS * fsKHz
	lag := minLag + lagIndex
	for k := 0; k < nbSubfr; k++ {
		pitchLags[k] = lag + int(lagCB[k][contourIndex])
		pitchLags[k] = int(silkLIMIT(int32(pitchLags[k]), int32(minLag), int32(maxLag)))
	}
}

// silkLPCAnalysisFilter runs the whitening MA filter (silk_LPC_analysis_filter).
func silkLPCAnalysisFilter(out, in []int16, B []int16, length, order int) {
	for ix := order; ix < length; ix++ {
		out32Q12 := silkSMULBB(int32(in[ix-1]), int32(B[0]))
		for j := 1; j < order; j++ {
			out32Q12 = silkSMLABBovflw(out32Q12, int32(in[ix-1-j]), int32(B[j]))
		}
		out32Q12 = silkSUB32ovflw(silkLSHIFT(int32(in[ix]), 12), out32Q12)
		out32 := silkRSHIFTROUND(out32Q12, 12)
		out[ix] = int16(silkSAT16(out32))
	}
	for j := 0; j < order; j++ {
		out[j] = 0
	}
}
