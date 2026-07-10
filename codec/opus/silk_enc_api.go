package opus

// SILK encoder top level, ported from libopus silk/enc_API.c and
// silk/float/encode_frame_FLP.c. One silk_Encode call consumes whole 10 ms
// blocks at the API rate and produces at most one payload per packet; the
// caller owns the range encoder so SILK-only, hybrid, and redundancy layouts
// all compose.

// newSILKEncoder returns an initialized SILK encoder (silk_InitEncoder).
func newSILKEncoder(channels int) *silkEncoder {
	e := &silkEncoder{}
	for n := 0; n < channels; n++ {
		e.channel[n].initEncoderChannel()
	}
	e.nChannelsAPI = 1
	e.nChannelsInternal = 1
	return e
}

// encodeDoVAD runs the VAD and converts activity into frame type flags
// (silk_encode_do_VAD_FLP). activity is the Opus-level DTX decision
// (negative means no decision).
func (ch *silkEncoderChannel) encodeDoVAD(activity int) {
	activityThreshold := silkFixConst(speechActivityDTXThres, 8)

	res := ch.sVAD.getSAQ8(ch.inputBuf[1:], ch.frameLength, ch.fsKHz)
	ch.speechActivityQ8 = res.speechActivityQ8
	ch.inputTiltQ15 = res.inputTiltQ15
	ch.inputQualityBandsQ15 = res.inputQualityBandsQ15

	if activity == 0 && ch.speechActivityQ8 >= activityThreshold {
		ch.speechActivityQ8 = activityThreshold - 1
	}

	if ch.speechActivityQ8 < activityThreshold {
		ch.indices.signalType = typeNoVoiceActivity
		ch.noSpeechCounter++
		if ch.noSpeechCounter <= nbSpeechFramesBeforeDTX {
			ch.inDTX = false
		} else if ch.noSpeechCounter > maxConsecutiveDTX+nbSpeechFramesBeforeDTX {
			ch.noSpeechCounter = nbSpeechFramesBeforeDTX
			ch.inDTX = false
		}
		ch.VADFlags[ch.nFramesEncoded] = 0
	} else {
		ch.noSpeechCounter = 0
		ch.inDTX = false
		ch.indices.signalType = typeUnvoiced
		ch.VADFlags[ch.nFramesEncoded] = 1
	}
}

// encodeFrame encodes one 20 ms (or 10 ms) frame (silk_encode_frame_FLP).
// Returns the payload byte count so far.
func (ch *silkEncoderChannel) encodeFrame(enc *rangeEncoder, condCoding, maxBits int, useCBR bool) int {
	var sEncCtrl silkEncoderControl
	resPitch := make([]float32, 2*silkMaxFrameLen+laPitchMax)

	var bitsMargin int
	if useCBR {
		bitsMargin = 5
	} else {
		bitsMargin = maxBits / 4
	}

	ch.indices.Seed = int8(ch.frameCounter & 3)
	ch.frameCounter++

	xFrameOff := ch.ltpMemLength
	resPitchFrameOff := ch.ltpMemLength

	// Smooth bandwidth transitions.
	ch.sLP.variableCutoff(ch.inputBuf[1:], ch.frameLength)

	// Copy new frame to front of input buffer.
	silkShort2FloatArray(ch.xBuf[xFrameOff+laShapeMS*ch.fsKHz:], ch.inputBuf[1:], ch.frameLength)

	// Avoid denormals.
	for i := 0; i < 8; i++ {
		v := float32(1 - (i & 2))
		ch.xBuf[xFrameOff+laShapeMS*ch.fsKHz+i*(ch.frameLength>>3)] += v * 1e-6
	}

	if !ch.prefillFlag {
		// Pitch, shaping, and prediction analysis.
		ch.findPitchLags(&sEncCtrl, resPitch, ch.xBuf[:])
		ch.noiseShapeAnalysis(&sEncCtrl, resPitch, resPitchFrameOff, ch.xBuf[:], xFrameOff)
		ch.findPredCoefs(&sEncCtrl, resPitch, resPitchFrameOff, ch.xBuf[:], xFrameOff, condCoding)
		ch.processGains(&sEncCtrl, condCoding)
		ch.lbrrEncode(&sEncCtrl, ch.xBuf[xFrameOff:], condCoding)

		// Loop over quantizer and entropy coding to control bitrate.
		maxIter := 6
		gainMultQ8 := int32(silkFixConst(1, 8))
		foundLower := false
		foundUpper := false
		gainsID := silkGainsID(ch.indices.GainsIndices[:], ch.nbSubfr)
		gainsIDLower := int32(-1)
		gainsIDUpper := int32(-1)
		var gainMultLower, gainMultUpper int32
		var nBitsLower, nBitsUpper int32
		var gainLock [silkMaxNBSubfr]bool
		var bestGainMult [silkMaxNBSubfr]int32
		var bestSum [silkMaxNBSubfr]int

		sRangeEncCopy := enc.snapshot()
		sNSQCopy0 := ch.sNSQ
		var sNSQCopy1 silkNSQState
		seedCopy := ch.indices.Seed
		ecPrevLagIndexCopy := ch.ecPrevLagIndex
		ecPrevSignalTypeCopy := ch.ecPrevSignalType
		var sRangeEncCopy2 rangeEncoder
		var ecBufCopy []byte
		var lastGainIndexCopy2 int8
		var pGainsQ16 [silkMaxNBSubfr]int32

		nBits := int32(0)
	iterLoop:
		for iter := 0; ; iter++ {
			switch gainsID {
			case gainsIDLower:
				nBits = nBitsLower
			case gainsIDUpper:
				nBits = nBitsUpper
			default:
				if iter > 0 {
					enc.restore(&sRangeEncCopy)
					ch.sNSQ = sNSQCopy0
					ch.indices.Seed = seedCopy
					ch.ecPrevLagIndex = ecPrevLagIndexCopy
					ch.ecPrevSignalType = ecPrevSignalTypeCopy
				}

				ch.nsqWrapper(&sEncCtrl, &ch.indices, &ch.sNSQ, ch.pulses[:], ch.xBuf[xFrameOff:])

				if iter == maxIter && !foundLower {
					sRangeEncCopy2 = enc.snapshot()
				}

				ch.encodeIndices(enc, ch.nFramesEncoded, false, condCoding)
				silkEncodePulses(enc, ch.indices.signalType, ch.indices.quantOffsetType, ch.pulses[:], ch.frameLength)

				nBits = int32(enc.tell())

				// Damage control on final-iteration bust.
				if iter == maxIter && !foundLower && nBits > int32(maxBits) {
					enc.restore(&sRangeEncCopy2)
					ch.sShape.lastGainIndex = sEncCtrl.lastGainIndexPrev
					for i := 0; i < ch.nbSubfr; i++ {
						ch.indices.GainsIndices[i] = 4
					}
					if condCoding != codeConditionally {
						ch.indices.GainsIndices[0] = sEncCtrl.lastGainIndexPrev
					}
					ch.ecPrevLagIndex = ecPrevLagIndexCopy
					ch.ecPrevSignalType = ecPrevSignalTypeCopy
					for i := 0; i < ch.frameLength; i++ {
						ch.pulses[i] = 0
					}
					ch.encodeIndices(enc, ch.nFramesEncoded, false, condCoding)
					silkEncodePulses(enc, ch.indices.signalType, ch.indices.quantOffsetType, ch.pulses[:], ch.frameLength)
					nBits = int32(enc.tell())
				}

				if !useCBR && iter == 0 && nBits <= int32(maxBits) {
					break iterLoop
				}
			}

			if iter == maxIter {
				if foundLower && (gainsID == gainsIDLower || nBits > int32(maxBits)) {
					// Restore the last budget-respecting output state.
					enc.restore(&sRangeEncCopy2)
					copy(enc.buf[:sRangeEncCopy2.offs], ecBufCopy)
					ch.sNSQ = sNSQCopy1
					ch.sShape.lastGainIndex = lastGainIndexCopy2
				}
				break
			}

			if nBits > int32(maxBits) {
				if !foundLower && iter >= 2 {
					// Harder rate/distortion tradeoff, less dithering.
					sEncCtrl.lambda *= 1.5
					if sEncCtrl.lambda < 1.5 {
						sEncCtrl.lambda = 1.5
					}
					ch.indices.quantOffsetType = 0
					foundUpper = false
					gainsIDUpper = -1
				} else {
					foundUpper = true
					nBitsUpper = nBits
					gainMultUpper = gainMultQ8
					gainsIDUpper = gainsID
				}
			} else if nBits < int32(maxBits-bitsMargin) {
				foundLower = true
				nBitsLower = nBits
				gainMultLower = gainMultQ8
				if gainsID != gainsIDLower {
					gainsIDLower = gainsID
					sRangeEncCopy2 = enc.snapshot()
					ecBufCopy = append(ecBufCopy[:0], enc.buf[:enc.offs]...)
					sNSQCopy1 = ch.sNSQ
					lastGainIndexCopy2 = ch.sShape.lastGainIndex
				}
			} else {
				// Close enough.
				break
			}

			if !foundLower && nBits > int32(maxBits) {
				for i := 0; i < ch.nbSubfr; i++ {
					sum := 0
					for j := i * ch.subfrLength; j < (i+1)*ch.subfrLength; j++ {
						v := int(ch.pulses[j])
						if v < 0 {
							v = -v
						}
						sum += v
					}
					if iter == 0 || (sum < bestSum[i] && !gainLock[i]) {
						bestSum[i] = sum
						bestGainMult[i] = gainMultQ8
					} else {
						gainLock[i] = true
					}
				}
			}
			if !foundLower || !foundUpper {
				// High-rate rate/distortion curve.
				if nBits > int32(maxBits) {
					gainMultQ8 = silkMinInt(1024, gainMultQ8*3/2)
				} else {
					gainMultQ8 = silkMaxInt(64, gainMultQ8*4/5)
				}
			} else {
				// Interpolate; clamp to the middle half of the old range.
				gainMultQ8 = gainMultLower + (gainMultUpper-gainMultLower)*(int32(maxBits)-nBitsLower)/(nBitsUpper-nBitsLower)
				if gainMultQ8 > gainMultLower+(gainMultUpper-gainMultLower)>>2 {
					gainMultQ8 = gainMultLower + (gainMultUpper-gainMultLower)>>2
				} else if gainMultQ8 < gainMultUpper-(gainMultUpper-gainMultLower)>>2 {
					gainMultQ8 = gainMultUpper - (gainMultUpper-gainMultLower)>>2
				}
			}

			for i := 0; i < ch.nbSubfr; i++ {
				var tmp int32
				if gainLock[i] {
					tmp = bestGainMult[i]
				} else {
					tmp = gainMultQ8
				}
				pGainsQ16[i] = silkLSHIFTSAT32(silkSMULWB(sEncCtrl.gainsUnqQ16[i], tmp), 8)
			}

			ch.sShape.lastGainIndex = sEncCtrl.lastGainIndexPrev
			silkGainsQuant(ch.indices.GainsIndices[:], pGainsQ16[:],
				&ch.sShape.lastGainIndex, condCoding == codeConditionally, ch.nbSubfr)
			gainsID = silkGainsID(ch.indices.GainsIndices[:], ch.nbSubfr)

			for i := 0; i < ch.nbSubfr; i++ {
				sEncCtrl.gains[i] = float32(pGainsQ16[i]) / 65536.0
			}
		}
	}

	// Update input buffer.
	copy(ch.xBuf[:ch.ltpMemLength+laShapeMS*ch.fsKHz],
		ch.xBuf[ch.frameLength:ch.frameLength+ch.ltpMemLength+laShapeMS*ch.fsKHz])

	if ch.prefillFlag {
		return 0
	}

	ch.prevLag = sEncCtrl.pitchL[ch.nbSubfr-1]
	ch.prevSignalType = ch.indices.signalType

	ch.firstFrameAfterReset = false
	return (enc.tell() + 7) >> 3
}

// lbrrEncode reuses the frame parameters to code a low-bitrate redundant
// excitation (silk_LBRR_encode_FLP). Inactive unless LBRR is enabled.
func (ch *silkEncoderChannel) lbrrEncode(ctrl *silkEncoderControl, xfw []float32, condCoding int) {
	if !ch.LBRREnabled || ch.speechActivityQ8 <= silkFixConst(lbrrSpeechActivityThres, 8) {
		return
	}
	ch.LBRRFlags[ch.nFramesEncoded] = 1

	sNSQLBRR := ch.sNSQ
	psIndicesLBRR := &ch.indicesLBRR[ch.nFramesEncoded]
	*psIndicesLBRR = ch.indices

	var tempGains [silkMaxNBSubfr]float32
	copy(tempGains[:], ctrl.gains[:ch.nbSubfr])

	if ch.nFramesEncoded == 0 || ch.LBRRFlags[ch.nFramesEncoded-1] == 0 {
		ch.LBRRprevLastGainIndex = ch.sShape.lastGainIndex
		psIndicesLBRR.GainsIndices[0] += ch.LBRRGainIncreases
		psIndicesLBRR.GainsIndices[0] = int8(silkMinInt(int32(psIndicesLBRR.GainsIndices[0]), nLevelsQGain-1))
	}

	var gainsQ16 [silkMaxNBSubfr]int32
	silkGainsDequant(gainsQ16[:], psIndicesLBRR.GainsIndices[:], &ch.LBRRprevLastGainIndex,
		condCoding == codeConditionally, ch.nbSubfr)
	for k := 0; k < ch.nbSubfr; k++ {
		ctrl.gains[k] = float32(gainsQ16[k]) * (1.0 / 65536.0)
	}

	ch.nsqWrapper(ctrl, psIndicesLBRR, &sNSQLBRR, ch.pulsesLBRR[ch.nFramesEncoded][:], xfw)

	copy(ctrl.gains[:ch.nbSubfr], tempGains[:ch.nbSubfr])
}

// encode buffers and encodes input, driving the per-frame encoder
// (silk_Encode). samplesIn holds nSamplesIn samples per channel, interleaved
// for stereo, at the API rate. Returns the payload size in bytes (0 until a
// whole packet is complete). maxBits bounds the packet size in bits.
func (e *silkEncoder) encode(encControl *silkEncControl, samplesIn []int16, nSamplesIn int,
	enc *rangeEncoder, nBytesOut *int, prefillFlag int, activity int) {

	var mStargetRatesBps [2]int32

	if encControl.reducedDependency {
		e.channel[0].firstFrameAfterReset = true
		e.channel[1].firstFrameAfterReset = true
	}
	for n := 0; n < encControl.nChannelsAPI; n++ {
		e.channel[n].nFramesEncoded = 0
	}

	encControl.switchReady = false
	if encControl.nChannelsInternal > e.nChannelsInternal {
		// Mono to stereo: init the second channel and the stereo state.
		e.channel[1].initEncoderChannel()
		e.sStereo.predPrevQ13 = [2]int16{}
		e.sStereo.sSide = [2]int16{}
		e.sStereo.midSideAmpQ0[0] = 0
		e.sStereo.midSideAmpQ0[1] = 1
		e.sStereo.midSideAmpQ0[2] = 0
		e.sStereo.midSideAmpQ0[3] = 1
		e.sStereo.widthPrevQ14 = 0
		e.sStereo.smthWidthQ14 = int16(silkFixConst(1, 14))
		if e.nChannelsAPI == 2 {
			e.channel[1].resampler = e.channel[0].resampler
			e.channel[1].inHPState = e.channel[0].inHPState
		}
	}

	transition := encControl.payloadSizeMS != e.channel[0].packetSizeMS ||
		e.nChannelsInternal != encControl.nChannelsInternal

	e.nChannelsAPI = encControl.nChannelsAPI
	e.nChannelsInternal = encControl.nChannelsInternal

	nBlocksOf10ms := 100 * nSamplesIn / encControl.apiSampleRate
	totBlocks := 1
	if nBlocksOf10ms > 1 {
		totBlocks = nBlocksOf10ms >> 1
	}
	currBlock := 0
	var tmpPayloadSizeMS, tmpComplexity int
	if prefillFlag != 0 {
		var saveLP silkLPState
		var savedFs int
		if prefillFlag == 2 {
			saveLP = e.channel[0].sLP
			savedFs = e.channel[0].fsKHz
		}
		for n := 0; n < encControl.nChannelsInternal; n++ {
			e.channel[n].initEncoderChannel()
			if prefillFlag == 2 {
				e.channel[n].sLP = saveLP
				e.channel[n].savedFsKHz = savedFs
			}
		}
		tmpPayloadSizeMS = encControl.payloadSizeMS
		encControl.payloadSizeMS = 10
		tmpComplexity = encControl.complexity
		encControl.complexity = 0
		for n := 0; n < encControl.nChannelsInternal; n++ {
			e.channel[n].controlledSinceLastPayload = false
			e.channel[n].prefillFlag = true
		}
	}

	for n := 0; n < encControl.nChannelsInternal; n++ {
		forceFsKHz := 0
		if n == 1 {
			forceFsKHz = e.channel[0].fsKHz
		}
		e.channel[n].controlEncoder(encControl, e.allowBandwidthSwitch, n, forceFsKHz)
		if e.channel[n].firstFrameAfterReset || transition {
			for i := 0; i < e.channel[0].nFramesPerPacket; i++ {
				e.channel[n].LBRRFlags[i] = 0
			}
		}
		e.channel[n].inDTX = e.channel[n].useDTX
	}

	// Input buffering/resampling and encoding.
	nSamplesToBufferMax := 10 * nBlocksOf10ms * e.channel[0].fsKHz
	nSamplesFromInputMax := nSamplesToBufferMax * e.channel[0].apiFsHz / (e.channel[0].fsKHz * 1000)
	buf := make([]int16, nSamplesFromInputMax)

	samplesPos := 0
	for {
		nSamplesToBuffer := e.channel[0].frameLength - e.channel[0].inputBufIx
		if nSamplesToBuffer > nSamplesToBufferMax {
			nSamplesToBuffer = nSamplesToBufferMax
		}
		nSamplesFromInput := nSamplesToBuffer * e.channel[0].apiFsHz / (e.channel[0].fsKHz * 1000)

		switch {
		case encControl.nChannelsAPI == 2 && encControl.nChannelsInternal == 2:
			id := e.channel[0].nFramesEncoded
			for n := 0; n < nSamplesFromInput; n++ {
				buf[n] = samplesIn[samplesPos+2*n]
			}
			if e.nPrevChannelsInternal == 1 && id == 0 {
				e.channel[1].resampler = e.channel[0].resampler
			}
			e.channel[0].resampler.resample(e.channel[0].inputBuf[e.channel[0].inputBufIx+2:], buf, nSamplesFromInput)
			e.channel[0].inputBufIx += nSamplesToBuffer

			nSamplesToBuffer = e.channel[1].frameLength - e.channel[1].inputBufIx
			if v := 10 * nBlocksOf10ms * e.channel[1].fsKHz; nSamplesToBuffer > v {
				nSamplesToBuffer = v
			}
			for n := 0; n < nSamplesFromInput; n++ {
				buf[n] = samplesIn[samplesPos+2*n+1]
			}
			e.channel[1].resampler.resample(e.channel[1].inputBuf[e.channel[1].inputBufIx+2:], buf, nSamplesFromInput)
			e.channel[1].inputBufIx += nSamplesToBuffer

		case encControl.nChannelsAPI == 2 && encControl.nChannelsInternal == 1:
			for n := 0; n < nSamplesFromInput; n++ {
				sum := int32(samplesIn[samplesPos+2*n]) + int32(samplesIn[samplesPos+2*n+1])
				buf[n] = int16(silkRSHIFTROUND(sum, 1))
			}
			e.channel[0].resampler.resample(e.channel[0].inputBuf[e.channel[0].inputBufIx+2:], buf, nSamplesFromInput)
			if e.nPrevChannelsInternal == 2 && e.channel[0].nFramesEncoded == 0 {
				e.channel[1].resampler.resample(e.channel[1].inputBuf[e.channel[1].inputBufIx+2:], buf, nSamplesFromInput)
				for n := 0; n < e.channel[0].frameLength; n++ {
					e.channel[0].inputBuf[e.channel[0].inputBufIx+n+2] =
						int16((int32(e.channel[0].inputBuf[e.channel[0].inputBufIx+n+2]) +
							int32(e.channel[1].inputBuf[e.channel[1].inputBufIx+n+2])) >> 1)
				}
			}
			e.channel[0].inputBufIx += nSamplesToBuffer

		default:
			copy(buf[:nSamplesFromInput], samplesIn[samplesPos:samplesPos+nSamplesFromInput])
			e.channel[0].resampler.resample(e.channel[0].inputBuf[e.channel[0].inputBufIx+2:], buf, nSamplesFromInput)
			e.channel[0].inputBufIx += nSamplesToBuffer
		}

		samplesPos += nSamplesFromInput * encControl.nChannelsAPI
		nSamplesIn -= nSamplesFromInput

		e.allowBandwidthSwitch = false

		if e.channel[0].inputBufIx < e.channel[0].frameLength {
			break
		}

		// Enough data buffered: encode one frame per channel.
		currNBitsUsedLBRR := 0
		if e.channel[0].nFramesEncoded == 0 && prefillFlag == 0 {
			// Space for VAD and FEC flags.
			iCDF := [2]uint8{0, 0}
			iCDF[0] = uint8(256 - silkRSHIFT(256, (e.channel[0].nFramesPerPacket+1)*encControl.nChannelsInternal))
			enc.encodeICDF(0, iCDF[:], 8)
			currNBitsUsedLBRR = enc.tell()

			// LBRR flags and data from the previous packet.
			for n := 0; n < encControl.nChannelsInternal; n++ {
				lbrrSymbol := int32(0)
				for i := 0; i < e.channel[n].nFramesPerPacket; i++ {
					lbrrSymbol |= silkLSHIFT(int32(e.channel[n].LBRRFlags[i]), i)
				}
				if lbrrSymbol > 0 {
					e.channel[n].LBRRFlag = 1
				} else {
					e.channel[n].LBRRFlag = 0
				}
				if lbrrSymbol != 0 && e.channel[n].nFramesPerPacket > 1 {
					enc.encodeICDF(int(lbrrSymbol-1), silkLBRRFlagsICDFPtr[e.channel[n].nFramesPerPacket-2], 8)
				}
			}
			for i := 0; i < e.channel[0].nFramesPerPacket; i++ {
				for n := 0; n < encControl.nChannelsInternal; n++ {
					if e.channel[n].LBRRFlags[i] != 0 {
						if encControl.nChannelsInternal == 2 && n == 0 {
							silkStereoEncodePred(enc, &e.sStereo.predIx[i])
							if e.channel[1].LBRRFlags[i] == 0 {
								silkStereoEncodeMidOnly(enc, e.sStereo.midOnlyFlags[i])
							}
						}
						condCoding := codeIndependently
						if i > 0 && e.channel[n].LBRRFlags[i-1] != 0 {
							condCoding = codeConditionally
						}
						e.channel[n].encodeIndices(enc, i, true, condCoding)
						silkEncodePulses(enc, e.channel[n].indicesLBRR[i].signalType,
							e.channel[n].indicesLBRR[i].quantOffsetType,
							e.channel[n].pulsesLBRR[i][:], e.channel[n].frameLength)
					}
				}
			}
			for n := 0; n < encControl.nChannelsInternal; n++ {
				e.channel[n].LBRRFlags = [maxFramesPerPacket]int{}
			}
			currNBitsUsedLBRR = enc.tell() - currNBitsUsedLBRR
		}

		// Adaptive high-pass tracking on channel 0.
		e.channel[0].variableHPSmth1Q15 = silkHPVariableCutoff(e.channel[0].variableHPSmth1Q15,
			int(e.channel[0].prevSignalType), e.channel[0].prevLag, e.channel[0].fsKHz,
			e.channel[0].inputQualityBandsQ15[0], e.channel[0].speechActivityQ8)

		// Total target bits for the packet.
		nBits := int(encControl.bitRate) * encControl.payloadSizeMS / 1000
		if prefillFlag == 0 {
			// Exponential moving average of LBRR usage.
			if currNBitsUsedLBRR < 10 {
				e.nBitsUsedLBRR = 0
			} else if e.nBitsUsedLBRR < 10 {
				e.nBitsUsedLBRR = int32(currNBitsUsedLBRR)
			} else {
				e.nBitsUsedLBRR = (e.nBitsUsedLBRR + int32(currNBitsUsedLBRR)) / 2
			}
			nBits -= int(e.nBitsUsedLBRR)
		}
		nBits /= e.channel[0].nFramesPerPacket
		var targetRateBps int32
		if encControl.payloadSizeMS == 10 {
			targetRateBps = int32(nBits) * 100
		} else {
			targetRateBps = int32(nBits) * 50
		}
		targetRateBps -= e.nBitsExceeded * 1000 / bitreservoirDecayTimeMS
		if prefillFlag == 0 && e.channel[0].nFramesEncoded > 0 {
			bitsBalance := int32(enc.tell()) - e.nBitsUsedLBRR - int32(nBits*e.channel[0].nFramesEncoded)
			targetRateBps -= bitsBalance * 1000 / bitreservoirDecayTimeMS
		}
		targetRateBps = silkLIMIT(targetRateBps, encControl.bitRate, 5000)

		// Left/Right to Mid/Side.
		if encControl.nChannelsInternal == 2 {
			e.sStereo.lrToMS(e.channel[0].inputBuf[:], e.channel[1].inputBuf[:],
				&e.sStereo.predIx[e.channel[0].nFramesEncoded],
				&e.sStereo.midOnlyFlags[e.channel[0].nFramesEncoded],
				mStargetRatesBps[:], targetRateBps, e.channel[0].speechActivityQ8,
				encControl.toMono, e.channel[0].fsKHz, e.channel[0].frameLength)
			if e.sStereo.midOnlyFlags[e.channel[0].nFramesEncoded] == 0 {
				if e.prevDecodeOnlyMiddle == 1 {
					side := &e.channel[1]
					side.sShape = silkShapeState{}
					side.sNSQ = silkNSQState{}
					side.prevNLSFqQ15 = [silkMaxLPCOrder]int16{}
					side.sLP.inLPState = [2]int32{}
					side.prevLag = 100
					side.sNSQ.lagPrev = 100
					side.sShape.lastGainIndex = 10
					side.prevSignalType = typeNoVoiceActivity
					side.sNSQ.prevGainQ16 = 65536
					side.firstFrameAfterReset = true
				}
				e.channel[1].encodeDoVAD(activity)
			} else {
				e.channel[1].VADFlags[e.channel[0].nFramesEncoded] = 0
			}
			if prefillFlag == 0 {
				silkStereoEncodePred(enc, &e.sStereo.predIx[e.channel[0].nFramesEncoded])
				if e.channel[1].VADFlags[e.channel[0].nFramesEncoded] == 0 {
					silkStereoEncodeMidOnly(enc, e.sStereo.midOnlyFlags[e.channel[0].nFramesEncoded])
				}
			}
		} else {
			// Buffering.
			copy(e.channel[0].inputBuf[:2], e.sStereo.sMid[:])
			copy(e.sStereo.sMid[:], e.channel[0].inputBuf[e.channel[0].frameLength:e.channel[0].frameLength+2])
		}
		e.channel[0].encodeDoVAD(activity)

		for n := 0; n < encControl.nChannelsInternal; n++ {
			// Rate constraints.
			maxBits := encControl.maxBits
			if totBlocks == 2 && currBlock == 0 {
				maxBits = maxBits * 3 / 5
			} else if totBlocks == 3 {
				// Cap the first two blocks so the last of three keeps its
				// full budget.
				switch currBlock {
				case 0:
					maxBits = maxBits * 2 / 5
				case 1:
					maxBits = maxBits * 3 / 4
				}
			}
			useCBR := encControl.useCBR && currBlock == totBlocks-1

			var channelRateBps int32
			if encControl.nChannelsInternal == 1 {
				channelRateBps = targetRateBps
			} else {
				channelRateBps = mStargetRatesBps[n]
				if n == 0 && mStargetRatesBps[1] > 0 {
					useCBR = false
					maxBits -= encControl.maxBits / (totBlocks * 2)
				}
			}

			if channelRateBps > 0 {
				e.channel[n].controlSNR(channelRateBps)
				condCoding := codeIndependently
				if e.channel[0].nFramesEncoded-n > 0 {
					if n > 0 && e.prevDecodeOnlyMiddle == 1 {
						condCoding = codeIndependentlyNoLTPScale
					} else {
						condCoding = codeConditionally
					}
				}
				*nBytesOut = e.channel[n].encodeFrame(enc, condCoding, maxBits, useCBR)
			}
			e.channel[n].controlledSinceLastPayload = false
			e.channel[n].inputBufIx = 0
			e.channel[n].nFramesEncoded++
		}
		e.prevDecodeOnlyMiddle = int(e.sStereo.midOnlyFlags[e.channel[0].nFramesEncoded-1])

		// Insert VAD and FEC flags at the front of the bitstream.
		if *nBytesOut > 0 && e.channel[0].nFramesEncoded == e.channel[0].nFramesPerPacket {
			flags := int32(0)
			for n := 0; n < encControl.nChannelsInternal; n++ {
				for i := 0; i < e.channel[n].nFramesPerPacket; i++ {
					flags = silkLSHIFT(flags, 1)
					flags |= int32(e.channel[n].VADFlags[i])
				}
				flags = silkLSHIFT(flags, 1)
				flags |= int32(e.channel[n].LBRRFlag)
			}
			if prefillFlag == 0 {
				enc.patchInitialBits(uint32(flags), uint((e.channel[0].nFramesPerPacket+1)*encControl.nChannelsInternal))
			}
			// DTX on all channels: no payload.
			if e.channel[0].inDTX && (encControl.nChannelsInternal == 1 || e.channel[1].inDTX) {
				*nBytesOut = 0
			}
			e.nBitsExceeded += int32(*nBytesOut) * 8
			e.nBitsExceeded -= encControl.bitRate * int32(encControl.payloadSizeMS) / 1000
			e.nBitsExceeded = silkLIMIT(e.nBitsExceeded, 0, 10000)

			speechActThrForSwitchQ8 := silkSMLAWB(silkFixConst(speechActivityDTXThres, 8),
				silkFixConst((1-speechActivityDTXThres)/maxBandwidthSwitchDelayMS, 16+8),
				int32(e.timeSinceSwitchAllowedMS))
			if e.channel[0].speechActivityQ8 < speechActThrForSwitchQ8 {
				e.allowBandwidthSwitch = true
				e.timeSinceSwitchAllowedMS = 0
			} else {
				e.allowBandwidthSwitch = false
				e.timeSinceSwitchAllowedMS += encControl.payloadSizeMS
			}
		}

		if nSamplesIn == 0 {
			break
		}
		currBlock++
	}

	e.nPrevChannelsInternal = encControl.nChannelsInternal

	encControl.allowBandwidthSwitch = e.allowBandwidthSwitch
	encControl.inWBmodeWithoutVariableLP = e.channel[0].fsKHz == 16 && e.channel[0].sLP.mode == 0
	encControl.internalSampleRate = e.channel[0].fsKHz * 1000
	if encControl.toMono {
		encControl.stereoWidthQ14 = 0
	} else {
		encControl.stereoWidthQ14 = e.sStereo.smthWidthQ14
	}
	if prefillFlag != 0 {
		encControl.payloadSizeMS = tmpPayloadSizeMS
		encControl.complexity = tmpComplexity
		for n := 0; n < encControl.nChannelsInternal; n++ {
			e.channel[n].controlledSinceLastPayload = false
			e.channel[n].prefillFlag = false
		}
	}
	encControl.signalType = e.channel[0].indices.signalType
	encControl.offset = silk_Quantization_Offsets_Q10[e.channel[0].indices.signalType>>1][e.channel[0].indices.quantOffsetType]
}
