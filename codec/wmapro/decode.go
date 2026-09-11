//go:build !wmaprotablesgen

package wmapro

import "github.com/colespringer/waxflow/audio"

// The packet and frame walk.
//
// Frames do not align with packets. A packet is exactly nBlockAlign bytes and
// holds a run of whole frames plus, usually, the head of one more; the head is
// carried into the next packet as a BIT string, because a carry stops mid-byte
// and a byte-aligned carry would resume the frame up to seven bits out.
//
// Unlike codec/wmalossless there is no output latency: every frame here opens
// with its own length, so a frame's extent is known before it is decoded and
// the frames of packet k are emitted on the call that delivers packet k. Drain
// therefore has nothing to flush.
//
// The one lead-in is a frame, not a packet: the first frame after any decode
// start overlap-adds against a transform tail the decoder does not have, so it
// is dropped. The second frame overlaps against the first frame's own tail and
// is exact. That is measured rather than assumed, at 65 resume points across
// thirteen files, where every frame after the first matched a linear decode
// with a maximum difference of zero.

// Decoder decodes one WMA Pro track.
type Decoder struct {
	cfg Config
	fmt audio.Format
	lay *layout
	bk  *bookSet

	frameLen       int
	frameLenBits   int
	maxSubframes   int
	minSubframeLen int
	frameSizeBits  int
	sizes          int

	r     bitReader   // over the current packet
	cr    bitReader   // over the carry
	carry bitAppender // the head of a frame that crosses into the next packet

	// seq is the previous packet's sequence number, or -1 when no packet has
	// been seen since the last Reset. dropNext marks the frame of lead-in that
	// every decode start owes.
	seq      int
	dropNext bool

	ch      []channel
	scratch *imdctScratch
	// spec is one subframe's dequantised coefficients, reused across channels.
	spec []float32
	// Per-subframe scratch, allocated once so the hot path never allocates.
	// groups is kept at full length with nGroups the live count, so that each
	// group's own slices survive being reused.
	takes     []bool
	part      []int
	ungrouped []int
	rest      []int
	angles    []int
	vec       []float32
	groups    []group
	nGroups   int
	// explicitLimit is the subframe's transmitted-vector-limit flag, which
	// also disables the coefficient phase's zero-run switch.
	explicitLimit bool

	out    *audio.Buffer
	planes [][]float32

	// plans holds the transform plan at every subframe size, indexed by size
	// index, so the hot path never touches the shared plan cache's lock.
	plans []*imdctPlan

	paths pathCounts
}

// pathCounts counts the bitstream paths a decode takes. It is for the tests:
// the corpus note's coverage matrix was measured with an instrumented decoder,
// and pinning the same counts here says this walk takes the same branches the
// same number of times, which no sample comparison can say. Every field is one
// increment on a path the walk is already on.
type pathCounts struct {
	Frames, Subframes int
	// Channel transforms (note sections 9.2 to 9.5).
	StereoMatrix, ScaledMatrix, NoTransform, ExplicitMatrix, BuiltinMatrix int
	PerBandEnables                                                         int
	// Scale factors (section 10): the two transmitted forms, the 14-bit
	// escape, a resample with no transmission, and a channel that transmits
	// coefficients but declines scale factors before it has any to resample.
	DPCM, RunLevelDiffs, ScaleEscape, ResampleOnly, ZeroScale int
	// Coefficients (section 11).
	Vec4Escape, Vec2Escape, LargeValue, TailReached, VectorCovers, TailEscape, EndOfBlock int
	// Quantisation (section 12).
	StepEscape, Modifiers, ExplicitLimit int
	// The frame and packet layers (sections 4 and 14).
	StartTrim, EndTrim, ContZero, ContSaturates int
}

// channel is one channel's state. The fields divide into three lifetimes and
// the divisions are load-bearing: the rolling buffer and the previous subframe
// length are the ONLY things that cross a frame boundary, which is what makes
// one frame of lead-in enough after a seek.
type channel struct {
	// Across frames.
	buf     []float32 // rolling output, 1.5 frames long
	prevLen int

	// Within one frame.
	subLens   []int // this frame's subframe lengths, in order
	placed    int   // the tiling walk's running sum
	decoded   int   // samples decoded so far this frame
	subIdx    int
	saved     []int // the last scale factors this channel transmitted
	savedSize int
	reuse     bool
	sfRes     int // the scale factor resolution, set on the first transmission

	// Within one subframe.
	scale     []int
	maxScale  int
	quantStep int
	vecLimit  int
	transmits bool
	// active is transmits widened by the channel transform: a channel that
	// coded nothing still has coefficients once a group it shares carries one.
	active bool
	coefs  []float32
}

// NewDecoder builds a decoder for one track. fm must be the format Config
// produces; the caller passes the container's so a disagreement is caught here
// rather than by a downstream stage.
func NewDecoder(cfg Config, fm audio.Format) (*Decoder, error) {
	// Validate before anything is derived from the config. A Config built by
	// hand rather than by ParseConfig reaches here unchecked, and every
	// geometry below is computed from it.
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
	lay, err := layoutFor(cfg)
	if err != nil {
		return nil, err
	}
	frameLen := cfg.SamplesPerFrame()
	d := &Decoder{
		cfg:            cfg,
		fmt:            fm,
		lay:            lay,
		bk:             books(),
		frameLen:       frameLen,
		frameLenBits:   cfg.FrameLenBits(),
		maxSubframes:   cfg.MaxSubframes(),
		minSubframeLen: cfg.MinSubframeLen(),
		frameSizeBits:  cfg.FrameSizeBits(),
		sizes:          cfg.sizeCount(),
		ch:             make([]channel, cfg.Channels),
		scratch:        newIMDCTScratch(frameLen),
		spec:           make([]float32, frameLen),
		takes:          make([]bool, cfg.Channels),
		part:           make([]int, 0, cfg.Channels),
		ungrouped:      make([]int, 0, cfg.Channels),
		rest:           make([]int, 0, cfg.Channels),
		angles:         make([]int, 0, cfg.Channels*cfg.Channels),
		vec:            make([]float32, cfg.Channels),
		groups:         make([]group, cfg.Channels),
		seq:            -1,
		dropNext:       true,
	}
	maxBands := 0
	d.plans = make([]*imdctPlan, d.sizes)
	for k := range d.sizes {
		maxBands = max(maxBands, lay.numBands(k))
		d.plans[k] = planFor(frameLen >> k)
	}
	for i := range d.groups {
		g := &d.groups[i]
		g.chans = make([]int, 0, cfg.Channels)
		g.matrix = make([]float32, 0, cfg.Channels*cfg.Channels)
		g.bands = make([]bool, 0, maxBands)
	}
	for c := range d.ch {
		ch := &d.ch[c]
		ch.buf = make([]float32, frameLen+frameLen/2)
		ch.prevLen = frameLen
		ch.subLens = make([]int, 0, maxSubframesPerChannel)
		ch.saved = make([]int, maxBands)
		ch.scale = make([]int, maxBands)
		ch.coefs = make([]float32, frameLen)
	}
	d.out = audio.Get(fm, frameLen)
	// ChanF is bounded by N, and Get leaves N zero, so the planes have to be
	// taken with N at the frame length or they come back empty and every write
	// is a reslice past the length into the capacity: legal, and not what
	// anyone reading it would assume. emitFrame sets N to what the frame
	// actually kept just before handing the buffer over.
	d.out.N = frameLen
	d.planes = make([][]float32, cfg.Channels)
	for c := range d.ch {
		d.planes[c] = d.out.ChanF(c)
	}
	return d, nil
}

// Decode consumes one container packet and emits the frames it completes.
//
// A bitstream failure does NOT latch. Every packet of this format is a decode
// start point (measured: a decode resumed at any packet is identical to a
// linear one from its second frame on), so the recovery from a damaged packet
// is the same as the recovery from a sequence break -- drop the carry, resync
// at the next packet's first frame, and owe one frame of lead-in -- and
// latching would turn a recoverable stream into an unplayable one for no gain.
// That is the opposite of codec/wma and codec/wmalossless, where a drifted
// walk cannot re-anchor, and it is a property of THIS codec's frame layer
// rather than a difference of taste.
//
// An emit failure is the caller's, not the stream's, and propagates as it is.
// It still costs the frames of the packet not yet delivered when the callback
// failed, so a caller that carries on owes the same lead-in a damaged packet
// costs; the decoder does not latch.
func (d *Decoder) Decode(pkt []byte, emit func(*audio.Buffer) error) error {
	// A container shape rather than a stream failure: nothing has been read
	// from it, so nothing has drifted and no lead-in is owed.
	if len(pkt) > d.cfg.BlockAlign {
		return malformed("packet of %d bytes, longer than nBlockAlign %d", len(pkt), d.cfg.BlockAlign)
	}
	var emitErr error
	guarded := func(b *audio.Buffer) error {
		err := emit(b)
		if err != nil {
			emitErr = err
		}
		return err
	}
	err := d.decode(pkt, guarded)
	if err == nil {
		return nil
	}
	// The carry goes with any failure: the walk stopped partway through the
	// packet, and leaving it in place would decode the same frames again. So
	// does one frame of lead-in, since the frames after the stop were lost.
	d.carry.reset()
	d.dropNext = true
	if emitErr != nil {
		return emitErr
	}
	return err
}

func (d *Decoder) decode(pkt []byte, emit func(*audio.Buffer) error) error {
	if len(pkt) == 0 {
		return nil
	}
	d.r.reset(pkt)
	seq := int(d.r.bits(4))
	// Two bits that are NOT reserved-zero: measured, they read 2 on almost
	// every packet and 0 on at least one, so validating them rejects real
	// files.
	d.r.skip(2)
	cont := int(d.r.bits(d.frameSizeBits))
	if d.r.err != nil {
		return d.r.err
	}

	gap := d.seq >= 0 && seq != (d.seq+1)&15
	d.seq = seq
	payload := d.r.left()

	switch {
	case gap:
		// Packets were lost or the stream was joined mid-file. The recovery is
		// the seek path: drop the carry, skip the continuation bits unread,
		// and owe a frame of lead-in.
		d.carry.reset()
		d.dropNext = true
		d.r.skip(min(cont, payload))
	case cont == 0:
		// Not theoretical, and a decoder that models the frame layer as a
		// concatenation of payloads desynchronises here: measured, a zero
		// count arrives with a non-empty carry on ten of thirteen files and
		// discards as much as 23583 bits of padded, abandoned packet tail.
		if d.carry.bits > 0 {
			d.paths.ContZero++
		}
		d.carry.reset()
	case d.carry.bits == 0:
		// The continued frame's head was never seen: this is a decode start,
		// a resume after a seek, or the packet before was refused. The bits
		// are the tail of a frame nobody began, and reading them as a frame
		// of their own is what a resumed decode used to do. That frame is
		// lost, so the next one overlap-adds against a tail that is not its
		// neighbour and is lead-in.
		d.r.skip(min(cont, payload))
		d.dropNext = true
	default:
		if cont > payload {
			d.paths.ContSaturates++
		}
		n := min(cont, payload)
		if (d.carry.bits+n+7)/8 > maxCarryBytes {
			return malformed("a frame spanning more than %d bytes of carry", maxCarryBytes)
		}
		d.carry.appendFrom(pkt, d.r.pos, n)
		d.r.skip(n)
		done, err := d.carriedFrame(emit)
		if err != nil {
			return err
		}
		if !done {
			if cont > payload {
				// The frame continues into the next packet. This is the
				// saturating case, where the count claims the whole payload;
				// it is measured once, at end of stream.
				return nil
			}
			// The count says the frame ends here and the frame's own length
			// says it does not: an inconsistent stream, treated as the
			// discontinuity it is rather than abandoning the frames that do
			// begin in this packet.
			d.carry.reset()
			d.dropNext = true
		}
	}
	if err := d.framesIn(emit); err != nil {
		return err
	}
	if len(pkt) < d.cfg.BlockAlign && d.carry.bits > 0 {
		// A packet shorter than nBlockAlign is a container shape, not damage,
		// but its tail is not the head of the frame the next packet
		// continues: the bits between here and the boundary are missing, so
		// joining the two would make a frame out of bits that do not follow
		// each other.
		d.carry.reset()
		d.dropNext = true
	}
	return nil
}

// carriedFrame decodes the frame the carry holds, and reports whether it was
// complete. An incomplete carry is kept rather than guessed at: the reference
// decodes it anyway and leans on its reader returning zeros past the end,
// which invents audio.
func (d *Decoder) carriedFrame(emit func(*audio.Buffer) error) (bool, error) {
	if d.carry.bits <= d.frameSizeBits {
		return false, nil
	}
	d.cr.resetBits(d.carry.buf, d.carry.bits)
	n := int(d.cr.bits(d.frameSizeBits))
	if n <= d.frameSizeBits {
		return false, malformed("a carried frame of %d bits cannot hold its own length prefix", n)
	}
	if n > d.carry.bits {
		return false, nil
	}
	if _, err := d.frame(&d.cr, 0, n, emit); err != nil {
		return false, err
	}
	d.carry.reset()
	return true, nil
}

// framesIn decodes the whole frames this packet holds and carries the rest.
//
// All three stop conditions are load-bearing and none alone is enough: the fit
// test fires 65 times across the corpus, so a frame whose declared length
// overruns the packet is the ordinary way a packet ends rather than an error.
func (d *Decoder) framesIn(emit func(*audio.Buffer) error) error {
	for i := 0; ; i++ {
		if i >= maxFramesPerPacket {
			return malformed("more than %d frames in one packet", maxFramesPerPacket)
		}
		if d.r.left() <= d.frameSizeBits {
			break
		}
		at := d.r.pos
		n := int(d.r.bits(d.frameSizeBits))
		if n == 0 || n > d.r.bitLen()-at {
			d.r.seek(at)
			break
		}
		if n <= d.frameSizeBits {
			return malformed("a frame of %d bits cannot hold its own length prefix", n)
		}
		more, err := d.frame(&d.r, at, n, emit)
		if err != nil {
			return err
		}
		d.r.seek(at + n)
		if !more {
			break
		}
	}
	d.carry.reset()
	if rest := d.r.bitLen() - d.r.pos; rest > 0 {
		d.carry.appendFrom(d.r.buf, d.r.pos, rest)
	}
	return nil
}

// frame decodes one frame spanning bits [at, at+n) of r, whose length prefix
// the caller has already consumed, and reports whether another frame follows
// in the same packet.
func (d *Decoder) frame(r *bitReader, at, n int, emit func(*audio.Buffer) error) (bool, error) {
	d.paths.Frames++
	if err := d.tiling(r); err != nil {
		return false, err
	}
	if d.cfg.Channels > 1 {
		if r.bit() != 0 {
			// A post-processing transform. Measured: clear on every frame of
			// every file, so the matrix this skips is entirely untested and
			// carried as a deficit rather than refused.
			if r.bit() != 0 {
				r.skip(4 * d.cfg.Channels * d.cfg.Channels)
			}
		}
	}
	if d.cfg.hasDRC() {
		// A dynamic range gain, read to stay in sync and discarded. Measured 0
		// on every frame that carries one, and there is no gain curve here to
		// apply it with.
		r.skip(8)
	}
	startTrim, endTrim := 0, 0
	if r.bit() != 0 {
		w := d.frameLenBits + 1
		if r.bit() != 0 {
			startTrim = int(r.bits(w))
		}
		if r.bit() != 0 {
			endTrim = int(r.bits(w))
		}
		// Counted by value: the first frame of every file carries both fields
		// with the end trim at zero.
		if startTrim > 0 {
			d.paths.StartTrim++
		}
		if endTrim > 0 {
			d.paths.EndTrim++
		}
	}
	if err := d.subframes(r); err != nil {
		return false, err
	}
	// The trailer bit sits at the last bit the length prefix accounts for.
	// Seeking there rather than assuming one padding bit is what makes a frame
	// with a wider gap decode instead of being refused; the reference treats
	// any other gap as fatal and its own comment doubts that. A walk that
	// went PAST it read bits belonging to the next frame, and seeking back
	// would quietly accept a frame that contradicts its own length.
	if r.pos > at+n-1 {
		return false, malformed("the frame reads %d bits past its declared length of %d", r.pos-(at+n-1), n)
	}
	r.seek(at + n - 1)
	more := r.bit() != 0
	if r.err != nil {
		return false, r.err
	}
	return more, d.emitFrame(startTrim, endTrim, emit)
}

// emitFrame hands the frame's samples over and rolls the output buffers.
//
// The trims and the lead-in drop say the same thing at the start of a file,
// where the first frame carries a start trim of exactly one frame, and they do
// not double-count: a dropped frame's trims are never applied because the
// frame is never output.
func (d *Decoder) emitFrame(startTrim, endTrim int, emit func(*audio.Buffer) error) error {
	defer d.roll()
	if d.dropNext {
		d.dropNext = false
		return nil
	}
	lo, hi := max(startTrim, 0), d.frameLen-endTrim
	hi = min(hi, d.frameLen)
	if lo >= hi {
		return nil
	}
	d.out.N = hi - lo
	for c := range d.ch {
		copy(d.planes[c][:hi-lo], d.ch[c].buf[lo:hi])
	}
	return emit(d.out)
}

// roll moves each channel's second half down to the first, which is where the
// next frame's subframes overlap into. Nothing needs zeroing: the next frame's
// subframes cover every position from half a frame upward.
func (d *Decoder) roll() {
	half := d.frameLen / 2
	for c := range d.ch {
		copy(d.ch[c].buf[:half], d.ch[c].buf[d.frameLen:])
	}
}

// Drain has nothing to flush. Every frame is emitted on the call that
// completes it, and the half-buffer a frame leaves behind is lead-out for a
// frame that never arrives: the reference emits it for the Xbox variant and
// not for this codec, and emitting it here would add half a frame of alias to
// the end of every file.
func (d *Decoder) Drain(func(*audio.Buffer) error) error { return nil }

// Reset discards everything after a seek. The decoder then owes one frame of
// lead-in, which is all the state that crosses a frame boundary.
func (d *Decoder) Reset() {
	d.carry.reset()
	d.seq = -1
	d.dropNext = true
	for c := range d.ch {
		ch := &d.ch[c]
		clear(ch.buf)
		ch.prevLen = d.frameLen
		ch.reuse = false
		ch.decoded, ch.subIdx, ch.placed = 0, 0, 0
		ch.subLens = ch.subLens[:0]
	}
}

// Release returns the pooled output buffer (format.Media calls it on Close).
func (d *Decoder) Release() {
	if d.out != nil {
		audio.Put(d.out)
		d.out, d.planes = nil, nil
	}
}
