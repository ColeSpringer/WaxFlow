//go:build !wmatablesgen

package wma

import (
	"math"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

var _ codec.Releaser = (*Decoder)(nil)

// expTableLo and expTableHi bound the VLC exponent index. The curve
// 10^(index/16) is tabulated over exactly this range in every derived decoder,
// and the table IS the definition of the value, so an index outside it is
// malformed input rather than something to saturate. Saturating would be the
// forgiving choice and the wrong one: a stream that indexes past the table is
// a stream no decoder agrees with.
const (
	expTableLo = -60
	expTableHi = 95
)

// bandRange is a half-open coefficient span.
type bandRange struct{ start, end int }

// blockSize is everything derived once per block length: the spans a block of
// that size is decoded and reconstructed over.
type blockSize struct {
	len int
	// coefsEnd is the last codeable coefficient, roughly 91% of the block.
	coefsEnd int
	// highStart is where the noise-filled high bands may begin.
	highStart int
	// bands are the exponent band widths, which partition the block.
	bands []int
	// high are the exponent bands clipped to [highStart, coefsEnd], which is
	// what the per-band noise flags and gains are counted over.
	high []bandRange
	// plan is the transform for this block length, resolved once at
	// construction so Decode never takes the plan cache's lock.
	plan *imdctPlan
}

// Decoder decodes one WMA v1 or v2 stream.
//
// Two pieces of state make this decoder unlike the self-contained ones in this
// tree. The overlap accumulator carries half a frame forward, as any MDCT codec
// does; and the noise index (section 8 of docs/notes/wma-bitstream.md) walks a
// generated table without ever being reset per block, frame or packet, so it is
// a function of the whole decode history in the CELT-PRNG sense. Reset zeroes
// it, which a seek needs: a cached transcode must not depend on the access
// pattern that produced it, and no decode that starts mid-file can match a
// linear one's noise anyway.
type Decoder struct {
	cfg Config
	fmt audio.Format

	frameLen     int
	frameLenBits int
	// selBits is the width of a block-length selector, 0 when the stream has
	// one block size and reads none.
	selBits int
	sizes   []blockSize
	coefBk  int // the coefficient book pair, chosen once per stream

	useNoise bool
	noise    []float32 // the generated noise source
	noiseAt  int       // running index into it, never reset per block or packet
	expTable [expTableHi - expTableLo + 1]float32
	lspCos   []float64 // 2*cos(pi*i/frameLen), the LSP curve's angular step

	r       bitReader
	carry   bitAppender
	carried bool

	// Block-length state walks forward one block at a time and is re-seeded
	// only at a reset: at the start of decoding, and in a reservoir stream on
	// the first frame that begins in a packet.
	prevBits, curBits, nextBits int
	resetLens                   bool

	blockPos int

	// Per-channel decode state. exp holds the exponent curve at the block
	// length it was decoded at (expLen), which a shorter or longer block that
	// reuses it indexes through the ratio between the two.
	exp     [][]float32
	expLen  []int
	maxExp  []float32
	coefs   [][]float32
	spec    [][]float32
	acc     [][]float32
	noiseOn [][]bool
	noiseG  [][]int

	expCur  []float32 // exp resampled to the current block length
	block   []float32 // one block's time samples, shared across channels
	scratch *imdctScratch

	// skip is the decoder lead-in still owed. See NewDecoder.
	skip    int
	drained bool
	out     *audio.Buffer

	// err latches the first failure. See Decode.
	err error
	// resumed marks a decoder that was Reset mid-stream, which changes what a
	// superframe header can legally say. See decodeSuperframe.
	resumed bool
	// noResume is the one stream shape a Reset cannot be honest about. See
	// Reset.
	noResume bool
}

// NewDecoder returns a decoder for a parsed Config. The track format must be
// what Config.Format produces.
//
// The decoder drops its own lead-in rather than leaving it to the container:
// WMA carries no gapless metadata at all, and ASF's declared play duration is
// neither the source length nor the coded capacity, so a Track.Delay would
// also cap the output at a length that is not one. Measured by matched filter
// at every frame length and both versions
// (docs/notes/wma-oracle-corpus.md section 5), an untrimmed decode trails its
// source by exactly one frame, so one frame comes off the head -- here, and
// again after every Reset, which is what keeps the emitted stream aligned with
// the packet timeline a seek lands on.
func NewDecoder(cfg Config, f audio.Format) (*Decoder, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := f.Valid(); err != nil {
		return nil, err
	}
	// The whole format, not three of its fields: the layout and bit depth are
	// what a container gets wrong on its own, and a mismatch there reaches an
	// encoder that assigns channels by name rather than failing here. This is
	// the check ape, wavpack, alac and flac all make.
	if f != cfg.Format() {
		return nil, malformed("track format %v does not match the stream's %v", f, cfg.Format())
	}
	d := &Decoder{
		cfg:          cfg,
		fmt:          f,
		frameLen:     cfg.FrameLen(),
		frameLenBits: cfg.frameLenBits(),
		coefBk:       2 * cfg.coefBookPair(),
	}
	if err := d.buildSizes(); err != nil {
		return nil, err
	}
	for i := range d.expTable {
		d.expTable[i] = float32(math.Pow(10, float64(i+expTableLo)/16))
	}
	if !cfg.expVLC() {
		d.lspCos = make([]float64, d.frameLen)
		for i := range d.lspCos {
			d.lspCos[i] = 2 * math.Cos(math.Pi*float64(i)/float64(d.frameLen))
		}
	}
	if d.useNoise {
		d.buildNoise()
	}
	maxHigh := 0
	for i := range d.sizes {
		maxHigh = max(maxHigh, len(d.sizes[i].high))
	}
	n := d.frameLen
	for range cfg.Channels {
		d.exp = append(d.exp, make([]float32, n))
		d.coefs = append(d.coefs, make([]float32, n))
		d.spec = append(d.spec, make([]float32, n))
		d.acc = append(d.acc, make([]float32, 2*n))
		d.noiseOn = append(d.noiseOn, make([]bool, maxHigh))
		d.noiseG = append(d.noiseG, make([]int, maxHigh))
	}
	d.expLen = make([]int, cfg.Channels)
	d.maxExp = make([]float32, cfg.Channels)
	d.expCur = make([]float32, n)
	d.block = make([]float32, 2*n)
	d.scratch = newIMDCTScratch(2 * n)
	for i := range d.sizes {
		d.sizes[i].plan = planFor(d.sizes[i].len)
	}
	d.resetLens = true
	d.skip = d.frameLen
	return d, nil
}

// buildSizes derives, once, everything that depends only on the block length:
// the exponent band layout, the codeable span, and where the noise-filled high
// bands may start.
func (d *Decoder) buildSizes() error {
	c := d.cfg
	mult, noise := c.highFreqMult()
	d.useNoise = noise
	nb := c.nbBlockSizes()
	if nb > 1 {
		d.selBits = bitsOf(nb-1) + 1
	}
	for k := range nb {
		blockLen := d.frameLen >> k
		s := blockSize{
			len:       blockLen,
			coefsEnd:  c.coefsEnd(k),
			highStart: int(math.Round(float64(blockLen) * mult)),
			bands:     c.expBandWidths(k, blockLen),
		}
		if len(s.bands) == 0 || s.coefsEnd <= 0 {
			return malformed("no exponent bands fit a %d-sample block at %d Hz", blockLen, c.Rate)
		}
		at := 0
		for _, w := range s.bands {
			bs, be := at, at+w
			at = be
			if lo, hi := max(bs, s.highStart), min(be, s.coefsEnd); lo < hi {
				s.high = append(s.high, bandRange{lo, hi})
			}
		}
		// The band run covers at least blockLen*49000/rate, which clears the
		// codeable span at every rate this decoder accepts; nothing reads an
		// exponent past it.
		if at < s.coefsEnd {
			return malformed("exponent bands cover %d of a %d-coefficient span at %d Hz", at, s.coefsEnd, c.Rate)
		}
		d.sizes = append(d.sizes, s)
	}
	return nil
}

// expBandWidths lays the exponent bands out over one block. Both versions
// place the ends on the critical-band edges; v2 rounds each to the NEAREST
// multiple of four (rounding down instead gives a wrong layout on every v2
// stream), except for the three shortest block sizes at 22.05 kHz and up,
// where the widths are tabulated. The table is picked from the RAW sample
// rate, so 48 kHz uses the 44100 one.
func (c Config) expBandWidths(k, blockLen int) []int {
	if c.V2 && c.Rate >= 22050 {
		if row := c.frameLenBits() - 7 - k; row >= 0 && row < 3 {
			var t *[3][]uint8
			switch {
			case c.Rate >= 44100:
				t = &expBands44100
			case c.Rate >= 32000:
				t = &expBands32000
			default:
				t = &expBands22050
			}
			out := make([]int, len(t[row]))
			for i, w := range t[row] {
				out[i] = int(w)
			}
			return out
		}
	}
	var out []int
	prev := 0
	for _, edge := range criticalFreqs {
		var end int
		if c.V2 {
			end = (blockLen*2*int(edge) + 2*c.Rate) / (4 * c.Rate) << 2
		} else {
			end = int(math.Round(float64(blockLen*2*int(edge)) / float64(c.Rate)))
		}
		end = min(end, blockLen)
		if end > prev {
			out = append(out, end-prev)
			prev = end
		}
		if end >= blockLen {
			break
		}
	}
	return out
}

// buildNoise generates the noise source. The 8192 entries are not stored
// anywhere: they come from a linear congruential generator seeded at 1.
func (d *Decoder) buildNoise() {
	const n = 8192
	scale := math.Ldexp(1, -31) * math.Sqrt(3) * d.cfg.noiseMult()
	d.noise = make([]float32, n)
	seed := uint32(1)
	for i := range d.noise {
		seed = seed*314159 + 1
		d.noise[i] = float32(float64(int32(seed)) * scale)
	}
}

// nextNoise draws one value and advances the running index.
func (d *Decoder) nextNoise() float32 {
	v := d.noise[d.noiseAt]
	d.noiseAt = (d.noiseAt + 1) & 8191
	return v
}

// Decode consumes one superframe.
//
// A failure is final until Reset. Nothing in a WMA packet re-anchors a reader
// that has drifted -- no sync word, no frame header, and a block-length walk
// and exponent curve that carry across packets -- so there is no state a
// second Decode could resume from. Latching the first error is what keeps a
// caller that retries from reading a half-updated walk as if it were a
// stream's.
func (d *Decoder) Decode(pkt []byte, emit func(*audio.Buffer) error) error {
	if d.err != nil {
		return d.err
	}
	if err := d.decode(pkt, emit); err != nil {
		d.err = err
		return err
	}
	return nil
}

func (d *Decoder) decode(pkt []byte, emit func(*audio.Buffer) error) error {
	if len(pkt) == 0 {
		return malformed("empty packet")
	}
	if d.noResume {
		return unsupported("this stream mixes block lengths without a bit reservoir, so a decode cannot resume mid-file: " +
			"the three block lengths are decoder state that nothing in the bitstream restates")
	}
	d.r.reset(pkt)
	if !d.cfg.reservoir() {
		// The superframe is one frame and decoding starts at bit 0. A
		// non-reservoir stream has no per-packet reset at all: the block
		// lengths walk forward across every packet in the file.
		return d.decodeFrame(emit)
	}
	return d.decodeSuperframe(pkt, emit)
}

// decodeSuperframe walks a reservoir packet: a header, the tail of the frame
// the previous packet left open, the frames that start here, and whatever is
// carried on.
func (d *Decoder) decodeSuperframe(pkt []byte, emit func(*audio.Buffer) error) error {
	offBits, err := d.cfg.offsetBits()
	if err != nil {
		return err
	}
	r := &d.r
	r.skip(4) // superframe index, which readers ignore
	frames := int(r.bits(4))
	bitOff := int(r.bits(offBits + 3))
	if r.err != nil {
		return r.err
	}
	headerBits := 8 + offBits + 3

	// The frame count field names how many frames the packet OUTPUTS, not how
	// many start in it: with a carry pending one of them is the carried frame.
	n := frames
	if !d.carried {
		n = frames - 1
	}
	if n < 0 {
		// F == 0 with no carry pending. In a linear decode that is damage,
		// and the notes say so. After a seek it is not: the decoder has no
		// carry but the stream does, and a landing on a packet that only
		// continues a frame started earlier is a legal landing -- container/asf
		// offers every media object as one. Nothing here can be decoded, and
		// the frame in progress is not ours to finish, so the packet is passed
		// over and the next one's own frames resynchronise the walk.
		if d.resumed {
			d.carry.reset()
			d.carried = false
			return nil
		}
		return malformed("superframe states %d frames with no frame carried in", frames)
	}
	// Past here the packet either starts a frame or completes one, so the
	// decoder is back on the stream's own frame boundaries.
	d.resumed = false
	if headerBits+bitOff > r.bitLen() {
		return malformed("superframe bit offset %d runs past the packet", bitOff)
	}
	at := headerBits + bitOff

	if n == 0 {
		// The all-continuation packet: nothing finishes here. With a frame
		// already open the whole payload continues it, so there is no head run
		// to skip and the carry resumes at the payload's start; with none, a
		// frame begins here and the carry starts where it does. A well-formed
		// writer states a zero offset either way, so the two agree; spelling
		// them apart is what keeps a nonzero one from eating payload.
		from := at
		if d.carried {
			from = headerBits
		} else {
			// A frame begins here, so the block-length walk resets for it --
			// the same reset the n >= 1 path arms below, owed at the same
			// point for the same reason. It has to be armed here rather than
			// where the carry is finally decoded, because by then the packet
			// that began the frame is gone.
			d.carry.reset()
			d.resetLens = true
		}
		if r.bitLen()-from <= 8 {
			return malformed("all-continuation superframe carries no payload")
		}
		if err := d.appendCarry(pkt, from, r.bitLen()-from); err != nil {
			return err
		}
		d.carried = true
		return nil
	}

	if d.carried {
		if err := d.appendCarry(pkt, headerBits, bitOff); err != nil {
			return err
		}
		saved := d.r
		d.r.resetBits(d.carry.buf, d.carry.bits)
		err := d.decodeFrame(emit)
		d.r = saved
		d.carried = false
		if err != nil {
			return err
		}
		n--
	}

	// The frames that begin here reset the block-length walk; the frame
	// completed from the carry does not, because it continues a run that began
	// in the previous packet.
	d.r.pos = at
	d.resetLens = true
	for range n {
		if err := d.decodeFrame(emit); err != nil {
			return err
		}
	}

	d.carry.reset()
	d.carried = false
	if left := d.r.bitLen() - d.r.pos; left > 0 {
		if err := d.appendCarry(pkt, d.r.pos, left); err != nil {
			return err
		}
		d.carried = true
	}
	return nil
}

// appendCarry adds a bit run to the reservoir carry. The carry is not byte
// aligned -- what a packet leaves unread stops mid-byte -- so it is repacked
// rather than stored as bytes plus an offset.
func (d *Decoder) appendCarry(pkt []byte, at, n int) error {
	if (d.carry.bits+n+7)/8 > maxCarryBytes {
		return malformed("the bit reservoir carry passes %d bytes", maxCarryBytes)
	}
	d.carry.appendFrom(pkt, at, n)
	return nil
}

// decodeFrame decodes blocks until they fill the frame, then emits it.
func (d *Decoder) decodeFrame(emit func(*audio.Buffer) error) error {
	d.blockPos = 0
	for d.blockPos < d.frameLen {
		if err := d.decodeBlock(); err != nil {
			return err
		}
	}
	return d.emitFrame(emit)
}

// emitFrame delivers the completed first half of the accumulator and shifts
// the second half down to become the first.
func (d *Decoder) emitFrame(emit func(*audio.Buffer) error) error {
	n := d.frameLen
	off := min(d.skip, n)
	d.skip -= off
	if off < n {
		if d.out == nil {
			d.out = audio.Get(d.fmt, n)
		}
		d.out.N = n - off
		for ch := range d.cfg.Channels {
			copy(d.out.ChanF(ch), d.acc[ch][off:n])
		}
	}
	for ch := range d.cfg.Channels {
		// The second half becomes the first. Clearing what it leaves behind
		// keeps the accumulator empty ahead of the write frontier; like the
		// zero-store in decodeBlock it is belt to that braces, since the
		// blocks' stores already tile the region a frame reads.
		copy(d.acc[ch][:n], d.acc[ch][n:])
		clear(d.acc[ch][n:])
	}
	if off < n {
		return emit(d.out)
	}
	return nil
}

// blockLens reads the block-length selectors and returns the previous,
// current, and next block lengths in bits.
func (d *Decoder) blockLens() error {
	if d.selBits == 0 {
		d.prevBits, d.curBits, d.nextBits = d.frameLenBits, d.frameLenBits, d.frameLenBits
		return nil
	}
	sel := func() (int, error) {
		v := int(d.r.bits(d.selBits))
		if v >= len(d.sizes) {
			return 0, malformed("block-length selector %d, stream has %d block sizes", v, len(d.sizes))
		}
		return d.frameLenBits - v, nil
	}
	// Nothing is committed until all three are in hand. A partial commit would
	// leave a selector's zero return standing as a block length, and zero is
	// not one: it indexes the size table past its end.
	prev, cur := d.curBits, d.nextBits
	var err error
	if d.resetLens {
		if prev, err = sel(); err != nil {
			return err
		}
		if cur, err = sel(); err != nil {
			return err
		}
	}
	next, err := sel()
	if err != nil {
		return err
	}
	d.prevBits, d.curBits, d.nextBits = prev, cur, next
	d.resetLens = false
	return nil
}

// escapeBits is the coefficient escape's level width, which the block's total
// gain picks.
func escapeBits(totalGain int) int {
	switch {
	case totalGain < 15:
		return 13
	case totalGain < 32:
		return 12
	case totalGain < 40:
		return 11
	case totalGain < 45:
		return 10
	default:
		return 9
	}
}

// decodeBlock reads and reconstructs one block.
func (d *Decoder) decodeBlock() error {
	r := &d.r
	if err := d.blockLens(); err != nil {
		return err
	}
	blockLen := 1 << d.curBits
	if d.blockPos+blockLen > d.frameLen {
		return malformed("a %d-sample block at %d overruns the %d-sample frame", blockLen, d.blockPos, d.frameLen)
	}
	k := d.frameLenBits - d.curBits
	sz := &d.sizes[k]

	msStereo := false
	if d.cfg.Channels == 2 {
		msStereo = r.bit() != 0
	}
	var coded [2]bool
	any := false
	for ch := range d.cfg.Channels {
		coded[ch] = r.bit() != 0
		any = any || coded[ch]
	}
	if r.err != nil {
		return r.err
	}

	if !any {
		// The block stops the bitstream reading and nothing else, but the
		// transform stage still runs with an all-zero spectrum. That is where
		// the block position advances, so a decoder that returned here instead
		// would never finish the frame; and it is what the format says
		// happens.
		//
		// Its zero-store is, measured, unobservable: the window transitions
		// make each block's non-zero span end where the next one's store
		// begins, so the region it clears is already zero. Kept so the
		// invariant is local to this walk, and recorded so nobody spends an
		// afternoon on the test that cannot fail.
		return d.transform(coded, msStereo, blockLen)
	}

	totalGain := 1
	for {
		v := int(r.bits(7))
		if r.err != nil {
			return r.err
		}
		totalGain += v
		if v != 127 {
			break
		}
		if totalGain > maxTotalGain {
			return malformed("block gain ladder passes %d", maxTotalGain)
		}
	}
	if totalGain > maxTotalGain {
		return malformed("block gain %d passes %d", totalGain, maxTotalGain)
	}
	escBits := escapeBits(totalGain)

	if d.useNoise {
		if err := d.readNoiseCoding(coded, sz); err != nil {
			return err
		}
	}

	// One bit for the WHOLE block, not one per channel: set means exponents
	// are transmitted, clear means the previous block's are reused. A
	// full-length block always transmits and reads no bit.
	transmit := true
	if blockLen < d.frameLen {
		transmit = r.bit() != 0
	}
	for ch := range d.cfg.Channels {
		if !coded[ch] {
			continue
		}
		if transmit {
			if err := d.decodeExponents(ch, sz, blockLen); err != nil {
				return err
			}
			d.expLen[ch] = blockLen
		} else if d.expLen[ch] == 0 {
			return malformed("channel %d reuses exponents that were never transmitted", ch)
		}
	}

	for ch := range d.cfg.Channels {
		if coded[ch] {
			if err := d.decodeCoefs(ch, sz, blockLen, escBits, msStereo); err != nil {
				return err
			}
		}
		// v1 byte-aligns after each channel slot of a stereo block, whether or
		// not that channel was coded; v2 never does. It is the one place the
		// two versions differ inside a block.
		if !d.cfg.V2 && d.cfg.Channels == 2 {
			r.align()
		}
	}
	if r.err != nil {
		return r.err
	}

	for ch := range d.cfg.Channels {
		if coded[ch] {
			if err := d.reconstruct(ch, sz, blockLen, totalGain); err != nil {
				return err
			}
		}
	}
	// Mid/side is undone on the coefficients, before the inverse transform.
	// Both one-sided cases occur and they are not symmetric: with only channel
	// 1 coded the mid is zero and the butterfly still runs (L = S, R = -S);
	// with only channel 0 coded the side is absent, the butterfly does not
	// run, and both outputs are the mid, which transform reaches by leaving
	// channel 1's time samples holding channel 0's.
	if msStereo && coded[1] {
		if !coded[0] {
			clear(d.spec[0][:blockLen])
			coded[0] = true
		}
		a, b := d.spec[0][:blockLen], d.spec[1][:blockLen]
		for i := range blockLen {
			a[i], b[i] = a[i]+b[i], a[i]-b[i]
		}
	}
	return d.transform(coded, msStereo, blockLen)
}

// readNoiseCoding reads the high-band flags and gains. They are TWO separate
// passes over the channels: every coded channel's flags, then every coded
// channel's gains. A reader that takes flags and gains together per channel
// desynchronises every stereo block.
func (d *Decoder) readNoiseCoding(coded [2]bool, sz *blockSize) error {
	r := &d.r
	for ch := range d.cfg.Channels {
		if !coded[ch] {
			continue
		}
		for j := range sz.high {
			d.noiseOn[ch][j] = r.bit() != 0
		}
	}
	book := hgainBook()
	for ch := range d.cfg.Channels {
		if !coded[ch] {
			continue
		}
		gain, first := 0, true
		for j := range sz.high {
			if !d.noiseOn[ch][j] {
				continue
			}
			if first {
				gain = int(r.bits(7)) - 19
				first = false
			} else {
				sym := book.decode(r)
				if sym < 0 {
					return malformed("noise-gain book desynchronised")
				}
				gain += hgainDelta(sym)
			}
			d.noiseG[ch][j] = gain
		}
	}
	return r.err
}

// decodeExponents fills channel ch's exponent curve for a blockLen block.
func (d *Decoder) decodeExponents(ch int, sz *blockSize, blockLen int) error {
	// Both paths write the curve in place, so a refusal partway leaves a
	// splice of this block's bands and the previous block's, with maxExp still
	// describing the old one. Marking it untransmitted here is what keeps a
	// later block from reusing the splice: the reuse path already refuses a
	// curve that was never transmitted, and a half-written one is no more
	// usable than none. decodeBlock sets the length back on success.
	d.expLen[ch] = 0
	if !d.cfg.expVLC() {
		return d.decodeLSPExponents(ch, blockLen)
	}
	r := &d.r
	exp := d.exp[ch][:blockLen]
	book := expScaleBook()
	// v1 seeds the running index from a 5-bit field plus 10 and pre-fills the
	// first band with it; v2 seeds it to 36 and reads a code for every band.
	//
	// Neither seed is observable in the output. The reconstruction divides by
	// the maximum exponent, so shifting the whole curve by a table index
	// cancels exactly -- in the coded span, in a noise band's power ratio, and
	// in the tail. Only the DELTAS reach the samples. What the seed can still
	// do is decide when the running index runs off the end of the table, which
	// is a refusal.
	idx, first := 36, 0
	if !d.cfg.V2 {
		idx = int(r.bits(5)) + 10
		first = 1
	}
	maxIdx, at := expTableLo-1, 0
	fill := func(w int) {
		v := d.expTable[idx-expTableLo]
		for range w {
			exp[at] = v
			at++
		}
		maxIdx = max(maxIdx, idx)
	}
	if first == 1 {
		fill(sz.bands[0])
	}
	for b := first; b < len(sz.bands); b++ {
		sym := book.decode(r)
		if sym < 0 {
			return malformed("exponent book desynchronised")
		}
		idx += sym - 60
		if idx < expTableLo || idx > expTableHi {
			return malformed("exponent index %d outside %d..%d", idx, expTableLo, expTableHi)
		}
		fill(sz.bands[b])
	}
	clear(exp[at:])
	d.maxExp[ch] = d.expTable[maxIdx-expTableLo]
	return r.err
}

// decodeLSPExponents reads the ten LSP coefficients and evaluates the curve.
// The angular step comes from the FRAME length, not the block length: a short
// block uses the first blockLen entries of a frame-length cosine table.
func (d *Decoder) decodeLSPExponents(ch, blockLen int) error {
	r := &d.r
	var lsp [nbLSPCoefs]float64
	for j := range nbLSPCoefs {
		n := 4
		if j == 0 || j == 8 || j == 9 {
			n = 3
		}
		lsp[j] = float64(lspCodebook[j][r.bits(n)])
	}
	if r.err != nil {
		return r.err
	}
	exp := d.exp[ch][:blockLen]
	maxv := float32(0)
	for i := range blockLen {
		w := d.lspCos[i]
		p, q := 0.5, 0.5
		for j := 1; j < nbLSPCoefs; j += 2 {
			q *= w - lsp[j-1]
			p *= w - lsp[j]
		}
		p *= p * (2 - w)
		q *= q * (2 + w)
		s := p + q
		if s <= 0 {
			return malformed("the LSP exponent curve is degenerate at bin %d", i)
		}
		// s^(-1/4) as two square roots and a divide. This is one
		// transcendental per BIN per coded channel -- one per output sample
		// per channel, against one table lookup per band on the VLC path -- so
		// it is the whole cost of the LSP path, and math.Pow here left a 48
		// kHz stereo LSP stream at 220x realtime against a 500x floor.
		// math.Sqrt is correctly rounded, so the two forms differ by under two
		// float64 ulps and agree exactly once stored as float32
		// (BenchmarkLSPExponentCurve, and 20M sampled values found no
		// disagreement); the Version constant is bumped anyway, since "agrees
		// on everything sampled" is not "cannot differ".
		v := float32(1 / math.Sqrt(math.Sqrt(s)))
		exp[i] = v
		maxv = max(maxv, v)
	}
	d.maxExp[ch] = maxv
	return nil
}

// decodeCoefs run-level decodes one channel into a dense array: the values
// skip the noise-filled high bands, which is what nbCoefs counts.
func (d *Decoder) decodeCoefs(ch int, sz *blockSize, blockLen, escBits int, msStereo bool) error {
	r := &d.r
	coefs := d.coefs[ch][:blockLen]
	clear(coefs)
	nb := sz.coefsEnd - d.cfg.coefsStart()
	if d.useNoise {
		for j := range sz.high {
			if d.noiseOn[ch][j] {
				nb -= sz.high[j].end - sz.high[j].start
			}
		}
	}
	if nb < 0 {
		return malformed("noise fills more of the block than it codes")
	}
	// Within the pair the first book codes an ordinary channel and the second
	// codes channel 1 of a mid/side block, where less energy is expected.
	book := coefBooks()[d.coefBk]
	if msStereo && ch == 1 {
		book = coefBooks()[d.coefBk+1]
	}
	mask := blockLen - 1
	offset := 0
	for offset < nb {
		code := book.vlc.decode(r)
		if code < 0 {
			return malformed("coefficient book desynchronised")
		}
		if code == 1 {
			break
		}
		var run, level int
		if code == 0 {
			// The escape reads its run with frameLenBits bits rather than the
			// block's own, which wastes bits on a short block; that is the
			// format, not a bug to fix.
			level = int(r.bits(escBits))
			run = int(r.bits(d.frameLenBits))
		} else {
			run, level = int(book.run[code]), int(book.level[code])
		}
		offset += run
		// A sign bit of 1 means POSITIVE, which is the opposite of the usual
		// convention and the same in both arms.
		if r.bit() == 0 {
			level = -level
		}
		coefs[offset&mask] = float32(level)
		offset++
	}
	if r.err != nil {
		return r.err
	}
	// End-of-block may be omitted when the run fills the channel exactly, so
	// running out is a normal exit; ending past it is not.
	if offset > nb {
		return malformed("run-level coding reaches %d past the channel's %d coefficients", offset, nb)
	}
	return nil
}

// reconstruct turns one channel's exponents, coefficients and noise gains into
// the block's spectrum.
func (d *Decoder) reconstruct(ch int, sz *blockSize, blockLen, totalGain int) error {
	ec := d.expCur[:blockLen]
	resampleExponents(ec, d.exp[ch], d.expLen[ch])

	norm := mdctNorm(blockLen, d.cfg.V2) / 32768
	mult := math.Pow(10, float64(totalGain)/20) / float64(d.maxExp[ch]) * norm
	if math.IsInf(mult, 0) || math.IsNaN(mult) {
		return malformed("block gain %d and exponent scale produce no finite coefficient scale", totalGain)
	}
	m := float32(mult)

	spec := d.spec[ch][:blockLen]
	coefs := d.coefs[ch]
	start, end := d.cfg.coefsStart(), sz.coefsEnd
	ptr := 0

	// Below the first codeable coefficient (v1 only, since v2 starts at 0).
	for i := range start {
		if d.useNoise {
			spec[i] = d.nextNoise() * ec[i] * m
		} else {
			spec[i] = 0
		}
	}

	// The coded span, walked as one region up to the high bands and then one
	// region per high band. The small noise is added to CODED coefficients
	// too, which is why a bit-exact decode is impossible without matching the
	// noise index.
	lo := min(sz.highStart, end)
	if d.useNoise {
		for i := start; i < lo; i++ {
			spec[i] = (coefs[ptr] + d.nextNoise()) * ec[i] * m
			ptr++
		}
	} else {
		for i := start; i < lo; i++ {
			spec[i] = coefs[ptr] * ec[i] * m
			ptr++
		}
	}

	if d.useNoise && len(sz.high) > 0 {
		last := -1
		for j := range sz.high {
			if d.noiseOn[ch][j] {
				last = j
			}
		}
		var refPower float64
		if last >= 0 {
			refPower = bandPower(ec, sz.high[last])
		}
		for j, b := range sz.high {
			if !d.noiseOn[ch][j] {
				for i := b.start; i < b.end; i++ {
					spec[i] = (coefs[ptr] + d.nextNoise()) * ec[i] * m
					ptr++
				}
				continue
			}
			g := math.Sqrt(bandPower(ec, b)/refPower) *
				math.Pow(10, float64(d.noiseG[ch][j])/20) /
				(float64(d.maxExp[ch]) * d.cfg.noiseMult()) * norm
			if math.IsInf(g, 0) || math.IsNaN(g) {
				return malformed("noise gain %d produces no finite band scale", d.noiseG[ch][j])
			}
			g32 := float32(g)
			for i := b.start; i < b.end; i++ {
				spec[i] = d.nextNoise() * ec[i] * g32
			}
		}
	}

	// Above the codeable span: one exponent, the last of the coded span,
	// reused for the whole tail rather than a curve.
	if d.useNoise {
		lastExp := ec[end-1] * m
		for i := end; i < blockLen; i++ {
			spec[i] = d.nextNoise() * lastExp
		}
	} else {
		clear(spec[end:])
	}
	return nil
}

// resampleExponents maps a curve decoded at expLen onto a block of len(dst)
// coefficients. Exponents reused from another block size are indexed through
// the RATIO between the length they were decoded at and this one, not through
// the current length alone: dst[i] is src[i*expLen/len(dst)]. Both lengths are
// powers of two, so the ratio is a shift.
func resampleExponents(dst, src []float32, expLen int) {
	switch n := len(dst); {
	case expLen == n:
		copy(dst, src[:n])
	case expLen > n:
		sh := bitsOf(expLen / n)
		for i := range dst {
			dst[i] = src[i<<sh]
		}
	default:
		sh := bitsOf(n / expLen)
		for i := range dst {
			dst[i] = src[i>>sh]
		}
	}
}

// bandPower is the mean of exponent-squared over a band.
func bandPower(ec []float32, b bandRange) float64 {
	var sum float64
	for i := b.start; i < b.end; i++ {
		v := float64(ec[i])
		sum += v * v
	}
	return sum / float64(b.end-b.start)
}

// transform runs the filterbank for every channel and folds the block into the
// accumulator. It runs for uncoded channels too: the block scratch is shared,
// so channel 1 of a mid/side block with only the mid coded takes the mid's own
// time samples, and a block with nothing coded folds in zeroes, which is what
// completes the previous block's overlap tail.
func (d *Decoder) transform(coded [2]bool, msStereo bool, blockLen int) error {
	// Every selector is range-checked before it becomes a block length, so
	// these indices are in bounds by the time they are used here.
	plan := d.sizes[d.frameLenBits-d.curBits].plan
	prevPlan := d.sizes[d.frameLenBits-d.prevBits].plan
	nextPlan := d.sizes[d.frameLenBits-d.nextBits].plan
	prevLen, nextLen := 1<<d.prevBits, 1<<d.nextBits
	block := d.block[:2*blockLen]
	at := d.frameLen/2 + d.blockPos - blockLen/2
	for ch := range d.cfg.Channels {
		switch {
		case coded[ch]:
			plan.imdct(d.spec[ch][:blockLen], block, d.scratch)
		case msStereo && ch == 1:
			// Keep what channel 0 left in the shared scratch. Clearing it here
			// silences the right channel on the common near-mono material.
		default:
			clear(block)
		}
		overlapAdd(d.acc[ch][at:at+2*blockLen], block, blockLen, prevLen, nextLen, plan, prevPlan, nextPlan)
	}
	d.blockPos += blockLen
	return nil
}

// Drain emits the half frame the accumulator still holds. Nothing follows it
// to complete its overlap, so it fades out under one window rather than two --
// but it is real coded output, and the stream is a frame short of its own
// source without it, since the decode trails the source by exactly that much.
// WMA has no padding count to trim it by, so a decoder emits it and downstream
// gaplessness is not on offer.
func (d *Decoder) Drain(emit func(*audio.Buffer) error) error {
	if d.err != nil {
		return d.err
	}
	if d.drained {
		return nil
	}
	d.drained = true
	if err := d.emitFrame(emit); err != nil {
		d.err = err
		return err
	}
	return nil
}

// Reset discards every piece of cross-packet state after a seek: the overlap
// accumulator, the exponents a later block may reuse, the reservoir carry, the
// block-length walk, the noise index, and any latched failure. Zeroing the
// noise index is what makes a seeked decode a function of the landing packet
// alone rather than of the access pattern that reached it; it cannot match a
// linear decode's noise either way, since the index is never reset by the
// format.
//
// Re-arming the block-length walk is a guess. A reservoir stream restates it
// per packet, so a landing recovers it; a varying-block stream WITHOUT a
// reservoir restates it nowhere, and re-arming there would read two selectors
// that were never written. That shape is refused a resume by name (noResume);
// no encoder writes it, so the refusal costs nothing real.
func (d *Decoder) Reset() {
	for ch := range d.acc {
		clear(d.acc[ch])
		d.expLen[ch] = 0
		d.maxExp[ch] = 0
	}
	d.carry.reset()
	d.carried = false
	d.resetLens = true
	d.resumed = true
	d.noResume = d.selBits > 0 && !d.cfg.reservoir()
	d.noiseAt = 0
	d.blockPos = 0
	d.skip = d.frameLen
	d.drained = false
	d.err = nil
	d.r.reset(nil)
}

// Release returns the pooled output buffer (format.Media calls it on Close).
func (d *Decoder) Release() {
	audio.Put(d.out)
	d.out = nil
}
