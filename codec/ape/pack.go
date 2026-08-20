package ape

// The range encoder, the inverse of rangecoder.go's decoder. Only the
// 3990 coding is written: the older one is a different value coding that
// nothing in circulation produces and that this package reads only so old
// files keep playing.

// shiftBits is where the range register's top byte sits, and the width the
// carry logic works at.
const shiftBits = codeBits - 9

// frameWriter accumulates a frame's coded bytes in the form they take in the
// file: 32-bit little-endian words whose most significant byte comes first, so
// logical byte 4k+j lands at physical 4k+(3-j).
//
// A frame gets its own words here, starting at logical byte zero. In the file
// the frames share the word at each boundary, but that is the muxer's business:
// it knows where the run has reached and repacks each frame as it lays it down.
// Keeping the packet word-aligned is what makes it a whole object -- its
// alignment skip is zero, so the decoder can take it as it stands -- and it
// costs the up-to-three padding bytes only inside the packet, never in the
// file.
type frameWriter struct {
	buf  []byte
	base int // bytes reserved ahead of the coded data, a whole number of words
	n    int // logical bytes written
}

// reset empties the writer and reserves prefix bytes in front of the coded
// data, which is where the packet's frame header goes: reserving it here is
// what keeps a finished frame from having to be copied to make room.
func (w *frameWriter) reset(prefix int) {
	w.buf = w.buf[:0]
	for range prefix {
		w.buf = append(w.buf, 0)
	}
	w.base, w.n = prefix, 0
}

func (w *frameWriter) putByte(v byte) {
	if w.n&3 == 0 {
		w.buf = append(w.buf, 0, 0, 0, 0)
	}
	w.buf[w.base+w.n&^3+3-w.n&3] = v
	w.n++
}

// putUint32 writes a whole word, most significant byte first. It is the frame
// header's form: the CRC and the special-code word both go out this way, and
// both are word-aligned, so the bytes land where a reader's 32-bit read finds
// them.
func (w *frameWriter) putUint32(v uint32) {
	w.putByte(byte(v >> 24))
	w.putByte(byte(v >> 16))
	w.putByte(byte(v >> 8))
	w.putByte(byte(v))
}

// rangeEncoder is the frame's entropy encoder.
//
// Carry is deferred rather than propagated: a byte that is all ones cannot be
// emitted until it is known whether something behind it carries into it, so
// buf holds the last byte and help counts the 0xff bytes waiting behind it.
type rangeEncoder struct {
	w frameWriter

	low  uint32
	rng  uint32
	help uint32
	buf  byte

	m *rangeModel
}

func newRangeEncoder() *rangeEncoder {
	return &rangeEncoder{m: modelCurrent()}
}

// start primes the range registers for a frame. Nothing is emitted here: the
// first normalize flushes the empty carry byte, which is the dummy byte the
// decoder skips.
func (e *rangeEncoder) start() {
	e.low, e.rng, e.help, e.buf = 0, topValue, 0, 0
}

// normalize emits bytes until the range register is wide again.
func (e *rangeEncoder) normalize() {
	for e.rng <= bottomValue {
		switch {
		case e.low < 0xff<<shiftBits:
			// No carry is possible any more: the held byte and the ones
			// waiting behind it are final.
			e.w.putByte(e.buf)
			for ; e.help > 0; e.help-- {
				e.w.putByte(0xff)
			}
			e.buf = byte(e.low >> shiftBits)
		case e.low&topValue != 0:
			// A carry: the held byte goes up by one and every 0xff behind it
			// wraps to zero.
			e.w.putByte(e.buf + 1)
			for ; e.help > 0; e.help-- {
				e.w.putByte(0)
			}
			e.buf = byte(e.low >> shiftBits)
		default:
			e.help++
		}
		e.low = e.low << 8 & (topValue - 1)
		e.rng <<= 8
	}
}

// encodeFast narrows the range onto one symbol of the overflow model.
func (e *rangeEncoder) encodeFast(sym uint32) {
	e.normalize()
	t := e.rng >> rangeOverflowShift
	e.rng = t * e.m.width[sym]
	e.low += t * e.m.total[sym]
}

// encodeDirect writes shift raw bits.
func (e *rangeEncoder) encodeDirect(v uint32, shift int) {
	e.normalize()
	e.rng >>= shift
	e.low += e.rng * v
}

// encodeDivided writes one value uniformly distributed over [0, n).
func (e *rangeEncoder) encodeDivided(v, n uint32) {
	e.normalize()
	t := e.rng / n
	e.rng = t
	e.low += t * v
}

// encodeValue writes one residual and folds its magnitude into the entropy
// state, which is the same fold the decoder applies on the way back.
func (e *rangeEncoder) encodeValue(v int32, s *entropyState) {
	// The coder works on magnitudes with the sign in the low bit. The
	// arithmetic is 64-bit because the most negative int32 doubles past the
	// type.
	value := -int64(v) * 2
	if v > 0 {
		value = int64(v)*2 - 1
	}

	pivot := max(s.kSum/32, 1)
	wide := uint64(value) / uint64(pivot)
	overflow := uint32(wide)
	if uint64(overflow) != wide {
		// The overflow count will not fit its field. Widen the pivot and say
		// so with a reserved count no ordinary value produces.
		pivot = overflowPivotValue
		overflow = uint32(uint64(value) / uint64(pivot))
		e.encodeFast(modelElements - 1)
		e.encodeDirect(overflowSignal>>16, 16)
		e.encodeDirect(overflowSignal&0xffff, 16)
	}
	base := uint32(value - int64(overflow)*int64(pivot))
	s.update(value)

	if overflow < modelElements-1 {
		e.encodeFast(overflow)
	} else {
		// Past the model's last symbol the count goes out as raw bits behind
		// the escape.
		e.encodeFast(modelElements - 1)
		e.encodeDirect(overflow>>16, 16)
		e.encodeDirect(overflow&0xffff, 16)
	}

	if pivot >= 1<<16 {
		// A pivot too wide to code in one go is split into a high part and a
		// power-of-two low part. The high part's range gets one added to it,
		// since dividing base and pivot by the same integer can make them
		// equal; maximizing the split factor is what keeps that cheap.
		bits := 0
		for pivot>>bits > 0 {
			bits++
		}
		shift := 0
		if bits >= 16 {
			shift = bits - 16
		}
		split := uint32(1) << shift
		e.encodeDivided(base/split, pivot/split+1)
		e.encodeDivided(base%split, split)
		return
	}
	e.encodeDivided(base, pivot)
}

// finalize flushes the range register. The three zero bytes at the end are not
// padding: the decoder reads a few bytes ahead of the values it has produced,
// so they are what its last value is decoded out of.
func (e *rangeEncoder) finalize() {
	e.normalize()
	t := (e.low >> shiftBits) + 1
	if t > 0xff {
		e.w.putByte(e.buf + 1)
		for ; e.help > 0; e.help-- {
			e.w.putByte(0)
		}
	} else {
		e.w.putByte(e.buf)
		for ; e.help > 0; e.help-- {
			e.w.putByte(0xff)
		}
	}
	e.w.putByte(byte(t))
	e.w.putByte(0)
	e.w.putByte(0)
	e.w.putByte(0)
}
