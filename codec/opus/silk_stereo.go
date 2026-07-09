package opus

// SILK stereo unmixing and the packet-level decode driver, ported from libopus
// silk/stereo_decode_pred.c, stereo_MS_to_LR.c, and dec_API.c.
// File decode only: no PLC, CNG, FEC, or DTX.

// silkControl carries the per-Opus-frame configuration the Opus layer supplies.
type silkControl struct {
	payloadSizeMs     int
	internalRate      int // Hz (8000/12000/16000)
	nChannelsInternal int
	nChannelsAPI      int
	apiSampleRate     int // 48000
}

// stereoDecodePred decodes the mid/side prediction weights (silk_stereo_decode_pred).
func stereoDecodePred(dec *rangeDecoder, predQ13 []int32) {
	var ix [2][3]int
	n := dec.decodeICDF(silk_stereo_pred_joint_iCDF, 8)
	ix[0][2] = n / 5
	ix[1][2] = n - 5*ix[0][2]
	for i := 0; i < 2; i++ {
		ix[i][0] = dec.decodeICDF(silk_uniform3_iCDF, 8)
		ix[i][1] = dec.decodeICDF(silk_uniform5_iCDF, 8)
	}
	for i := 0; i < 2; i++ {
		ix[i][0] += 3 * ix[i][2]
		lowQ13 := int32(silk_stereo_pred_quant_Q13[ix[i][0]])
		stepQ13 := silkSMULWB(int32(silk_stereo_pred_quant_Q13[ix[i][0]+1])-lowQ13,
			silkFixConst(0.5/stereoQuantSubSteps, 16))
		predQ13[i] = silkSMLABB(lowQ13, stepQ13, int32(2*ix[i][1]+1))
	}
	predQ13[0] -= predQ13[1]
}

// stereoDecodeMidOnly decodes the mid-only flag (silk_stereo_decode_mid_only).
func stereoDecodeMidOnly(dec *rangeDecoder) int {
	return dec.decodeICDF(silk_stereo_only_code_mid_iCDF, 8)
}

// stereoMSToLR converts mid/side back to left/right in place (silk_stereo_MS_to_LR).
// x1/x2 carry a 2-sample lead: indices 0..1 are inter-frame history.
func stereoMSToLR(st *stereoState, x1, x2 []int16, predQ13 []int32, fsKHz, frameLength int) {
	x1[0], x1[1] = st.sMid[0], st.sMid[1]
	x2[0], x2[1] = st.sSide[0], st.sSide[1]
	st.sMid[0], st.sMid[1] = x1[frameLength], x1[frameLength+1]
	st.sSide[0], st.sSide[1] = x2[frameLength], x2[frameLength+1]

	pred0 := st.predPrevQ13[0]
	pred1 := st.predPrevQ13[1]
	denomQ16 := silkDIV32_16(int32(1)<<16, int32(stereoInterpLenMS*fsKHz))
	delta0 := silkRSHIFTROUND(silkSMULBB(predQ13[0]-st.predPrevQ13[0], denomQ16), 16)
	delta1 := silkRSHIFTROUND(silkSMULBB(predQ13[1]-st.predPrevQ13[1], denomQ16), 16)
	interp := stereoInterpLenMS * fsKHz
	for n := 0; n < interp; n++ {
		pred0 += delta0
		pred1 += delta1
		sum := silkLSHIFT(int32(x1[n])+int32(x1[n+2])+silkLSHIFT(int32(x1[n+1]), 1), 9)
		sum = silkSMLAWB(silkLSHIFT(int32(x2[n+1]), 8), sum, pred0)
		sum = silkSMLAWB(sum, silkLSHIFT(int32(x1[n+1]), 11), pred1)
		x2[n+1] = int16(silkSAT16(silkRSHIFTROUND(sum, 8)))
	}
	pred0 = predQ13[0]
	pred1 = predQ13[1]
	for n := interp; n < frameLength; n++ {
		sum := silkLSHIFT(int32(x1[n])+int32(x1[n+2])+silkLSHIFT(int32(x1[n+1]), 1), 9)
		sum = silkSMLAWB(silkLSHIFT(int32(x2[n+1]), 8), sum, pred0)
		sum = silkSMLAWB(sum, silkLSHIFT(int32(x1[n+1]), 11), pred1)
		x2[n+1] = int16(silkSAT16(silkRSHIFTROUND(sum, 8)))
	}
	st.predPrevQ13[0] = predQ13[0]
	st.predPrevQ13[1] = predQ13[1]
	for n := 0; n < frameLength; n++ {
		sum := int32(x1[n+1]) + int32(x2[n+1])
		diff := int32(x1[n+1]) - int32(x2[n+1])
		x1[n+1] = int16(silkSAT16(sum))
		x2[n+1] = int16(silkSAT16(diff))
	}
}

// decode decodes one SILK frame (all internal channels) and resamples it to the
// API rate, writing nSamplesOut samples per API channel into out. Mirrors one
// silk_Decode call (silk/dec_API.c, FLAG_DECODE_NORMAL path).
func (d *silkDecoder) decode(dec *rangeDecoder, ctrl silkControl, out [][]int16) int {
	cs0 := &d.channel[0]
	nInt := ctrl.nChannelsInternal
	nAPI := ctrl.nChannelsAPI

	if cs0.nFramesDecoded == 0 {
		for n := 0; n < nInt; n++ {
			cs := &d.channel[n]
			switch ctrl.payloadSizeMs {
			case 10:
				cs.nFramesPerPacket, cs.nbSubfr = 1, 2
			case 20:
				cs.nFramesPerPacket, cs.nbSubfr = 1, 4
			case 40:
				cs.nFramesPerPacket, cs.nbSubfr = 2, 4
			case 60:
				cs.nFramesPerPacket, cs.nbSubfr = 3, 4
			}
			cs.setFS((ctrl.internalRate>>10)+1, ctrl.apiSampleRate)
		}
		if nAPI == 2 && nInt == 2 && (d.nChannelsAPI == 1 || d.nChannelsInternal == 1) {
			d.sStereo.predPrevQ13 = [2]int32{}
			d.sStereo.sSide = [2]int16{}
			d.channel[1].resampler = d.channel[0].resampler
		}
		d.nChannelsAPI = nAPI
		d.nChannelsInternal = nInt

		for n := 0; n < nInt; n++ {
			cs := &d.channel[n]
			for i := 0; i < cs.nFramesPerPacket; i++ {
				cs.VADFlags[i] = dec.decodeBitLogp(1)
			}
			cs.LBRRFlag = dec.decodeBitLogp(1)
		}
		for n := 0; n < nInt; n++ {
			cs := &d.channel[n]
			cs.LBRRFlags = [3]int{}
			if cs.LBRRFlag != 0 {
				if cs.nFramesPerPacket == 1 {
					cs.LBRRFlags[0] = 1
				} else {
					sym := dec.decodeICDF(silkLBRRFlagsICDFPtr[cs.nFramesPerPacket-2], 8) + 1
					for i := 0; i < cs.nFramesPerPacket; i++ {
						cs.LBRRFlags[i] = (sym >> i) & 1
					}
				}
			}
		}
		// Consume (and discard) LBRR redundancy frames to stay bit-aligned.
		for i := 0; i < cs0.nFramesPerPacket; i++ {
			for n := 0; n < nInt; n++ {
				cs := &d.channel[n]
				if cs.LBRRFlags[i] == 0 {
					continue
				}
				if nInt == 2 && n == 0 {
					var msp [2]int32
					stereoDecodePred(dec, msp[:])
					if d.channel[1].LBRRFlags[i] == 0 {
						stereoDecodeMidOnly(dec)
					}
				}
				condCoding := codeIndependently
				if i > 0 && cs.LBRRFlags[i-1] != 0 {
					condCoding = codeConditionally
				}
				lpulses := make([]int16, silkMaxFrameLen)
				cs.decodeIndices(dec, i, true, condCoding)
				silkDecodePulses(dec, lpulses, cs.indices.signalType, cs.indices.quantOffsetType, cs.frameLength)
			}
		}
	}

	var msPredQ13 [2]int32
	decodeOnlyMiddle := 0
	if nInt == 2 {
		stereoDecodePred(dec, msPredQ13[:])
		if d.channel[1].VADFlags[cs0.nFramesDecoded] == 0 {
			decodeOnlyMiddle = stereoDecodeMidOnly(dec)
		}
	} else {
		msPredQ13 = [2]int32{}
	}

	if nInt == 2 && decodeOnlyMiddle == 0 && d.prevDecodeOnlyMiddle == 1 {
		cs1 := &d.channel[1]
		clear(cs1.outBuf[:])
		clear(cs1.sLPCQ14Buf[:])
		cs1.lagPrev = 100
		cs1.LastGainIndex = 10
		cs1.prevSignalType = typeNoVoiceActivity
		cs1.firstFrameAfterReset = true
	}

	hasSide := decodeOnlyMiddle == 0
	frameLen := cs0.frameLength
	var buf [2][]int16
	for n := 0; n < nInt; n++ {
		buf[n] = make([]int16, frameLen+2)
	}
	for n := 0; n < nInt; n++ {
		cs := &d.channel[n]
		if n == 0 || hasSide {
			frameIndex := cs0.nFramesDecoded - n
			condCoding := codeConditionally
			if frameIndex <= 0 {
				condCoding = codeIndependently
			} else if n > 0 && d.prevDecodeOnlyMiddle != 0 {
				condCoding = codeIndependentlyNoLTPScale
			}
			cs.decodeFrame(dec, buf[n][2:], condCoding)
		}
		cs.nFramesDecoded++
	}

	nSamplesOutDec := frameLen
	if nAPI == 2 && nInt == 2 {
		stereoMSToLR(&d.sStereo, buf[0], buf[1], msPredQ13[:], cs0.fsKHz, nSamplesOutDec)
	} else {
		// Only the mid history is buffered here, matching dec_API.c: sSide
		// is read solely by stereoMSToLR, which a mono-API track can never
		// reach, and the API-2/internal-2 transition above resets it.
		buf[0][0], buf[0][1] = d.sStereo.sMid[0], d.sStereo.sMid[1]
		d.sStereo.sMid[0], d.sStereo.sMid[1] = buf[0][nSamplesOutDec], buf[0][nSamplesOutDec+1]
	}

	nSamplesOut := nSamplesOutDec * ctrl.apiSampleRate / (cs0.fsKHz * 1000)
	for n := 0; n < min(nAPI, nInt); n++ {
		cs := &d.channel[n]
		tmp := make([]int16, nSamplesOut)
		cs.resampler.resample(tmp, buf[n][1:1+nSamplesOutDec], nSamplesOutDec)
		copy(out[n][:nSamplesOut], tmp)
	}
	if nAPI == 2 && nInt == 1 {
		copy(out[1][:nSamplesOut], out[0][:nSamplesOut])
	}

	d.prevDecodeOnlyMiddle = decodeOnlyMiddle
	return nSamplesOut
}
