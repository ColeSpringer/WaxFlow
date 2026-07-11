package opus

// Opus encoder facade and mode plumbing, ported from libopus
// src/opus_encoder.c (opus_encode_native/opus_encode_frame_native and its
// helpers). The encoder runs the tonality analyser on every frame, picks
// SILK-only, hybrid, or CELT-only per the reference mode and bandwidth
// decision logic, and stitches the layers through one shared range coder,
// with redundant CELT frames plus SILK/CELT prefill on mode transitions.
//
// Scope notes against the reference: the application is fixed to
// OPUS_APPLICATION_AUDIO (dc_reject input filter, analysis-driven
// voice estimate); frames are always 20 ms at 48 kHz; DTX, in-band FEC
// (packet loss is always 0 for file encoding), surround masking, and the
// adaptive stereo->mono downmix are not wired (stream channels == input
// channels).

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/waxerr"
)

// Opus encoding constants. Frames are fixed at 20 ms (960 samples), one
// frame per packet.
const (
	opusFrameLM   = celtMaxLM // a 20 ms frame is 2^3 short blocks
	opusFrameSize = celtShortMDCTSize << opusFrameLM
	opusCELTBands = 21 // fullband
	// delayCompensation is the encoder-side buffering that lets the
	// analyser and the mode switches run ahead of the coded signal
	// (libopus st->delay_compensation = Fs/250).
	delayCompensation = SampleRate / 250
	// EncoderDelay is the total encoder lookahead in 48 kHz samples,
	// carried as the OpusHead pre-skip so the decoder trims priming
	// (libopus OPUS_GET_LOOKAHEAD: Fs/400 + delay_compensation).
	EncoderDelay = SampleRate/400 + delayCompensation
	// DefaultBitrate is the target when none is requested.
	DefaultBitrate = 96000
	// maxFrameBytes is RFC 6716's cap on one frame's compressed data (1275
	// bytes); the packet clamp keeps TOC+payload within it, one byte
	// conservative, matching the reference's packet_size_cap.
	maxFrameBytes = 1275
	// encoderBuffer is the delay ring the frame path reads through
	// (libopus st->encoder_buffer = Fs/100).
	encoderBuffer = SampleRate / 100
)

// Coding modes reuse the decoder's TOC constants (opus.go): modeSILK,
// modeHybrid, modeCELT. prevMode uses -1 for "none yet" so modeSILK's zero
// value stays distinguishable.
const modeNone = -1

// Signal is a content-type hint steering the encoder's speech/music mode and
// bandwidth decisions (OPUS_SET_SIGNAL): voice/music pin the voice estimate
// the decisions interpolate on, overriding the analyser's probability. The
// analyser keeps running either way (its tonality/activity features feed CELT
// regardless of the mode decision).
type Signal int

const (
	// SignalAuto (the zero value) lets the tonality analyser drive the
	// speech/music decision.
	SignalAuto Signal = iota
	// SignalVoice biases mode selection toward SILK/hybrid, for content known
	// to be speech (audiobooks, podcasts).
	SignalVoice
	// SignalMusic biases mode selection toward CELT.
	SignalMusic
)

// Internal aliases keeping the frame encoder's spelling close to libopus.
const (
	signalAuto  = SignalAuto
	signalVoice = SignalVoice
	signalMusic = SignalMusic
)

// Audio bandwidths (opus_defines.h OPUS_BANDWIDTH_*).
const (
	bandwidthNarrow    = 1101
	bandwidthMedium    = 1102
	bandwidthWide      = 1103
	bandwidthSuperwide = 1104
	bandwidthFull      = 1105
)

// EncoderVersion identifies the encoder bitstream/algorithm revision for the
// cache key. Bump it when a change alters the produced bytes.
const EncoderVersion = "opus-enc-5"

// EncoderOptions configures the Opus encoder.
type EncoderOptions struct {
	// Bitrate is the target bit rate in bits per second (0 uses the default).
	Bitrate int
	// Complexity gates the encoder's analysis depth (0..10). Higher is slower but
	// higher quality: >=2 enables tf_analysis, >=3 the spreading decision, >=4 the
	// two-pass coarse energy, >=5 patch-transient detection and the pitch
	// pre-filter, >=8 the second MDCT and the stereo theta RDO search; the SILK
	// layer's knobs (pitch/shaping orders, delayed-decision states, warping)
	// scale over the whole range. The zero value selects the default (5); -1
	// selects complexity 0, which the zero value cannot mean without stealing
	// the default.
	Complexity int
	// VBR selects variable bit rate, where each frame is sized to its content
	// around the Bitrate target. The zero value is constant bit rate.
	VBR bool
	// ConstrainedVBR bounds VBR's short-term rate excursions with a bit
	// reservoir (libopus OPUS_SET_VBR_CONSTRAINT), for transports that cannot
	// absorb unbounded bursts. It only applies when VBR is set; unconstrained
	// VBR gives strictly better quality for file and HTTP delivery.
	ConstrainedVBR bool
	// Signal hints the content type (voice or music), pinning the
	// speech/music mode decision the analyser would otherwise drive. The
	// zero value (SignalAuto) keeps the analyser's decision.
	Signal Signal
	// LSBDepth is the true bit depth of the float input, 8 to 24
	// (OPUS_SET_LSB_DEPTH): the analyser's bandwidth detector places its
	// noise floor at that depth's quantization level, so a 16-bit-sourced
	// stream does not have inaudible HF dither mistaken for content. The
	// zero value means 24 (full float precision, the libopus float-API
	// default).
	LSBDepth int
}

// DefaultComplexity is the analysis depth used when none is requested.
const DefaultComplexity = 5

// Encoder is a full Opus encoder (SILK, hybrid, and CELT modes with
// analysis-driven selection) producing raw Opus packets. A container muxer
// frames them into Ogg-Opus.
type Encoder struct {
	fmt      audio.Format
	channels int
	bitrate  int
	vbr      bool
	useCVBR  bool

	celt     *celtEncoder
	silk     *silkEncoder
	silkMode silkEncControl
	analysis tonalityAnalysisState

	mode              int
	prevMode          int
	forcedMode        int // modeNone = auto; tests force modeSILK etc.
	signal            Signal
	lsbDepth          int
	bandwidth         int
	autoBandwidth     int
	detectedBandwidth int
	voiceRatio        int
	first             bool
	silkBwSwitch      bool

	hpMem                [4]float32
	delayBuffer          [][]float32 // per channel, encoderBuffer samples
	prevHBGain           float32
	hybridStereoWidthQ14 int16
	variableHPSmth2Q15   int32
	widthMem             stereoWidthState
	rangeFinal           uint32

	complexity int

	buf       [][]float32 // per-channel input FIFO
	inSamples int64
	outFrames int64
}

// NewEncoder returns an Opus encoder for the given input format, which must
// be 48 kHz float32, mono or stereo.
func NewEncoder(f audio.Format, opts *EncoderOptions) (*Encoder, error) {
	if err := f.Valid(); err != nil {
		return nil, err
	}
	if f.Type != audio.Float || f.Rate != SampleRate || f.Channels < 1 || f.Channels > 2 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("opus: input %v is not an Opus encode shape (48 kHz float32, 1-2 ch)", f))
	}
	bitrate := DefaultBitrate
	if opts != nil && opts.Bitrate != 0 {
		bitrate = opts.Bitrate
	}
	if bitrate <= 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("opus: bit rate %d must be positive", bitrate))
	}
	// Below ~6 kb/s there is no usable 20 ms mode.
	if bitrate < 6000 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("opus: bit rate %d below the 6000 b/s floor", bitrate))
	}
	complexity := DefaultComplexity
	if opts != nil && opts.Complexity != 0 {
		complexity = opts.Complexity
	}
	if complexity == -1 {
		complexity = 0
	}
	if complexity < 0 || complexity > 10 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("opus: complexity %d outside -1..10", complexity))
	}
	signal := SignalAuto
	if opts != nil {
		signal = opts.Signal
	}
	if signal != SignalAuto && signal != SignalVoice && signal != SignalMusic {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("opus: signal hint %d is not auto, voice, or music", signal))
	}
	lsbDepth := 24
	if opts != nil && opts.LSBDepth != 0 {
		lsbDepth = opts.LSBDepth
	}
	if lsbDepth < 8 || lsbDepth > 24 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("opus: lsb depth %d outside 8..24", lsbDepth))
	}
	vbr := opts != nil && opts.VBR
	e := &Encoder{
		fmt:        f,
		channels:   f.Channels,
		bitrate:    bitrate,
		vbr:        vbr,
		useCVBR:    vbr && opts != nil && opts.ConstrainedVBR,
		celt:       newCELTEncoder(f.Channels),
		silk:       newSILKEncoder(f.Channels),
		complexity: complexity,
		signal:     signal,
		lsbDepth:   lsbDepth,
		voiceRatio: -1,
		first:      true,
		prevMode:   modeNone,
		forcedMode: modeNone,
		prevHBGain: 1.0,
		bandwidth:  bandwidthFull,
	}
	e.hybridStereoWidthQ14 = 1 << 14
	e.variableHPSmth2Q15 = silkLSHIFT(silkLin2Log(silkFixConst(variableHPMinCutoffHz, 16))-16<<7, 8)
	e.celt.bitrate = bitrate
	e.celt.complexity = complexity
	e.silkMode = silkEncControl{
		nChannelsAPI:              f.Channels,
		nChannelsInternal:         f.Channels,
		apiSampleRate:             SampleRate,
		maxInternalSampleRate:     16000,
		minInternalSampleRate:     8000,
		desiredInternalSampleRate: 16000,
		payloadSizeMS:             20,
		bitRate:                   int32(bitrate),
		complexity:                complexity,
	}
	e.delayBuffer = make([][]float32, f.Channels)
	e.buf = make([][]float32, f.Channels)
	for c := 0; c < f.Channels; c++ {
		e.delayBuffer[c] = make([]float32, encoderBuffer)
	}
	return e, nil
}

// InputFormat is the PCM format the encoder consumes.
func (e *Encoder) InputFormat() audio.Format { return e.fmt }

// FrameSize is the encoder-native chunk: one 20 ms frame.
func (e *Encoder) FrameSize() int { return opusFrameSize }

// Bitrate reports the bit rate in bits per second the stream can be relied
// on to hold: the exact rate in CBR, the reservoir-bounded long-term target
// in constrained VBR, and 0 for unconstrained VBR, whose rate is
// signal-dependent (size and rate hints are then honestly unknown).
func (e *Encoder) Bitrate() int {
	if e.vbr && !e.useCVBR {
		return 0
	}
	if e.vbr {
		return e.bitrate
	}
	return e.cbrBytes() * 8 * (SampleRate / opusFrameSize)
}

// cbrBytes is the fixed packet size (TOC included) in CBR mode.
func (e *Encoder) cbrBytes() int {
	n := (e.bitrate*opusFrameSize + 4*SampleRate) / (8 * SampleRate)
	if n < 3 {
		n = 3
	}
	if n > maxFrameBytes {
		n = maxFrameBytes
	}
	return n
}

// FinalRange reports the range coder's final state after the most recently
// emitted packet, the integrity value libopus exposes as
// OPUS_GET_FINAL_RANGE: a conformant decoder's range state after decoding
// that packet must equal it. The encoder-quality harness writes it into
// opus_demo bitstream files, where the reference decoder cross-checks every
// packet and hard-fails on a mismatch.
func (e *Encoder) FinalRange() uint32 { return e.rangeFinal }

// CodecConfig returns the OpusHead identification header (RFC 7845).
func (e *Encoder) CodecConfig() []byte {
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8] = 1 // version
	head[9] = byte(e.channels)
	binary.LittleEndian.PutUint16(head[10:], uint16(EncoderDelay)) // pre-skip
	binary.LittleEndian.PutUint32(head[12:], SampleRate)           // input rate (informational)
	binary.LittleEndian.PutUint16(head[16:], 0)                    // output gain
	head[18] = 0                                                   // channel mapping family 0
	return head
}

// Encode buffers src and emits every whole 20 ms frame that becomes available.
func (e *Encoder) Encode(src *audio.Buffer, emit func(codec.Packet) error) error {
	if src.Fmt != e.fmt {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("opus: encode input %v disagrees with %v", src.Fmt, e.fmt))
	}
	for c := 0; c < e.channels; c++ {
		e.buf[c] = append(e.buf[c], src.ChanF(c)[:src.N]...)
	}
	e.inSamples += int64(src.N)
	return e.drainFrames(emit)
}

func (e *Encoder) drainFrames(emit func(codec.Packet) error) error {
	for len(e.buf[0]) >= opusFrameSize {
		if err := e.emitFrame(emit); err != nil {
			return err
		}
		for c := 0; c < e.channels; c++ {
			e.buf[c] = append(e.buf[c][:0], e.buf[c][opusFrameSize:]...)
		}
	}
	return nil
}

// emitFrame encodes the leading 20 ms of the FIFO into one Opus packet.
func (e *Encoder) emitFrame(emit func(codec.Packet) error) error {
	pcm := make([][]float32, e.channels)
	for c := 0; c < e.channels; c++ {
		pcm[c] = e.buf[c][:opusFrameSize]
	}
	pkt := e.encodePacket(pcm)
	e.outFrames++
	return emit(codec.Packet{Data: pkt, PTS: (e.outFrames - 1) * opusFrameSize, Dur: opusFrameSize, Sync: true})
}

// Finish pads the tail to a whole frame, emits enough frames to cover the
// pre-skip priming, and reports the gapless trailer.
func (e *Encoder) Finish(emit func(codec.Packet) error) (codec.Trailer, error) {
	if n := len(e.buf[0]); n > 0 {
		for c := 0; c < e.channels; c++ {
			e.buf[c] = append(e.buf[c], make([]float32, opusFrameSize-n)...)
		}
		if err := e.emitFrame(emit); err != nil {
			return codec.Trailer{}, err
		}
		for c := 0; c < e.channels; c++ {
			e.buf[c] = e.buf[c][:0]
		}
	}
	// Emit silent frames until the output covers pre-skip + all real samples.
	for e.outFrames*opusFrameSize < e.inSamples+EncoderDelay {
		for c := 0; c < e.channels; c++ {
			e.buf[c] = append(e.buf[c][:0], make([]float32, opusFrameSize)...)
		}
		if err := e.emitFrame(emit); err != nil {
			return codec.Trailer{}, err
		}
		for c := 0; c < e.channels; c++ {
			e.buf[c] = e.buf[c][:0]
		}
	}
	delay := int64(EncoderDelay)
	padding := e.outFrames*opusFrameSize - e.inSamples - delay
	if padding < 0 {
		padding = 0
	}
	return codec.Trailer{Samples: e.inSamples, Delay: delay, Padding: padding}, nil
}

// --- reference helpers ------------------------------------------------------

// Mode and channel decision thresholds (opus_encoder.c).
var opusModeThresholds = [2][2]int32{
	// voice, music
	{64000, 10000}, // mono
	{44000, 10000}, // stereo
}

var monoVoiceBandwidthThresholds = [8]int32{9000, 700, 9000, 700, 13500, 1000, 14000, 2000}
var monoMusicBandwidthThresholds = [8]int32{9000, 700, 9000, 700, 11000, 1000, 12000, 2000}
var stereoVoiceBandwidthThresholds = [8]int32{9000, 700, 9000, 700, 13500, 1000, 14000, 2000}
var stereoMusicBandwidthThresholds = [8]int32{9000, 700, 9000, 700, 11000, 1000, 12000, 2000}

// computeEquivRate maps a bitrate to the equivalent 20 ms complexity-10 VBR
// rate the decision thresholds are calibrated for (compute_equiv_rate).
func computeEquivRate(bitrate int32, channels, frameRate int, vbr bool, mode, complexity, loss int) int32 {
	equiv := bitrate
	if frameRate > 50 {
		equiv -= int32((40*channels + 20) * (frameRate - 50))
	}
	if !vbr {
		equiv -= equiv / 12
	}
	equiv = equiv * int32(90+complexity) / 100
	switch {
	case mode == modeSILK || mode == modeHybrid:
		if complexity < 2 {
			equiv = equiv * 4 / 5
		}
		equiv -= equiv * int32(loss) / int32(6*loss+10)
	case mode == modeCELT:
		if complexity < 5 {
			equiv = equiv * 9 / 10
		}
	default:
		equiv -= equiv * int32(loss) / int32(12*loss+20)
	}
	return equiv
}

// computeSILKRateForHybrid splits the total rate between SILK and CELT
// (compute_silk_rate_for_hybrid).
func computeSILKRateForHybrid(rate int, bandwidth int, frame20ms, vbr, fec bool, channels int) int {
	rateTable := [7][5]int{
		{0, 0, 0, 0, 0},
		{12000, 10000, 10000, 11000, 11000},
		{16000, 13500, 13500, 15000, 15000},
		{20000, 16000, 16000, 18000, 18000},
		{24000, 18000, 18000, 21000, 21000},
		{32000, 22000, 22000, 28000, 28000},
		{64000, 38000, 38000, 50000, 50000},
	}
	rate /= channels
	entry := 1 + b2i(frame20ms) + 2*b2i(fec)
	N := len(rateTable)
	i := 1
	for ; i < N; i++ {
		if rateTable[i][0] > rate {
			break
		}
	}
	var silkRate int
	if i == N {
		silkRate = rateTable[i-1][entry]
		silkRate += (rate - rateTable[i-1][0]) / 2
	} else {
		lo := rateTable[i-1][entry]
		hi := rateTable[i][entry]
		x0 := rateTable[i-1][0]
		x1 := rateTable[i][0]
		silkRate = (lo*(x1-rate) + hi*(rate-x0)) / (x1 - x0)
	}
	if !vbr {
		silkRate += 100
	}
	if bandwidth == bandwidthSuperwide {
		silkRate += 300
	}
	silkRate *= channels
	if channels == 2 && rate >= 12000 {
		silkRate -= 1000
	}
	return silkRate
}

// computeRedundancyBytes sizes the redundant CELT frame on a mode switch
// (compute_redundancy_bytes).
func computeRedundancyBytes(maxDataBytes int, bitrateBps int32, frameRate, channels int) int {
	baseBits := 40*channels + 20
	redundancyRate := int(bitrateBps) + baseBits*(200-frameRate)
	redundancyRate = 3 * redundancyRate / 2
	redundancyBytes := redundancyRate / 1600

	availableBits := maxDataBytes*8 - 2*baseBits
	cap := (availableBits*240/(240+48000/frameRate) + baseBits) / 8
	if redundancyBytes > cap {
		redundancyBytes = cap
	}
	if redundancyBytes > 4+8*channels {
		if redundancyBytes > 257 {
			redundancyBytes = 257
		}
	} else {
		redundancyBytes = 0
	}
	return redundancyBytes
}

// genTOC builds the packet's first byte (gen_toc), code 0 (one frame).
func genTOC(mode, frameRate, bandwidth, channels int) byte {
	period := 0
	for frameRate < 400 {
		frameRate <<= 1
		period++
	}
	var toc byte
	switch mode {
	case modeSILK:
		toc = byte(bandwidth-bandwidthNarrow) << 5
		toc |= byte(period-2) << 3
	case modeCELT:
		tmp := bandwidth - bandwidthMedium
		if tmp < 0 {
			tmp = 0
		}
		toc = 0x80
		toc |= byte(tmp) << 5
		toc |= byte(period) << 3
	default: // hybrid
		toc = 0x60
		toc |= byte(bandwidth-bandwidthSuperwide) << 4
		toc |= byte(period-2) << 3
	}
	if channels == 2 {
		toc |= 1 << 2
	}
	return toc
}

// dcReject is the 3 Hz DC rejection filter applied to all input
// (dc_reject, float build), operating in the +-1.0 domain planar.
func dcReject(in [][]float32, cutoffHz int, out [][]float32, hpMem []float32, length, channels int) {
	coef := 6.3 * float32(cutoffHz) / SampleRate
	coef2 := 1 - coef
	for c := 0; c < channels; c++ {
		m0 := hpMem[2*c]
		for i := 0; i < length; i++ {
			x := in[c][i]
			y := x - m0
			m0 = coef*x + 1e-30 + coef2*m0
			out[c][i] = y
		}
		hpMem[2*c] = m0
	}
}

// gainFade fades from gain g1 to g2 over the CELT overlap (gain_fade),
// planar in place.
func (e *Encoder) gainFade(buf [][]float32, g1, g2 float32, frameSize int) {
	overlap := e.celt.overlap
	for i := 0; i < overlap; i++ {
		w := float32(opusFadeWindow[i])
		w = w * w
		g := w*g2 + (1-w)*g1
		for c := 0; c < e.channels; c++ {
			buf[c][i] *= g
		}
	}
	for c := 0; c < e.channels; c++ {
		for i := overlap; i < frameSize; i++ {
			buf[c][i] *= g2
		}
	}
}

// stereoFade narrows the stereo image from width g1 to g2 (stereo_fade).
func (e *Encoder) stereoFade(buf [][]float32, g1, g2 float32, frameSize int) {
	overlap := e.celt.overlap
	g1 = 1 - g1
	g2 = 1 - g2
	for i := 0; i < frameSize; i++ {
		var g float32
		if i < overlap {
			w := float32(opusFadeWindow[i])
			w = w * w
			g = w*g2 + (1-w)*g1
		} else {
			g = g2
		}
		diff := 0.5 * (buf[0][i] - buf[1][i]) * g
		buf[0][i] -= diff
		buf[1][i] += diff
	}
}

// stereoWidthState tracks smoothed correlation for the stereo width estimate
// (StereoWidthState).
type stereoWidthState struct {
	XX, XY, YY    float32
	smoothedWidth float32
	maxFollower   float32
}

// computeStereoWidth estimates the stereo width in Q15-ish [0,1]
// (compute_stereo_width).
func (m *stereoWidthState) compute(pcm [][]float32, frameSize int) float32 {
	frameRate := SampleRate / frameSize
	shortAlpha := float32(25) / float32(maxIntA(50, frameRate))
	var xx, xy, yy float32
	for i := 0; i+3 < frameSize; i += 4 {
		var pxx, pxy, pyy float32
		for j := 0; j < 4; j++ {
			x := pcm[0][i+j]
			y := pcm[1][i+j]
			pxx += x * x
			pxy += x * y
			pyy += y * y
		}
		xx += pxx
		xy += pxy
		yy += pyy
	}
	if !(xx < 1e9) || math.IsNaN(float64(xx)) || !(yy < 1e9) || math.IsNaN(float64(yy)) {
		xx, xy, yy = 0, 0, 0
	}
	m.XX += shortAlpha * (xx - m.XX)
	m.XY = (1-shortAlpha)*m.XY + shortAlpha*xy
	m.YY += shortAlpha * (yy - m.YY)
	m.XX = maxA(0, m.XX)
	m.XY = maxA(0, m.XY)
	m.YY = maxA(0, m.YY)
	if maxA(m.XX, m.YY) > 8e-4 {
		sqrtXX := float32(math.Sqrt(float64(m.XX)))
		sqrtYY := float32(math.Sqrt(float64(m.YY)))
		qrrtXX := float32(math.Sqrt(float64(sqrtXX)))
		qrrtYY := float32(math.Sqrt(float64(sqrtYY)))
		m.XY = minA(m.XY, sqrtXX*sqrtYY)
		corr := m.XY / (1e-15 + sqrtXX*sqrtYY)
		ldiff := 1.0 * absA(qrrtXX-qrrtYY) / (1e-15 + qrrtXX + qrrtYY)
		width := float32(math.Sqrt(float64(1.0-corr*corr))) * ldiff
		m.smoothedWidth += (width - m.smoothedWidth) / float32(frameRate)
		m.maxFollower = maxA(m.maxFollower-0.02/float32(frameRate), m.smoothedWidth)
	}
	return minA(1.0, 20*m.maxFollower)
}

// opusFadeWindow is the CELT overlap window the mode-transition fades read.
var opusFadeWindow = celtWindow(celtOverlap)

// --- the frame encoder ------------------------------------------------------

// encodePacket encodes one 20 ms frame (planar float, exactly opusFrameSize
// samples per channel) into a complete Opus packet
// (opus_encode_frame_native).
func (e *Encoder) encodePacket(pcm [][]float32) []byte {
	frameSize := opusFrameSize
	C := e.channels
	frameRate := SampleRate / frameSize

	var maxDataBytes int
	if e.vbr {
		maxDataBytes = maxFrameBytes + 1
	} else {
		maxDataBytes = e.cbrBytes()
	}

	// Tonality analysis over the incoming frame; the encoded signal trails it
	// by delayCompensation samples.
	var info analysisInfo
	e.analysis.runAnalysis(pcm, frameSize, frameSize, 0, -2, C, e.lsbDepth, &info)

	isSilence := true
	for c := 0; c < C && isSilence; c++ {
		for _, v := range pcm[c] {
			if absA(v) > 1.0/(1<<24) {
				isSilence = false
				break
			}
		}
	}

	e.voiceRatio = -1
	e.detectedBandwidth = 0
	if info.valid {
		var prob float32
		switch {
		case e.prevMode == modeNone:
			prob = info.musicProb
		case e.prevMode == modeCELT:
			prob = info.musicProbMax
		default:
			prob = info.musicProbMin
		}
		e.voiceRatio = int(math.Floor(0.5 + 100*float64(1-prob)))

		switch {
		case info.bandwidth <= 12:
			e.detectedBandwidth = bandwidthNarrow
		case info.bandwidth <= 14:
			e.detectedBandwidth = bandwidthMedium
		case info.bandwidth <= 16:
			e.detectedBandwidth = bandwidthWide
		case info.bandwidth <= 18:
			e.detectedBandwidth = bandwidthSuperwide
		default:
			e.detectedBandwidth = bandwidthFull
		}
	}

	var stereoWidth float32
	if C == 2 {
		stereoWidth = e.widthMem.compute(pcm, frameSize)
	}

	maxRate := int32(maxDataBytes * 8 * frameRate)

	// Voice estimate in Q7 (OPUS_SIGNAL_VOICE/MUSIC override the analysis).
	voiceEst := 48
	switch {
	case e.signal == signalVoice:
		voiceEst = 127
	case e.signal == signalMusic:
		voiceEst = 0
	case e.voiceRatio >= 0:
		voiceEst = e.voiceRatio * 327 >> 8
		if voiceEst > 115 {
			voiceEst = 115 // AUDIO application cap
		}
	}

	// Mode not decided yet: the reference passes 0 here, which is not one of
	// its MODE_* values (they start at 1000) and lands in compute_equiv_rate's
	// mode-unknown branch. Our modeSILK is 0, so the sentinel must be
	// modeNone or the SILK complexity<2 penalty would leak into the mode
	// decision itself.
	equivRate := computeEquivRate(int32(e.bitrate), C, frameRate, e.vbr, modeNone, e.complexity, 0)

	// Mode selection.
	redundancy := false
	celtToSilk := false
	toCELT := false
	prefill := 0
	if e.forcedMode != modeNone {
		e.mode = e.forcedMode
	} else {
		modeVoice := int32(float32(1-stereoWidth)*float32(opusModeThresholds[0][0]) +
			stereoWidth*float32(opusModeThresholds[1][0]))
		// The music term reads the stereo threshold [1][1] for both mix
		// weights; the reference spells it that way (opus_encoder.c), and the
		// mono/stereo music thresholds hold the same value, so [0][1] would
		// behave identically. Kept verbatim for reference parity.
		modeMusic := int32(float32(1-stereoWidth)*float32(opusModeThresholds[1][1]) +
			stereoWidth*float32(opusModeThresholds[1][1]))
		threshold := modeMusic + int32(voiceEst*voiceEst)*(modeVoice-modeMusic)>>14
		// Hysteresis.
		if e.prevMode == modeCELT {
			threshold -= 4000
		} else if e.prevMode != modeNone {
			threshold += 4000
		}
		if equivRate >= threshold {
			e.mode = modeCELT
		} else {
			e.mode = modeSILK
		}
		// Too little space for anything but CELT.
		if maxDataBytes < 9000/(frameRate*8) {
			e.mode = modeCELT
		}
	}

	if e.prevMode != modeNone &&
		((e.mode != modeCELT && e.prevMode == modeCELT) ||
			(e.mode == modeCELT && e.prevMode != modeCELT)) {
		redundancy = true
		celtToSilk = e.mode != modeCELT
		if !celtToSilk {
			// Switching to CELT: encode this frame in the old mode and switch
			// on the next one (20 ms frames always qualify).
			e.mode = e.prevMode
			toCELT = true
		}
	}

	equivRate = computeEquivRate(int32(e.bitrate), C, frameRate, e.vbr, e.mode, e.complexity, 0)

	if e.mode != modeCELT && e.prevMode == modeCELT {
		e.silk = newSILKEncoder(C)
		prefill = 1
	}

	// Automatic bandwidth selection.
	if e.mode == modeCELT || e.first || e.silkMode.allowBandwidthSwitch {
		var voiceTh, musicTh *[8]int32
		if C == 2 {
			voiceTh, musicTh = &stereoVoiceBandwidthThresholds, &stereoMusicBandwidthThresholds
		} else {
			voiceTh, musicTh = &monoVoiceBandwidthThresholds, &monoMusicBandwidthThresholds
		}
		var thresholds [8]int32
		for i := 0; i < 8; i++ {
			thresholds[i] = musicTh[i] + int32(voiceEst*voiceEst)*(voiceTh[i]-musicTh[i])>>14
		}
		bandwidth := bandwidthFull
		for bandwidth > bandwidthNarrow {
			threshold := thresholds[2*(bandwidth-bandwidthMedium)]
			hysteresis := thresholds[2*(bandwidth-bandwidthMedium)+1]
			if !e.first {
				if e.autoBandwidth >= bandwidth {
					threshold -= hysteresis
				} else {
					threshold += hysteresis
				}
			}
			if equivRate >= threshold {
				break
			}
			bandwidth--
		}
		// Mediumband is only used during transitions.
		if bandwidth == bandwidthMedium {
			bandwidth = bandwidthWide
		}
		e.bandwidth = bandwidth
		e.autoBandwidth = bandwidth
		// No SWB/FB transition until SILK has fully switched to WB.
		if !e.first && e.mode != modeCELT &&
			!e.silkMode.inWBmodeWithoutVariableLP && e.bandwidth > bandwidthWide {
			e.bandwidth = bandwidthWide
		}
	}

	// Hybrid needs a safe rate.
	if e.mode != modeCELT && maxRate < 15000 {
		if e.bandwidth > bandwidthWide {
			e.bandwidth = bandwidthWide
		}
	}

	// Use the detected bandwidth to reduce the coded bandwidth.
	if e.detectedBandwidth != 0 {
		var minDetected int
		switch {
		case equivRate <= int32(18000*C) && e.mode == modeCELT:
			minDetected = bandwidthNarrow
		case equivRate <= int32(24000*C) && e.mode == modeCELT:
			minDetected = bandwidthMedium
		case equivRate <= int32(30000*C):
			minDetected = bandwidthWide
		case equivRate <= int32(44000*C):
			minDetected = bandwidthSuperwide
		default:
			minDetected = bandwidthFull
		}
		if e.detectedBandwidth < minDetected {
			e.detectedBandwidth = minDetected
		}
		if e.bandwidth > e.detectedBandwidth {
			e.bandwidth = e.detectedBandwidth
		}
	}

	// CELT doesn't support mediumband.
	if e.mode == modeCELT && e.bandwidth == bandwidthMedium {
		e.bandwidth = bandwidthWide
	}

	curBandwidth := e.bandwidth
	// SILK-only can't code above wideband; hybrid can't code at or below it.
	if e.mode == modeSILK && curBandwidth > bandwidthWide {
		e.mode = modeHybrid
	}
	if e.mode == modeHybrid && curBandwidth <= bandwidthWide {
		e.mode = modeSILK
	}

	if isSilence {
		e.voiceRatio = -1
	}

	// For the first frame at a new SILK bandwidth.
	if e.silkBwSwitch {
		redundancy = true
		celtToSilk = true
		e.silkBwSwitch = false
		prefill = 2
	}
	if e.mode == modeCELT {
		redundancy = false
	}

	redundancyBytes := 0
	if redundancy {
		redundancyBytes = computeRedundancyBytes(minIntA(maxDataBytes, maxFrameBytes+1), int32(e.bitrate), frameRate, C)
		if redundancyBytes == 0 {
			redundancy = false
		}
	}

	bitsTarget := minIntA(8*(maxDataBytes-redundancyBytes), e.bitrate*frameSize/SampleRate) - 8

	// Range coder over the payload (the TOC byte is prepended at the end).
	payloadCap := minIntA(maxDataBytes, maxFrameBytes+1) - 1
	data := make([]byte, payloadCap)
	enc := newRangeEncoder(data)

	// Delayed input: [delayCompensation history | filtered new frame].
	totalBuffer := delayCompensation
	pcmBuf := make([][]float32, C)
	for c := 0; c < C; c++ {
		pcmBuf[c] = make([]float32, totalBuffer+frameSize)
		copy(pcmBuf[c][:totalBuffer], e.delayBuffer[c][encoderBuffer-totalBuffer:])
	}

	// Variable HP smoothing (the actual filtering for the AUDIO application
	// is the 3 Hz dc_reject).
	var hpFreqSmth1 int32
	if e.mode == modeCELT {
		hpFreqSmth1 = silkLSHIFT(silkLin2Log(variableHPMinCutoffHz), 8)
	} else {
		hpFreqSmth1 = e.silk.channel[0].variableHPSmth1Q15
	}
	e.variableHPSmth2Q15 = silkSMLAWB(e.variableHPSmth2Q15,
		hpFreqSmth1-e.variableHPSmth2Q15, silkFixConst(variableHPSmthCoef2, 16))

	{
		out := make([][]float32, C)
		for c := 0; c < C; c++ {
			out[c] = pcmBuf[c][totalBuffer:]
		}
		dcReject(pcm, 3, out, e.hpMem[:], frameSize, C)
	}

	// NaN guard (float API).
	{
		var sum float64
		for c := 0; c < C; c++ {
			sum += silkEnergyFLP(pcmBuf[c][totalBuffer:], frameSize)
		}
		if !(sum < 1e9) || math.IsNaN(sum) {
			for c := 0; c < C; c++ {
				for i := range pcmBuf[c][totalBuffer:] {
					pcmBuf[c][totalBuffer+i] = 0
				}
			}
			e.hpMem = [4]float32{}
		}
	}

	// SILK processing.
	hbGain := float32(1.0)
	if e.mode != modeCELT {
		totalRate := int32(bitsTarget) * int32(frameRate)
		if e.mode == modeHybrid {
			e.silkMode.bitRate = int32(computeSILKRateForHybrid(int(totalRate),
				curBandwidth, true, e.vbr, false, C))
			celtRate := totalRate - e.silkMode.bitRate
			hbGain = 1.0 - float32(math.Exp2(-float64(celtRate)*(1.0/1024)))
		} else {
			e.silkMode.bitRate = totalRate
		}

		e.silkMode.payloadSizeMS = 20
		e.silkMode.nChannelsAPI = C
		e.silkMode.nChannelsInternal = C
		switch curBandwidth {
		case bandwidthNarrow:
			e.silkMode.desiredInternalSampleRate = 8000
		case bandwidthMedium:
			e.silkMode.desiredInternalSampleRate = 12000
		default:
			e.silkMode.desiredInternalSampleRate = 16000
		}
		if e.mode == modeHybrid {
			e.silkMode.minInternalSampleRate = 16000
		} else {
			e.silkMode.minInternalSampleRate = 8000
		}
		e.silkMode.maxInternalSampleRate = 16000
		if e.mode == modeSILK {
			effectiveMaxRate := int32(maxDataBytes * 8 * frameRate)
			if effectiveMaxRate < 8000 {
				e.silkMode.maxInternalSampleRate = 12000
				e.silkMode.desiredInternalSampleRate = minIntA(12000, e.silkMode.desiredInternalSampleRate)
			}
			if effectiveMaxRate < 7000 {
				e.silkMode.maxInternalSampleRate = 8000
				e.silkMode.desiredInternalSampleRate = minIntA(8000, e.silkMode.desiredInternalSampleRate)
			}
		}

		e.silkMode.useCBR = !e.vbr
		e.silkMode.complexity = e.complexity
		e.silkMode.maxBits = (minIntA(maxDataBytes, maxFrameBytes+1) - 1) * 8
		if redundancy && redundancyBytes >= 2 {
			e.silkMode.maxBits -= redundancyBytes*8 + 1
			if e.mode == modeHybrid {
				e.silkMode.maxBits -= 20
			}
		}
		if e.silkMode.useCBR {
			if e.mode == modeHybrid {
				// SILK may steal up to 25% of the remaining bits, with CELT
				// absorbing the variation to keep the packet CBR.
				otherBits := maxIntA(0, e.silkMode.maxBits-int(e.silkMode.bitRate)*frameSize/SampleRate)
				e.silkMode.maxBits = maxIntA(0, e.silkMode.maxBits-otherBits*3/4)
				e.silkMode.useCBR = false
			}
		} else {
			if e.mode == modeHybrid {
				// SILK bitrate corresponding to the max total bits available.
				maxBitRate := computeSILKRateForHybrid(e.silkMode.maxBits*frameRate,
					curBandwidth, true, e.vbr, false, C)
				e.silkMode.maxBits = maxBitRate / frameRate
			}
		}

		if prefill != 0 {
			// Smooth onset for the SILK prefill so the encoder doesn't code a
			// discontinuity; the ramp lands right where the redundant CELT
			// frame will cover.
			prefillOffset := encoderBuffer - delayCompensation - SampleRate/400
			for c := 0; c < C; c++ {
				ramp := e.delayBuffer[c][prefillOffset:]
				for i := 0; i < e.celt.overlap && i < SampleRate/400; i++ {
					w := float32(opusFadeWindow[i])
					ramp[i] *= w * w
				}
				for i := 0; i < prefillOffset; i++ {
					e.delayBuffer[c][i] = 0
				}
			}
			pcm16 := interleaveToInt16(e.delayBuffer, C, encoderBuffer)
			zero := 0
			e.silk.encode(&e.silkMode, pcm16, encoderBuffer, nil, &zero, prefill, -1)
			e.silkMode.opusCanSwitch = false
		}

		pcm16 := interleaveToInt16planarView(pcmBuf, C, totalBuffer, frameSize)
		nBytes := 0
		e.silk.encode(&e.silkMode, pcm16, frameSize, enc, &nBytes, 0, -1)

		if e.mode == modeSILK {
			switch e.silkMode.internalSampleRate {
			case 8000:
				curBandwidth = bandwidthNarrow
			case 12000:
				curBandwidth = bandwidthMedium
			case 16000:
				curBandwidth = bandwidthWide
			}
		}

		e.silkMode.opusCanSwitch = e.silkMode.switchReady
		if e.silkMode.opusCanSwitch {
			redundancyBytes = computeRedundancyBytes(minIntA(maxDataBytes, maxFrameBytes+1), int32(e.bitrate), frameRate, C)
			redundancy = redundancyBytes != 0
			celtToSilk = false
			e.silkBwSwitch = true
		}
	}

	// CELT processing.
	endband := 21
	switch curBandwidth {
	case bandwidthNarrow:
		endband = 13
	case bandwidthMedium, bandwidthWide:
		endband = 17
	case bandwidthSuperwide:
		endband = 19
	}
	e.celt.disablePF = e.mode != modeCELT // prediction reduced in non-CELT modes
	e.celt.forceIntra = false

	// tmp_prefill: the 2.5 ms just before this frame, for CELT prefill on
	// mode transitions.
	var tmpPrefill [][]float32
	if e.mode != modeSILK && e.mode != e.prevMode && e.prevMode != modeNone {
		tmpPrefill = make([][]float32, C)
		for c := 0; c < C; c++ {
			tmpPrefill[c] = make([]float32, SampleRate/400)
			copy(tmpPrefill[c], e.delayBuffer[c][encoderBuffer-totalBuffer-SampleRate/400:])
		}
	}

	// Update the delay buffer.
	for c := 0; c < C; c++ {
		if encoderBuffer-(frameSize+totalBuffer) > 0 {
			copy(e.delayBuffer[c], e.delayBuffer[c][frameSize:])
			copy(e.delayBuffer[c][encoderBuffer-frameSize-totalBuffer:], pcmBuf[c])
		} else {
			copy(e.delayBuffer[c], pcmBuf[c][frameSize+totalBuffer-encoderBuffer:])
		}
	}

	// High-band gain fade (hybrid attenuates the CELT band when it's poor).
	if e.prevHBGain < 1.0 || hbGain < 1.0 {
		e.gainFade(pcmBuf, e.prevHBGain, hbGain, totalBuffer+frameSize)
	}
	e.prevHBGain = hbGain

	// Stereo width reduction at low rates.
	if e.mode != modeHybrid || C == 1 {
		var w int32
		switch {
		case equivRate > 32000:
			w = 16384
		case equivRate < 16000:
			w = 0
		default:
			w = 16384 - 2048*(32000-equivRate)/(equivRate-14000)
		}
		e.silkMode.stereoWidthQ14 = int16(w)
	}
	if C == 2 {
		if e.hybridStereoWidthQ14 < 1<<14 || e.silkMode.stereoWidthQ14 < 1<<14 {
			g1 := float32(e.hybridStereoWidthQ14) / 16384
			g2 := float32(e.silkMode.stereoWidthQ14) / 16384
			e.stereoFade(pcmBuf, g1, g2, totalBuffer+frameSize)
			e.hybridStereoWidthQ14 = e.silkMode.stereoWidthQ14
		}
	}

	nbCompressedBytes := 0
	var redundantRng uint32
	if e.mode != modeCELT && enc.tell()+17+20*b2i(e.mode == modeHybrid) <= 8*(payloadCap) {
		// For SILK-only the redundancy flag is inferred from the length.
		if e.mode == modeHybrid {
			enc.encodeBitLogp(b2i(redundancy), 12)
		}
		if redundancy {
			enc.encodeBitLogp(b2i(celtToSilk), 1)
			var maxRedundancy int
			if e.mode == modeHybrid {
				maxRedundancy = payloadCap - (enc.tell()+8+3+7)>>3
			} else {
				maxRedundancy = payloadCap - (enc.tell()+7)>>3
			}
			redundancyBytes = minIntA(maxRedundancy, redundancyBytes)
			redundancyBytes = minIntA(257, maxIntA(2, redundancyBytes))
			if e.mode == modeHybrid {
				enc.encodeUint(uint32(redundancyBytes-2), 256)
			}
		}
	} else {
		redundancy = false
	}
	if !redundancy {
		e.silkBwSwitch = false
		redundancyBytes = 0
	}
	startBand := 0
	if e.mode != modeCELT {
		startBand = 17
	}

	if e.mode == modeSILK {
		nbCompressedBytes = (enc.tell() + 7) >> 3
		enc.done()
	} else {
		nbCompressedBytes = payloadCap - redundancyBytes
		enc.shrink(nbCompressedBytes)
	}

	// Hand the analyser's frame to CELT (CELT_SET_ANALYSIS); a celt.Reset()
	// below clears it again, so mode-transition frames encode analyser-less
	// exactly like the reference.
	if redundancy || e.mode != modeSILK {
		e.celt.analysis = info
	}
	if e.mode == modeHybrid {
		e.celt.silkInfoSignalType = int(e.silkMode.signalType)
		e.celt.silkInfoOffset = int(e.silkMode.offset)
	} else {
		e.celt.silkInfoSignalType = 0
		e.celt.silkInfoOffset = 0
	}

	// 5 ms redundant CELT frame for a CELT->SILK transition, coded at the
	// tail of the packet before the switch.
	if redundancy && celtToSilk {
		e.celt.vbr = false
		prevBitrate := e.celt.bitrate
		e.celt.bitrate = 0
		sub := subFrames(pcmBuf, 0, SampleRate/200)
		redPayload := e.celt.celtEncode(sub, SampleRate/200, 1, C, 0, endband, redundancyBytes)
		copy(data[nbCompressedBytes:], redPayload)
		redundantRng = e.celt.rng
		e.celt.bitrate = prevBitrate
		e.celt.Reset()
	}

	if e.mode != modeSILK {
		e.celt.vbr = e.vbr
		e.celt.constrainedVBR = false
		if e.mode == modeHybrid {
			e.celt.bitrate = e.bitrate - int(e.silkMode.bitRate)
		} else {
			e.celt.bitrate = e.bitrate
			e.celt.constrainedVBR = e.useCVBR
		}
		if !e.vbr {
			e.celt.bitrate = 0 // CBR: fill nbCompressedBytes exactly
		}

		if e.mode != e.prevMode && e.prevMode != modeNone {
			e.celt.Reset()
			// Prefill with the 2.5 ms before the frame; discard the output.
			e.celt.disablePF = true
			e.celt.forceIntra = true
			prevVBR := e.celt.vbr
			e.celt.vbr = false
			bitratePrev := e.celt.bitrate
			e.celt.bitrate = 0
			e.celt.celtEncode(tmpPrefill, SampleRate/400, 0, C, 0, endband, 2)
			e.celt.bitrate = bitratePrev
			e.celt.vbr = prevVBR
			e.celt.forceIntra = false
			// Prediction stays off for the first regular frame.
			e.celt.disablePF = true
		}

		if enc.tell() <= 8*nbCompressedBytes {
			frame := subFrames(pcmBuf, 0, frameSize)
			ret := e.celt.celtEncodeWithEC(frame, frameSize, opusFrameLM, C, startBand, endband, enc, nbCompressedBytes)
			// Put CELT->SILK redundancy at its final place after a VBR CELT
			// frame came up short.
			if redundancy && celtToSilk && e.mode == modeHybrid && nbCompressedBytes != ret {
				copy(data[ret:ret+redundancyBytes], data[nbCompressedBytes:nbCompressedBytes+redundancyBytes])
			}
			nbCompressedBytes = ret
		}
		e.rangeFinal = e.celt.rng
	} else {
		e.rangeFinal = enc.rng
	}

	// 5 ms redundant CELT frame for a SILK->CELT transition, at the packet
	// tail after the switch, primed with the preceding 2.5 ms.
	if redundancy && !celtToSilk {
		e.celt.Reset()
		e.celt.disablePF = true
		e.celt.forceIntra = true
		prevVBR := e.celt.vbr
		e.celt.vbr = false
		prevBitrate := e.celt.bitrate
		e.celt.bitrate = 0

		N2 := SampleRate / 200
		N4 := SampleRate / 400
		pre := subFrames(pcmBuf, frameSize-N2-N4, N4)
		e.celt.celtEncode(pre, N4, 0, C, 0, endband, 2)

		red := subFrames(pcmBuf, frameSize-N2, N2)
		redPayload := e.celt.celtEncode(red, N2, 1, C, 0, endband, redundancyBytes)
		copy(data[nbCompressedBytes:], redPayload)
		redundantRng = e.celt.rng
		e.celt.bitrate = prevBitrate
		e.celt.vbr = prevVBR
		e.celt.forceIntra = false
	}

	e.rangeFinal ^= redundantRng

	if toCELT {
		e.prevMode = modeCELT
	} else {
		e.prevMode = e.mode
	}
	e.first = false

	// SILK bust: ask the decoder for PLC.
	if enc.tell() > payloadCap*8 {
		nbCompressedBytes = 1
		redundancyBytes = 0
		data[0] = 0
	} else if e.mode == modeSILK && !redundancy {
		// Trailing zeros are implicit in SILK-only mode: the decoder's range
		// coder zero-fills past the packet end, so stripping them is free.
		for nbCompressedBytes > 2 && data[nbCompressedBytes-1] == 0 {
			nbCompressedBytes--
		}
	}

	total := nbCompressedBytes + redundancyBytes
	pkt := make([]byte, 1+total)
	pkt[0] = genTOC(e.mode, frameRate, curBandwidth, C)
	copy(pkt[1:], data[:total])

	// CBR packets keep their fixed size via RFC 6716 code-3 padding.
	if !e.vbr && len(pkt) < maxDataBytes {
		pkt = opusPacketPad(pkt, maxDataBytes)
	}
	return pkt
}

// opusPacketPad grows a code-0 single-frame packet to exactly targetLen bytes
// by converting it to a code-3 padded packet (opus_packet_pad for the
// single-frame case).
func opusPacketPad(pkt []byte, targetLen int) []byte {
	if len(pkt) >= targetLen {
		return pkt
	}
	frame := pkt[1:]
	// Layout: TOC(code 3) + frame-count byte (v=0,p,M=1) + padding length
	// bytes + frame + padding zeros.
	out := make([]byte, targetLen)
	out[0] = pkt[0] | 0x3
	// Budget after TOC and count byte for length bytes plus padding data.
	rem := targetLen - 2 - len(frame)
	if rem == 0 {
		// One byte short of the target: the padding form cannot express it
		// (the padding bit demands at least one length byte, two bytes of
		// overhead total), so grow by the count byte alone, padding bit
		// clear. The reference repacketizer's pad_amount==0 branch does the
		// same.
		out[1] = 0x01
		copy(out[2:], frame)
		return out
	}
	out[1] = 0x41
	// rem = L (length bytes) + P (padding data); each 255 length byte adds
	// 254 to P and needs another length byte.
	pos := 2
	for rem > 255 {
		out[pos] = 255
		pos++
		rem -= 255 // one length byte consumed, 254 padding data accounted
	}
	out[pos] = byte(rem - 1)
	pos++
	copy(out[pos:], frame)
	// The padding data bytes at the end stay zero.
	return out
}

// subFrames views a planar buffer slice [off, off+n) per channel.
func subFrames(buf [][]float32, off, n int) [][]float32 {
	out := make([][]float32, len(buf))
	for c := range buf {
		out[c] = buf[c][off : off+n]
	}
	return out
}

// interleaveToInt16 converts planar float [-1,1) to interleaved int16.
func interleaveToInt16(buf [][]float32, C, n int) []int16 {
	out := make([]int16, C*n)
	for c := 0; c < C; c++ {
		for i := 0; i < n; i++ {
			out[i*C+c] = int16(silkSAT16(silkFloat2Int(buf[c][i] * 32768)))
		}
	}
	return out
}

// interleaveToInt16planarView converts the frame part of pcmBuf.
func interleaveToInt16planarView(buf [][]float32, C, off, n int) []int16 {
	out := make([]int16, C*n)
	for c := 0; c < C; c++ {
		for i := 0; i < n; i++ {
			out[i*C+c] = int16(silkSAT16(silkFloat2Int(buf[c][off+i] * 32768)))
		}
	}
	return out
}
