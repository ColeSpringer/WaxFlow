package flac

import (
	"crypto/md5"
	"fmt"
	"hash"
	"math/bits"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/waxerr"
)

var _ codec.Encoder = (*Encoder)(nil)

// DefaultEncoderLevel is the compression level used when EncoderOptions
// is nil, matching the reference encoder's default.
const DefaultEncoderLevel = 5

// EncoderOptions configures the encoder. A nil options pointer selects
// DefaultEncoderLevel; a non-nil one uses Level literally, so level 0 is
// expressible.
type EncoderOptions struct {
	// Level is the compression level, 0 (fastest) through 8 (smallest).
	// Levels trade encode speed for size and never affect decoded audio.
	Level int
}

// levelParams maps a compression level to its search effort, modeled on
// the reference encoder's presets: block size, LPC ceiling (0 keeps to
// fixed predictors), whether to search all stereo decorrelation modes,
// the Rice partition ceiling, and the apodization windows to try (Tukey
// taper fractions). Every level stays inside the streamable subset:
// blocks of at most 4608 and predictor orders of at most 12.
type levelParams struct {
	block   int
	maxLPC  int
	search  bool
	maxPart int
	apod    []float64
}

var levels = [9]levelParams{
	{block: 1152, maxLPC: 0, search: false, maxPart: 3},
	{block: 1152, maxLPC: 0, search: true, maxPart: 3},
	{block: 1152, maxLPC: 0, search: true, maxPart: 3},
	{block: 4096, maxLPC: 6, search: false, maxPart: 4, apod: []float64{0.5}},
	{block: 4096, maxLPC: 8, search: true, maxPart: 4, apod: []float64{0.5}},
	{block: 4096, maxLPC: 8, search: true, maxPart: 5, apod: []float64{0.5}},
	{block: 4096, maxLPC: 8, search: true, maxPart: 6, apod: []float64{0.5, 0.25}},
	{block: 4096, maxLPC: 12, search: true, maxPart: 6, apod: []float64{0.5, 0.25}},
	{block: 4096, maxLPC: 12, search: true, maxPart: 6, apod: []float64{0.5, 0.25, 0.75}},
}

// EncoderVersion is the encoder's algorithm revision for cache keys
// (ADR-0004): compressed bytes for the same input must never change
// without a bump. The level is part of the version because it changes
// the bytes while leaving the decoded samples untouched.
func EncoderVersion(level int) string {
	return fmt.Sprintf("flacenc-1-l%d", level)
}

// EncoderBlockSize returns the frame length in samples the given level
// encodes at, which is also Encoder.FrameSize for the pipeline framer.
// Levels outside 0..8 return 0.
func EncoderBlockSize(level int) int {
	if level < 0 || level >= len(levels) {
		return 0
	}
	return levels[level].block
}

// subframe kinds, distinct from the wire type codes they map to.
const (
	kindConstant = iota
	kindVerbatim
	kindFixed
	kindLPC
)

// subplan is one channel signal's chosen encoding, retained until the
// stereo decorrelation decision and the frame write.
type subplan struct {
	kind   int
	order  int
	wasted uint
	bps    uint // warmup/verbatim sample width: signal width minus wasted
	shift  uint
	qcoef  [maxLPCOrder]int64
	rice   ricePlan
	cost   int64
	x      []int64 // signal, shifted right by wasted
	res    []int64 // residual for fixed and LPC kinds
}

// slot is the per-signal scratch a subplan points into.
type slot struct {
	x, res, trial []int64
	params        [1 << 8]uint8
	plan          subplan
}

// Encoder encodes integer PCM into FLAC frames. It implements
// codec.Encoder: one input chunk of FrameSize samples becomes exactly
// one frame packet, so the pipeline framer drives the fixed blocking
// strategy (a short chunk is legal only as the last one).
type Encoder struct {
	fmt   audio.Format
	si    StreamInfo
	lv    levelParams
	level int

	frameNo  int64
	pos      int64
	short    bool // a short chunk was encoded; the stream must end
	finished bool

	md5    hash.Hash
	md5sum [16]byte
	md5buf []byte

	slots []slot
	wins  map[float64][]float64 // apodization windows for the full block size
	wx    []float64
	acf   []float64
	errs  []float64
	co    [][]float64
	rs    riceScratch
	w     bitWriter
}

// NewEncoder returns an Encoder for integer PCM in format f. A nil opts
// selects DefaultEncoderLevel. The format's layout must be the channel
// convention FLAC implies (the default layout), which is what the
// pipeline produces.
func NewEncoder(f audio.Format, opts *EncoderOptions) (*Encoder, error) {
	level := DefaultEncoderLevel
	if opts != nil {
		level = opts.Level
	}
	if level < 0 || level >= len(levels) {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("flac: compression level %d outside 0..8", level))
	}
	if err := f.Valid(); err != nil {
		return nil, err
	}
	lv := levels[level]
	si := StreamInfo{
		MinBlock: lv.block,
		MaxBlock: lv.block,
		Rate:     f.Rate,
		Channels: f.Channels,
		Bits:     f.BitDepth,
	}
	if _, err := si.MarshalBinary(); err != nil {
		return nil, err
	}
	if want := si.PCMFormat(); f != want {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("flac: cannot encode format %v (nearest encodable is %v)", f, want))
	}
	e := &Encoder{fmt: f, si: si, lv: lv, level: level, md5: md5.New()}
	if lv.maxLPC > 0 {
		e.wins = make(map[float64][]float64, len(lv.apod))
		for _, p := range lv.apod {
			w := make([]float64, lv.block)
			tukey(w, p)
			e.wins[p] = w
		}
		e.wx = make([]float64, lv.block)
		e.acf = make([]float64, lv.maxLPC+1)
		e.errs = make([]float64, lv.maxLPC+1)
		e.co = make([][]float64, lv.maxLPC)
		for i := range e.co {
			e.co[i] = make([]float64, i+1)
		}
	}
	return e, nil
}

// InputFormat returns the exact PCM format Encode expects.
func (e *Encoder) InputFormat() audio.Format { return e.fmt }

// FrameSize returns the block size the framer must deliver.
func (e *Encoder) FrameSize() int { return e.lv.block }

// CodecConfig returns the stream's STREAMINFO block body. Totals, frame
// size bounds, and the MD5 signature are unknown until the stream ends;
// a muxer with a seekable destination back-patches them (MD5 via the
// MD5 method after Finish).
func (e *Encoder) CodecConfig() []byte {
	b, err := e.si.MarshalBinary()
	if err != nil {
		// NewEncoder validated the fields; this cannot happen.
		panic(err)
	}
	return b
}

// MD5 returns the RFC 9639 signature of the unencoded audio: MD5 over
// interleaved samples, each in the fewest whole little-endian bytes the
// bit depth needs. Valid only after Finish; zero before.
func (e *Encoder) MD5() [16]byte { return e.md5sum }

// Finish reports the stream totals. FLAC has no encoder delay, padding,
// or buffered frames, so nothing flushes.
func (e *Encoder) Finish(func(codec.Packet) error) (codec.Trailer, error) {
	if e.finished {
		return codec.Trailer{}, waxerr.New(waxerr.CodeInternal, "flac: Finish called twice")
	}
	e.finished = true
	e.md5.Sum(e.md5sum[:0])
	return codec.Trailer{Samples: e.pos}, nil
}

// Encode encodes one chunk as one frame. Chunks must be FrameSize
// samples; a shorter chunk is accepted only as the stream's last (the
// framer guarantees this), because the fixed blocking strategy codes
// frame indexes, not positions. For the same reason a mid-stream
// discontinuity cannot be represented: a splice or a seek needs a fresh
// encoder, and the engine never routes one mid-stream today. The
// emitted packet is borrowed: Data is valid only during the callback.
func (e *Encoder) Encode(src *audio.Buffer, emit func(codec.Packet) error) error {
	switch {
	case e.finished:
		return waxerr.New(waxerr.CodeInternal, "flac: Encode after Finish")
	case src.Fmt != e.fmt:
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("flac: buffer format %v, encoder expects %v", src.Fmt, e.fmt))
	case src.N == 0:
		return nil
	case src.N > e.lv.block:
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("flac: %d-sample chunk exceeds the %d-sample block size", src.N, e.lv.block))
	case e.short:
		return waxerr.New(waxerr.CodeInternal,
			"flac: chunk after a short chunk (a short frame is final under the fixed blocking strategy; a splice needs a fresh encoder)")
	case e.frameNo > 1<<31-1:
		return waxerr.New(waxerr.CodeUnsupportedFormat, "flac: frame number overflows 31 bits")
	}
	if src.N < e.lv.block {
		e.short = true
	}

	e.updateMD5(src)
	e.encodeFrame(src)
	pkt := codec.Packet{Data: e.w.buf, PTS: e.pos, Dur: int64(src.N), Sync: true}
	e.frameNo++
	e.pos += int64(src.N)
	return emit(pkt)
}

// updateMD5 folds the chunk into the running signature: interleaved,
// little-endian, ceil(bits/8) bytes per sample, two's complement.
func (e *Encoder) updateMD5(src *audio.Buffer) {
	bs := (e.fmt.BitDepth + 7) / 8
	need := src.N * e.fmt.Channels * bs
	if cap(e.md5buf) < need {
		e.md5buf = make([]byte, need)
	}
	buf := e.md5buf[:need]
	ch := e.fmt.Channels
	for c := 0; c < ch; c++ {
		s := src.ChanI(c)
		off := c * bs
		step := ch * bs
		switch bs {
		case 1:
			for i, v := range s {
				buf[off+i*step] = byte(v)
			}
		case 2:
			for i, v := range s {
				idx := off + i*step
				buf[idx] = byte(v)
				buf[idx+1] = byte(v >> 8)
			}
		case 3:
			for i, v := range s {
				idx := off + i*step
				buf[idx] = byte(v)
				buf[idx+1] = byte(v >> 8)
				buf[idx+2] = byte(v >> 16)
			}
		default:
			for i, v := range s {
				idx := off + i*step
				buf[idx] = byte(v)
				buf[idx+1] = byte(v >> 8)
				buf[idx+2] = byte(v >> 16)
				buf[idx+3] = byte(v >> 24)
			}
		}
	}
	e.md5.Write(buf)
}

// Channel assignment plan indexes for the stereo search slots.
const (
	sigLeft = iota
	sigRight
	sigMid
	sigSide
)

// encodeFrame plans every channel signal and assembles the frame into
// e.w.
func (e *Encoder) encodeFrame(src *audio.Buffer) {
	n := src.N
	ch := e.fmt.Channels
	bps := uint(e.fmt.BitDepth)
	stereo := ch == 2 && e.lv.search

	nslots := ch
	if stereo {
		nslots = 4
	}
	if e.slots == nil {
		e.slots = make([]slot, nslots)
		for i := range e.slots {
			e.slots[i].x = make([]int64, e.lv.block)
			e.slots[i].res = make([]int64, e.lv.block)
			e.slots[i].trial = make([]int64, e.lv.block)
		}
	}

	for c := 0; c < ch; c++ {
		x := e.slots[c].x[:n]
		for i, v := range src.ChanI(c) {
			x[i] = int64(v)
		}
	}
	if stereo {
		l, r := e.slots[sigLeft].x[:n], e.slots[sigRight].x[:n]
		m, s := e.slots[sigMid].x[:n], e.slots[sigSide].x[:n]
		for i := 0; i < n; i++ {
			m[i] = (l[i] + r[i]) >> 1
			s[i] = l[i] - r[i]
		}
	}

	for i := 0; i < nslots; i++ {
		w := bps
		if stereo && i == sigSide {
			w++
		}
		e.planSignal(&e.slots[i], n, w)
	}

	assign := ch - 1
	order := make([]int, 0, 8) // slot write order
	if stereo {
		indep := e.slots[sigLeft].plan.cost + e.slots[sigRight].plan.cost
		left := e.slots[sigLeft].plan.cost + e.slots[sigSide].plan.cost
		right := e.slots[sigSide].plan.cost + e.slots[sigRight].plan.cost
		mid := e.slots[sigMid].plan.cost + e.slots[sigSide].plan.cost
		switch min(indep, left, right, mid) {
		case indep:
			assign, order = 1, append(order, sigLeft, sigRight)
		case left:
			assign, order = assignLeftSide, append(order, sigLeft, sigSide)
		case right:
			assign, order = assignRightSide, append(order, sigSide, sigRight)
		default:
			assign, order = assignMidSide, append(order, sigMid, sigSide)
		}
	} else {
		for c := 0; c < ch; c++ {
			order = append(order, c)
		}
	}

	e.w.reset()
	e.writeFrameHeader(n, assign)
	for _, i := range order {
		e.writeSubframe(&e.slots[i].plan, n)
	}
	e.w.align()
	crc := CRC16(e.w.buf)
	e.w.buf = append(e.w.buf, byte(crc>>8), byte(crc))
}

// planSignal picks the cheapest subframe encoding for one channel
// signal of width w bits, leaving the choice in sl.plan.
func (e *Encoder) planSignal(sl *slot, n int, w uint) {
	x := sl.x[:n]

	all := x[0]
	var or int64
	constant := true
	for _, v := range x {
		or |= v
		if v != all {
			constant = false
		}
	}
	if constant {
		sl.plan = subplan{kind: kindConstant, bps: w, cost: int64(8 + w), x: x}
		return
	}

	// Wasted bits: trailing zeros common to every sample shift out once
	// per subframe instead of riding every residual.
	wasted := uint(0)
	if tz := uint(bits.TrailingZeros64(uint64(or))); tz > 0 {
		wasted = min(tz, w-1)
		for i := range x {
			x[i] >>= wasted
		}
		w -= wasted
	}
	hdr := 8
	if wasted > 0 {
		hdr += int(wasted) // flag already in the 8; unary of wasted-1
	}

	sl.plan = subplan{kind: kindVerbatim, wasted: wasted, bps: w, cost: int64(hdr) + int64(n)*int64(w), x: x}

	// Fixed predictors: estimate the best order, then price it exactly.
	fo := bestFixedOrder(x, sl.trial[:n])
	fixedResidual(x, fo, sl.trial[:n])
	plan := planRice(sl.trial[:n], fo, n, e.lv.maxPart, &e.rs)
	cost := int64(hdr) + int64(fo)*int64(w) + 6 + plan.bits
	if cost < sl.plan.cost {
		e.adopt(sl, subplan{kind: kindFixed, order: fo, wasted: wasted, bps: w, rice: plan, cost: cost, x: x}, n)
	}

	// LPC: one candidate per apodization window at the estimated best
	// order. A predictor longer than half the block prices more header
	// than residual, so short final blocks cap the order.
	maxOrder := min(e.lv.maxLPC, (n-1)/2)
	if maxOrder < 1 {
		return
	}
	for _, p := range e.lv.apod {
		win := e.wins[p]
		if len(win) != n {
			win = make([]float64, n)
			tukey(win, p)
		}
		wx := e.wx[:n]
		for i, v := range x {
			wx[i] = float64(v) * win[i]
		}
		autocorr(wx, e.acf[:maxOrder+1])
		usable := levinson(e.acf[:maxOrder+1], e.co, e.errs)
		if usable < 1 {
			continue
		}
		m := estimateOrder(e.errs, usable, n, w)
		var q [maxLPCOrder]int64
		shift := quantizeLPC(e.co[m-1][:m], q[:m])
		lpcResidual(x, q[:m], shift, sl.trial[:n])
		plan := planRice(sl.trial[:n], m, n, e.lv.maxPart, &e.rs)
		cost := int64(hdr) + int64(m)*int64(w) + 4 + 5 + int64(m)*lpcPrecision + 6 + plan.bits
		if cost < sl.plan.cost {
			e.adopt(sl, subplan{kind: kindLPC, order: m, wasted: wasted, bps: w,
				shift: shift, qcoef: q, rice: plan, cost: cost, x: x}, n)
		}
	}
}

// adopt installs a trial plan as the slot's best: the trial residual
// swaps into the plan and the Rice parameters copy out of the shared
// scratch, which the next planRice call overwrites.
func (e *Encoder) adopt(sl *slot, plan subplan, n int) {
	sl.res, sl.trial = sl.trial, sl.res
	plan.res = sl.res[:n]
	copy(sl.params[:len(plan.rice.params)], plan.rice.params)
	plan.rice.params = sl.params[:len(plan.rice.params)]
	sl.plan = plan
}

// writeFrameHeader emits the fixed-strategy frame header including its
// CRC-8.
func (e *Encoder) writeFrameHeader(n, assign int) {
	w := &e.w
	w.writeBits(16, 0xFFF8)

	bsCode, bsBits, bsVal := 0, uint(0), 0
	for c, s := range blockSizes {
		if s == n {
			bsCode = c
			break
		}
	}
	if bsCode == 0 {
		if n <= 1<<8 {
			bsCode, bsBits, bsVal = 6, 8, n-1
		} else {
			bsCode, bsBits, bsVal = 7, 16, n-1
		}
	}

	rate := e.fmt.Rate
	rateCode, rateBits, rateVal := 0, uint(0), 0
	for c, r := range sampleRates {
		if r == rate && r != 0 {
			rateCode = c
			break
		}
	}
	if rateCode == 0 {
		switch {
		case rate%1000 == 0 && rate/1000 <= 0xFF:
			rateCode, rateBits, rateVal = 12, 8, rate/1000
		case rate <= 0xFFFF:
			rateCode, rateBits, rateVal = 13, 16, rate
		case rate%10 == 0 && rate/10 <= 0xFFFF:
			rateCode, rateBits, rateVal = 14, 16, rate/10
		}
		// Other rates defer to STREAMINFO (code 0), which is valid FLAC
		// but outside the streamable subset.
	}

	bitsCode := 0
	for c, b := range sampleBits {
		if b == e.fmt.BitDepth {
			bitsCode = c
			break
		}
	}

	w.writeBits(4, uint64(bsCode))
	w.writeBits(4, uint64(rateCode))
	w.writeBits(4, uint64(assign))
	w.writeBits(3, uint64(bitsCode))
	w.writeBits(1, 0)
	e.writeCodedNumber(uint64(e.frameNo))
	w.writeBits(bsBits, uint64(bsVal))
	w.writeBits(rateBits, uint64(rateVal))
	w.writeBits(8, uint64(crc8(w.buf)))
}

// writeCodedNumber emits the UTF-8-like variable-length integer frame
// headers carry, up to 36 bits.
func (e *Encoder) writeCodedNumber(v uint64) {
	w := &e.w
	if v < 0x80 {
		w.writeBits(8, v)
		return
	}
	// An encoding of total bytes holds 6*(total-1) continuation bits plus
	// 7-total lead bits: 11 bits at two bytes, five more per byte after.
	total := 2
	for lim := uint64(1) << 11; v >= lim && total < 7; total++ {
		lim <<= 5
	}
	lead := uint64(0xFF<<(8-total)) & 0xFF
	payload := uint(6 * (total - 1))
	head := 8 - total - 1 // payload bits in the lead byte
	if head < 0 {
		head = 0
	}
	w.writeBits(8, lead|(v>>payload)&(1<<uint(head)-1))
	for i := total - 2; i >= 0; i-- {
		w.writeBits(8, 0x80|(v>>(6*uint(i)))&0x3F)
	}
}

// writeSubframe emits one subframe per its plan.
func (e *Encoder) writeSubframe(p *subplan, n int) {
	w := &e.w
	w.writeBits(1, 0)
	switch p.kind {
	case kindConstant:
		w.writeBits(6, subConstant)
	case kindVerbatim:
		w.writeBits(6, subVerbatim)
	case kindFixed:
		w.writeBits(6, uint64(8+p.order))
	case kindLPC:
		w.writeBits(6, uint64(32+p.order-1))
	}
	if p.wasted > 0 {
		w.writeBits(1, 1)
		w.writeUnary(uint64(p.wasted - 1))
	} else {
		w.writeBits(1, 0)
	}

	switch p.kind {
	case kindConstant:
		w.writeSigned(p.bps, p.x[0])
	case kindVerbatim:
		for _, v := range p.x[:n] {
			w.writeSigned(p.bps, v)
		}
	case kindFixed:
		for _, v := range p.x[:p.order] {
			w.writeSigned(p.bps, v)
		}
		w.writeRice(p.res[:n], p.order, n, p.rice)
	case kindLPC:
		for _, v := range p.x[:p.order] {
			w.writeSigned(p.bps, v)
		}
		w.writeBits(4, uint64(lpcPrecision-1))
		w.writeBits(5, uint64(p.shift))
		for _, c := range p.qcoef[:p.order] {
			w.writeSigned(lpcPrecision, c)
		}
		w.writeRice(p.res[:n], p.order, n, p.rice)
	}
}
