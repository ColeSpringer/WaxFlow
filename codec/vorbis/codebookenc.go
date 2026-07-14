package vorbis

import "math"

// The encode side of a codebook. It mirrors the decoder's codebook (codebook.go)
// but adds what emission needs: per-entry codewords ready for the LSB-first
// bitWriter, and, for VQ books, the nearest-entry search that turns a residue
// vector into an entry number. Its serialization (writeSetup below) is the exact
// inverse of parseCodebook, so a book built here parses back to the same tree.
type encBook struct {
	dimensions int
	entries    int

	// Emission: writeBits(codeLen[e], codeword[e]) emits entry e's Huffman
	// codeword. The decoder builds its tree from the SAME lengths via
	// assignCodewords, so the trees agree. codeword[e] is the entry's MSB-first
	// codeword bit-reversed into the low codeLen[e] bits, because the writer
	// packs LSB-first while the tree walks MSB-first.
	lengths  []uint8
	codeword []uint32
	codeLen  []uint8

	// Product-lattice VQ (lookupType 1, sequenceP false), for vectorEntry. Each
	// dimension shares one uniform lattice of lookupValues points: dimension k of
	// entry e reads lattice index (e / lookupValues^k) % lookupValues, whose value
	// is minimum + index*delta (the decoder's valueVector, since multiplicand[i]==i).
	// A dimension-1 book is the scalar case (lookupValues == entries).
	minimum      float64
	delta        float64
	lookupValues int
}

// bookSpec is the compact description a book is built from: the per-entry
// codeword lengths plus, for a VQ book, the scalar lattice. Keeping books as
// specs (not literal trees) lets books.go stay small and lets the offline
// generator emit specs.
type bookSpec struct {
	dimensions int
	lengths    []uint8 // len == entries; 0 marks an unused entry

	// VQ (lookupType 1). Left zero for a scalar book.
	lookupType   int
	minimum      float64
	delta        float64
	valueBits    int
	sequenceP    bool
	multiplicand []uint32 // lookup1Values(entries,dim) raw values
}

// buildEncBook realizes a spec: it assigns codewords, reverses them for the
// LSB-first writer. It carries the scalar lattice (minimum/delta) for the
// residue quantizer.
func buildEncBook(s bookSpec) *encBook {
	entries := len(s.lengths)
	lookup := entries // scalar default (dimension-1)
	if s.lookupType == 1 {
		lookup = lookup1Values(entries, s.dimensions)
	}
	b := &encBook{
		dimensions:   s.dimensions,
		entries:      entries,
		lengths:      s.lengths,
		codeword:     make([]uint32, entries),
		codeLen:      make([]uint8, entries),
		minimum:      s.minimum,
		delta:        s.delta,
		lookupValues: lookup,
	}
	codes, ok := assignCodewords(s.lengths)
	if !ok {
		panic("vorbis: over-subscribed encoder codebook")
	}
	for e := 0; e < entries; e++ {
		l := s.lengths[e]
		b.codeLen[e] = l
		b.codeword[e] = reverseCodeword(codes[e], l)
	}
	return b
}

// reverseCodeword bit-reverses the top l bits of an MSB-first codeword into the
// low l bits, the form writeBits emits so the decoder reads the codeword
// MSB-first.
func reverseCodeword(code uint32, l uint8) uint32 {
	var v uint32
	for b := 0; b < int(l); b++ {
		v |= ((code >> (31 - uint(b))) & 1) << uint(b)
	}
	return v
}

// emit writes entry e's codeword.
func (b *encBook) emit(w *bitWriter, e int) {
	w.writeBits(uint(b.codeLen[e]), b.codeword[e])
}

// latIndex returns the lattice index nearest v on this book's shared per-dimension
// uniform lattice. The lattice is a monotone ramp (value == minimum + index*delta),
// so the nearest point is a rounded index; the clamp keeps out-of-range residue on
// the lattice. This is the per-dimension half of the separable nearest-entry search.
func (b *encBook) latIndex(v float64) int {
	idx := int(math.Round((v - b.minimum) / b.delta))
	if idx < 0 {
		idx = 0
	}
	if idx >= b.lookupValues {
		idx = b.lookupValues - 1
	}
	return idx
}

// latValue reconstructs the lattice point at an index (the decoder's per-dimension
// value), so the cascade encoder can subtract a coarse pass's reconstruction before
// refining it.
func (b *encBook) latValue(idx int) float64 { return b.minimum + float64(idx)*b.delta }

// latIndexSigned is latIndex constrained never to quantize a nonzero value to a
// lattice point of the opposite sign (or to zero). It is used for the coupled
// magnitude channel's final cascade pass on a line the earlier passes left at
// zero: the decoder picks its decouple branch from sign(magnitude), so a small
// nonzero magnitude the whole cascade rounds into its zero dead-zone would flip
// the branch and invert the angle channel. Nudging that last pass to the nearest
// same-sign point costs at most one final-step of magnitude accuracy but keeps
// the branch correct; exact zero (a genuinely silent line) stays zero and takes
// the decoder's mag<=0 branch. Applying it only on the last pass (not pass 0) is
// what keeps the refinement cascade convergent: a pass-0 nudge to a same-sign
// coarse point lands a residual outside the refinement books' range, which they
// cannot walk back.
func (b *encBook) latIndexSigned(v float64) int {
	idx := b.latIndex(v)
	switch {
	case v > 0 && b.latValue(idx) <= 0:
		for idx < b.lookupValues-1 && b.latValue(idx) <= 0 {
			idx++
		}
	case v < 0 && b.latValue(idx) >= 0:
		for idx > 0 && b.latValue(idx) >= 0 {
			idx--
		}
	}
	return idx
}

// vectorEntry returns the product-lattice entry nearest the dimension-length vector
// vals: each dimension picks its nearest lattice index independently (the lattice is
// separable because sequenceP is false), and the indices compose low-dimension-first
// into the entry, exactly the base-lookupValues digit order the decoder's valueVector
// reads back. vals must have length b.dimensions. A non-nil sign selects, per
// dimension, latIndexSigned over latIndex where sign[k] holds (the last cascade pass
// of a coupled magnitude line the earlier passes left at zero; see latIndexSigned).
func (b *encBook) vectorEntry(vals []float64, sign []bool) int {
	entry, pow := 0, 1
	for k := 0; k < b.dimensions; k++ {
		idx := b.latIndex(vals[k])
		if sign != nil && sign[k] {
			idx = b.latIndexSigned(vals[k])
		}
		entry += idx * pow
		pow *= b.lookupValues
	}
	return entry
}

// writeCodebook serializes one codebook, the exact inverse of parseCodebook. It
// emits an unordered code, sparse only when the book has unused entries, and a
// lookupType-1 (or 0) lattice.
func writeCodebook(w *bitWriter, s bookSpec) {
	entries := len(s.lengths)
	w.writeBits(24, 0x564342) // sync pattern "BCV" little-endian
	w.writeBits(16, uint32(s.dimensions))
	w.writeBits(24, uint32(entries))

	sparse := false
	for _, l := range s.lengths {
		if l == 0 {
			sparse = true
			break
		}
	}
	w.writeBit(0) // not ordered
	if sparse {
		w.writeBit(1)
		for _, l := range s.lengths {
			if l == 0 {
				w.writeBit(0)
				continue
			}
			w.writeBit(1)
			w.writeBits(5, uint32(l-1))
		}
	} else {
		w.writeBit(0)
		for _, l := range s.lengths {
			w.writeBits(5, uint32(l-1))
		}
	}

	w.writeBits(4, uint32(s.lookupType))
	if s.lookupType == 0 {
		return
	}
	w.writeBits(32, float32Pack(s.minimum))
	w.writeBits(32, float32Pack(s.delta))
	w.writeBits(4, uint32(s.valueBits-1))
	w.writeBit(boolBit(s.sequenceP))
	for _, m := range s.multiplicand {
		w.writeBits(uint(s.valueBits), m)
	}
}

func boolBit(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

// float32Pack is the inverse of float32Unpack (codebook.go): it packs v into
// Vorbis's 21-bit-mantissa / 10-bit-exponent representation. The books' minimum
// and delta round-trip through it, so a book's decoded lattice matches what the
// encoder searched.
func float32Pack(v float64) uint32 {
	if v == 0 {
		return 0
	}
	var sign uint32
	if v < 0 {
		sign = 0x80000000
		v = -v
	}
	frac, exp2 := math.Frexp(v) // v = frac * 2^exp2, frac in [0.5, 1)
	m := uint32(math.Round(frac * (1 << 21)))
	e := exp2 + 767 // frac*2^21 * 2^(e-788) == frac*2^exp2  =>  e = exp2 + 767
	if m >= 1<<21 { // frac rounded up to 1.0
		m >>= 1
		e++
	}
	if e < 0 {
		return sign // underflow: smallest representable is ~0
	}
	if e > 0x3ff {
		e = 0x3ff
	}
	return sign | uint32(e)<<21 | (m & 0x1fffff)
}
