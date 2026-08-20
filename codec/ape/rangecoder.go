package ape

import "sync"

// The range coder, ported from the reference decoder. Two generations of it
// ship in the versions this package covers and they share everything but the
// symbol model and how the residual's low bits are coded: 3990 and later split
// a value into an overflow count and a base modulo an adaptive pivot, while
// 3950..3989 code the overflow and then k raw bits, k tracking the same
// running magnitude sum. Both feed the same predictor.

const (
	// codeBits is the range register's width; the constants below are the
	// reference's derivations from it.
	codeBits    = 32
	topValue    = uint32(1) << (codeBits - 1)
	bottomValue = topValue >> 8
	extraBits   = (codeBits-2)%8 + 1

	// modelElements is the symbol count of the overflow model; the last
	// symbol is the escape.
	modelElements = 64
	// rangeOverflowShift is the model's total-frequency width: 1<<16.
	rangeOverflowShift = 16

	// overflowSignal is the escaped overflow value the encoder reserves to
	// say it had to widen the pivot, and overflowPivotValue is the pivot it
	// widens to.
	overflowSignal     = 1
	overflowPivotValue = 32768
)

// rangeTotal2 is the 3990-and-later overflow model: cumulative frequencies
// over a 1<<16 total.
var rangeTotal2 = [65]uint32{
	0, 19578, 36160, 48417, 56323, 60899, 63265, 64435, 64971, 65232, 65351,
	65416, 65447, 65466, 65476, 65482, 65485, 65488, 65490, 65491, 65492,
	65493, 65494, 65495, 65496, 65497, 65498, 65499, 65500, 65501, 65502,
	65503, 65504, 65505, 65506, 65507, 65508, 65509, 65510, 65511, 65512,
	65513, 65514, 65515, 65516, 65517, 65518, 65519, 65520, 65521, 65522,
	65523, 65524, 65525, 65526, 65527, 65528, 65529, 65530, 65531, 65532,
	65533, 65534, 65535, 65536,
}

// rangeTotal1 is the 3950..3989 model, which spends its probability mass
// differently because the value coded after the overflow is k raw bits rather
// than a base modulo a pivot.
var rangeTotal1 = [65]uint32{
	0, 14824, 28224, 39348, 47855, 53994, 58171, 60926, 62682, 63786, 64463,
	64878, 65126, 65276, 65365, 65419, 65450, 65469, 65480, 65487, 65491,
	65493, 65494, 65495, 65496, 65497, 65498, 65499, 65500, 65501, 65502,
	65503, 65504, 65505, 65506, 65507, 65508, 65509, 65510, 65511, 65512,
	65513, 65514, 65515, 65516, 65517, 65518, 65519, 65520, 65521, 65522,
	65523, 65524, 65525, 65526, 65527, 65528, 65529, 65530, 65531, 65532,
	65533, 65534, 65535, 65536,
}

// kSumBoundary is the magnitude ladder k walks: k rises when the running sum
// reaches the next rung and falls when it drops below its own. The four zeros
// at the top are the stop, so k never leaves 0..27.
var kSumBoundary = [32]uint32{
	0, 32, 64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536,
	131072, 262144, 524288, 1048576, 2097152, 4194304, 8388608, 16777216,
	33554432, 67108864, 134217728, 268435456, 536870912, 1073741824,
	2147483648, 0, 0, 0, 0,
}

// rangeModel is one overflow model, ready to decode with: the cumulative
// frequencies, each symbol's width, and the inverse table that turns a decoded
// frequency back into its symbol.
//
// The reference lists the widths as their own constants; they are the totals'
// first differences, so deriving them keeps 128 transcribed integers from
// being a place a typo can silently break bit-exactness, and building both
// halves together is what makes a model and its table impossible to mismatch.
// The reference builds the same inverse table for the same reason: a search
// over 65 rungs would otherwise run once per sample.
type rangeModel struct {
	total *[65]uint32
	width [64]uint32
	tab   [65536]uint8
}

func newRangeModel(total *[65]uint32) *rangeModel {
	m := &rangeModel{total: total}
	for i := range m.width {
		m.width[i] = total[i+1] - total[i]
	}
	sym := uint8(0)
	for i := range m.tab {
		if uint32(i) >= total[sym+1] {
			sym++
		}
		m.tab[i] = sym
	}
	return m
}

// The two models are built on first use, so a process that never decodes APE
// never pays the 64 KB apiece.
var (
	modelCurrent = sync.OnceValue(func() *rangeModel { return newRangeModel(&rangeTotal2) })
	modelOld     = sync.OnceValue(func() *rangeModel { return newRangeModel(&rangeTotal1) })
)

// overSlack is how far past its bytes a reader may run before the frame is
// called truncated. A frame decodes whole before its CRC is checked, so
// without it a stream declaring a million blocks and carrying none grinds
// through all of them on zeros first. Real frames never reach it: measured
// against the reference encoder they stop one to five bytes short of their
// own last byte.
const overSlack = 8

// bitReader reads a frame's coded bytes.
//
// The bytes are not in file order: the coder works over 32-bit words and takes
// their most significant byte first, so within each aligned four-byte group
// the file's bytes are consumed back to front. A frame starts wherever in a
// group the frame before it ended, which is what the packet's skip counts.
//
// Reads past the end return zero, which is what the reference's own
// zero-filled tail does; running well past it is what exhausted reports.
type bitReader struct {
	data []byte
	bit  int
	over bool
}

func (r *bitReader) reset(data []byte, skip int) {
	r.data, r.bit, r.over = data, skip*8, false
}

func (r *bitReader) byteAt(i int) uint32 {
	if p := i&^3 + (3 - i&3); p < len(r.data) {
		return uint32(r.data[p])
	}
	r.over = true
	return 0
}

// exhausted reports that the frame's coded bytes ran out.
func (r *bitReader) exhausted() bool { return r.over && r.bit>>3 > len(r.data)+overSlack }

// bits reads n bits, most significant first, from wherever the reader stands.
func (r *bitReader) bits(n int) uint32 {
	var v uint32
	for n > 0 {
		avail := 8 - r.bit&7
		take := min(n, avail)
		b := r.byteAt(r.bit>>3) >> (avail - take) & (1<<take - 1)
		v = v<<take | b
		r.bit += take
		n -= take
	}
	return v
}

// readByte is bits(8) for a reader standing on a byte boundary, which is where
// the range coder always leaves it.
func (r *bitReader) readByte() uint32 {
	b := r.byteAt(r.bit >> 3)
	r.bit += 8
	return b
}

// alignByte moves to the next byte boundary.
func (r *bitReader) alignByte() { r.bit = (r.bit + 7) &^ 7 }

// entropyState is one channel's adaptive magnitude tracking: kSum is a
// decaying sum of coded magnitudes and k its position on the ladder. Every
// frame resets both, which is what makes a frame a sync point.
type entropyState struct {
	k    uint32
	kSum uint32
}

func (s *entropyState) flush() {
	s.k = 10
	s.kSum = 1 << s.k * 16
}

// update folds a decoded magnitude into the state. The arithmetic is uint32
// throughout and is allowed to wrap: the encoder does the same, so a stream
// that drives the sum around zero decodes to what was encoded.
func (s *entropyState) update(value int64) {
	s.kSum += uint32((value+1)/2) - (s.kSum+16)>>5
	if s.kSum < kSumBoundary[s.k] {
		s.k--
	} else if kSumBoundary[s.k+1] != 0 && s.kSum >= kSumBoundary[s.k+1] {
		s.k++
	}
}

// rangeDecoder is the frame's entropy decoder.
type rangeDecoder struct {
	br  bitReader
	low uint32
	rng uint32
	buf uint32

	// old selects the 3950..3989 value coding, which comes with its own model.
	old bool
	m   *rangeModel
}

func newRangeDecoder(fileVersion int) *rangeDecoder {
	d := &rangeDecoder{old: fileVersion < 3990}
	if d.old {
		d.m = modelOld()
	} else {
		d.m = modelCurrent()
	}
	return d
}

// start positions the decoder on a frame's coded bytes and primes the range
// registers. The first byte after the frame header is skipped: the encoder
// emits a dummy there because not emitting it costs more than it saves.
func (d *rangeDecoder) start() {
	d.br.alignByte()
	d.br.readByte()
	d.buf = d.br.readByte()
	d.low = d.buf >> (8 - extraBits)
	d.rng = 1 << extraBits
}

// normalize refills the range register a byte at a time. It returns false when
// the range collapses, which only a damaged stream does; the reference stops
// producing values there too and lets the frame's CRC report it.
func (d *rangeDecoder) normalize() bool {
	for d.rng <= bottomValue {
		d.buf = d.buf<<8 | d.br.readByte()
		d.low = d.low<<8 | d.buf>>1&0xff
		d.rng <<= 8
		if d.rng == 0 {
			return false
		}
	}
	return true
}

// decodeFast reads a symbol's cumulative frequency without consuming it; the
// caller narrows the range once it knows which symbol that was.
func (d *rangeDecoder) decodeFast(shift int) uint32 {
	if !d.normalize() {
		return 0
	}
	d.rng >>= shift
	return d.low / d.rng
}

// decodeDirect reads shift raw bits.
func (d *rangeDecoder) decodeDirect(shift int) (uint32, error) {
	for d.rng <= bottomValue {
		if d.rng == 0 {
			return 0, malformed("range coder collapsed")
		}
		d.buf = d.buf<<8 | d.br.readByte()
		d.low = d.low<<8 | d.buf>>1&0xff
		d.rng <<= 8
	}
	d.rng >>= shift
	if d.rng == 0 {
		return 0, malformed("range coder collapsed")
	}
	v := d.low / d.rng
	d.low %= d.rng
	return v, nil
}

// decodeOverflow reads the value's overflow symbol, following the escape to a
// literal count and, in the 3990 coding, to the widened pivot the encoder
// signals when a value will not fit the adaptive one.
func (d *rangeDecoder) decodeOverflow(pivot *uint32) (uint32, error) {
	// The escape can widen the pivot once per value: the encoder raises it
	// and re-codes, and never signals twice. A stream that does is damaged,
	// and looping here rather than recursing is what keeps it bounded.
	for range 2 {
		total := d.decodeFast(rangeOverflowShift)
		if total >= 65536 {
			return 0, malformed("range coder frequency %d out of model", total)
		}
		overflow := uint32(d.m.tab[total])
		d.low -= d.rng * d.m.total[overflow]
		d.rng *= d.m.width[overflow]
		if overflow != modelElements-1 {
			return overflow, nil
		}
		if d.old {
			// The old coding escapes to a literal k rather than a literal
			// overflow; the caller reads it and the overflow is zero.
			return overflow, nil
		}
		hi, err := d.decodeDirect(16)
		if err != nil {
			return 0, err
		}
		lo, err := d.decodeDirect(16)
		if err != nil {
			return 0, err
		}
		if overflow = hi<<16 | lo; overflow != overflowSignal {
			return overflow, nil
		}
		*pivot = overflowPivotValue
	}
	return 0, malformed("range coder repeats its overflow signal")
}

// decodeValue reads one residual.
func (d *rangeDecoder) decodeValue(s *entropyState) (int32, error) {
	if d.br.exhausted() {
		return 0, malformed("frame ends mid-value: its coded data ran out")
	}
	var (
		value int64
		live  bool
		err   error
	)
	if d.old {
		value, live, err = d.decodeValueOld(s)
	} else {
		value, live, err = d.decodeValueCurrent(s)
	}
	if err != nil {
		return 0, err
	}
	if !live {
		// The range collapsed. The reference stops producing values there
		// without folding anything into the entropy state, so neither do we:
		// the frame is already lost and its CRC will say so, but the state
		// two decoders arrive at should still be the same one.
		return 0, nil
	}
	s.update(value)
	// Fold the sign back in: the coder works on magnitudes with the sign in
	// the low bit.
	if value&1 != 0 {
		return int32(value>>1 + 1), nil
	}
	return int32(-(value >> 1)), nil
}

// decodeValueCurrent is the 3990-and-later coding: an overflow count times an
// adaptive pivot, plus a base below it. live is false when the range collapsed
// and no value was produced.
func (d *rangeDecoder) decodeValueCurrent(s *entropyState) (value int64, live bool, err error) {
	pivot := max(s.kSum/32, 1)
	overflow, err := d.decodeOverflow(&pivot)
	if err != nil {
		return 0, false, err
	}
	var base uint32
	if pivot >= 1<<16 {
		// A pivot too wide to code in one go is split into a high part and a
		// power-of-two low part, each coded against its own range.
		bits := 0
		for pivot>>bits > 0 {
			bits++
		}
		shift := 0
		if bits >= 16 {
			shift = bits - 16
		}
		splitFactor := uint32(1) << shift
		hi, err := d.decodeDivided(pivot/splitFactor + 1)
		if err != nil {
			return 0, false, err
		}
		lo, err := d.decodeDivided(splitFactor)
		if err != nil {
			return 0, false, err
		}
		base = hi*splitFactor + lo
	} else {
		if !d.normalize() {
			return 0, false, nil
		}
		d.rng /= pivot
		base = d.low / d.rng
		d.low %= d.rng
	}
	return int64(base) + int64(overflow)*int64(pivot), true, nil
}

// decodeDivided reads one value uniformly distributed over [0, n).
func (d *rangeDecoder) decodeDivided(n uint32) (uint32, error) {
	if !d.normalize() {
		return 0, malformed("range coder collapsed")
	}
	d.rng /= n
	if d.rng == 0 {
		return 0, malformed("range coder collapsed")
	}
	v := d.low / d.rng
	d.low %= d.rng
	return v, nil
}

// decodeValueOld is the 3950..3989 coding: an overflow count shifted up by k
// raw bits, k coming from the magnitude ladder rather than from an adaptive
// pivot. The escape codes k itself and drops the overflow.
func (d *rangeDecoder) decodeValueOld(s *entropyState) (int64, bool, error) {
	pivot := uint32(0) // unused by this coding; the escape never widens it
	overflow, err := d.decodeOverflow(&pivot)
	if err != nil {
		return 0, false, err
	}
	var k uint32
	if overflow == modelElements-1 {
		if k, err = d.decodeDirect(5); err != nil {
			return 0, false, err
		}
		overflow = 0
	} else if s.k >= 1 {
		k = s.k - 1
	}
	var value int64
	if k <= 16 {
		v, err := d.decodeDirect(int(k))
		if err != nil {
			return 0, false, err
		}
		value = int64(v)
	} else {
		lo, err := d.decodeDirect(16)
		if err != nil {
			return 0, false, err
		}
		hi, err := d.decodeDirect(int(k) - 16)
		if err != nil {
			return 0, false, err
		}
		value = int64(lo) | int64(hi)<<16
	}
	return value + int64(overflow)<<k, true, nil
}
