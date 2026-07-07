package mp3

// Baseline CBR Layer III encoder. It implements codec.Encoder: PCM chunks in,
// whole MP3 frames out. The signal chain is the exact inverse of the decoder
// (analysis.go: polyphase filterbank, forward MDCT, inverse alias), followed
// by the quantizer and Huffman planner (quantize.go, huffenc.go) and frame
// assembly with a bit reservoir here.
//
// One PCM frame is 1152 samples (MPEG-1, two granules) or 576 (MPEG-2/2.5,
// one granule); FrameSize reports it so the engine's framer chunks to native
// size. The encoder buffers internally anyway, so short chunks and the final
// partial chunk are handled by padding to a granule boundary.
//
// The bit reservoir makes the main-data bitstream continuous across frames:
// a frame's main data can begin up to reservoir-cap bytes before its own
// slot (main_data_begin). Physical frames are therefore held in a short
// queue and emitted only once the logical write cursor has filled their
// main-data region, which is what lets a quiet frame lend bits to a later
// busy one.

import (
	"fmt"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/waxerr"
)

var _ codec.Encoder = (*Encoder)(nil)

// EncoderVersion is the encoder's cache-key version constant (ADR-0004):
// bump on any change that alters the encoded bitstream.
const EncoderVersion = "mp3-enc-1"

// EncoderOptions configures a CBR Layer III encoder.
type EncoderOptions struct {
	// Bitrate is the constant bit rate in bits per second. It must be one of
	// the layer's legal rates for the input sample rate; zero selects the
	// default (128 kbit/s, or the closest legal rate at low sample rates).
	Bitrate int
}

// DefaultBitrate is the CBR bit rate used when EncoderOptions leaves it zero.
const DefaultBitrate = 128000

// Encoder is a baseline CBR MPEG-1/2/2.5 Layer III encoder.
type Encoder struct {
	fmt      audio.Format
	version  MPEGVersion
	rateIdx  int
	bitrate  int
	channels int
	row      int
	granules int // granules per frame: 2 (MPEG-1) or 1 (MPEG-2/2.5)
	siLen    int // side-info length in bytes
	resCap   int // reservoir cap in bytes (main_data_begin field maximum)

	ana [2]analyzer        // per-channel filterbank + MDCT state
	buf [2][]float32       // per-channel PCM FIFO awaiting whole frames
	xr  [2][2][576]float32 // staged spectra: [granule][channel]

	inSamples  int64 // real input samples fed (Encode only, not the flush)
	outSamples int64 // PCM samples represented by emitted frames

	// CBR padding accumulator and its per-frame step and threshold, fixed at
	// construction so nextPadding does no per-frame re-derivation.
	padAcc, padStep, padThresh int

	// Bit reservoir: pending physical frames and the logical write cursor.
	frames   []pendingFrame
	physEnd  int
	writePos int

	sw bitWriter // reusable side-info writer
	mw bitWriter // reusable main-data writer
}

// pendingFrame is a physical frame awaiting emission: its header and side
// info are final, its main-data slot fills as the logical cursor advances.
type pendingFrame struct {
	hdr   [4]byte
	si    []byte
	main  []byte // slots bytes, filled from the logical main-data stream
	start int    // logical offset of this frame's main-data region
	spf   int    // PCM samples this frame represents
}

// legalRate resolves a sample rate to its MPEG version and header rate index.
func legalRate(rate int) (MPEGVersion, int, bool) {
	for _, r := range []struct {
		v  MPEGVersion
		hz [3]int
	}{
		{MPEG1, [3]int{44100, 48000, 32000}},
		{MPEG2, [3]int{22050, 24000, 16000}},
		{MPEG25, [3]int{11025, 12000, 8000}},
	} {
		for idx, hz := range r.hz {
			if hz == rate {
				return r.v, idx, true
			}
		}
	}
	return 0, 0, false
}

// clampBitrate returns the highest legal CBR rate (kbit/s) at or below kbps for
// the version, floored at the version's minimum. The bit rate is a target: the
// encoder emits the nearest rate the layer actually supports, so a quality
// preset like 192 resolves to 160 on the low-sampling-frequency layers rather
// than failing, and the header always carries a real bit-rate index (never the
// free-format 0).
func clampBitrate(v MPEGVersion, kbps int) int {
	lsf := 0
	if v != MPEG1 {
		lsf = 1
	}
	best := 0
	for i := 1; i < 15; i++ {
		if r := bitrateKbps[lsf][i]; r <= kbps && r > best {
			best = r
		}
	}
	if best == 0 {
		best = bitrateKbps[lsf][1] // below the minimum: use the smallest rate
	}
	return best
}

// NewEncoder returns a baseline CBR encoder for the given input format. The
// format must be float32, mono or stereo, at a Layer III sample rate.
func NewEncoder(f audio.Format, opts *EncoderOptions) (*Encoder, error) {
	if err := f.Valid(); err != nil {
		return nil, err
	}
	if f.Type != audio.Float || f.Channels < 1 || f.Channels > 2 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("mp3: input %v is not a Layer III encode shape (float32, 1-2 ch)", f))
	}
	ver, rateIdx, ok := legalRate(f.Rate)
	if !ok {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("mp3: sample rate %d Hz is not an MPEG Layer III rate", f.Rate))
	}
	bitrate := DefaultBitrate
	if opts != nil && opts.Bitrate != 0 {
		bitrate = opts.Bitrate
	}
	// The bit rate is whole kbit/s; a non-multiple is malformed (and would
	// otherwise pick no header index and emit free format). The value is then
	// clamped to a rate the layer actually supports.
	if bitrate <= 0 || bitrate%1000 != 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("mp3: bit rate %d must be a positive whole number of kbit/s", bitrate))
	}
	bitrate = clampBitrate(ver, bitrate/1000) * 1000

	e := &Encoder{
		fmt:      f,
		version:  ver,
		rateIdx:  rateIdx,
		bitrate:  bitrate,
		channels: f.Channels,
	}
	e.granules = 1
	e.resCap = 255
	if ver == MPEG1 {
		e.granules = 2
		e.resCap = 511
	}
	h := e.header(false)
	e.row = h.rateRow()
	e.siLen = h.SideInfoLen()
	// Padding accumulator constants: each frame carries the byte fraction the
	// floored Header.Size drops, ((spf/8)*bitrate) % rate, and a padding slot
	// lands when a whole byte accumulates.
	e.padStep = ((h.SamplesPerFrame() / 8) * e.bitrate) % h.Rate
	e.padThresh = h.Rate
	return e, nil
}

// header builds the frame header for this encoder with the given padding.
func (e *Encoder) header(pad bool) Header {
	rate := rateHz[e.rateIdx]
	if e.version != MPEG1 {
		rate >>= 1
	}
	if e.version == MPEG25 {
		rate >>= 1
	}
	mode := ModeStereo
	if e.channels == 1 {
		mode = ModeMono
	}
	return Header{
		rateIdx:  e.rateIdx,
		Version:  e.version,
		Rate:     rate,
		Channels: e.channels,
		Mode:     mode,
		Bitrate:  e.bitrate,
		Padding:  pad,
	}
}

// InputFormat is the PCM format the encoder consumes.
func (e *Encoder) InputFormat() audio.Format { return e.fmt }

// Bitrate is the actual constant bit rate in bits per second, after the
// requested rate is clamped to one the layer supports.
func (e *Encoder) Bitrate() int { return e.bitrate }

// FrameSize is the encoder-native chunk in frames: one whole MP3 frame.
func (e *Encoder) FrameSize() int { return 576 * e.granules }

// CodecConfig is nil: MP3 is self-framing and carries no out-of-band setup.
func (e *Encoder) CodecConfig() []byte { return nil }

// Encode buffers src and emits every whole frame that becomes available.
func (e *Encoder) Encode(src *audio.Buffer, emit func(codec.Packet) error) error {
	if src.Fmt != e.fmt {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("mp3: encode input %v disagrees with %v", src.Fmt, e.fmt))
	}
	for ch := 0; ch < e.channels; ch++ {
		e.buf[ch] = append(e.buf[ch], src.ChanF(ch)[:src.N]...)
	}
	e.inSamples += int64(src.N)
	return e.drainFrames(emit)
}

// drainFrames encodes whole frames while the FIFO holds at least one.
func (e *Encoder) drainFrames(emit func(codec.Packet) error) error {
	fs := e.FrameSize()
	for len(e.buf[0]) >= fs {
		if err := e.encodeFrame(fs, emit); err != nil {
			return err
		}
		for ch := 0; ch < e.channels; ch++ {
			e.buf[ch] = append(e.buf[ch][:0], e.buf[ch][fs:]...)
		}
	}
	return nil
}

// Finish pads the tail to a frame, flushes the filterbank latency with
// silent frames so every real sample reaches the output, drains the
// reservoir queue, and reports the gapless trailer.
func (e *Encoder) Finish(emit func(codec.Packet) error) (codec.Trailer, error) {
	fs := e.FrameSize()
	// Pad the partial tail frame to a whole frame with silence.
	if n := len(e.buf[0]); n > 0 {
		for ch := 0; ch < e.channels; ch++ {
			e.buf[ch] = append(e.buf[ch], make([]float32, fs-n)...)
		}
		if err := e.encodeFrame(fs, emit); err != nil {
			return codec.Trailer{}, err
		}
		for ch := 0; ch < e.channels; ch++ {
			e.buf[ch] = e.buf[ch][:0]
		}
	}
	// Flush the encoder's own latency: silent frames push the last real
	// samples out of the MDCT overlap and polyphase history. The count is
	// flushFrames, the same constant FramesFor projects against.
	for i := 0; i < flushFrames; i++ {
		for ch := 0; ch < e.channels; ch++ {
			e.buf[ch] = append(e.buf[ch][:0], make([]float32, fs)...)
		}
		if err := e.encodeFrame(fs, emit); err != nil {
			return codec.Trailer{}, err
		}
	}
	for ch := 0; ch < e.channels; ch++ {
		e.buf[ch] = e.buf[ch][:0]
	}
	// Emit every remaining queued frame; their main slots are final (any
	// unwritten tail is legal zero stuffing).
	if err := e.flushQueue(emit); err != nil {
		return codec.Trailer{}, err
	}

	// Gapless trailer: the decoder drops encDelay leading and padding
	// trailing samples. encDelay is the fixed encoder priming; padding makes
	// the kept length equal the real input length.
	delay := int64(EncoderDelay)
	padding := e.outSamples - e.inSamples - delay
	if padding < 0 {
		padding = 0
	}
	return codec.Trailer{Samples: e.inSamples, Delay: delay, Padding: padding}, nil
}

// EncoderDelay is the encoder's intrinsic priming in samples: the leading
// output samples that precede the first real input sample, carried in the
// LAME tag so decoders trim them. It is the encoder half of the round-trip
// latency (481-sample polyphase group delay plus the MDCT's 576-sample
// one-granule overlap, 1057 total) measured against the decoder, minus the
// decoder's own 529-sample delay that the read side adds back.
const EncoderDelay = 1057 - 529

// flushFrames is the number of silent frames Finish appends to push the last
// real samples out of the filterbank and MDCT overlap.
const flushFrames = 2

// Delay reports the encoder's gapless delay (EncoderDelay) for the muxer's
// LAME tag.
func (e *Encoder) Delay() int { return EncoderDelay }

// FramesFor returns the number of MP3 frames the encoder emits for n input
// samples at the given sample rate: the whole and padded-tail frames plus
// the flush. The muxer uses it to size the gapless padding up front on a
// non-seekable (live) stream, where the exact trailer cannot be back-patched.
func FramesFor(n int64, rate int) int {
	fs := int64(1152)
	if v, _, ok := legalRate(rate); ok && v != MPEG1 {
		fs = 576
	}
	return int((n+fs-1)/fs) + flushFrames
}

// encodeFrame encodes one whole frame (fs samples per channel) from the head
// of the FIFO and places it in the reservoir queue, emitting any frames the
// write cursor has completed.
func (e *Encoder) encodeFrame(fs int, emit func(codec.Packet) error) error {
	// Analysis: fill the staged spectra for every granule and channel.
	for gr := 0; gr < e.granules; gr++ {
		for ch := 0; ch < e.channels; ch++ {
			e.ana[ch].granuleMDCT(e.buf[ch][gr*576:gr*576+576], &e.xr[gr][ch])
		}
	}

	pad := e.nextPadding()
	h := e.header(pad)
	frameLen := h.Size()
	slots := frameLen - HeaderLen - e.siLen

	// Reservoir bytes available as backward reference, capped by the field.
	res := e.physEnd - e.writePos
	if res > e.resCap {
		// Unreferenceable old bytes become stuffing; advance past them.
		e.writePos += res - e.resCap
		res = e.resCap
	}
	mdb := res
	availBits := slots*8 + res*8

	// Quantize each granule-channel against a share of the budget, letting
	// busy ones borrow the reservoir and quiet ones replenish it.
	nGC := e.granules * e.channels
	perGC := availBits / nGC
	var q [2][2]gcQuant
	spent := 0
	idx := 0
	for gr := 0; gr < e.granules; gr++ {
		for ch := 0; ch < e.channels; ch++ {
			idx++
			budget := perGC
			if idx == nGC {
				budget = availBits - spent // last one takes the remainder
			}
			if budget < 0 {
				budget = 0
			}
			q[gr][ch] = quantizeGranule(&e.xr[gr][ch], e.row, budget)
			spent += q[gr][ch].part23
		}
	}

	// Build the main data: per granule, per channel, scalefactors (none at
	// baseline) then Huffman spectrum, byte-aligned for the whole frame.
	e.mw.reset()
	for gr := 0; gr < e.granules; gr++ {
		for ch := 0; ch < e.channels; ch++ {
			e.writeGranuleData(&q[gr][ch])
		}
	}
	e.mw.align()
	main := e.mw.buf

	si := e.writeSideInfo(mdb, &q)

	f := pendingFrame{hdr: headerBytes(h), si: si, main: make([]byte, slots), start: e.physEnd, spf: h.SamplesPerFrame()}
	e.frames = append(e.frames, f)
	e.physEnd += slots
	e.outSamples += int64(h.SamplesPerFrame())

	// Place the logical main data into the stream starting at the cursor.
	e.writeLogical(main)

	return e.emitReady(emit)
}

// writeGranuleData writes one granule-channel's main data: scalefactors
// (zero at baseline, so nothing) then the Huffman-coded spectrum. The region
// boundaries were resolved once by planHuffman (region0End/region1End).
func (e *Encoder) writeGranuleData(q *gcQuant) {
	bigEnd := q.bigValues * 2
	for i := 0; i+1 < bigEnd; i += 2 {
		t := q.table[0]
		if i >= q.region1End {
			t = q.table[2]
		} else if i >= q.region0End {
			t = q.table[1]
		}
		e.mw.writePair(t, q.ix[i], q.ix[i+1])
	}
	for i := bigEnd; i+3 < bigEnd+q.count1*4 && i+3 < 576; i += 4 {
		e.mw.writeQuad(q.count1Table, q.ix[i], q.ix[i+1], q.ix[i+2], q.ix[i+3])
	}
}

// writeSideInfo serializes the frame's side information into a fresh byte
// slice of siLen bytes.
func (e *Encoder) writeSideInfo(mdb int, q *[2][2]gcQuant) []byte {
	w := &e.sw
	w.reset()
	if e.version == MPEG1 {
		w.writeBits(9, uint32(mdb))
		if e.channels == 1 {
			w.writeBits(5, 0)
		} else {
			w.writeBits(3, 0)
		}
		// scfsi: never share scalefactors (all zero anyway).
		for ch := 0; ch < e.channels; ch++ {
			w.writeBits(4, 0)
		}
	} else {
		w.writeBits(8, uint32(mdb))
		w.writeBits(uint(e.channels), 0)
	}
	for gr := 0; gr < e.granules; gr++ {
		for ch := 0; ch < e.channels; ch++ {
			e.writeGranuleSide(w, &q[gr][ch])
		}
	}
	w.align()
	out := make([]byte, len(w.buf))
	copy(out, w.buf)
	return out
}

// writeGranuleSide serializes one granule-channel's side-info fields.
func (e *Encoder) writeGranuleSide(w *bitWriter, q *gcQuant) {
	w.writeBits(12, uint32(q.part23))
	w.writeBits(9, uint32(q.bigValues))
	w.writeBits(8, uint32(q.globalGain))
	if e.version == MPEG1 {
		w.writeBits(4, 0) // scalefac_compress: slen1=slen2=0
	} else {
		w.writeBits(9, 0)
	}
	w.writeBits(1, 0) // window_switching: long blocks only
	w.writeBits(5, uint32(q.table[0]))
	w.writeBits(5, uint32(q.table[1]))
	w.writeBits(5, uint32(q.table[2]))
	w.writeBits(4, uint32(q.region0Count))
	w.writeBits(3, uint32(q.region1Count))
	if e.version == MPEG1 {
		w.writeBits(1, 0) // preflag
	}
	w.writeBits(1, 0)                     // scalefac_scale
	w.writeBits(1, uint32(q.count1Table)) // count1table_select
}

// writeLogical writes main-data bytes into the pending physical frames
// starting at the logical cursor, advancing it. The pending frames tile the
// logical stream contiguously, so it copies whole runs frame by frame in a
// single forward walk (O(bytes+frames)) rather than searching per byte.
func (e *Encoder) writeLogical(main []byte) {
	pos, src := e.writePos, 0
	for i := range e.frames {
		if src >= len(main) {
			break
		}
		f := &e.frames[i]
		if pos >= f.start+len(f.main) {
			continue // this frame lies entirely before the cursor
		}
		n := copy(f.main[pos-f.start:], main[src:])
		pos += n
		src += n
	}
	e.writePos = pos
}

// emitReady emits and drops every front frame whose main-data region the
// write cursor has fully passed.
func (e *Encoder) emitReady(emit func(codec.Packet) error) error {
	for len(e.frames) > 0 {
		f := &e.frames[0]
		if f.start+len(f.main) > e.writePos {
			break
		}
		if err := e.emit(f, emit); err != nil {
			return err
		}
		e.frames = e.frames[1:]
	}
	return nil
}

// flushQueue emits every remaining pending frame at end of stream.
func (e *Encoder) flushQueue(emit func(codec.Packet) error) error {
	for i := range e.frames {
		if err := e.emit(&e.frames[i], emit); err != nil {
			return err
		}
	}
	e.frames = e.frames[:0]
	return nil
}

// emit assembles and emits one physical frame packet.
func (e *Encoder) emit(f *pendingFrame, emit func(codec.Packet) error) error {
	pkt := make([]byte, 0, HeaderLen+len(f.si)+len(f.main))
	pkt = append(pkt, f.hdr[:]...)
	pkt = append(pkt, f.si...)
	pkt = append(pkt, f.main...)
	return emit(codec.Packet{Data: pkt, Dur: int64(f.spf), Sync: true})
}

// nextPadding returns the CBR padding slot bit for the next frame, keeping
// the running average bit rate exact via a fractional accumulator.
func (e *Encoder) nextPadding() bool {
	// Accumulate only the byte-fraction step (bounded below the sample rate),
	// so the accumulator never pads every frame nor overflows a 32-bit int.
	e.padAcc += e.padStep
	if e.padAcc >= e.padThresh {
		e.padAcc -= e.padThresh
		return true
	}
	return false
}

// headerBytes serializes a frame header to its 4 wire bytes.
func headerBytes(h Header) [4]byte {
	var b [4]byte
	b[0] = 0xFF
	verBits := byte(3)
	switch h.Version {
	case MPEG2:
		verBits = 2
	case MPEG25:
		verBits = 0
	}
	b[1] = 0xE0 | verBits<<3 | 1<<1 | 1 // layer III (01), no CRC protection (1)
	lsf := 0
	if h.Version != MPEG1 {
		lsf = 1
	}
	bi := 0
	for i := 1; i < 15; i++ {
		if bitrateKbps[lsf][i]*1000 == h.Bitrate {
			bi = i
			break
		}
	}
	pad := byte(0)
	if h.Padding {
		pad = 1
	}
	b[2] = byte(bi)<<4 | byte(h.rateIdx)<<2 | pad<<1
	b[3] = byte(h.Mode) << 6 // mode_ext 0, copyright/original/emphasis 0
	return b
}
