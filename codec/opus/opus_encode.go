package opus

import (
	"encoding/binary"
	"fmt"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/waxerr"
)

// Opus encoding constants. Phase-1 encodes CELT-only music at a fixed 20 ms
// frame (config 31: CELT fullband, 960 samples at 48 kHz), one frame per packet.
const (
	opusFrameLM   = celtMaxLM // a 20 ms frame is 2^3 short blocks
	opusFrameSize = celtShortMDCTSize << opusFrameLM
	opusCELTBands = 21 // fullband
	// EncoderDelay is the CELT algorithmic delay in 48 kHz samples (the MDCT
	// overlap), carried as the OpusHead pre-skip so the decoder trims priming.
	EncoderDelay = celtOverlap
	// DefaultBitrate is the CBR target when none is requested.
	DefaultBitrate    = 96000
	tocConfigCELTFB20 = 31
	// maxFrameBytes is RFC 6716's cap on one frame's compressed data (1275
	// bytes); the packet clamp below keeps TOC+payload within it, one byte
	// conservative, matching the reference's packet_size_cap.
	maxFrameBytes = 1275
)

// EncoderVersion identifies the encoder bitstream/algorithm revision for the
// cache key. Bump it when a change alters the produced bytes.
const EncoderVersion = "opus-celt-2"

// EncoderOptions configures the Opus encoder.
type EncoderOptions struct {
	// Bitrate is the target bit rate in bits per second (0 uses the default).
	Bitrate int
	// Complexity gates the encoder's analysis depth (0..10). Higher is slower but
	// higher quality: >=2 enables tf_analysis, >=3 the spreading decision, >=4 the
	// two-pass coarse energy, >=5 patch-transient detection and the pitch
	// pre-filter, >=8 the second MDCT and the stereo theta RDO search. The zero
	// value selects the default (5); -1 selects complexity 0, which the zero
	// value cannot mean without stealing the default.
	Complexity int
	// VBR selects variable bit rate, where each frame is sized to its content
	// around the Bitrate target. The zero value is constant bit rate.
	VBR bool
	// ConstrainedVBR bounds VBR's short-term rate excursions with a bit
	// reservoir (libopus OPUS_SET_VBR_CONSTRAINT), for transports that cannot
	// absorb unbounded bursts. It only applies when VBR is set; unconstrained
	// VBR gives strictly better quality for file and HTTP delivery.
	ConstrainedVBR bool
}

// DefaultComplexity is the analysis depth used when none is requested.
const DefaultComplexity = 5

// Encoder is a CELT-only Opus encoder producing raw Opus packets (TOC byte plus
// CELT payload). A container muxer frames them into Ogg-Opus.
type Encoder struct {
	fmt        audio.Format
	channels   int
	bitrate    int
	vbr        bool
	pktBytes   int // total packet size (TOC + CELT payload), CBR
	celtBudget int // CELT payload size passed per frame: fixed (CBR) or max (VBR)
	toc        byte
	celt       *celtEncoder

	buf       [][]float32 // per-channel input FIFO
	inSamples int64
	outFrames int64
}

// NewEncoder returns a CELT-only Opus encoder for the given input format, which
// must be 48 kHz float32, mono or stereo.
func NewEncoder(f audio.Format, opts *EncoderOptions) (*Encoder, error) {
	if err := f.Valid(); err != nil {
		return nil, err
	}
	if f.Type != audio.Float || f.Rate != SampleRate || f.Channels < 1 || f.Channels > 2 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("opus: input %v is not a CELT encode shape (48 kHz float32, 1-2 ch)", f))
	}
	bitrate := DefaultBitrate
	if opts != nil && opts.Bitrate != 0 {
		bitrate = opts.Bitrate
	}
	if bitrate <= 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("opus: bit rate %d must be positive", bitrate))
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
	// CBR packet size for a 20 ms frame, clamped to Opus's per-packet limits.
	pktBytes := (bitrate*opusFrameSize + 4*SampleRate) / (8 * SampleRate)
	if pktBytes < 3 {
		pktBytes = 3
	}
	if pktBytes > maxFrameBytes {
		pktBytes = maxFrameBytes
	}
	vbr := opts != nil && opts.VBR
	// The CELT payload budget per frame: the fixed CBR size, or the packet cap
	// (1275) minus the TOC byte for VBR, which sizes each frame to its content.
	celtBudget := pktBytes - 1
	if vbr {
		celtBudget = maxFrameBytes - 1
	}
	toc := byte(tocConfigCELTFB20<<3) | 0 // code 0: one frame per packet
	if f.Channels == 2 {
		toc |= 1 << 2 // stereo
	}
	e := &Encoder{
		fmt:        f,
		channels:   f.Channels,
		bitrate:    bitrate,
		vbr:        vbr,
		pktBytes:   pktBytes,
		celtBudget: celtBudget,
		toc:        toc,
		celt:       newCELTEncoder(f.Channels),
	}
	e.celt.bitrate = bitrate
	e.celt.complexity = complexity
	e.celt.vbr = vbr
	e.celt.constrainedVBR = vbr && opts != nil && opts.ConstrainedVBR
	e.buf = make([][]float32, f.Channels)
	return e, nil
}

// InputFormat is the PCM format the encoder consumes.
func (e *Encoder) InputFormat() audio.Format { return e.fmt }

// FrameSize is the encoder-native chunk: one 20 ms CELT frame.
func (e *Encoder) FrameSize() int { return opusFrameSize }

// Bitrate reports the bit rate in bits per second the stream can be relied
// on to hold: the exact rate in CBR, the reservoir-bounded long-term target
// in constrained VBR, and 0 for unconstrained VBR, whose rate is
// signal-dependent (size and rate hints are then honestly unknown).
func (e *Encoder) Bitrate() int {
	if e.vbr && !e.celt.constrainedVBR {
		return 0
	}
	if e.vbr {
		return e.bitrate
	}
	return e.pktBytes * 8 * (SampleRate / opusFrameSize)
}

// FinalRange reports the range coder's final state after the most recently
// emitted packet, the integrity value libopus exposes as
// OPUS_GET_FINAL_RANGE: a conformant decoder's range state after decoding
// that packet must equal it. The encoder-quality harness writes it into
// opus_demo bitstream files, where the reference decoder cross-checks every
// packet and hard-fails on a mismatch.
func (e *Encoder) FinalRange() uint32 { return e.celt.rng }

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
		if err := e.encodeFrame(emit); err != nil {
			return err
		}
		for c := 0; c < e.channels; c++ {
			e.buf[c] = append(e.buf[c][:0], e.buf[c][opusFrameSize:]...)
		}
	}
	return nil
}

// encodeFrame encodes the leading 20 ms of the FIFO into one Opus packet.
func (e *Encoder) encodeFrame(emit func(codec.Packet) error) error {
	pcm := make([][]float32, e.channels)
	for c := 0; c < e.channels; c++ {
		pcm[c] = e.buf[c][:opusFrameSize]
	}
	payload := e.celt.celtEncode(pcm, opusFrameSize, opusFrameLM, e.channels, 0, opusCELTBands, e.celtBudget)
	pkt := make([]byte, 1+len(payload))
	pkt[0] = e.toc
	copy(pkt[1:], payload)
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
		if err := e.encodeFrame(emit); err != nil {
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
		if err := e.encodeFrame(emit); err != nil {
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
