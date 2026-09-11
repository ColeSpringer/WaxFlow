package wmalossless

import (
	"errors"
	"fmt"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// The packet, frame and subframe walk.
//
// The shape of this decoder is forced by one measurement: no encoder sets the
// length-prefix flag, so a frame carries no length and its extent is known
// only after its last subframe has been parsed. The frames that begin in
// packet k therefore cannot be decoded until packet k+1 supplies the bits that
// finish the last of them, and a decoder handed one packet per call emits
// packet k's frames on the call that delivers packet k+1. That is a one-packet
// output latency and not a sample delay: nothing is dropped, nothing is
// duplicated, and the frames come out in order. Drain is what recovers the
// frames of the last packet, which is between one and five frames of real
// audio, so it is not optional.
//
// A frame is not bounded by a packet in either direction and may span three or
// more of them, so the carry is a bit string with its own bit length rather
// than a byte slice: the join is bit-continuous with no padding and no
// realignment.

// errNotStarted is the seek condition, not damage: a coded subframe arrived
// before any seekable tile, so no filter has an order and nothing can be
// reconstructed. The frame is dropped and the rest of the packet with it.
var errNotStarted = errors.New("wmalossless: no seekable tile yet")

// Decoder decodes one WMA Lossless track.
type Decoder struct {
	cfg Config
	fmt audio.Format

	ch    []channel
	coded []bool

	// Shared state, all of it restated at a seekable tile.
	ac        acFilter
	acOn      bool
	mc        mclms
	mcOn      bool
	decorr    bool
	movave    uint
	quantStep int32
	// started is false until a seekable tile has defined the filters. A coded
	// subframe before that is undecodable.
	started bool

	// The cross-packet carry: the bits of the frames that began in the
	// previous packet, packed from bit 0.
	carry     bitAppender
	carryOpen bool
	// seq is the sequence number expected next, or -1 when unknown.
	seq int

	// The derived geometry, taken from the config once: it is a function of
	// the wire facts and cannot change for the life of a track.
	frameLen       int
	frameLenBits   int
	maxSubframes   int
	minSubframeLen int
	frameSizeBits  int

	r       bitReader
	out     *audio.Buffer
	subLens []int
	planes  [][]int32
	// view is the per-channel window MCLMS wants, rebuilt in place rather than
	// allocated per subframe.
	view [][]int32
	// err latches a bitstream failure. This format has no sync word, so a
	// walk that has drifted cannot re-anchor: every packet after the first
	// failure would decode noise, and the caller has to be told once rather
	// than handed it.
	err error
}

// channel is one channel's state: everything that survives across subframes.
type channel struct {
	g       golomb
	filters []cdlms
	// updateSpeed is per channel rather than per filter, and it is cleared by
	// neither a seekable tile nor a seek: its value is a function of whether
	// the last subframe was seekable and of nothing else. It starts at neither
	// of its two values so the first tile always takes the rescale branch,
	// harmlessly, over a window that tile has just zeroed.
	updateSpeed int32
	acPrev      []int32
}

// NewDecoder builds a decoder for one track. fm must be the format Config
// produces; the caller passes the container's so a disagreement is caught here
// rather than by a downstream stage.
func NewDecoder(cfg Config, fm audio.Format) (*Decoder, error) {
	// Validate before anything is derived from the config. A Config built by
	// hand rather than by ParseConfig reaches here unchecked, and every
	// geometry below is computed from it: without this a zero rate is not a
	// refusal but a panic inside the buffer pool.
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	want := cfg.Format()
	if fm != want {
		return nil, malformed("track format %+v does not match the codec config's %+v", fm, want)
	}
	if err := fm.Valid(); err != nil {
		return nil, err
	}
	d := &Decoder{
		cfg:            cfg,
		fmt:            fm,
		frameLen:       cfg.SamplesPerFrame(),
		frameLenBits:   cfg.FrameLenBits(),
		maxSubframes:   cfg.MaxSubframes(),
		minSubframeLen: cfg.MinSubframeLen(),
		frameSizeBits:  cfg.FrameSizeBits(),
		ch:             make([]channel, cfg.Channels),
		coded:          make([]bool, cfg.Channels),
		planes:         make([][]int32, cfg.Channels),
		view:           make([][]int32, cfg.Channels),
		quantStep:      1,
		seq:            -1,
	}
	d.out = audio.Get(fm, cfg.SamplesPerFrame())
	// ChanI is bounded by N, and Get leaves N zero, so the planes have to be
	// taken with N at the frame length or they come back empty and every write
	// below is a reslice past the length into the capacity: legal, and not
	// what anyone reading it would assume. emitFrame sets N to what the frame
	// actually produced just before handing the buffer over; the planes stay
	// full length because they are captured once here.
	d.out.N = d.frameLen
	for c := range d.ch {
		d.planes[c] = d.out.ChanI(c)
	}
	return d, nil
}

// Decode consumes one container packet and emits the frames that began in the
// previous one. It emits nothing for a packet that is entirely continuation,
// which is ordinary rather than an error.
func (d *Decoder) Decode(pkt []byte, emit func(*audio.Buffer) error) error {
	if d.err != nil {
		return d.err
	}
	// A refusal that reads nothing but the packet header does NOT latch. The
	// latch exists because a walk that has drifted through this format cannot
	// re-anchor, and neither of these has consumed a bit of any frame: the
	// decoder is exactly as able to decode the next packet as it was, and
	// killing it would also cost the frames still waiting in the carry, which
	// Drain would otherwise flush.
	if len(pkt) > d.cfg.BlockAlign {
		return malformed("packet of %d bytes, longer than nBlockAlign %d", len(pkt), d.cfg.BlockAlign)
	}
	// An emit failure is the caller's, not the stream's, so it does not latch
	// either. The carry goes with it: the walk stopped partway, and leaving it
	// in place would decode the same frames again on the next call.
	var emitErr error
	guarded := func(b *audio.Buffer) error {
		err := emit(b)
		if err != nil {
			emitErr = err
		}
		return err
	}
	if err := d.decode(pkt, guarded); err != nil {
		if emitErr != nil {
			d.carry.reset()
			d.carryOpen = false
			return emitErr
		}
		if errors.Is(err, errPacketHeader) {
			return err
		}
		d.err = err
		return err
	}
	return nil
}

// PacketIsSync reports whether a decoder can begin at this packet.
//
// The packet header carries a flag, the bit after the four-bit sequence
// number, that the reference decoder reads and discards. Measured across every
// packet of fifteen files, it is set exactly when a frame BEGINNING in that
// packet carries a seekable tile or a raw PCM tile, which are the only two
// places a decoder with no filter state can start (agreement 381 of 381).
//
// It is a hint and the decoder still confirms by finding the tile, so a
// container using this to pick a seek landing cannot be misled into producing
// wrong audio: the worst a wrong flag can do is cost a landing.
func PacketIsSync(pkt []byte) bool {
	if len(pkt) == 0 {
		return false
	}
	return pkt[0]>>3&1 == 1
}

// errPacketHeader marks a refusal taken from the packet header alone, before
// any frame bit is read. Wrapped so the code and text stay the caller's.
var errPacketHeader = errors.New("wmalossless: packet header")

func headerRefusal(err error) error {
	return fmt.Errorf("%w%w", errPacketHeader, err)
}

func (d *Decoder) decode(pkt []byte, emit func(*audio.Buffer) error) error {
	var hdr bitReader
	hdr.resetBits(pkt, len(pkt)*8)
	seq := int(hdr.bits(4))
	hdr.bit() // seekable frame in packet: an index hint, confirmed by finding the tile
	spliced := hdr.bit() == 1
	n := int(hdr.bits(d.frameSizeBits))
	if hdr.failed() {
		return hdr.errored()
	}
	if spliced {
		// What the bit changes is described nowhere, and a decoder that
		// continues is guessing that it changes nothing. Read from the header
		// alone, so it does not latch: the next packet may be ordinary.
		return headerRefusal(unsupported("a spliced packet"))
	}

	// A sequence jump means packets were lost. That is a transport fact, not
	// damage: drop the carry and wait for a seekable tile rather than
	// predicting from state that belongs to samples never seen.
	if d.seq >= 0 && seq != (d.seq+1)&15 {
		d.carry.reset()
		d.carryOpen = false
		d.started = false
	}
	d.seq = seq

	off := 6 + d.frameSizeBits
	payload := len(pkt)*8 - off
	if payload <= 0 {
		return malformed("nBlockAlign %d leaves no payload after a %d-bit header", len(pkt), off)
	}
	// The "whole packet is continuation" case is expressed by overshooting:
	// the encoder writes the packet size in bits, which is wider than the
	// payload by the header. Clamp rather than refuse.
	if n > payload {
		n = payload
	}

	if d.carryOpen {
		if d.carry.bits+n > maxCarryBytes*8 {
			return malformed("a frame spanning more than %d bytes of packets", maxCarryBytes)
		}
		d.carry.appendFrom(pkt, off, n)
	}
	if n >= payload {
		return nil
	}
	if d.carryOpen {
		d.r.resetBits(d.carry.buf, d.carry.bits)
		err := d.frames(&d.r, emit)
		switch {
		case err == nil || errors.Is(err, errNotStarted):
			// A landing before the first seekable tile loses the rest of THAT
			// carry and nothing else. The frames beginning in this packet are
			// still carried forward below, because one of them may be the tile
			// that lets the decode start; dropping them too would cost a
			// packet on every seek for no reason.
		case n == 0:
			// A zero continuation count is the one place the stream itself says
			// the carry may not be ours: benign when the previous packet's
			// frames ended on its boundary, and a lost run of packets when not.
			// The carry is decoded either way, and a failure here is the
			// discontinuity rather than drift. The notes disagree with
			// themselves about this; their 2026-09-10 addendum has the why.
			d.carry.reset()
			d.carryOpen = false
			d.started = false
		default:
			return err
		}
	}
	d.carry.reset()
	d.carry.appendFrom(pkt, off+n, payload-n)
	d.carryOpen = true
	return nil
}

// Drain decodes the frames that began in the last packet. Without it they are
// lost, which is a fifth of a second of real audio at the end of every file.
func (d *Decoder) Drain(emit func(*audio.Buffer) error) error {
	if d.err != nil {
		return d.err
	}
	if !d.carryOpen {
		return nil
	}
	// The carry is closed before the walk, not after, so an error leaves
	// nothing in flight. buf survives the reset: reset only reslices, so the
	// reader still sees the bytes it was handed.
	buf, bits := d.carry.buf, d.carry.bits
	d.carry.reset()
	d.carryOpen = false
	d.r.resetBits(buf, bits)
	// The same guard Decode uses: a callback failure is the caller's and does
	// not latch, so a caller that aborts one drain can still use the decoder.
	var emitErr error
	guarded := func(b *audio.Buffer) error {
		err := emit(b)
		if err != nil {
			emitErr = err
		}
		return err
	}
	if err := d.frames(&d.r, guarded); err != nil {
		if emitErr != nil {
			return emitErr
		}
		if errors.Is(err, errNotStarted) {
			return nil
		}
		d.err = err
		return err
	}
	return nil
}

// Reset discards everything after a seek. The decoder then waits for a
// seekable tile, which is the only place filter state is restated.
func (d *Decoder) Reset() {
	d.carry.reset()
	d.carryOpen = false
	d.seq = -1
	d.started = false
	d.err = nil
	d.r.resetBits(nil, 0)
}

// Release returns the pooled output buffer (format.Media calls it on Close).
func (d *Decoder) Release() {
	audio.Put(d.out)
	d.out = nil
	d.planes = nil
	d.view = nil
}

var _ codec.Decoder = (*Decoder)(nil)
var _ codec.Releaser = (*Decoder)(nil)

// frames decodes every frame in one carry. The trailer bit is 1 while more
// frames begin in the same packet and 0 on the last of them, and on a
// well-formed stream that bit falls exactly at the end of the carry.
func (d *Decoder) frames(r *bitReader, emit func(*audio.Buffer) error) error {
	for n := 0; r.left() > 0; n++ {
		if n >= maxFramesPerPacket {
			return malformed("more than %d frames in one packet", maxFramesPerPacket)
		}
		more, err := d.frame(r, emit)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
	// The trailer bit of the last frame that begins in a packet reads 0, and
	// on a well-formed stream it falls exactly at the end of the carry. Running
	// out with one still claiming a frame follows means the carry is not what
	// the stream said it was.
	return malformed("a packet whose last frame claims another follows it")
}

// frame decodes one frame and reports whether another begins in the same
// carry.
func (d *Decoder) frame(r *bitReader, emit func(*audio.Buffer) error) (bool, error) {
	prefixed := d.cfg.DecodeFlags&flagLengthPrefix != 0
	start := r.pos
	frameLen := 0
	if prefixed {
		frameLen = int(r.bits(d.frameSizeBits))
		if frameLen < 2 || start+frameLen > r.bitLen() {
			return false, malformed("frame length prefix of %d bits", frameLen)
		}
	}
	if err := d.tiling(r); err != nil {
		return false, err
	}
	if d.cfg.DecodeFlags&flagDRC != 0 {
		r.skip(8) // the dynamic-range gain, read and discarded
	}
	startSkip, endSkip := 0, 0
	if r.bit() == 1 {
		if r.bit() == 1 {
			startSkip = int(r.bits(d.frameLenBits + 1))
		}
		if r.bit() == 1 {
			endSkip = int(r.bits(d.frameLenBits + 1))
		}
	}
	if startSkip+endSkip > d.frameLen {
		return false, malformed("skips of %d and %d in a %d-sample frame",
			startSkip, endSkip, d.frameLen)
	}

	at := 0
	for _, n := range d.subLens {
		if err := d.subframe(r, at, n); err != nil {
			return false, err
		}
		at += n
	}
	if r.failed() {
		return false, r.errored()
	}

	if prefixed {
		// Exactly one uninterpreted bit sits between the last subframe and
		// the trailer. Positioning from the prefix is the robust spelling and
		// agrees with skipping one on everything conforming.
		r.pos = start + frameLen - 1
	}
	more := r.bit() == 1
	if r.failed() {
		return false, r.errored()
	}
	if err := d.emitFrame(startSkip, endSkip, emit); err != nil {
		return false, err
	}
	return more, nil
}

// emitFrame hands the frame's samples over. The end skip is what makes the
// last frame of a stream end on the source's last sample; the start skip is
// the same field at the head, and no measured file carries one.
func (d *Decoder) emitFrame(startSkip, endSkip int, emit func(*audio.Buffer) error) error {
	n := d.frameLen - startSkip - endSkip
	if n <= 0 {
		return nil
	}
	if startSkip > 0 {
		for _, p := range d.planes {
			copy(p[:n], p[startSkip:startSkip+n])
		}
	}
	d.out.N = n
	return emit(d.out)
}

// tiling reads the tile header into d.subLens.
//
// The split may in principle differ between channels. It never does in
// anything an encoder writes, and the only reference decoder reads a
// non-aligned frame off its own layout, so a frame whose channels disagree is
// refused by name rather than guessed at.
func (d *Decoder) tiling(r *bitReader) error {
	// The bit is read unconditionally: it is in the tile header whatever the
	// subframe depth, and only its MEANING is subsumed when there can be only
	// one subframe. Folding the read into the `||` short-circuits it at depth
	// 0 and puts every field after the tile header one bit out of phase, on
	// every frame of such a stream.
	aligned := r.bit() == 1
	fixed := d.maxSubframes == 1 || aligned
	last := d.frameLen - d.minSubframeLen
	ratioBits := floorLog2(d.maxSubframes-1) + 1

	d.subLens = d.subLens[:0]
	counts := make([]int, d.cfg.Channels)
	for {
		minLen, atMin := d.frameLen, 0
		for _, v := range counts {
			if v < minLen {
				minLen, atMin = v, 1
			} else if v == minLen {
				atMin++
			}
		}
		if minLen == d.frameLen {
			break
		}
		takes := 0
		for _, v := range counts {
			if v != minLen {
				continue
			}
			if fixed || atMin == 1 || minLen == last || r.bit() == 1 {
				takes++
			}
		}
		if r.failed() {
			return r.errored()
		}
		if takes == 0 {
			return malformed("a subframe no channel takes")
		}
		if takes != d.cfg.Channels {
			return unsupported("a frame whose channels are tiled differently (%d of %d channels take a subframe)",
				takes, d.cfg.Channels)
		}
		length := d.minSubframeLen
		if minLen != last {
			length = d.minSubframeLen * (int(r.bits(ratioBits)) + 1)
		}
		if length < d.minSubframeLen || length > d.frameLen {
			return malformed("subframe of %d samples, want %d..%d",
				length, d.minSubframeLen, d.frameLen)
		}
		if minLen+length > d.frameLen {
			return malformed("a subframe of %d samples overruns a %d-sample frame",
				length, d.frameLen)
		}
		if len(d.subLens) >= maxSubframesPerChannel {
			return malformed("more than %d subframes on one channel", maxSubframesPerChannel)
		}
		d.subLens = append(d.subLens, length)
		for i := range counts {
			counts[i] += length
		}
	}
	return nil
}

// subframe decodes one subframe of every channel at [off, off+n).
func (d *Decoder) subframe(r *bitReader, off, n int) error {
	seekable := r.bit() == 1
	if seekable {
		if err := d.filterDefs(r); err != nil {
			return err
		}
		d.started = true
	}
	rawPCM := r.bit() == 1
	if !rawPCM {
		for c := range d.coded {
			d.coded[c] = r.bit() == 1
		}
		if d.cfg.DecodeFlags&flagLPC != 0 && r.bit() == 1 {
			// The reference reads the definition to stay in sync and never
			// applies the filter, so nothing anywhere describes it and no
			// decoder can be checked against another on such a stream.
			return unsupported("the LPC filter, which no available description covers")
		}
	}
	padding := 0
	if r.bit() == 1 {
		padding = int(r.bits(5))
	}
	if r.failed() {
		return r.errored()
	}

	if rawPCM {
		w := d.cfg.BitsPerSample - padding
		if w < 1 {
			return malformed("%d padding zeroes in a %d-bit raw PCM tile", padding, d.cfg.BitsPerSample)
		}
		// Channel-major, and every channel of the stream is present whatever
		// the coded flags say, because they are not read on this path.
		for c := range d.planes {
			x := d.planes[c][off : off+n]
			for i := range x {
				x[i] = r.signed(w)
			}
		}
		if r.failed() {
			return r.errored()
		}
		// No prediction and no filter update: the histories are left alone.
		d.shiftOut(off, n, padding, false)
		return nil
	}

	if padding > d.cfg.BitsPerSample {
		return malformed("%d padding zeroes in a %d-bit stream", padding, d.cfg.BitsPerSample)
	}
	if !d.started {
		return errNotStarted
	}

	clipLo := int32(-1) << (d.cfg.BitsPerSample - 1)
	clipHi := -(clipLo + 1)
	extra := 0
	if d.decorr {
		extra = 1
	}
	for c := range d.planes {
		x := d.planes[c][off : off+n]
		if !d.coded[c] {
			clear(x)
			continue
		}
		// The transient flag and its position are read and never used. The
		// bits must still be consumed or everything after them desynchronises.
		if r.bit() == 1 {
			r.skip(floorLog2(n))
		}
		first := 0
		if seekable {
			d.ch[c].g.seed(r.bits(d.cfg.BitsPerSample), d.movave)
			// The seeds use the full depth, not the depth less the padding,
			// and the decorrelated value needs one more bit of range than the
			// sample does.
			x[0] = r.signed(d.cfg.BitsPerSample + extra)
			first = 1
		}
		for i := first; i < n; i++ {
			v, err := d.ch[c].g.next(r)
			if err != nil {
				return err
			}
			x[i] = v
		}
		if r.failed() {
			return r.errored()
		}
		d.cascade(c, x, seekable, clipLo, clipHi)
	}

	if d.mcOn {
		d.mc.run(d.planeSlices(off, n), d.coded, n, clipLo, clipHi)
	}
	// The lifting runs only on a two-channel stream, and only when at least
	// one channel is coded. With one coded it copies the coded channel into
	// the other, which is what mono-in-stereo material decodes to.
	if d.decorr && d.cfg.Channels == 2 && (d.coded[0] || d.coded[1]) {
		a := d.planes[0][off : off+n]
		b := d.planes[1][off : off+n]
		for i := range a {
			a[i] -= b[i] >> 1
			b[i] += a[i]
		}
	}
	if d.acOn {
		for c := range d.planes {
			d.ac.run(d.planes[c][off:off+n], d.ch[c].acPrev)
		}
	}
	d.shiftOut(off, n, padding, true)
	return nil
}

// cascade runs one channel's CDLMS filters, highest index first, each over the
// whole subframe before the next begins: each consumes what the one after it
// produced.
func (d *Decoder) cascade(c int, x []int32, seekable bool, clipLo, clipHi int32) {
	ch := &d.ch[c]
	want := int32(8)
	if seekable {
		want = 16
	}
	if ch.updateSpeed != want {
		live := d.cfg.DecodeFlags&flagLPC != 0
		for f := range ch.filters {
			ch.filters[f].rescaleUpdates(want > ch.updateSpeed, live)
		}
		ch.updateSpeed = want
	}
	for f := len(ch.filters) - 1; f >= 0; f-- {
		ch.filters[f].run(x, ch.updateSpeed, clipLo, clipHi)
	}
}

// shiftOut applies the quantisation step and the padding shift, which is the
// whole output stage: no dither, no clipping, no gain, no depth conversion.
func (d *Decoder) shiftOut(off, n, padding int, quantise bool) {
	sh := uint(padding)
	q := d.quantStep
	for _, p := range d.planes {
		x := p[off : off+n]
		if quantise && q != 1 {
			for i := range x {
				x[i] *= q
			}
		}
		if d.cfg.BitsPerSample == 16 {
			// The reference truncates to 16 bits on both sides of the shift.
			// Nothing conforming overflows, so this is invisible on a real
			// stream and keeps a crafted one inside the depth the buffer
			// declares.
			for i, v := range x {
				x[i] = int32(int16(int16(v) << sh))
			}
			continue
		}
		for i, v := range x {
			x[i] = int32(v<<sh) << 8 >> 8
		}
	}
}

// planeSlices is the per-channel view MCLMS wants, over the decoder's own
// slice rather than a fresh one: it would otherwise allocate on every subframe
// of every frame.
func (d *Decoder) planeSlices(off, n int) [][]int32 {
	for c, p := range d.planes {
		d.view[c] = p[off : off+n]
	}
	return d.view
}

// filterDefs reads a seekable tile's definitions. It first clears every filter
// it is about to use: a seekable tile is the only place state is restated, and
// it is the only place a decoder with no history can begin.
func (d *Decoder) filterDefs(r *bitReader) error {
	if r.bit() == 1 {
		// The reference refuses this outright, so no description of the coder
		// exists anywhere and the bits after the flag mean something else.
		return unsupported("arithmetic coding")
	}
	d.acOn = r.bit() == 1
	d.decorr = r.bit() == 1
	d.mcOn = r.bit() == 1
	if d.acOn {
		if err := d.acDef(r); err != nil {
			return err
		}
	} else {
		d.ac.order = 0
	}
	for c := range d.ch {
		d.ch[c].acPrev = grow(d.ch[c].acPrev, d.ac.order)
	}
	if d.mcOn {
		if err := d.mclmsDef(r); err != nil {
			return err
		}
	}
	if err := d.cdlmsDef(r); err != nil {
		return err
	}
	d.movave = uint(r.bits(3))
	d.quantStep = int32(r.bits(8)) + 1
	if r.failed() {
		return r.errored()
	}
	// The moving-average accumulator is filter state and a seekable tile
	// clears it, along with the scaling that reads it. Only a CODED channel is
	// re-seeded below, so without this a channel that is silent across one
	// seekable tile and coded again later would set its Golomb parameter from
	// an accumulator belonging to samples before the tile, read the wrong
	// number of remainder bits, and desynchronise everything after it.
	for c := range d.ch {
		d.ch[c].g = golomb{scaling: d.movave}
	}
	return nil
}

func (d *Decoder) acDef(r *bitReader) error {
	d.ac.order = int(r.bits(4)) + 1
	d.ac.scaling = int(r.bits(4))
	d.ac.coeff = grow(d.ac.coeff, d.ac.order)
	for i := range d.ac.coeff {
		// Unsigned, at least 1, never 0. The reference holds them in signed
		// 16-bit slots, so the widest field at the highest scaling wraps
		// negative; no file reaches that corner and matching it costs one
		// conversion.
		d.ac.coeff[i] = int32(int16(uint16(r.bits(d.ac.scaling) + 1)))
	}
	return nil
}

func (d *Decoder) mclmsDef(r *bitReader) error {
	order := (int(r.bits(4)) + 1) * 2
	scaling := int(r.bits(4))
	d.mc.resize(order, scaling, d.cfg.Channels)
	if r.bit() != 1 {
		return nil
	}
	w := int(r.bits(ceilLog2(scaling+1))) + 2
	for i := range d.mc.coeff {
		d.mc.coeff[i] = int32(int16(uint16(r.bits(w))))
	}
	// The strictly lower triangle, row by row: for channel c from 1 upward, c
	// values for channels 0 through c-1.
	for c := 1; c < d.cfg.Channels; c++ {
		for j := 0; j < c; j++ {
			d.mc.curCoeff[c*d.cfg.Channels+j] = int32(int16(uint16(r.bits(w))))
		}
	}
	return nil
}

func (d *Decoder) cdlmsDef(r *bitReader) error {
	if r.bit() == 1 {
		// Two readings of the reference's rescale disagree everywhere except
		// at one value of the scaling field, no encoder exercises the path,
		// and in a lossless codec a wrong initial coefficient is not a small
		// error.
		return unsupported("transmitted CDLMS coefficients, whose scaling is undetermined")
	}
	for c := range d.ch {
		count := int(r.bits(3)) + 1
		var orders, scalings [maxCDLMSFilters]int
		for f := 0; f < count; f++ {
			orders[f] = (int(r.bits(7)) + 1) * 8
		}
		for f := 0; f < count; f++ {
			scalings[f] = int(r.bits(4))
		}
		if r.failed() {
			return r.errored()
		}
		if cap(d.ch[c].filters) < count {
			d.ch[c].filters = make([]cdlms, count)
		}
		d.ch[c].filters = d.ch[c].filters[:count]
		for f := 0; f < count; f++ {
			if orders[f] > maxCDLMSOrder {
				return malformed("CDLMS order %d, want at most %d", orders[f], maxCDLMSOrder)
			}
			d.ch[c].filters[f].resize(orders[f], scalings[f])
		}
	}
	return nil
}
