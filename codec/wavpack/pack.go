package wavpack

import (
	"math/bits"
)

// The encoding half of the block codec: the forward decorrelation cascade,
// the entropy coder's writer, and the quantizers that hand one block's state
// to the decoder.
//
// Every routine here is the exact inverse of one in unpack.go or words.go, and
// that is the whole correctness argument: the decoder is verified bit-exact
// against the reference on the official suite, so an encoder built as its
// mirror produces streams the reference reads. Where a decoder path has two
// spellings that agree only in infinite precision (applyWeight's narrow and
// wide forms, term 18's mono and stereo shapes), the mirror keeps the same
// one, because the pair has to wrap identically for the round trip to close.
//
// Block state carries across blocks. It could be reset at every boundary and
// the stream would still decode, since each block writes what it needs, but
// the medians take a few hundred samples to climb from zero and the weights
// as long to converge: paying that transient once per stream instead of once
// per block is worth the quantization step below.

// storeWeight is the inverse of restoreWeight: a decorrelation weight narrowed
// to the signed byte the block carries. The pre-bias on positive weights
// undoes the rounding restoreWeight adds back, so the pair is idempotent.
func storeWeight(w int32) int8 {
	switch {
	case w > 1024:
		w = 1024
	case w < -1024:
		w = -1024
	}
	if w > 0 {
		w -= (w + 64) >> 7
	}
	return int8((w + 4) >> 3)
}

// log2Inv inverts exp2Table's mantissa: for a mantissa byte, the largest index
// whose entry is at or below it. The reference carries a hand-written second
// table for this direction; deriving ours from the exponential at init costs
// nothing and needs no argument that the two round alike, since there is only
// one table.
var log2Inv = func() [256]byte {
	var inv [256]byte
	i := 0
	for t := range inv {
		// exp2Table is non-decreasing and starts at zero, so the largest
		// index at or below t only ever moves forward.
		for i+1 < len(exp2Table) && int(exp2Table[i+1]) <= t {
			i++
		}
		inv[t] = byte(i)
	}
	return inv
}()

// log2u is the inverse of exp2s for a non-negative value: the stored log form
// whose exp2s is the largest representable value at or below a.
//
// Nothing requires this to be the reference's rounding: the encoder adopts
// exp2s(log2u(a)) as its own state and the decoder reads exp2s of the same
// stored number, so the two stay in step whichever representable value was
// picked.
func log2u(a uint32) int {
	if a == 0 {
		return 0
	}
	// The exponent is the value's bit length, since exp2s reads its nine-bit
	// mantissa as 2^8..2^9-1 and shifts by e-9.
	e := bits.Len32(a)
	var frac uint32
	if e >= 9 {
		frac = a >> uint(e-9)
	} else {
		frac = a << uint(9-e)
	}
	return e<<8 | int(log2Inv[byte(frac-0x100)])
}

// log2s is log2u with a sign, the form the decorrelation history is stored in.
func log2s(v int32) int {
	if v < 0 {
		// Negating the smallest int32 overflows; one off the rail costs a
		// sample of prediction accuracy at a magnitude no real signal reaches.
		if v == -1<<31 {
			v++
		}
		return -log2u(uint32(-v))
	}
	return log2u(uint32(v))
}

// quantizeSample rounds a decorrelation history value to what the block's
// stored log form decodes to, and returns both. The encoder adopts the
// rounded value so its cascade runs on exactly what the decoder's will.
func quantizeSample(v int32) (int32, int) {
	log := log2s(v)
	return exp2s(log), log
}

// bitWriter appends to a WavPack sub-block bitstream, least significant bit
// first: the mirror of bitReader, including the padding to a whole number of
// 16-bit words that the sub-block framing needs.
type bitWriter struct {
	buf []byte
	sr  uint64 // shift register, next bit in the low position
	bc  uint   // valid bits in sr
}

// reset points the writer at buf, whose contents stand as the payload's
// prefix: an empty slice starts a bare bitstream, and a short one reserves
// room for a header the payload carries in front of its bits.
func (b *bitWriter) reset(buf []byte) {
	b.buf, b.sr, b.bc = buf, 0, 0
}

// drain moves whole bytes out of the shift register, leaving under eight bits.
func (b *bitWriter) drain() {
	for b.bc >= 8 {
		b.buf = append(b.buf, byte(b.sr))
		b.sr >>= 8
		b.bc -= 8
	}
}

func (b *bitWriter) putBit(v uint32) {
	b.sr |= uint64(v&1) << b.bc
	b.bc++
	if b.bc >= 32 {
		b.drain()
	}
}

// putBits writes the low n bits of v, n <= 32. The register holds at most 31
// bits on entry (putBit drains at 32, putBits always drains), so 63 bits is
// the high-water mark and nothing is lost.
func (b *bitWriter) putBits(v uint32, n int) {
	if n <= 0 {
		return
	}
	if n < 32 {
		v &= 1<<uint(n) - 1
	}
	b.sr |= uint64(v) << b.bc
	b.bc += uint(n)
	b.drain()
}

// flush pads the pending bits with zeros and returns the sub-block payload,
// an even number of bytes. Padding is safe because the decoder reads exactly
// as many bits as were written: it stops when it has the block's samples.
func (b *bitWriter) flush() []byte {
	b.drain()
	if b.bc > 0 {
		b.buf = append(b.buf, byte(b.sr))
		b.sr, b.bc = 0, 0
	}
	if len(b.buf)&1 != 0 {
		b.buf = append(b.buf, 0)
	}
	return b.buf
}

// putCode writes code in 0..maxcode using the same split readCode reads: the
// low codes take one bit less than the high ones. The low n-1 bits go first
// and the top bit last, which is the order readCode's single fill unpacks.
func (b *bitWriter) putCode(code, maxcode uint32) {
	if maxcode < 2 {
		if maxcode == 1 {
			b.putBit(code)
		}
		return
	}
	n := bits.Len32(maxcode)
	extras := uint32(uint64(1)<<uint(n) - uint64(maxcode) - 1)
	if code < extras {
		b.putBits(code, n-1)
		return
	}
	v := code + extras
	b.putBits(v>>1, n-1)
	b.putBit(v & 1)
}

// putElias writes the escaped count form readElias reads: a unary length,
// then the bits below the implicit leading one.
func (b *bitWriter) putElias(v uint32) {
	if v < 2 {
		// One and zero are the two lengths with no payload at all.
		b.putBits(1<<uint(v)-1, int(v))
		b.putBit(0)
		return
	}
	cbits := bits.Len32(v)
	for range cbits {
		b.putBit(1)
	}
	b.putBit(0)
	b.putBits(v&(1<<uint(cbits-1)-1), cbits-1)
}

// putOnes writes a zone count: unary up to the escape threshold, then an
// Elias field for the remainder. At exactly limitOnes the decoder still
// consumes the terminating zero before the escape, so it is written either
// way.
func (b *bitWriter) putOnes(u uint32) {
	if u < limitOnes {
		for range u {
			b.putBit(1)
		}
		b.putBit(0)
		return
	}
	for range limitOnes {
		b.putBit(1)
	}
	b.putBit(0)
	b.putElias(u - limitOnes)
}

// magSign splits a sample into the magnitude the code space carries and the
// trailing sign bit, the inverse of signed.
func magSign(v int32) (mag, sign uint32) {
	if v < 0 {
		return uint32(^v), 1
	}
	return uint32(v), 0
}

// zone places a magnitude in the median-split code space, returning the zone
// count and the code range, and advances the medians exactly as the decoder's
// switch does. The reads all precede the updates for the same reason they do
// there: the range is the one the medians described before this sample moved
// them.
func (c *entropyChan) zone(mag uint32) (ones, low, high uint32) {
	m0 := c.med(0)
	if mag < m0 {
		c.decMed0()
		return 0, 0, m0 - 1
	}
	low = m0
	c.incMed0()
	m1 := c.med(1)
	if mag-low < m1 {
		c.decMed1()
		return 1, low, low + m1 - 1
	}
	low += m1
	c.incMed1()
	m2 := c.med(2)
	if mag-low < m2 {
		c.decMed2()
		return 2, low, low + m2 - 1
	}
	ones = 2 + (mag-low)/m2
	low += (ones - 2) * m2
	c.incMed2()
	return ones, low, low + m2 - 1
}

// wordEncoder is the entropy coder's writer state: the same medians the
// decoder keeps, plus the shared unary carry.
type wordEncoder struct {
	c           [2]entropyChan
	holdingOne  bool
	holdingZero bool
}

// putWords encodes n frames from buf, interleaved for stereo.
//
// The one subtlety is that the unary zone count is shared between adjacent
// samples: its low bit is the next sample's carry, so an even count promises
// the next sample lands in zone zero and codes with no count of its own. That
// leaves the encoder no freedom at all -- the count is even exactly when the
// next sample's magnitude is below its channel's first median -- but it does
// mean one sample of lookahead, taken against the medians as they stand after
// the current sample moves them.
func (w *wordEncoder) putWords(bw *bitWriter, buf []int32, n int, mono bool) {
	total := n
	if !mono {
		total *= 2
	}
	chanOf := func(i int) *entropyChan {
		if mono {
			return &w.c[0]
		}
		return &w.c[i&1]
	}
	for i := 0; i < total; {
		c := chanOf(i)
		if w.holdingZero {
			// The previous count promised zone zero, so this sample carries
			// its code and sign and nothing else.
			w.holdingZero = false
			mag, sign := magSign(buf[i])
			bw.putCode(mag, c.med(0)-1)
			c.decMed0()
			bw.putBit(sign)
			i++
			continue
		}
		if w.c[0].median[0] < 2 && !w.holdingOne && w.c[1].median[0] < 2 {
			// Collapsed medians on both channels mean silence, coded as a run
			// length. A zero run is written even when it is empty: the
			// decoder reads the field unconditionally in this state.
			run := uint32(0)
			for i+int(run) < total && buf[i+int(run)] == 0 {
				run++
			}
			bw.putElias(run)
			if run != 0 {
				w.c[0].median = [3]uint32{}
				w.c[1].median = [3]uint32{}
				i += int(run)
				if i == total {
					break
				}
				c = chanOf(i)
			}
		}
		mag, sign := magSign(buf[i])
		var carry uint32
		if w.holdingOne {
			carry = 1
		}
		ones, low, high := c.zone(mag)
		// An odd count hands the next sample a carry of one, which forces it
		// out of zone zero; an even one forces it into zone zero. Which of
		// those is true is a fact about the next sample, not a choice.
		next := uint32(1)
		if i+1 == total {
			next = 0 // nothing follows; the cheaper count ends the block
		} else if nc := chanOf(i + 1); magOf(buf[i+1]) < nc.med(0) {
			next = 0
		}
		bw.putOnes(2*(ones-carry) + next)
		w.holdingOne = next != 0
		w.holdingZero = !w.holdingOne
		bw.putCode(mag-low, high-low)
		bw.putBit(sign)
		i++
	}
}

// magOf is magSign's magnitude alone, for the lookahead that only needs to
// know which side of a median the next sample falls on.
func magOf(v int32) uint32 {
	if v < 0 {
		return uint32(^v)
	}
	return uint32(v)
}

// packMonoPass runs one forward decorrelation pass over a mono block, in
// place: each sample becomes the residual its decoder counterpart adds back.
func packMonoPass(dpp *decorrPass, buf []int32) {
	delta, weight := dpp.delta, dpp.weightA
	switch dpp.term {
	case 17:
		for i, v := range buf {
			sam := 2*dpp.samplesA[0] - dpp.samplesA[1]
			dpp.samplesA[1] = dpp.samplesA[0]
			dpp.samplesA[0] = v
			res := v - applyWeight(weight, sam)
			updateWeight(&weight, delta, sam, res)
			buf[i] = res
		}
	case 18:
		for i, v := range buf {
			sam := (3*dpp.samplesA[0] - dpp.samplesA[1]) >> 1
			dpp.samplesA[1] = dpp.samplesA[0]
			dpp.samplesA[0] = v
			res := v - applyWeight(weight, sam)
			updateWeight(&weight, delta, sam, res)
			buf[i] = res
		}
	default:
		m, k := 0, dpp.term&(maxTerm-1)
		for i, v := range buf {
			sam := dpp.samplesA[m]
			dpp.samplesA[k] = v
			res := v - applyWeight(weight, sam)
			updateWeight(&weight, delta, sam, res)
			buf[i] = res
			m = (m + 1) & (maxTerm - 1)
			k = (k + 1) & (maxTerm - 1)
		}
	}
	dpp.weightA = weight
	dpp.normalize(len(buf))
}

// packStereoPass runs one forward decorrelation pass over an interleaved
// stereo block, in place.
func packStereoPass(dpp *decorrPass, buf []int32) {
	switch dpp.term {
	case 17:
		for i := 0; i < len(buf); i += 2 {
			sam := 2*dpp.samplesA[0] - dpp.samplesA[1]
			dpp.samplesA[1] = dpp.samplesA[0]
			dpp.samplesA[0] = buf[i]
			res := buf[i] - applyWeight(dpp.weightA, sam)
			updateWeight(&dpp.weightA, dpp.delta, sam, res)
			buf[i] = res

			sam = 2*dpp.samplesB[0] - dpp.samplesB[1]
			dpp.samplesB[1] = dpp.samplesB[0]
			dpp.samplesB[0] = buf[i+1]
			res = buf[i+1] - applyWeight(dpp.weightB, sam)
			updateWeight(&dpp.weightB, dpp.delta, sam, res)
			buf[i+1] = res
		}
	case 18:
		for i := 0; i < len(buf); i += 2 {
			sam := dpp.samplesA[0] + ((dpp.samplesA[0] - dpp.samplesA[1]) >> 1)
			dpp.samplesA[1] = dpp.samplesA[0]
			dpp.samplesA[0] = buf[i]
			res := buf[i] - applyWeight(dpp.weightA, sam)
			updateWeight(&dpp.weightA, dpp.delta, sam, res)
			buf[i] = res

			sam = dpp.samplesB[0] + ((dpp.samplesB[0] - dpp.samplesB[1]) >> 1)
			dpp.samplesB[1] = dpp.samplesB[0]
			dpp.samplesB[0] = buf[i+1]
			res = buf[i+1] - applyWeight(dpp.weightB, sam)
			updateWeight(&dpp.weightB, dpp.delta, sam, res)
			buf[i+1] = res
		}
	case -1:
		for i := 0; i < len(buf); i += 2 {
			l, r := buf[i], buf[i+1]
			resL := l - applyWeight(dpp.weightA, dpp.samplesA[0])
			updateWeightClip(&dpp.weightA, dpp.delta, dpp.samplesA[0], resL)
			resR := r - applyWeight(dpp.weightB, l)
			updateWeightClip(&dpp.weightB, dpp.delta, l, resR)
			dpp.samplesA[0] = r
			buf[i], buf[i+1] = resL, resR
		}
	case -2:
		for i := 0; i < len(buf); i += 2 {
			l, r := buf[i], buf[i+1]
			resR := r - applyWeight(dpp.weightB, dpp.samplesB[0])
			updateWeightClip(&dpp.weightB, dpp.delta, dpp.samplesB[0], resR)
			resL := l - applyWeight(dpp.weightA, r)
			updateWeightClip(&dpp.weightA, dpp.delta, r, resL)
			dpp.samplesB[0] = l
			buf[i], buf[i+1] = resL, resR
		}
	case -3:
		for i := 0; i < len(buf); i += 2 {
			l, r := buf[i], buf[i+1]
			resL := l - applyWeight(dpp.weightA, dpp.samplesA[0])
			updateWeightClip(&dpp.weightA, dpp.delta, dpp.samplesA[0], resL)
			resR := r - applyWeight(dpp.weightB, dpp.samplesB[0])
			updateWeightClip(&dpp.weightB, dpp.delta, dpp.samplesB[0], resR)
			dpp.samplesB[0], dpp.samplesA[0] = l, r
			buf[i], buf[i+1] = resL, resR
		}
	default:
		m, k := 0, dpp.term&(maxTerm-1)
		for i := 0; i < len(buf); i += 2 {
			sam := dpp.samplesA[m]
			dpp.samplesA[k] = buf[i]
			res := buf[i] - applyWeight(dpp.weightA, sam)
			updateWeight(&dpp.weightA, dpp.delta, sam, res)
			buf[i] = res

			sam = dpp.samplesB[m]
			dpp.samplesB[k] = buf[i+1]
			res = buf[i+1] - applyWeight(dpp.weightB, sam)
			updateWeight(&dpp.weightB, dpp.delta, sam, res)
			buf[i+1] = res

			m = (m + 1) & (maxTerm - 1)
			k = (k + 1) & (maxTerm - 1)
		}
	}
	dpp.normalize(len(buf) / 2)
}

// normalize rotates a lag pass's sample history back to canonical order after
// n frames. The cascade indexes history by position modulo maxTerm, so the
// ring ends a block rotated; the decoder starts every block reading from index
// zero, so what the block stores has to be unrotated. The reference does the
// same thing for the same reason, one block at a time.
func (dpp *decorrPass) normalize(n int) {
	if dpp.term <= 0 || dpp.term > maxTerm {
		return
	}
	var a, b [maxTerm]int32
	for k := range maxTerm {
		a[k] = dpp.samplesA[(k+n)&(maxTerm-1)]
		b[k] = dpp.samplesB[(k+n)&(maxTerm-1)]
	}
	dpp.samplesA, dpp.samplesB = a, b
}
