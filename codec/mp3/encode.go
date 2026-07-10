package mp3

// Layer III encoder. It implements codec.Encoder: PCM chunks in, whole MP3
// frames out. The signal chain is the exact inverse of the decoder
// (analysis.go: polyphase filterbank, forward MDCT, inverse alias), guided
// by the shared psychoacoustic model (dsp/psy), followed by the two-loop
// quantizer and Huffman planner (quantize.go, huffenc.go) and frame
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
//
// Stereo frames choose per frame between independent L/R coding and
// mid/side joint stereo (mode_extension bit 2) by comparing perceptual bit
// demand; the forward butterfly is m=(l+r)/sqrt2, s=(l-r)/sqrt2, whose
// 1/sqrt2 the decoder folds into requantization. In VBR mode each frame
// picks the smallest legal bit-rate index that carries its noise-driven
// demand, instead of fitting a fixed budget.

import (
	"fmt"
	"math"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/dsp/psy"
	"github.com/colespringer/waxflow/waxerr"
)

var _ codec.Encoder = (*Encoder)(nil)

// EncoderVersion is the encoder's cache-key version constant (ADR-0004):
// bump on any change that alters the encoded bitstream. It composes the
// psychoacoustic model's revision: retuning dsp/psy changes these streams.
const EncoderVersion = "mp3-enc-2+" + psy.Version

// thrCalib maps psy thresholds (FFT energy of unit-full-scale input, 1024
// point Hann analysis) onto the encoder's spectral energy scale (the
// polyphase+MDCT lines the quantizer sees). Measured, not derived: the
// FFT side is the analytic (3/32)N^2 Hann sine energy, but the polyphase
// window's passband gain does not factor cleanly. Full-scale sines land
// at ratios 3e-6..1.5e-5 across the band (block phase and subband-edge
// leakage account for the spread); the pinning test (TestPsyCalibration)
// re-measures the geometry and fails if this constant drifts out of it.
const thrCalib = 1.0e-5

// psyOffsetDB is the model's SNR-demand offset for CBR encodes, the
// encoder's master quality tuning constant (positive demands lower
// thresholds and so more bits per band before the rate loop pushes back).
const psyOffsetDB = 0.0

// EncoderOptions configures a Layer III encoder.
type EncoderOptions struct {
	// Bitrate is the bit rate in bits per second: the constant rate in CBR
	// mode, the quality anchor in VBR mode. It must be a whole number of
	// kbit/s; zero selects the default (128 kbit/s, or the closest legal
	// rate at low sample rates).
	Bitrate int
	// VBR sizes each frame to its content instead of a constant rate: the
	// psychoacoustic demand picks the bits, and the frame header carries
	// the smallest legal bit-rate index that holds them. Bitrate anchors
	// the quality level.
	VBR bool
}

// DefaultBitrate is the CBR bit rate used when EncoderOptions leaves it zero.
const DefaultBitrate = 128000

// psyHistLen is the analysis history kept per channel: the psy window is
// the trailing 1024 samples ending at each granule's end, so 1024-576
// samples of the previous frame stay reachable.
const psyHistLen = 1024 - 576

// Encoder is an MPEG-1/2/2.5 Layer III encoder (CBR or VBR).
type Encoder struct {
	fmt      audio.Format
	version  MPEGVersion
	rateIdx  int
	bitrate  int
	vbr      bool
	channels int
	row      int
	granules int // granules per frame: 2 (MPEG-1) or 1 (MPEG-2/2.5)
	siLen    int // side-info length in bytes
	resCap   int // reservoir cap in bytes (main_data_begin field maximum)
	cutLine  int // spectral lines at and above this index are zeroed

	ana [2]analyzer        // per-channel filterbank + MDCT state
	buf [2][]float32       // per-channel PCM FIFO awaiting whole frames
	xr  [2][2][576]float32 // staged spectra: [granule][channel]

	// Psychoacoustics: one model per channel (prediction and pre-echo
	// history are per channel), a rolling analysis window, and the
	// per-frame threshold/energy/demand state the quantizer consumes.
	psy     [2]*psy.Model
	psyHist [2][psyHistLen]float32
	psyBuf  []float32
	thr     [2][2][nSfBands]float64 // allowed noise energy: [granule][channel][band]
	bandE   [2][2][nSfBands]float64 // spectral band energy, post stereo transform
	avgPE   float64                 // perceptual-entropy EMA for reservoir modulation

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

// NewEncoder returns an encoder for the given input format. The format
// must be float32, mono or stereo, at a Layer III sample rate.
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
	vbr := false
	if opts != nil {
		if opts.Bitrate != 0 {
			bitrate = opts.Bitrate
		}
		vbr = opts.VBR
	}
	// The bit rate is whole kbit/s; a non-multiple is malformed (and would
	// otherwise pick no header index and emit free format). The value is then
	// clamped to a rate the layer actually supports. In VBR mode the same
	// clamped value anchors the quality level.
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
		vbr:      vbr,
		channels: f.Channels,
	}
	e.granules = 1
	e.resCap = 255
	if ver == MPEG1 {
		e.granules = 2
		e.resCap = 511
	}
	h := e.header(false, false)
	e.row = h.rateRow()
	e.siLen = h.SideInfoLen()
	// Padding accumulator constants: each frame carries the byte fraction the
	// floored Header.Size drops, ((spf/8)*bitrate) % rate, and a padding slot
	// lands when a whole byte accumulates. (CBR only; VBR frames never pad.)
	e.padStep = ((h.SamplesPerFrame() / 8) * e.bitrate) % h.Rate
	e.padThresh = h.Rate

	// Bandwidth cutoff: spending the budget below the cutoff beats coding
	// masked content at the top; the offsets scale with the per-channel
	// rate (the aac row's formula).
	cutoff := 3000.0 + float64(bitrate)/float64(f.Channels)/5
	cutoff = math.Min(cutoff, 0.94*float64(h.Rate)/2)
	lineHz := float64(h.Rate) / (2 * 576)
	e.cutLine = min(int(cutoff/lineHz), 576)

	// Psychoacoustic models, one per channel: 576-line granules analyzed
	// with a 1024-point FFT (the model 2 arrangement), band offsets from
	// the shared long-block edge table. VBR anchors quality by shifting
	// every SNR demand with the anchor rate; CBR distributes whatever the
	// fixed rate provides, so its offset stays at the tuned constant.
	offsetDB := psyOffsetDB
	if vbr {
		offsetDB = math.Min(math.Max(10*math.Log2(float64(bitrate)/128000), -18), 12)
	}
	offs := make([]int, len(sfbEdgesLong[e.row]))
	for i, v := range sfbEdgesLong[e.row] {
		offs[i] = v
	}
	for c := 0; c < f.Channels; c++ {
		m, err := psy.New(psy.Config{
			Rate: h.Rate, Lines: 576, FFTSize: 1024,
			BandOffsets: offs, OffsetDB: offsetDB,
		})
		if err != nil {
			return nil, err
		}
		e.psy[c] = m
	}
	e.psyBuf = make([]float32, psyHistLen+576*e.granules)
	return e, nil
}

// header builds the frame header for this encoder with the given padding
// and joint-stereo choice (mid/side sets mode_extension bit 2).
func (e *Encoder) header(pad, ms bool) Header {
	rate := rateHz[e.rateIdx]
	if e.version != MPEG1 {
		rate >>= 1
	}
	if e.version == MPEG25 {
		rate >>= 1
	}
	mode := ModeStereo
	modeExt := 0
	if e.channels == 1 {
		mode = ModeMono
	} else if ms {
		mode = ModeJoint
		modeExt = 2
	}
	return Header{
		rateIdx:  e.rateIdx,
		Version:  e.version,
		Rate:     rate,
		Channels: e.channels,
		Mode:     mode,
		ModeExt:  modeExt,
		Bitrate:  e.bitrate,
		Padding:  pad,
	}
}

// InputFormat is the PCM format the encoder consumes.
func (e *Encoder) InputFormat() audio.Format { return e.fmt }

// Bitrate is the actual constant bit rate in bits per second, after the
// requested rate is clamped to one the layer supports. In VBR mode it
// reports 0: frames size themselves, so no fixed rate exists (the
// PlanTranscode VBR contract).
func (e *Encoder) Bitrate() int {
	if e.vbr {
		return 0
	}
	return e.bitrate
}

// FrameSize is the encoder-native chunk in frames: one whole MP3 frame.
func (e *Encoder) FrameSize() int { return 576 * e.granules }

// CodecConfig is nil: MP3 is self-framing and carries no out-of-band setup.
func (e *Encoder) CodecConfig() []byte { return nil }

// maxSample bounds accepted input magnitudes (nominal full scale is 1;
// the bound is far above any legitimate pipeline level). Non-finite
// samples become 0 and larger magnitudes clamp, so the quantizer and the
// psychoacoustic model never see values their arithmetic cannot bound.
const maxSample = 8.0

// Encode buffers src and emits every whole frame that becomes available.
func (e *Encoder) Encode(src *audio.Buffer, emit func(codec.Packet) error) error {
	if src.Fmt != e.fmt {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("mp3: encode input %v disagrees with %v", src.Fmt, e.fmt))
	}
	for ch := 0; ch < e.channels; ch++ {
		e.buf[ch] = appendSanitized(e.buf[ch], src.ChanF(ch)[:src.N])
	}
	e.inSamples += int64(src.N)
	return e.drainFrames(emit)
}

// appendSanitized appends src to dst with non-finite samples zeroed and
// magnitudes clamped to maxSample.
func appendSanitized(dst, src []float32) []float32 {
	for _, v := range src {
		switch {
		case math.IsNaN(float64(v)) || math.IsInf(float64(v), 0):
			v = 0
		case v > maxSample:
			v = maxSample
		case v < -maxSample:
			v = -maxSample
		}
		dst = append(dst, v)
	}
	return dst
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
	// Analysis: fill the staged spectra for every granule and channel,
	// then zero everything past the bandwidth cutoff.
	for gr := 0; gr < e.granules; gr++ {
		for ch := 0; ch < e.channels; ch++ {
			e.ana[ch].granuleMDCT(e.buf[ch][gr*576:gr*576+576], &e.xr[gr][ch])
			for i := e.cutLine; i < 576; i++ {
				e.xr[gr][ch][i] = 0
			}
		}
	}

	// Psychoacoustics: each granule's window is the trailing 1024 samples
	// ending at its granule boundary, hopping 576 so the model's two-frame
	// prediction history stays aligned. PE feeds the reservoir modulation.
	pe := 0.0
	for ch := 0; ch < e.channels; ch++ {
		copy(e.psyBuf[:psyHistLen], e.psyHist[ch][:])
		copy(e.psyBuf[psyHistLen:], e.buf[ch][:fs])
		copy(e.psyHist[ch][:], e.buf[ch][fs-psyHistLen:fs])
		chPE := 0.0
		for gr := 0; gr < e.granules; gr++ {
			res, err := e.psy[ch].Analyze(e.psyBuf[gr*576 : gr*576+1024])
			if err != nil {
				return err
			}
			for b := 0; b < nSfBands; b++ {
				e.thr[gr][ch][b] = res.Thr[b] * thrCalib
			}
			chPE += res.PE
		}
		pe = math.Max(pe, chPE)
	}

	// Spectral band energies (post cutoff), for the stereo decision and
	// the demand-driven budget split.
	edges := &sfbEdgesLong[e.row]
	for gr := 0; gr < e.granules; gr++ {
		for ch := 0; ch < e.channels; ch++ {
			for b := 0; b < nSfBands; b++ {
				s := 0.0
				for i := edges[b]; i < edges[b+1]; i++ {
					v := float64(e.xr[gr][ch][i])
					s += v * v
				}
				e.bandE[gr][ch][b] = s
			}
		}
	}

	// Per-frame stereo mode: mid/side when its perceptual demand is lower.
	ms := false
	if e.channels == 2 {
		ms = e.decideMS()
	}

	// Reservoir bytes available as backward reference, capped by the field.
	res := e.physEnd - e.writePos
	if res > e.resCap {
		// Unreferenceable old bytes become stuffing; advance past them.
		e.writePos += res - e.resCap
		res = e.resCap
	}
	mdb := res

	var q [2][2]gcQuant
	var h Header
	var slots int
	if e.vbr {
		e.quantizeVBR(&q, res)
	} else {
		pad := e.nextPadding()
		h = e.header(pad, ms)
		slots = h.Size() - HeaderLen - e.siLen
		e.quantizeCBR(&q, slots, res, pe)
	}

	// Build the main data: per granule, per channel, scalefactors then the
	// Huffman spectrum, byte-aligned for the whole frame.
	e.mw.reset()
	for gr := 0; gr < e.granules; gr++ {
		for ch := 0; ch < e.channels; ch++ {
			e.writeGranuleData(&q[gr][ch])
		}
	}
	e.mw.align()
	main := e.mw.buf

	if e.vbr {
		// The frame is the smallest legal rate whose slots carry what the
		// reservoir cannot.
		h, slots = e.vbrFrameFor(len(main)-res, ms)
	}

	si := e.writeSideInfo(mdb, &q)

	f := pendingFrame{hdr: headerBytes(h), si: si, main: make([]byte, slots), start: e.physEnd, spf: h.SamplesPerFrame()}
	e.frames = append(e.frames, f)
	e.physEnd += slots
	e.outSamples += int64(h.SamplesPerFrame())

	// Place the logical main data into the stream starting at the cursor.
	e.writeLogical(main)

	return e.emitReady(emit)
}

// quantizeCBR runs the frame's granule-channels against the fixed frame
// budget: the spend target is modulated by perceptual difficulty (hard
// frames borrow the reservoir, easy frames replenish it, bounded so
// unspent bits stay referenceable), and the target splits across
// granule-channels by perceptual demand.
func (e *Encoder) quantizeCBR(q *[2][2]gcQuant, slots, res int, pe float64) {
	availBits := slots*8 + res*8
	if e.avgPE == 0 {
		e.avgPE = pe
	}
	e.avgPE = 0.95*e.avgPE + 0.05*pe
	difficulty := 1.0
	if e.avgPE > 0 {
		difficulty = math.Min(math.Max(pe/e.avgPE, 0.7), 1.4)
	}
	target := int(float64(slots*8) * difficulty)
	// Never leave more behind than the next frame can reference.
	minSpend := slots*8 + res*8 - e.resCap*8
	target = max(target, minSpend, 0)
	target = min(target, availBits)

	e.splitAndQuantize(q, target)
}

// vbrTightness converts perceptual demand (information bits above the
// masking threshold) into a granule budget: Huffman coding spends more
// than the entropy bound, and scalefactors ride along. vbrFloorBits keeps
// quiet granules from starving their side structure.
const (
	vbrTightness = 1.1
	vbrFloorBits = 150
)

// quantizeVBR sizes each granule-channel by its own noise-driven demand,
// capped so the frame always fits the largest legal rate plus the
// reservoir (the final rate is chosen after the main data is built).
func (e *Encoder) quantizeVBR(q *[2][2]gcQuant, res int) {
	_, maxSlots := e.vbrFrameFor(1<<30, false) // largest legal frame
	capacity := (maxSlots+res)*8 - 8           // alignment margin

	nGC := e.granules * e.channels
	var budgets [4]int
	total := 0
	i := 0
	for gr := 0; gr < e.granules; gr++ {
		for ch := 0; ch < e.channels; ch++ {
			d := e.gcDemand(gr, ch)
			budgets[i] = min(int(d*vbrTightness)+vbrFloorBits, part23Max)
			total += budgets[i]
			i++
		}
	}
	if total > capacity {
		for i := range budgets[:nGC] {
			budgets[i] = budgets[i] * capacity / total
		}
	}
	i = 0
	for gr := 0; gr < e.granules; gr++ {
		for ch := 0; ch < e.channels; ch++ {
			q[gr][ch] = quantizeGranule(quantIn{
				xr: &e.xr[gr][ch], row: e.row,
				thr: &e.thr[gr][ch], mpeg1: e.version == MPEG1,
			}, budgets[i])
			i++
		}
	}
}

// splitAndQuantize distributes target bits across the frame's
// granule-channels proportionally to perceptual demand (with a floor so
// no granule starves) and quantizes each against its share.
func (e *Encoder) splitAndQuantize(q *[2][2]gcQuant, target int) {
	nGC := e.granules * e.channels
	var demand [4]float64
	total := 0.0
	i := 0
	for gr := 0; gr < e.granules; gr++ {
		for ch := 0; ch < e.channels; ch++ {
			demand[i] = e.gcDemand(gr, ch)
			total += demand[i]
			i++
		}
	}
	floor := target / (nGC * 4)
	spent := 0
	i = 0
	for gr := 0; gr < e.granules; gr++ {
		for ch := 0; ch < e.channels; ch++ {
			var budget int
			if i == nGC-1 {
				budget = target - spent
			} else if total > 0 {
				budget = int(float64(target) * demand[i] / total)
			} else {
				budget = target / nGC
			}
			budget = max(budget, floor)
			budget = min(budget, target-spent)
			budget = max(budget, 0)
			q[gr][ch] = quantizeGranule(quantIn{
				xr: &e.xr[gr][ch], row: e.row,
				thr: &e.thr[gr][ch], mpeg1: e.version == MPEG1,
			}, budget)
			spent += budget
			i++
		}
	}
}

// gcDemand is one granule-channel's perceptual bit demand: information
// above the masking threshold, in bits.
func (e *Encoder) gcDemand(gr, ch int) float64 {
	edges := &sfbEdgesLong[e.row]
	d := 0.0
	for b := 0; b < nSfBands; b++ {
		en, thr := e.bandE[gr][ch][b], e.thr[gr][ch][b]
		if thr > 0 && en > thr {
			d += float64(edges[b+1]-edges[b]) * math.Log2(en/thr)
		}
	}
	return d
}

// vbrFrameFor returns the header of the smallest legal bit rate whose
// main-data slots hold need bytes, and its slot count. Past the largest
// rate it returns that largest frame (the caller's budgets are capped so
// this cannot underrun).
func (e *Encoder) vbrFrameFor(need int, ms bool) (Header, int) {
	lsf := 0
	if e.version != MPEG1 {
		lsf = 1
	}
	h := e.header(false, ms)
	var slots int
	for i := 1; i < 15; i++ {
		h.Bitrate = bitrateKbps[lsf][i] * 1000
		slots = h.Size() - HeaderLen - e.siLen
		if slots >= need {
			break
		}
	}
	return h, slots
}

// decideMS chooses this frame's stereo coding by total perceptual demand:
// left/right against mid/side under the conservative shared threshold
// min(thrL, thrR). When mid/side wins it transforms the spectra in place
// (m=(l+r)/sqrt2, s=(l-r)/sqrt2, the energy-preserving butterfly whose
// 1/sqrt2 the decoder's requantizer folds back), and installs the shared
// thresholds and energies.
func (e *Encoder) decideMS() bool {
	const invSqrt2 = 0.7071067811865476
	edges := &sfbEdgesLong[e.row]
	var eM, eS [2][nSfBands]float64
	demLR, demMS := 0.0, 0.0
	for gr := 0; gr < e.granules; gr++ {
		l, r := &e.xr[gr][0], &e.xr[gr][1]
		for b := 0; b < nSfBands; b++ {
			var em, es float64
			for i := edges[b]; i < edges[b+1]; i++ {
				lv, rv := float64(l[i]), float64(r[i])
				m := (lv + rv) * invSqrt2
				s := (lv - rv) * invSqrt2
				em += m * m
				es += s * s
			}
			eM[gr][b], eS[gr][b] = em, es
			w := float64(edges[b+1] - edges[b])
			thrL, thrR := e.thr[gr][0][b], e.thr[gr][1][b]
			thrMS := math.Min(thrL, thrR)
			demLR += demandOf(e.bandE[gr][0][b], thrL, w) + demandOf(e.bandE[gr][1][b], thrR, w)
			demMS += demandOf(em, thrMS, w) + demandOf(es, thrMS, w)
		}
	}
	if demMS >= demLR {
		return false
	}
	for gr := 0; gr < e.granules; gr++ {
		l, r := &e.xr[gr][0], &e.xr[gr][1]
		// The whole spectrum transforms: the decoder's mid/side butterfly
		// covers all 576 lines when intensity stereo is off.
		for i := 0; i < 576; i++ {
			lv, rv := float64(l[i]), float64(r[i])
			l[i] = float32((lv + rv) * invSqrt2)
			r[i] = float32((lv - rv) * invSqrt2)
		}
		for b := 0; b < nSfBands; b++ {
			thrMS := math.Min(e.thr[gr][0][b], e.thr[gr][1][b])
			e.thr[gr][0][b] = thrMS
			e.thr[gr][1][b] = thrMS
			e.bandE[gr][0][b] = eM[gr][b]
			e.bandE[gr][1][b] = eS[gr][b]
		}
	}
	return true
}

// demandOf is the perceptual bit demand of one band: information above
// the masking threshold.
func demandOf(energy, thr, width float64) float64 {
	if thr <= 0 || energy <= thr {
		return 0
	}
	return width * math.Log2(energy/thr)
}

// writeGranuleData writes one granule-channel's main data: scalefactors
// (scfsi is always zero, so every granule carries all four partitions)
// then the Huffman-coded spectrum. The region boundaries were resolved
// once by planHuffman (region0End/region1End).
func (e *Encoder) writeGranuleData(q *gcQuant) {
	band := 0
	for p, cnt := range sfPartCount {
		bits := uint(q.slen[p])
		for i := 0; i < cnt; i++ {
			if bits > 0 {
				e.mw.writeBits(bits, uint32(q.sfTx[band]))
			}
			band++
		}
	}
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
		w.writeBits(4, uint32(q.scfCompress))
	} else {
		w.writeBits(9, uint32(q.scfCompress))
	}
	w.writeBits(1, 0) // window_switching: long blocks only
	w.writeBits(5, uint32(q.table[0]))
	w.writeBits(5, uint32(q.table[1]))
	w.writeBits(5, uint32(q.table[2]))
	w.writeBits(4, uint32(q.region0Count))
	w.writeBits(3, uint32(q.region1Count))
	if e.version == MPEG1 {
		pre := uint32(0)
		if q.preflag {
			pre = 1
		}
		w.writeBits(1, pre)
	}
	w.writeBits(1, uint32(q.scfScale))
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
	b[3] = byte(h.Mode)<<6 | byte(h.ModeExt)<<4 // copyright/original/emphasis 0
	return b
}
