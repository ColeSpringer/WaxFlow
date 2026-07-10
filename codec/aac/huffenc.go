package aac

// Forward Huffman coding: the encoder consumes the same tables_hcb.go
// codeword/length data the decoder builds its trees from, so encode and
// decode share one wire-format source. Values arrive as raw quantized
// integers; each book's index decomposition (hcbDim/Mod/Off), sign bits,
// and codebook 11's escape words mirror decodeSpectral exactly.

// specBookMax is the largest magnitude each spectral codebook expresses
// directly, indexed [cb-1]. Codebook 11 escapes past 16.
var specBookMax = [11]int{1, 1, 2, 2, 4, 4, 7, 7, 12, 12, 16}

// escMaxValue caps codebook-11 escape magnitudes: the largest value the
// quantizer may produce (13-bit escape word ceiling).
const escMaxValue = 8191

// specTupleBits returns the bit cost of one tuple under codebook cb
// (1..11), or -1 when the values exceed the book's range. v must hold
// exactly hcbDim[cb-1] raw signed values.
func specTupleBits(cb int, v []int) int {
	dim, mod, off := hcbDim[cb-1], hcbMod[cb-1], hcbOff[cb-1]
	idx := 0
	extra := 0
	if hcbUnsigned[cb-1] {
		for d := 0; d < dim; d++ {
			m := v[d]
			if m < 0 {
				m = -m
			}
			if m != 0 {
				extra++ // sign bit
			}
			if cb == escHCB {
				if m > escMaxValue {
					return -1
				}
				if m >= 16 {
					extra += escBits(m)
					m = 16
				}
			} else if m > specBookMax[cb-1] {
				return -1
			}
			idx = idx*mod + m
		}
	} else {
		for d := 0; d < dim; d++ {
			s := v[d] + off
			if s < 0 || s >= mod {
				return -1
			}
			idx = idx*mod + s
		}
	}
	return int(spectralBits[cb-1][idx]) + extra
}

// escBits is the escape word cost for a magnitude >= 16: N ones, a zero,
// then N+4 bits, where 2^(N+4) <= m < 2^(N+5).
func escBits(m int) int {
	n := 0
	for m >= 1<<uint(n+5) {
		n++
	}
	return n + 1 + n + 4
}

// writeSpecTuple appends one tuple's codeword, sign bits, and escape
// words in the order decodeSpectral consumes them. The values must fit
// the book (cost checked beforehand via specTupleBits).
func (w *bitWriter) writeSpecTuple(cb int, v []int) {
	dim, mod, off := hcbDim[cb-1], hcbMod[cb-1], hcbOff[cb-1]
	idx := 0
	var mag [4]int
	if hcbUnsigned[cb-1] {
		for d := 0; d < dim; d++ {
			m := v[d]
			if m < 0 {
				m = -m
			}
			mag[d] = m
			c := m
			if cb == escHCB && c >= 16 {
				c = 16
			}
			idx = idx*mod + c
		}
	} else {
		for d := 0; d < dim; d++ {
			idx = idx*mod + v[d] + off
		}
	}
	w.writeBits(uint(spectralBits[cb-1][idx]), uint64(spectralCodes[cb-1][idx]))
	if !hcbUnsigned[cb-1] {
		return
	}
	for d := 0; d < dim; d++ {
		if mag[d] != 0 {
			s := uint64(0)
			if v[d] < 0 {
				s = 1
			}
			w.writeBits(1, s)
		}
	}
	if cb == escHCB {
		for d := 0; d < dim; d++ {
			if mag[d] >= 16 {
				n := 0
				for mag[d] >= 1<<uint(n+5) {
					n++
				}
				w.writeBits(uint(n), 1<<uint(n)-1) // n ones
				w.writeBits(1, 0)
				w.writeBits(uint(n+4), uint64(mag[d]-1<<uint(n+4)))
			}
		}
	}
}

// specRunBits sums the tuple costs of coding q under cb, or -1 when any
// value exceeds the book. len(q) must be a multiple of the book's dim
// (scalefactor-band widths are multiples of four, so runs always align).
func specRunBits(cb int, q []int) int {
	dim := hcbDim[cb-1]
	total := 0
	for i := 0; i+dim <= len(q); i += dim {
		b := specTupleBits(cb, q[i:i+dim])
		if b < 0 {
			return -1
		}
		total += b
	}
	return total
}

// writeSpecRun writes q's tuples under cb.
func (w *bitWriter) writeSpecRun(cb int, q []int) {
	dim := hcbDim[cb-1]
	for i := 0; i+dim <= len(q); i += dim {
		w.writeSpecTuple(cb, q[i:i+dim])
	}
}

// sfDeltaBits is the scalefactor DPCM cost for a delta in [-60, 60].
func sfDeltaBits(delta int) int { return int(scalefactorBits[delta+60]) }

// writeSFDelta writes one scalefactor DPCM codeword.
func (w *bitWriter) writeSFDelta(delta int) {
	w.writeBits(uint(scalefactorBits[delta+60]), uint64(scalefactorCodes[delta+60]))
}
