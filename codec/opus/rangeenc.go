package opus

// rangeEncoder is Opus's entropy encoder (RFC 6716 section 4.1), the inverse of
// rangeDecoder: range-coded symbols accumulate from the front of the packet and
// raw bits from the back. It is a clean-room port of libopus entenc.c; the
// arithmetic is bit-exact with the reference, so any correctly written stream
// decodes with rangeDecoder (and with libopus).
type rangeEncoder struct {
	buf     []byte
	storage int    // logical packet size; done() zero-fills the front/back gap
	offs    int    // next byte written from the front
	endOffs int    // bytes written from the back
	endWin  uint32 // raw-bit window (fills toward the back)
	nEnd    int    // valid bits in endWin
	nbits   int    // total bits emitted, for ec_tell
	rng     uint32
	val     uint32
	ext     uint32 // count of carry-buffered 0xFF symbols
	rem     int    // buffered byte awaiting carry propagation, -1 if none
	err     bool   // set once the buffer overflows; matches libopus's error flag
}

// ecCodeShift is the shift that lifts the high-order symbol out of val during
// normalization (libopus EC_CODE_SHIFT).
const ecCodeShift = ecCodeBits - ecSymBits - 1 // 23

// newRangeEncoder initializes an encoder over buf; buf's length is the packet
// size (done() pads the gap between the range data and the raw-bit tail).
func newRangeEncoder(buf []byte) *rangeEncoder {
	return &rangeEncoder{
		buf:     buf,
		storage: len(buf),
		nbits:   ecCodeBits + 1,
		rng:     ecCodeTop,
		rem:     -1,
	}
}

// The encoder's two-pass searches (the intra/inter coarse energy and the
// theta RDO) rewind and replay it through the four methods below, mirroring
// libopus's `ec_save = *ec` idiom. snapshot copies the struct by value: buf
// is deliberately the only reference-typed field, so its backing stays
// shared (keep it that way, or update these call sites). A bare restore
// mid-frame is sound on its own because everything encoded past the rewind
// point is re-encoded, or zero-filled by done(), before the packet
// finishes; reverting to a *finished* attempt after replaying additionally
// needs that attempt's bytes back, which tailBytes/restoreTail carry: the
// region [from.offs, from.storage) a replay can still change, covering both
// front-written symbols and the raw-bit tail.

func (e *rangeEncoder) snapshot() rangeEncoder { return *e }

func (e *rangeEncoder) restore(s *rangeEncoder) { *e = *s }

// tailBytes copies the mutable region as it stands now, sized by the rewind
// point `from`; dst is reused across calls and grown as needed.
func (e *rangeEncoder) tailBytes(from *rangeEncoder, dst []byte) []byte {
	return append(dst[:0], e.buf[from.offs:from.storage]...)
}

// restoreTail writes a tailBytes capture back over the mutable region.
func (e *rangeEncoder) restoreTail(from *rangeEncoder, saved []byte) {
	copy(e.buf[from.offs:from.storage], saved)
}

func (e *rangeEncoder) writeByte(v byte) {
	if e.offs+e.endOffs >= e.storage {
		e.err = true
		return
	}
	e.buf[e.offs] = v
	e.offs++
}

func (e *rangeEncoder) writeByteAtEnd(v byte) {
	if e.offs+e.endOffs >= e.storage {
		e.err = true
		return
	}
	e.endOffs++
	e.buf[e.storage-e.endOffs] = v
}

// carryOut emits a symbol with a deferred carry: a run of 0xFF symbols is
// buffered (in ext) until a non-0xFF settles whether the carry propagates
// (libopus ec_enc_carry_out).
func (e *rangeEncoder) carryOut(c int) {
	if c != ecSymMax {
		carry := c >> ecSymBits
		if e.rem >= 0 {
			e.writeByte(byte(e.rem + carry))
		}
		if e.ext > 0 {
			sym := byte((ecSymMax + carry) & ecSymMax)
			for {
				e.writeByte(sym)
				e.ext--
				if e.ext == 0 {
					break
				}
			}
		}
		e.rem = c & ecSymMax
	} else {
		e.ext++
	}
}

func (e *rangeEncoder) normalize() {
	for e.rng <= ecCodeBot {
		e.carryOut(int(e.val >> ecCodeShift))
		e.val = (e.val << ecSymBits) & (ecCodeTop - 1)
		e.rng <<= ecSymBits
		e.nbits += ecSymBits
	}
}

// encode codes a symbol with cumulative range [fl, fh) of total ft (RFC 6716
// 4.1.2), the inverse of rangeDecoder.decode+update.
func (e *rangeEncoder) encode(fl, fh, ft uint32) {
	r := e.rng / ft
	if fl > 0 {
		e.val += e.rng - r*(ft-fl)
		e.rng = r * (fh - fl)
	} else {
		e.rng -= r * (ft - fh)
	}
	e.normalize()
}

// encodeBin is encode with ft == 1<<bits (RFC 6716 4.1.3.1).
func (e *rangeEncoder) encodeBin(fl, fh uint32, bits uint) {
	r := e.rng >> bits
	if fl > 0 {
		e.val += e.rng - r*((1<<bits)-fl)
		e.rng = r * (fh - fl)
	} else {
		e.rng -= r * ((1 << bits) - fh)
	}
	e.normalize()
}

// encodeBitLogp codes one bit whose probability of being one is 2^-logp
// (RFC 6716 4.1.3.2).
func (e *rangeEncoder) encodeBitLogp(val int, logp uint) {
	r := e.rng
	l := e.val
	s := r >> logp
	r -= s
	if val != 0 {
		e.val = l + r
		e.rng = s
	} else {
		e.rng = r
	}
	e.normalize()
}

// encodeICDF codes symbol s from an inverse cumulative distribution scaled to
// 2^ftb (RFC 6716 4.1.3.3). icdf is non-increasing and ends in 0.
func (e *rangeEncoder) encodeICDF(s int, icdf []byte, ftb uint) {
	r := e.rng >> ftb
	if s > 0 {
		e.val += e.rng - r*uint32(icdf[s-1])
		e.rng = r * uint32(icdf[s-1]-icdf[s])
	} else {
		e.rng -= r * uint32(icdf[s])
	}
	e.normalize()
}

// encodeUint codes a uniformly distributed integer fl in [0, ft) (RFC 6716
// 4.1.5). ft must be at least 2.
func (e *rangeEncoder) encodeUint(fl, ft uint32) {
	ft--
	ftb := ilog(ft)
	if ftb > ecUintBits {
		ftb -= ecUintBits
		t := (ft >> uint(ftb)) + 1
		e.encode(fl>>uint(ftb), (fl>>uint(ftb))+1, t)
		e.encodeRawBits(fl&((1<<uint(ftb))-1), uint(ftb))
	} else {
		e.encode(fl, fl+1, ft+1)
	}
}

// encodeRawBits writes bits raw toward the back of the packet (RFC 6716 4.1.4),
// the inverse of rangeDecoder.decodeRawBits.
func (e *rangeEncoder) encodeRawBits(val uint32, bits uint) {
	window := e.endWin
	used := e.nEnd
	if used+int(bits) > ecWindow {
		for {
			e.writeByteAtEnd(byte(window & ecSymMax))
			window >>= ecSymBits
			used -= ecSymBits
			if used < ecSymBits {
				break
			}
		}
	}
	window |= val << uint(used)
	used += int(bits)
	e.endWin = window
	e.nEnd = used
	e.nbits += int(bits)
}

// patchInitialBits overwrites the first nbits already-written bits with val
// (libopus ec_enc_patch_initial_bits); CELT uses it to backfill the silence
// flag once the frame's true content is known.
func (e *rangeEncoder) patchInitialBits(val uint32, nbits uint) {
	shift := ecSymBits - int(nbits)
	mask := uint32((1<<nbits)-1) << uint(shift)
	switch {
	case e.offs > 0:
		e.buf[0] = byte((uint32(e.buf[0]) &^ mask) | val<<uint(shift))
	case e.rem >= 0:
		e.rem = int((uint32(e.rem) &^ mask) | val<<uint(shift))
	case e.rng <= ecCodeTop>>nbits:
		e.val = (e.val &^ (mask << ecCodeShift)) | val<<uint(ecCodeShift+shift)
	default:
		e.err = true
	}
}

// tell returns the number of whole bits emitted so far (RFC 6716 4.1.6).
func (e *rangeEncoder) tell() int { return e.nbits - ilog(e.rng) }

// tellFrac returns bits emitted in eighth-bit units (RFC 6716 4.1.6).
func (e *rangeEncoder) tellFrac() int {
	return ecTellFrac(e.nbits, e.rng)
}

// shrink reduces the logical packet size to size bytes, relocating the raw-bit
// tail already written from the back so it stays flush against the new end
// (libopus ec_enc_shrink). Used by VBR rate control after the frame's size is
// decided. size must leave room for the bytes already emitted from both ends;
// the copy handles the (forward) overlap like memmove.
func (e *rangeEncoder) shrink(size int) {
	copy(e.buf[size-e.endOffs:size], e.buf[e.storage-e.endOffs:e.storage])
	e.storage = size
}

// done finalizes the stream: it emits the minimum bits that keep every encoded
// symbol decodable regardless of what follows, flushes the carry buffer and the
// raw-bit tail, and zero-fills the gap so the whole storage is a valid packet
// (libopus ec_enc_done).
func (e *rangeEncoder) done() {
	l := ecCodeBits - ilog(e.rng)
	msk := uint32(ecCodeTop-1) >> uint(l)
	end := (e.val + msk) &^ msk
	if end|msk >= e.val+e.rng {
		l++
		msk >>= 1
		end = (e.val + msk) &^ msk
	}
	for l > 0 {
		e.carryOut(int(end >> ecCodeShift))
		end = (end << ecSymBits) & (ecCodeTop - 1)
		l -= ecSymBits
	}
	if e.rem >= 0 || e.ext > 0 {
		e.carryOut(0)
	}
	window := e.endWin
	used := e.nEnd
	for used >= ecSymBits {
		e.writeByteAtEnd(byte(window & ecSymMax))
		window >>= ecSymBits
		used -= ecSymBits
	}
	if !e.err {
		for i := e.offs; i < e.storage-e.endOffs; i++ {
			e.buf[i] = 0
		}
		if used > 0 {
			if e.endOffs >= e.storage {
				e.err = true
			} else {
				l = -l
				if e.offs+e.endOffs >= e.storage && l < used {
					window &= (1 << uint(l)) - 1
					e.err = true
				}
				e.buf[e.storage-e.endOffs-1] |= byte(window)
			}
		}
	}
}

// payload returns the finished packet bytes. Call after done().
func (e *rangeEncoder) payload() []byte { return e.buf[:e.storage] }
