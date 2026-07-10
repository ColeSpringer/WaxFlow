package opus

// SILK encoder state, ported from libopus silk/structs.h (silk_encoder_state,
// silk_nsq_state, stereo_enc_state) and silk/float/structs_FLP.h
// (silk_shape_state_FLP, silk_encoder_state_FLP, silk_encoder_control_FLP,
// silk_encoder). The analysis chain is the reference's float build (FLP);
// quantization and entropy coding are the shared fixed-point core, matching
// the decoder bit-for-bit.

// Encoder constants (silk/define.h).
const (
	silkMaxFsKHz           = 16
	laShapeMS              = 5
	laShapeMax             = laShapeMS * silkMaxFsKHz
	laPitchMS              = 2
	laPitchMax             = laPitchMS * silkMaxFsKHz
	findPitchLPCWinMS      = 20 + laPitchMS<<1
	findPitchLPCWinMS2SF   = 10 + laPitchMS<<1
	shapeLPCWinMax         = 15 * silkMaxFsKHz
	maxShapeLPCOrder       = 24
	harmShapeFIRTaps       = 3
	maxDelDecStates        = 4
	decisionDelay          = 40
	nsqLPCBufLength        = silkMaxLPCOrder
	maxFramesPerPacket     = 3
	encoderNumChannels     = 2
	maxPredictionPowerGain = 1e4
	silkMaxOrderLPC        = 24 // MAX_ORDER_LPC (whitening filter order cap)
)

// silkNSQState is the noise shaping quantizer state (silk_nsq_state).
type silkNSQState struct {
	xq            [2 * silkMaxFrameLen]int16
	sLTPShpQ14    [2 * silkMaxFrameLen]int32
	sLPCQ14       [silkMaxSubfrLen + nsqLPCBufLength]int32
	sAR2Q14       [maxShapeLPCOrder]int32
	sLFARShpQ14   int32
	sDiffShpQ14   int32
	lagPrev       int
	sLTPBufIdx    int
	sLTPShpBufIdx int
	randSeed      int32
	prevGainQ16   int32
	rewhiteFlag   int
}

// reset restores the initial quantizer state (part of silk_init_encoder).
func (s *silkNSQState) reset() {
	*s = silkNSQState{}
	s.lagPrev = 100
	s.prevGainQ16 = 65536
}

// silkShapeState is the noise shaping analysis state (silk_shape_state_FLP).
type silkShapeState struct {
	lastGainIndex     int8
	harmShapeGainSmth float32
	tiltSmth          float32
}

// silkEncoderControl carries one frame's analysis results into quantization
// (silk_encoder_control_FLP).
type silkEncoderControl struct {
	gains    [silkMaxNBSubfr]float32
	predCoef [2][silkMaxLPCOrder]float32
	ltpCoef  [silkLTPOrder * silkMaxNBSubfr]float32
	ltpScale float32
	pitchL   [silkMaxNBSubfr]int

	ar            [silkMaxNBSubfr * maxShapeLPCOrder]float32
	lfMAShp       [silkMaxNBSubfr]float32
	lfARShp       [silkMaxNBSubfr]float32
	tilt          [silkMaxNBSubfr]float32
	harmShapeGain [silkMaxNBSubfr]float32
	lambda        float32
	inputQuality  float32
	codingQuality float32

	predGain      float32
	ltpredCodGain float32
	resNrg        [silkMaxNBSubfr]float32

	gainsUnqQ16       [silkMaxNBSubfr]int32
	lastGainIndexPrev int8
}

// silkEncoderChannel is one internal channel's encoder state
// (silk_encoder_state fused with its FLP wrapper silk_encoder_state_FLP).
type silkEncoderChannel struct {
	inHPState                   [2]int32
	variableHPSmth1Q15          int32
	variableHPSmth2Q15          int32
	sLP                         silkLPState
	sVAD                        silkVADState
	sNSQ                        silkNSQState
	prevNLSFqQ15                [silkMaxLPCOrder]int16
	speechActivityQ8            int32
	allowBandwidthSwitch        bool
	LBRRprevLastGainIndex       int8
	prevSignalType              int8
	prevLag                     int
	pitchLPCWinLength           int
	maxPitchLag                 int
	apiFsHz                     int
	prevAPIFsHz                 int
	maxInternalFsHz             int
	minInternalFsHz             int
	desiredInternalFsHz         int
	fsKHz                       int
	nbSubfr                     int
	frameLength                 int
	subfrLength                 int
	ltpMemLength                int
	laPitch                     int
	laShape                     int
	shapeWinLength              int
	targetRateBps               int32
	packetSizeMS                int
	packetLossPerc              int
	frameCounter                int32
	complexity                  int
	nStatesDelayedDecision      int
	useInterpolatedNLSFs        bool
	shapingLPCOrder             int
	predictLPCOrder             int
	pitchEstimationComplexity   int
	pitchEstimationLPCOrder     int
	pitchEstimationThresholdQ16 int32
	sumLogGainQ7                int32
	nlsfMSVQSurvivors           int
	firstFrameAfterReset        bool
	controlledSinceLastPayload  bool
	warpingQ16                  int32
	useCBR                      bool
	prefillFlag                 bool
	pitchLagLowBitsICDF         []uint8
	pitchContourICDF            []uint8
	psNLSFCB                    *nlsfCB
	inputQualityBandsQ15        [vadNBands]int32
	inputTiltQ15                int32
	snrDBQ7                     int32

	VADFlags  [maxFramesPerPacket]int8
	LBRRFlag  int8
	LBRRFlags [maxFramesPerPacket]int

	indices silkIndices
	pulses  [silkMaxFrameLen]int8

	inputBuf         [silkMaxFrameLen + 2]int16
	inputBufIx       int
	nFramesPerPacket int
	nFramesEncoded   int

	nChannelsAPI      int
	nChannelsInternal int
	channelNb         int

	framesSinceOnset int

	ecPrevSignalType int
	ecPrevLagIndex   int

	resampler silkResampler

	useDTX          bool
	inDTX           bool
	noSpeechCounter int
	savedFsKHz      int // sLP.saved_fs_kHz: rate before a bandwidth-switch reset

	useInBandFEC      bool
	LBRREnabled       bool
	LBRRGainIncreases int8
	indicesLBRR       [maxFramesPerPacket]silkIndices
	pulsesLBRR        [maxFramesPerPacket][silkMaxFrameLen]int8

	// FLP wrapper state (silk_encoder_state_FLP).
	sShape  silkShapeState
	xBuf    [2*silkMaxFrameLen + laShapeMax]float32
	LTPCorr float32
}

// stereoEncState is the stereo mixing encoder state (stereo_enc_state).
type stereoEncState struct {
	predPrevQ13   [2]int16
	sMid          [2]int16
	sSide         [2]int16
	midSideAmpQ0  [4]int32
	smthWidthQ14  int16
	widthPrevQ14  int16
	silentSideLen int16
	predIx        [maxFramesPerPacket][2][3]int8
	midOnlyFlags  [maxFramesPerPacket]int8
}

// silkEncoder is the top-level SILK encoder (silk_encoder).
type silkEncoder struct {
	sStereo                  stereoEncState
	nBitsUsedLBRR            int32
	nBitsExceeded            int32
	nChannelsAPI             int
	nChannelsInternal        int
	nPrevChannelsInternal    int
	timeSinceSwitchAllowedMS int
	allowBandwidthSwitch     bool
	prevDecodeOnlyMiddle     int
	channel                  [encoderNumChannels]silkEncoderChannel
}
