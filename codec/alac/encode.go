package alac

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/waxerr"
)

var _ codec.Encoder = (*Encoder)(nil)

// Encoder configuration, fixed for the baseline encoder. The magic cookie
// records the adaptive-Golomb tuning (pb/mb/kb/maxRun) the decoder reads
// back, so these values are part of the produced bitstream, not a private
// choice: the decoder adapts from exactly them.
const (
	// FrameSize is the encoder's native frame length in samples, the
	// reference default. The pipeline framer re-chunks to it.
	FrameSize = 4096

	encOrder    = 8 // adaptive-FIR predictor order (numactive)
	encDenShift = 4 // predictor coefficient denominator shift
	encMode     = 0 // single-pass FIR predictor (no order-1 cascade)
	encPBFactor = 4 // per-element pb scaling, /4: 4 keeps the cookie's pb
	encMixBits  = 2 // stereo decorrelation shift; mixRes searched against it

	defPB     = 40  // rice history multiplier (AGParamRec default)
	defMB     = 10  // rice initial history
	defKB     = 14  // rice k modifier
	defMaxRun = 255 // maximum zero-run seed
)

// EncoderVersion is the encoder's algorithm revision for cache keys
// (ADR-0004): the compressed bytes for a given input must not change
// without a bump.
const EncoderVersion = "alacenc-1"

// EncoderOptions configures the encoder. It is currently empty: ALAC is
// lossless with no bitstream-visible quality knob (unlike FLAC levels or
// MP3 bit rates), so a nil pointer is the norm. The type exists so the
// signature can gain options without a breaking change.
type EncoderOptions struct{}

// initCoefsAINIT seeds the adaptive predictor. The three leading taps are
// the reference encoder's lag-window seed; the decoder reads the written
// coefficients from the stream and adapts from them identically, so any
// seed round-trips losslessly and these only shape the first frames'
// compression.
var initCoefs = [encOrder]int16{38, -29, 12, 0, 0, 0, 0, 0}

// Encoder encodes integer PCM into ALAC frames. It implements
// codec.Encoder: one input chunk of FrameSize samples becomes one frame
// packet (a shorter final chunk becomes a partial frame). Each frame
// decodes independently, so there is no encoder delay and Finish flushes
// nothing.
type Encoder struct {
	fmt      audio.Format
	cfg      Config
	chanBits uint // predictor/Golomb channel width (coded depth, +1 for a CPE side)
	// bytesShifted is how many low bytes are stripped from each sample and
	// stored raw in the shift region, leaving a narrower high part to code.
	// It is 0 for 16/20/24-bit and 1 for 32-bit, where coding the full width
	// would need a 33-bit CPE side channel that overflows int32, the Golomb
	// escape's 32-bit window, and third-party decoders (ffmpeg rejects it).
	bytesShifted int

	pos      int64
	short    bool
	finished bool

	w bitWriter

	// per-frame scratch, sized to FrameSize at construction.
	chL, chR   []int32 // deinterleaved input channels
	hiL, hiR   []int32 // high parts fed to mix/predict when bytesShifted > 0
	mixU, mixV []int32 // decorrelated channels
	resU, resV []int32 // residuals kept for the chosen stereo mode
	tmpU, tmpV []int32 // residual scratch during the mixRes search
	coefScr    [encOrder]int16
}

// NewEncoder returns an Encoder for integer PCM in format f. ALAC carries
// mono or stereo at 16/20/24/32-bit; the row's adjust hook snaps other
// depths onto these before the chain reaches here.
func NewEncoder(f audio.Format, _ *EncoderOptions) (*Encoder, error) {
	if err := f.Valid(); err != nil {
		return nil, err
	}
	if f.Type != audio.Int {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			"alac: encodes integer PCM only, got "+f.String())
	}
	switch f.BitDepth {
	case 16, 20, 24, 32:
	default:
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("alac: bit depth %d, want 16/20/24/32", f.BitDepth))
	}
	if f.Channels < 1 || f.Channels > 2 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("alac: %d channels: only mono and stereo are supported", f.Channels))
	}
	cfg := Config{
		FrameLength:   FrameSize,
		BitDepth:      f.BitDepth,
		PB:            defPB,
		MB:            defMB,
		KB:            defKB,
		Channels:      f.Channels,
		MaxRun:        defMaxRun,
		MaxFrameBytes: 0,
		AvgBitRate:    0,
		SampleRate:    f.Rate,
	}
	cfg.Cookie = buildCookie(cfg)
	// Confirm the cookie round-trips: NewDecoder(cfg, f) must accept it, so
	// a wiring bug surfaces here, not mid-stream.
	if _, err := ParseMagicCookie(cfg.Cookie); err != nil {
		return nil, err
	}

	e := &Encoder{fmt: f, cfg: cfg}
	// 32-bit strips its low byte into the shift region so the coded high part
	// is 24-bit (a CPE side then fits in 25 bits, the same width as native
	// 24-bit that every decoder handles). 16/20/24-bit code the full width.
	if f.BitDepth == 32 {
		e.bytesShifted = 1
	}
	sideBit := uint(0)
	if f.Channels == 2 {
		sideBit = 1 // a CPE codes the side channel one bit wider
	}
	e.chanBits = uint(f.BitDepth-e.bytesShifted*8) + sideBit

	n := FrameSize
	e.chL = make([]int32, n)
	e.mixU = make([]int32, n)
	e.resU = make([]int32, n)
	if e.bytesShifted > 0 {
		e.hiL = make([]int32, n)
	}
	if f.Channels == 2 {
		e.chR = make([]int32, n)
		e.mixV = make([]int32, n)
		e.resV = make([]int32, n)
		e.tmpU = make([]int32, n)
		e.tmpV = make([]int32, n)
		if e.bytesShifted > 0 {
			e.hiR = make([]int32, n)
		}
	}
	return e, nil
}

// InputFormat returns the exact PCM format Encode expects.
func (e *Encoder) InputFormat() audio.Format { return e.fmt }

// FrameSize returns the block size the framer must deliver.
func (e *Encoder) FrameSize() int { return FrameSize }

// CodecConfig returns the 24-byte ALACSpecificConfig (the magic cookie) the
// mp4 muxer stores in the sample description.
func (e *Encoder) CodecConfig() []byte {
	return append([]byte(nil), e.cfg.Cookie...)
}

// Finish reports the stream totals. ALAC has no encoder delay, padding, or
// buffered frames, so nothing flushes.
func (e *Encoder) Finish(func(codec.Packet) error) (codec.Trailer, error) {
	if e.finished {
		return codec.Trailer{}, waxerr.New(waxerr.CodeInternal, "alac: Finish called twice")
	}
	e.finished = true
	return codec.Trailer{Samples: e.pos}, nil
}

// Encode encodes one chunk as one frame. Chunks must be FrameSize samples;
// a shorter chunk is accepted only as the stream's last (the framer
// guarantees this) and becomes a partial frame. The emitted packet is
// borrowed: Data is valid only during the callback.
func (e *Encoder) Encode(src *audio.Buffer, emit func(codec.Packet) error) error {
	switch {
	case e.finished:
		return waxerr.New(waxerr.CodeInternal, "alac: Encode after Finish")
	case src.Fmt != e.fmt:
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("alac: buffer format %v, encoder expects %v", src.Fmt, e.fmt))
	case src.N == 0:
		return nil
	case src.N > FrameSize:
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("alac: %d-sample chunk exceeds the %d-sample frame", src.N, FrameSize))
	case e.short:
		return waxerr.New(waxerr.CodeInternal,
			"alac: chunk after a short chunk (a partial frame is final; a splice needs a fresh encoder)")
	}
	if src.N < FrameSize {
		e.short = true
	}

	e.encodeFrame(src)
	pkt := codec.Packet{Data: e.w.buf, PTS: e.pos, Dur: int64(src.N), Sync: true}
	e.pos += int64(src.N)
	return emit(pkt)
}

// encodeFrame assembles one frame into e.w: a channel element, then the
// frame stop tag, byte-aligned. The compressed form is written first; for
// 16/20/24-bit the smaller of it and the uncompressed escape form is kept,
// but a shifted (32-bit) stream stays compressed because the escape element
// codes the full un-shifted width, which is exactly what shifting avoids.
func (e *Encoder) encodeFrame(src *audio.Buffer) {
	n := src.N
	stereo := e.fmt.Channels == 2

	copy(e.chL[:n], src.ChanI(0))
	if stereo {
		copy(e.chR[:n], src.ChanI(1))
	}

	tag := uint64(idSCE)
	if stereo {
		tag = idCPE
	}

	e.w.reset()
	e.w.writeBits(3, tag)
	if stereo {
		e.writeStereoCompressed(n)
	} else {
		e.writeMonoCompressed(n)
	}
	if e.bytesShifted == 0 && e.w.bitLen() >= 3+verbatimElemBits(n, e.fmt.Channels, e.fmt.BitDepth) {
		e.w.reset() // verbatim is no larger; prefer it
		e.writeVerbatimFrame(n, tag)
	}
	e.w.writeBits(3, idEND)
	e.w.align()
}

// writeVerbatimFrame writes the whole frame (tag + uncompressed escape
// element) into a freshly reset writer.
func (e *Encoder) writeVerbatimFrame(n int, tag uint64) {
	e.w.writeBits(3, tag)
	if e.fmt.Channels == 2 {
		e.writeStereoVerbatim(n)
	} else {
		e.writeMonoVerbatim(n)
	}
}

// verbatimElemBits is the bit cost of an uncompressed element: the element
// header (20 bits, plus 32 for a partial frame's sample count) and the raw
// samples at the stream depth.
func verbatimElemBits(n, channels, bitDepth int) int {
	hdr := 20
	if n < FrameSize {
		hdr += 32
	}
	return hdr + n*bitDepth*channels
}

// writeElementHeader emits the common per-element header (the inverse of the
// decoder's elementHeader). The escape (uncompressed) form carries no shift,
// so a shifted stream never takes it.
func (e *Encoder) writeElementHeader(n int, escape bool) {
	e.w.writeBits(4, 0)  // elementInstanceTag
	e.w.writeBits(12, 0) // reserved
	hdr := uint64(e.bytesShifted) << 1
	if n < FrameSize {
		hdr |= 8 // partialFrame
	}
	if escape {
		hdr |= 1
	}
	e.w.writeBits(4, hdr)
	if n < FrameSize {
		e.w.writeBits(32, uint64(uint32(n)))
	}
}

// writeMonoCompressed encodes one channel: header, predictor coefficients,
// the shift region, then the adaptive-Golomb residuals of the high part.
func (e *Encoder) writeMonoCompressed(n int) {
	in := e.chL
	if e.bytesShifted > 0 {
		sh := uint(e.bytesShifted * 8)
		for i := 0; i < n; i++ {
			e.hiL[i] = e.chL[i] >> sh
		}
		in = e.hiL
	}
	e.predict(in, e.resU, n)

	e.writeElementHeader(n, false)
	e.w.writeBits(8, 0) // mixBits, unused for mono
	e.w.writeBits(8, 0) // mixRes
	e.writePredictorHeader()
	e.writeShift(n)
	e.dynComp(e.resU[:n], e.chanBits, e.pbParam())
}

// writeStereoCompressed searches the stereo decorrelation, then writes the
// mixing parameters, both channels' predictor headers, the shift region, and
// the residuals.
func (e *Encoder) writeStereoCompressed(n int) {
	l, r := e.chL, e.chR
	if e.bytesShifted > 0 {
		sh := uint(e.bytesShifted * 8)
		for i := 0; i < n; i++ {
			e.hiL[i] = e.chL[i] >> sh
			e.hiR[i] = e.chR[i] >> sh
		}
		l, r = e.hiL, e.hiR
	}
	mixRes := e.decorrelate(l, r, n)

	e.writeElementHeader(n, false)
	e.w.writeBits(8, encMixBits)
	e.w.writeBits(8, uint64(uint8(int8(mixRes))))
	e.writePredictorHeader() // U
	e.writePredictorHeader() // V
	e.writeShift(n)
	e.dynComp(e.resU[:n], e.chanBits, e.pbParam())
	e.dynComp(e.resV[:n], e.chanBits, e.pbParam())
}

// decorrelate searches the mixRes candidates against l, r and leaves the
// winner's residuals in resU/resV, returning the chosen mixRes. The
// candidates span the mid/side family for mixBits=2: 0 is independent L/R, 2
// is mid/side, 4 is left/side.
func (e *Encoder) decorrelate(l, r []int32, n int) int {
	best, bestCost := 0, math.MaxInt
	for res := 0; res <= 4; res++ {
		mix(l, r, e.mixU, e.mixV, n, encMixBits, res)
		e.predict(e.mixU, e.tmpU, n)
		e.predict(e.mixV, e.tmpV, n)
		cost := residualCost(e.tmpU[:n]) + residualCost(e.tmpV[:n])
		if cost < bestCost {
			best, bestCost = res, cost
			copy(e.resU[:n], e.tmpU[:n])
			copy(e.resV[:n], e.tmpV[:n])
		}
	}
	return best
}

// writeShift emits the shift region: the low bytesShifted*8 bits of every
// sample, interleaved across channels, matching the decoder's readShift. It
// is a no-op when nothing is shifted.
func (e *Encoder) writeShift(n int) {
	if e.bytesShifted == 0 {
		return
	}
	sh := uint(e.bytesShifted * 8)
	if e.fmt.Channels == 2 {
		for i := 0; i < n; i++ {
			e.w.writeBits(sh, uint64(uint32(e.chL[i])))
			e.w.writeBits(sh, uint64(uint32(e.chR[i])))
		}
		return
	}
	for i := 0; i < n; i++ {
		e.w.writeBits(sh, uint64(uint32(e.chL[i])))
	}
}

// writePredictorHeader emits the fixed predictor mode/order and the initial
// coefficients (mode 0, order encOrder, denShift encDenShift, pbFactor 4).
func (e *Encoder) writePredictorHeader() {
	e.w.writeBits(8, uint64(encMode<<4|encDenShift))
	e.w.writeBits(8, uint64(encPBFactor<<5|encOrder))
	for _, c := range initCoefs {
		e.w.writeBits(16, uint64(uint16(c)))
	}
}

// pbParam is the effective adaptive-Golomb pb: the cookie pb scaled by the
// per-element pbFactor, /4, matching the decoder's (PB*pbFactor)/4.
func (e *Encoder) pbParam() uint32 { return (e.cfg.PB * encPBFactor) / 4 }

// predict runs the forward FIR predictor over in, writing residuals into
// res. It uses a fresh copy of the initial coefficients each call: the
// coefficients adapt in place, and the decoder reproduces the same
// adaptation from the written initials.
func (e *Encoder) predict(in, res []int32, n int) {
	e.coefScr = initCoefs
	pcBlock(in, res, n, e.coefScr[:], encOrder, e.chanBits, encDenShift)
}

// writeMonoVerbatim writes the uncompressed escape element for one channel.
func (e *Encoder) writeMonoVerbatim(n int) {
	e.writeElementHeader(n, true)
	cb := uint(e.fmt.BitDepth)
	for i := 0; i < n; i++ {
		e.w.writeBits(cb, uint64(uint32(e.chL[i])))
	}
}

// writeStereoVerbatim writes the uncompressed escape element for a pair,
// samples interleaved L, R at the full stream depth.
func (e *Encoder) writeStereoVerbatim(n int) {
	e.writeElementHeader(n, true)
	cb := uint(e.fmt.BitDepth)
	for i := 0; i < n; i++ {
		e.w.writeBits(cb, uint64(uint32(e.chL[i])))
		e.w.writeBits(cb, uint64(uint32(e.chR[i])))
	}
}

// mix decorrelates a stereo pair into u, v, the inverse of the decoder's
// unmix. mixRes 0 is independent L/R; otherwise u carries a weighted blend
// and v the difference, exactly reconstructible in 32-bit two's complement.
func mix(l, r, u, v []int32, n, mixBits, mixRes int) {
	if mixRes == 0 {
		copy(u[:n], l[:n])
		copy(v[:n], r[:n])
		return
	}
	mod := int32(1) << uint(mixBits)
	m2 := mod - int32(mixRes)
	for i := 0; i < n; i++ {
		u[i] = (int32(mixRes)*l[i] + m2*r[i]) >> uint(mixBits)
		v[i] = l[i] - r[i]
	}
}

// pcBlock is the forward adaptive-FIR predictor, the inverse of the
// decoder's unpcBlock: residuals in pc1[j] such that the decoder's
// prediction plus residual reconstructs in[j] exactly. The coefficient
// adaptation is byte-identical to the decoder's, driven by the residual
// sign, so encoder and decoder stay in lockstep. int32 arithmetic (with its
// two's-complement wrap) matches the decoder's, part of the format.
func pcBlock(in, pc1 []int32, num int, coefs []int16, numactive int, chanBits, denShift uint) {
	if num <= 0 {
		return
	}
	var chanShift uint
	if chanBits < 32 {
		chanShift = 32 - chanBits
	}
	var denHalf int32
	if denShift > 0 {
		denHalf = int32(1) << (denShift - 1)
	}

	pc1[0] = in[0]
	if numactive == 0 {
		copy(pc1[1:num], in[1:num])
		return
	}
	// warm-up: the first numactive residuals are exact differences. A short
	// final frame may hold fewer samples than the order, so clamp the run.
	warm := numactive
	if warm > num-1 {
		warm = num - 1
	}
	for j := 1; j <= warm; j++ {
		pc1[j] = (in[j] - in[j-1]) << chanShift >> chanShift
	}
	lim := numactive + 1
	for j := lim; j < num; j++ {
		top := in[j-lim]
		var sum1 int32
		for k := 0; k < numactive; k++ {
			sum1 += int32(coefs[k]) * (in[j-1-k] - top)
		}
		del := (in[j] - top - ((sum1 + denHalf) >> denShift)) << chanShift >> chanShift
		pc1[j] = del

		del0 := del
		sg := signOf(del)
		if sg > 0 {
			for k := numactive - 1; k >= 0; k-- {
				dd := top - in[j-1-k]
				sgn := signOf(dd)
				coefs[k] -= int16(sgn)
				del0 -= int32(numactive-k) * ((sgn * dd) >> denShift)
				if del0 <= 0 {
					break
				}
			}
		} else if sg < 0 {
			for k := numactive - 1; k >= 0; k-- {
				dd := top - in[j-1-k]
				sgn := signOf(dd)
				coefs[k] += int16(sgn)
				del0 -= int32(numactive-k) * ((-sgn * dd) >> denShift)
				if del0 >= 0 {
					break
				}
			}
		}
	}
}

// zigzag maps a signed residual to the unsigned symbol the Golomb coder
// writes: non-negative d becomes 2d, negative d becomes -2d-1, branchless and
// exact at the int32 boundary.
func zigzag(d int32) uint32 { return uint32(d)<<1 ^ uint32(d>>31) }

// residualCost approximates the Golomb cost of a residual block for the
// mixRes search: the zigzag magnitude's bit length plus one per sample.
func residualCost(res []int32) int {
	c := 0
	for _, d := range res {
		c += bits.Len32(zigzag(d)) + 1
	}
	return c
}

// dynComp encodes residuals with the adaptive-Golomb coder, the inverse of
// the decoder's dynDecomp: the running mean mb drives the Golomb parameter,
// short residual runs collapse into a zero-run escape, and both sides evolve
// mb identically so the decoder tracks the same parameters.
func (e *Encoder) dynComp(pc []int32, chanBits uint, pb uint32) {
	numSamples := len(pc)
	maxSize := uint32(chanBits)
	mb := e.cfg.MB
	kb := e.cfg.KB
	wb := uint32(1)<<kb - 1
	zmode := uint32(0)
	c := 0
	for c < numSamples {
		m := mb >> qbShift
		k := lg3a(m)
		if k > kb {
			k = kb
		}
		m = 1<<k - 1

		nfull := zigzag(pc[c])
		nr := nfull - zmode
		e.w.dynJam32(nr, m, k, maxSize)
		c++

		mb = pb*(nr+zmode) + mb - ((pb * mb) >> qbShift)
		if nr > nMaxMeanClamp {
			mb = nMeanClampVal
		}
		zmode = 0

		if (mb<<mmulShift) < qb && c < numSamples {
			zmode = 1
			k = uint32(int(lead(mb)) - bitOff + int((mb+mOff)>>mDenShift))
			mz := (uint32(1)<<k - 1) & wb
			run := uint32(0)
			for c < numSamples && pc[c] == 0 && run < 65535 {
				run++
				c++
			}
			e.w.dynJam16(run, mz, k)
			if run >= 65535 {
				zmode = 0
			}
			mb = 0
		}
	}
}

// dynJam32 writes one adaptive-Golomb value, the inverse of dynGet32: a
// unary prefix of ones then a truncated-binary suffix, or a 9-ones escape
// followed by the value in maxbits bits when the prefix would overflow.
func (w *bitWriter) dynJam32(n, m, k, maxbits uint32) {
	pre := n / m
	if pre >= maxPrefix32 {
		w.writeOnes(maxPrefix32)
		w.writeBits(uint(maxbits), uint64(n))
		return
	}
	w.writeOnes(int(pre))
	w.writeBits(1, 0)
	if k != 1 {
		writeGolombSuffix(w, n-pre*m, k)
	}
}

// dynJam16 writes one zero-run length, the inverse of dynGet16: the same
// prefix/suffix scheme with a 16-bit escape.
func (w *bitWriter) dynJam16(n, m, k uint32) {
	pre := n / m
	if pre >= maxPrefix16 {
		w.writeOnes(maxPrefix16)
		w.writeBits(maxDataTypeBits16, uint64(n))
		return
	}
	w.writeOnes(int(pre))
	w.writeBits(1, 0)
	writeGolombSuffix(w, n-pre*m, k)
}

// writeGolombSuffix writes the truncated-binary suffix s in [0, 2^k-2]: a
// zero suffix costs k-1 zero bits, and a nonzero suffix s is the k-bit value
// s+1, matching the decoder's k/k-1-bit read.
func writeGolombSuffix(w *bitWriter, s, k uint32) {
	if s == 0 {
		w.writeBits(uint(k-1), 0)
	} else {
		w.writeBits(uint(k), uint64(s+1))
	}
}

// buildCookie packs cfg into the 24-byte ALACSpecificConfig.
func buildCookie(cfg Config) []byte {
	b := make([]byte, CookieLen)
	binary.BigEndian.PutUint32(b[0:], cfg.FrameLength)
	// b[4] compatibleVersion = 0
	b[5] = byte(cfg.BitDepth)
	b[6] = byte(cfg.PB)
	b[7] = byte(cfg.MB)
	b[8] = byte(cfg.KB)
	b[9] = byte(cfg.Channels)
	binary.BigEndian.PutUint16(b[10:], uint16(cfg.MaxRun))
	binary.BigEndian.PutUint32(b[12:], cfg.MaxFrameBytes)
	binary.BigEndian.PutUint32(b[16:], cfg.AvgBitRate)
	binary.BigEndian.PutUint32(b[20:], uint32(cfg.SampleRate))
	return b
}
