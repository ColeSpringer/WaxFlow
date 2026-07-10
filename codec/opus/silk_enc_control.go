package opus

// SILK encoder control, ported from libopus silk/control_codec.c,
// control_SNR.c, control_audio_bandwidth.c, and init_encoder.c.

// silkEncControl mirrors silk_EncControlStruct (silk/control.h), with the
// DRED/OSCE fields dropped (out of scope like the decoder-side neural paths).
type silkEncControl struct {
	nChannelsAPI              int
	nChannelsInternal         int
	apiSampleRate             int
	maxInternalSampleRate     int
	minInternalSampleRate     int
	desiredInternalSampleRate int
	payloadSizeMS             int
	bitRate                   int32
	packetLossPercentage      int
	complexity                int
	useInBandFEC              bool
	lbrrCoded                 bool
	useDTX                    bool
	useCBR                    bool
	maxBits                   int
	toMono                    bool
	opusCanSwitch             bool
	reducedDependency         bool

	// Outputs.
	internalSampleRate        int
	allowBandwidthSwitch      bool
	inWBmodeWithoutVariableLP bool
	stereoWidthQ14            int16
	switchReady               bool
	signalType                int8
	offset                    int16
}

// initEncoderChannel resets one channel state (silk_init_encoder).
func (ch *silkEncoderChannel) initEncoderChannel() {
	*ch = silkEncoderChannel{}
	ch.variableHPSmth1Q15 = silkLSHIFT(silkLin2Log(silkFixConst(variableHPMinCutoffHz, 16))-16<<7, 8)
	ch.variableHPSmth2Q15 = ch.variableHPSmth1Q15
	ch.firstFrameAfterReset = true
	ch.sVAD.init()
}

// controlEncoder configures the channel per the control struct
// (silk_control_encoder).
func (ch *silkEncoderChannel) controlEncoder(encControl *silkEncControl, allowBWSwitch bool, channelNb int, forceFsKHz int) {
	ch.useDTX = encControl.useDTX
	ch.useCBR = encControl.useCBR
	ch.apiFsHz = encControl.apiSampleRate
	ch.maxInternalFsHz = encControl.maxInternalSampleRate
	ch.minInternalFsHz = encControl.minInternalSampleRate
	ch.desiredInternalFsHz = encControl.desiredInternalSampleRate
	ch.useInBandFEC = encControl.useInBandFEC
	ch.nChannelsAPI = encControl.nChannelsAPI
	ch.nChannelsInternal = encControl.nChannelsInternal
	ch.allowBandwidthSwitch = allowBWSwitch
	ch.channelNb = channelNb

	if ch.controlledSinceLastPayload && !ch.prefillFlag {
		if ch.apiFsHz != ch.prevAPIFsHz && ch.fsKHz > 0 {
			ch.setupResamplers(ch.fsKHz)
		}
		return
	}

	fsKHz := ch.controlAudioBandwidth(encControl)
	if forceFsKHz != 0 {
		fsKHz = forceFsKHz
	}
	ch.setupResamplers(fsKHz)
	ch.setupFS(fsKHz, encControl.payloadSizeMS)
	ch.setupComplexity(encControl.complexity)
	ch.packetLossPerc = encControl.packetLossPercentage
	ch.setupLBRR(encControl)
	ch.controlledSinceLastPayload = true
}

// setupResamplers prepares the API-rate to internal-rate resampler,
// re-buffering the analysis history at the new rate (silk_setup_resamplers).
func (ch *silkEncoderChannel) setupResamplers(fsKHz int) {
	if ch.fsKHz != fsKHz || ch.prevAPIFsHz != ch.apiFsHz {
		if ch.fsKHz == 0 {
			ch.resampler.init(ch.apiFsHz, fsKHz*1000, true)
		} else {
			bufLengthMS := silkLSHIFTint(ch.nbSubfr*5, 1) + laShapeMS
			oldBufSamples := bufLengthMS * ch.fsKHz
			newBufSamples := bufLengthMS * fsKHz
			maxS := oldBufSamples
			if newBufSamples > maxS {
				maxS = newBufSamples
			}
			xBufFIX := make([]int16, maxS)
			silkFloat2ShortArray(xBufFIX, ch.xBuf[:], oldBufSamples)

			var tempResampler silkResampler
			tempResampler.init(ch.fsKHz*1000, ch.apiFsHz, false)
			apiBufSamples := bufLengthMS * (ch.apiFsHz / 1000)
			xBufAPIFsHz := make([]int16, apiBufSamples)
			tempResampler.resample(xBufAPIFsHz, xBufFIX, oldBufSamples)

			ch.resampler.init(ch.apiFsHz, fsKHz*1000, true)
			ch.resampler.resample(xBufFIX, xBufAPIFsHz, apiBufSamples)
			silkShort2FloatArray(ch.xBuf[:], xBufFIX, newBufSamples)
		}
	}
	ch.prevAPIFsHz = ch.apiFsHz
}

// setupFS sets the packet size and internal sampling frequency
// (silk_setup_fs).
func (ch *silkEncoderChannel) setupFS(fsKHz, packetSizeMS int) {
	if packetSizeMS != ch.packetSizeMS {
		if packetSizeMS <= 10 {
			ch.nFramesPerPacket = 1
			if packetSizeMS == 10 {
				ch.nbSubfr = 2
			} else {
				ch.nbSubfr = 1
			}
			ch.frameLength = packetSizeMS * fsKHz
			ch.pitchLPCWinLength = findPitchLPCWinMS2SF * fsKHz
			if ch.fsKHz == 8 {
				ch.pitchContourICDF = silk_pitch_contour_10_ms_NB_iCDF
			} else {
				ch.pitchContourICDF = silk_pitch_contour_10_ms_iCDF
			}
		} else {
			ch.nFramesPerPacket = packetSizeMS / 20 // MAX_FRAME_LENGTH_MS
			ch.nbSubfr = silkMaxNBSubfr
			ch.frameLength = 20 * fsKHz
			ch.pitchLPCWinLength = findPitchLPCWinMS * fsKHz
			if ch.fsKHz == 8 {
				ch.pitchContourICDF = silk_pitch_contour_NB_iCDF
			} else {
				ch.pitchContourICDF = silk_pitch_contour_iCDF
			}
		}
		ch.packetSizeMS = packetSizeMS
		ch.targetRateBps = 0 // trigger new SNR computation
	}

	if ch.fsKHz != fsKHz {
		ch.sShape = silkShapeState{}
		ch.sNSQ = silkNSQState{}
		ch.prevNLSFqQ15 = [silkMaxLPCOrder]int16{}
		ch.sLP.inLPState = [2]int32{}
		ch.inputBufIx = 0
		ch.nFramesEncoded = 0
		ch.targetRateBps = 0
		ch.prevLag = 100
		ch.firstFrameAfterReset = true
		ch.sShape.lastGainIndex = 10
		ch.sNSQ.lagPrev = 100
		ch.sNSQ.prevGainQ16 = 65536
		ch.prevSignalType = typeNoVoiceActivity

		ch.fsKHz = fsKHz
		if ch.fsKHz == 8 {
			if ch.nbSubfr == silkMaxNBSubfr {
				ch.pitchContourICDF = silk_pitch_contour_NB_iCDF
			} else {
				ch.pitchContourICDF = silk_pitch_contour_10_ms_NB_iCDF
			}
		} else {
			if ch.nbSubfr == silkMaxNBSubfr {
				ch.pitchContourICDF = silk_pitch_contour_iCDF
			} else {
				ch.pitchContourICDF = silk_pitch_contour_10_ms_iCDF
			}
		}
		if ch.fsKHz == 8 || ch.fsKHz == 12 {
			ch.predictLPCOrder = silkMinLPCOrder
			ch.psNLSFCB = silkNLSFCBNBMB
		} else {
			ch.predictLPCOrder = silkMaxLPCOrder
			ch.psNLSFCB = silkNLSFCBWB
		}
		ch.subfrLength = silkSubFrameMS * fsKHz
		ch.frameLength = ch.subfrLength * ch.nbSubfr
		ch.ltpMemLength = silkLTPMemMS * fsKHz
		ch.laPitch = laPitchMS * fsKHz
		ch.maxPitchLag = 18 * fsKHz
		if ch.nbSubfr == silkMaxNBSubfr {
			ch.pitchLPCWinLength = findPitchLPCWinMS * fsKHz
		} else {
			ch.pitchLPCWinLength = findPitchLPCWinMS2SF * fsKHz
		}
		switch ch.fsKHz {
		case 16:
			ch.pitchLagLowBitsICDF = silk_uniform8_iCDF
		case 12:
			ch.pitchLagLowBitsICDF = silk_uniform6_iCDF
		default:
			ch.pitchLagLowBitsICDF = silk_uniform4_iCDF
		}
	}
}

// setupComplexity maps the 0..10 complexity setting onto the analysis knobs
// (silk_setup_complexity).
func (ch *silkEncoderChannel) setupComplexity(complexity int) {
	switch {
	case complexity < 1:
		ch.pitchEstimationComplexity = silkPEMinComplex
		ch.pitchEstimationThresholdQ16 = silkFixConst(0.8, 16)
		ch.pitchEstimationLPCOrder = 6
		ch.shapingLPCOrder = 12
		ch.laShape = 3 * ch.fsKHz
		ch.nStatesDelayedDecision = 1
		ch.useInterpolatedNLSFs = false
		ch.nlsfMSVQSurvivors = 2
		ch.warpingQ16 = 0
	case complexity < 2:
		ch.pitchEstimationComplexity = silkPEMidComplex
		ch.pitchEstimationThresholdQ16 = silkFixConst(0.76, 16)
		ch.pitchEstimationLPCOrder = 8
		ch.shapingLPCOrder = 14
		ch.laShape = 5 * ch.fsKHz
		ch.nStatesDelayedDecision = 1
		ch.useInterpolatedNLSFs = false
		ch.nlsfMSVQSurvivors = 3
		ch.warpingQ16 = 0
	case complexity < 3:
		ch.pitchEstimationComplexity = silkPEMinComplex
		ch.pitchEstimationThresholdQ16 = silkFixConst(0.8, 16)
		ch.pitchEstimationLPCOrder = 6
		ch.shapingLPCOrder = 12
		ch.laShape = 3 * ch.fsKHz
		ch.nStatesDelayedDecision = 2
		ch.useInterpolatedNLSFs = false
		ch.nlsfMSVQSurvivors = 2
		ch.warpingQ16 = 0
	case complexity < 4:
		ch.pitchEstimationComplexity = silkPEMidComplex
		ch.pitchEstimationThresholdQ16 = silkFixConst(0.76, 16)
		ch.pitchEstimationLPCOrder = 8
		ch.shapingLPCOrder = 14
		ch.laShape = 5 * ch.fsKHz
		ch.nStatesDelayedDecision = 2
		ch.useInterpolatedNLSFs = false
		ch.nlsfMSVQSurvivors = 4
		ch.warpingQ16 = 0
	case complexity < 6:
		ch.pitchEstimationComplexity = silkPEMidComplex
		ch.pitchEstimationThresholdQ16 = silkFixConst(0.74, 16)
		ch.pitchEstimationLPCOrder = 10
		ch.shapingLPCOrder = 16
		ch.laShape = 5 * ch.fsKHz
		ch.nStatesDelayedDecision = 2
		ch.useInterpolatedNLSFs = true
		ch.nlsfMSVQSurvivors = 6
		ch.warpingQ16 = int32(ch.fsKHz) * silkFixConst(warpingMultiplier, 16)
	case complexity < 8:
		ch.pitchEstimationComplexity = silkPEMidComplex
		ch.pitchEstimationThresholdQ16 = silkFixConst(0.72, 16)
		ch.pitchEstimationLPCOrder = 12
		ch.shapingLPCOrder = 20
		ch.laShape = 5 * ch.fsKHz
		ch.nStatesDelayedDecision = 3
		ch.useInterpolatedNLSFs = true
		ch.nlsfMSVQSurvivors = 8
		ch.warpingQ16 = int32(ch.fsKHz) * silkFixConst(warpingMultiplier, 16)
	default:
		ch.pitchEstimationComplexity = silkPEMaxComplex
		ch.pitchEstimationThresholdQ16 = silkFixConst(0.7, 16)
		ch.pitchEstimationLPCOrder = 16
		ch.shapingLPCOrder = 24
		ch.laShape = 5 * ch.fsKHz
		ch.nStatesDelayedDecision = maxDelDecStates
		ch.useInterpolatedNLSFs = true
		ch.nlsfMSVQSurvivors = 16
		ch.warpingQ16 = int32(ch.fsKHz) * silkFixConst(warpingMultiplier, 16)
	}

	if ch.pitchEstimationLPCOrder > ch.predictLPCOrder {
		ch.pitchEstimationLPCOrder = ch.predictLPCOrder
	}
	ch.shapeWinLength = silkSubFrameMS*ch.fsKHz + 2*ch.laShape
	ch.complexity = complexity
}

// setupLBRR configures low-bitrate-redundancy coding (silk_setup_LBRR).
func (ch *silkEncoderChannel) setupLBRR(encControl *silkEncControl) {
	lbrrInPreviousPacket := ch.LBRREnabled
	ch.LBRREnabled = encControl.lbrrCoded
	if ch.LBRREnabled {
		if !lbrrInPreviousPacket {
			ch.LBRRGainIncreases = 7
		} else {
			v := 7 - silkSMULWB(int32(ch.packetLossPerc), silkFixConst(0.2, 16))
			if v < 3 {
				v = 3
			}
			ch.LBRRGainIncreases = int8(v)
		}
	}
}

// controlSNR maps the target rate to the SNR setting via the measured
// rate curves (silk_control_SNR).
func (ch *silkEncoderChannel) controlSNR(targetRateBps int32) {
	ch.targetRateBps = targetRateBps
	if ch.nbSubfr == 2 {
		targetRateBps -= int32(2000 + ch.fsKHz/16)
	}
	var snrTable []uint8
	switch ch.fsKHz {
	case 8:
		snrTable = silk_TargetRate_NB_21
	case 12:
		snrTable = silk_TargetRate_MB_21
	default:
		snrTable = silk_TargetRate_WB_21
	}
	id := int((targetRateBps + 200) / 400)
	id = silkMinIntGo(id-10, len(snrTable)-1)
	if id <= 0 {
		ch.snrDBQ7 = 0
	} else {
		ch.snrDBQ7 = int32(snrTable[id]) * 21
	}
}

// controlAudioBandwidth runs the internal-sampling-rate state machine
// (silk_control_audio_bandwidth). The sLP.saved_fs_kHz path is only needed
// for prefill-style resets, which store the rate in the LP state; we keep a
// direct field instead.
func (ch *silkEncoderChannel) controlAudioBandwidth(encControl *silkEncControl) int {
	origKHz := ch.fsKHz
	if origKHz == 0 {
		origKHz = ch.savedFsKHz
	}
	fsKHz := origKHz
	fsHz := fsKHz * 1000
	if fsHz == 0 {
		fsHz = silkMinIntGo(ch.desiredInternalFsHz, ch.apiFsHz)
		fsKHz = fsHz / 1000
	} else if fsHz > ch.apiFsHz || fsHz > ch.maxInternalFsHz || fsHz < ch.minInternalFsHz {
		fsHz = ch.apiFsHz
		fsHz = silkMinIntGo(fsHz, ch.maxInternalFsHz)
		fsHz = silkMaxIntGo(fsHz, ch.minInternalFsHz)
		fsKHz = fsHz / 1000
	} else {
		if ch.sLP.transitionFrameNo >= transitionFrames {
			ch.sLP.mode = 0
		}
		if ch.allowBandwidthSwitch || encControl.opusCanSwitch {
			if origKHz*1000 > ch.desiredInternalFsHz {
				// Switch down.
				if ch.sLP.mode == 0 {
					ch.sLP.transitionFrameNo = transitionFrames
					ch.sLP.inLPState = [2]int32{}
				}
				if encControl.opusCanSwitch {
					ch.sLP.mode = 0
					if origKHz == 16 {
						fsKHz = 12
					} else {
						fsKHz = 8
					}
				} else if ch.sLP.transitionFrameNo <= 0 {
					encControl.switchReady = true
					encControl.maxBits -= encControl.maxBits * 5 / (encControl.payloadSizeMS + 5)
				} else {
					ch.sLP.mode = -2 // down at double speed
				}
			} else if origKHz*1000 < ch.desiredInternalFsHz {
				// Switch up.
				if encControl.opusCanSwitch {
					if origKHz == 8 {
						fsKHz = 12
					} else {
						fsKHz = 16
					}
					ch.sLP.transitionFrameNo = 0
					ch.sLP.inLPState = [2]int32{}
					ch.sLP.mode = 1
				} else if ch.sLP.mode == 0 {
					encControl.switchReady = true
					encControl.maxBits -= encControl.maxBits * 5 / (encControl.payloadSizeMS + 5)
				} else {
					ch.sLP.mode = 1
				}
			} else if ch.sLP.mode < 0 {
				ch.sLP.mode = 1
			}
		}
	}
	return fsKHz
}
