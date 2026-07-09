package mp3

// Forward Huffman coding for the encoder. The codeword tables are derived
// at init from the decoder's tree blob (huffTree) rather than carried as a
// second copy: a depth-first walk of each tree recovers every leaf's bit
// path, which is exactly the codeword the encoder must emit. This keeps
// one source of truth for the wire format and inherits the decode blob's
// clean-room provenance.

// hcode is one Huffman codeword: the low n bits of bits, MSB first.
type hcode struct {
	bits uint32
	n    uint8
}

// bigEnc[t] maps a big-values pair to its codeword, indexed by x<<4 | y
// (x, y each clamped to 0..15; values >= 15 spill into linbits). Tables 0,
// 4, and 14 stay empty (nil rows): the zero region needs no codeword and
// the reserved tables are never selected.
var bigEnc [32][]hcode

// bigMax[t] is the largest leaf value present in table t, i.e. the largest
// pair component it can code without linbits. Selection compares a region's
// peak against the table limit (bigMax, or 15+2^linbits-1 when linbits>0).
var bigMax [32]int

// cnt1Enc maps a count1 quad (v<<3 | w<<2 | x<<1 | y, each bit an absolute
// magnitude of 0 or 1) to its codeword, one row per count1 table (32, 33).
var cnt1Enc [2][16]hcode

func init() {
	for t := 1; t < 32; t++ {
		ht := &huffTables[t]
		if ht.treeLen == 0 {
			continue // reserved (4, 14) or the zero table copied at t==0
		}
		row := make([]hcode, 256)
		bigMax[t] = walkEncode(huffTree[ht.off:ht.off+ht.treeLen], func(val byte, c hcode) {
			row[val] = c
		})
		bigEnc[t] = row
	}
	for i, t := range [2]int{32, 33} {
		ht := &huffTables[t]
		walkEncode(huffTree[ht.off:ht.off+ht.treeLen], func(val byte, c hcode) {
			cnt1Enc[i][val] = c
		})
	}
}

// walkEncode depth-first walks a decode tree, calling emit with each leaf's
// value byte and the codeword (bit path) that reaches it, and returns the
// largest nibble seen across all leaves. The branch arithmetic mirrors
// walkTree exactly, including the >= 250 offset chaining, so encode and
// decode agree bit for bit.
func walkEncode(tree []uint16, emit func(val byte, c hcode)) int {
	maxLeaf := 0
	type frame struct {
		point int
		code  uint32
		n     uint8
	}
	stack := []frame{{0, 0, 0}}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		node := tree[f.point]
		if node&0xFF00 == 0 { // leaf
			val := byte(node & 0xFF)
			emit(val, hcode{bits: f.code, n: f.n})
			if hi := int(val >> 4); hi > maxLeaf {
				maxLeaf = hi
			}
			if lo := int(val & 0xF); lo > maxLeaf {
				maxLeaf = lo
			}
			continue
		}
		// Bit 0 follows the high-byte offset chain, bit 1 the low-byte one.
		p0 := f.point
		for tree[p0]>>8 >= 250 {
			p0 += int(tree[p0] >> 8)
		}
		p0 += int(tree[p0] >> 8)
		p1 := f.point
		for tree[p1]&0xFF >= 250 {
			p1 += int(tree[p1] & 0xFF)
		}
		p1 += int(tree[p1] & 0xFF)
		stack = append(stack,
			frame{p0, f.code << 1, f.n + 1},
			frame{p1, f.code<<1 | 1, f.n + 1})
	}
	return maxLeaf
}

// tableLimit is the largest absolute value table t can code (accounting
// for linbits escapes). 0 for the empty tables.
func tableLimit(t int) int {
	if t < 0 || t >= 32 || bigEnc[t] == nil {
		if t == 0 {
			return 0 // the zero table codes only zeros
		}
		return -1
	}
	if lb := huffTables[t].linbits; lb != 0 {
		return 15 + (1 << lb) - 1
	}
	return bigMax[t]
}

// bigCandidates lists every big-values table (used by tests that exercise
// all of them). Selection uses the pruned lists below.
var bigCandidates = [...]int{
	1, 2, 3, 5, 6, 7, 8, 9, 10, 11, 12, 13, 15,
	16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31,
}

// noLinbitsTables code values up to 15 with no escape; treeA (16-23) and
// treeB (24-31) each share a tree and differ only in linbits, ascending. For
// a region whose peak exceeds 15 the cheapest choice in each escape tree is
// the smallest linbits that still reaches the peak (more linbits only adds
// escape bits), so selection tries just those two.
var (
	noLinbitsTables = []int{1, 2, 3, 5, 6, 7, 8, 9, 10, 11, 12, 13, 15}
	treeA           = []int{16, 17, 18, 19, 20, 21, 22, 23}
	treeB           = []int{24, 25, 26, 27, 28, 29, 30, 31}
)

// pairBits returns the bit cost of coding the pair (ax, ay) of absolute
// values with table t, including linbits and sign bits, or -1 if the table
// cannot reach the values. It assumes t is a real (non-empty) table.
func pairBits(t, ax, ay int) int {
	lb := huffTables[t].linbits
	xi, yi := ax, ay
	extra := 0
	if lb != 0 {
		if ax >= 15 {
			xi = 15
			extra += lb
		}
		if ay >= 15 {
			yi = 15
			extra += lb
		}
	} else if ax > bigMax[t] || ay > bigMax[t] {
		return -1
	}
	c := bigEnc[t][xi<<4|yi]
	if c.n == 0 && !(xi == 0 && yi == 0) {
		return -1 // no codeword for this pair in this table
	}
	bits := int(c.n) + extra
	if ax != 0 {
		bits++
	}
	if ay != 0 {
		bits++
	}
	return bits
}

// writePair emits the pair (x, y) with table t: the codeword, any linbits
// escape magnitude, then a sign bit per nonzero component (0 positive). The
// zero table (and the reserved 4/14) code only zeros, so an all-zero region
// selected for table 0 writes nothing.
func (w *bitWriter) writePair(t, x, y int) {
	if bigEnc[t] == nil {
		return
	}
	lb := huffTables[t].linbits
	ax, ay := abs(x), abs(y)
	xi, yi := ax, ay
	if lb != 0 {
		if ax >= 15 {
			xi = 15
		}
		if ay >= 15 {
			yi = 15
		}
	}
	c := bigEnc[t][xi<<4|yi]
	w.writeBits(uint(c.n), c.bits)
	if lb != 0 && ax >= 15 {
		w.writeBits(uint(lb), uint32(ax-15))
	}
	if x != 0 {
		w.writeBits(1, boolBit(x < 0))
	}
	if lb != 0 && ay >= 15 {
		w.writeBits(uint(lb), uint32(ay-15))
	}
	if y != 0 {
		w.writeBits(1, boolBit(y < 0))
	}
}

// quadBits returns the bit cost of a count1 quad with table sel (0 or 1),
// codeword plus one sign bit per nonzero component.
func quadBits(sel int, v, w, x, y int) int {
	idx := abs1(v)<<3 | abs1(w)<<2 | abs1(x)<<1 | abs1(y)
	bits := int(cnt1Enc[sel][idx].n)
	for _, c := range [4]int{v, w, x, y} {
		if c != 0 {
			bits++
		}
	}
	return bits
}

// writeQuad emits a count1 quad with table sel: codeword then a sign bit
// per nonzero component.
func (bw *bitWriter) writeQuad(sel, v, w, x, y int) {
	idx := abs1(v)<<3 | abs1(w)<<2 | abs1(x)<<1 | abs1(y)
	c := cnt1Enc[sel][idx]
	bw.writeBits(uint(c.n), c.bits)
	for _, comp := range [4]int{v, w, x, y} {
		if comp != 0 {
			bw.writeBits(1, boolBit(comp < 0))
		}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// abs1 clamps a magnitude to 0 or 1 for count1 indexing.
func abs1(x int) int {
	if x != 0 {
		return 1
	}
	return 0
}

func boolBit(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}
