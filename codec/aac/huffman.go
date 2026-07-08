package aac

// Huffman decode: my own logic (ISO 14496-3 9.3), consuming the codeword
// and length data from the tables_hcb.go parameter artifact. Each book is
// built into a binary tree stored flat in an int32 array (two entries per
// node): decode walks the tree one bit at a time by array index, with no
// per-bit hashing. A slot holds a child node index (> 0), a leaf value
// (negative, encoding the codebook index), or 0 for an unreached path.

type huffBook struct {
	tree []int32 // node k occupies tree[2k] (bit 0) and tree[2k+1] (bit 1)
}

var (
	spectralBooks   [11]huffBook
	scalefactorBook huffBook
)

func init() {
	for cb := 0; cb < 11; cb++ {
		codes := make([]uint32, len(spectralCodes[cb]))
		for i, c := range spectralCodes[cb] {
			codes[i] = uint32(c)
		}
		spectralBooks[cb] = buildBook(codes, spectralBits[cb])
	}
	scalefactorBook = buildBook(scalefactorCodes[:], scalefactorBits[:])
}

// buildBook inserts each (codeword, length) pair into the tree, MSB first.
// Node 0 is the root; since codewords are prefix-free and never empty, no
// slot legitimately points back at node 0, so 0 marks an unreached path.
func buildBook(codes []uint32, bits []uint8) huffBook {
	tree := make([]int32, 2) // root node
	for i := range codes {
		cw, l := codes[i], int(bits[i])
		node := 0
		for b := l - 1; b >= 0; b-- {
			slot := 2*node + int(cw>>uint(b)&1)
			if b == 0 {
				tree[slot] = -(int32(i) + 1) // leaf: encodes index i
				break
			}
			if tree[slot] == 0 {
				tree[slot] = int32(len(tree) / 2)
				tree = append(tree, 0, 0)
			}
			node = int(tree[slot])
		}
	}
	return huffBook{tree}
}

// decode walks the tree bit by bit, returning the leaf's index or false on
// an unreached path (a damaged bitstream). The walk is bounded by the tree
// depth (the book's longest codeword).
func (b *huffBook) decode(r *bitReader) (int, bool) {
	node := 0
	for {
		v := b.tree[2*node+int(r.bit())]
		switch {
		case v < 0:
			return int(-v - 1), true
		case v == 0:
			return 0, false
		default:
			node = int(v)
		}
	}
}

// decodeSpectral reads one codebook tuple into out[:dim], applying the
// index-to-value decomposition, sign bits (unsigned books), and the escape
// sequence (codebook 11).
func decodeSpectral(r *bitReader, cb int, out []int) bool {
	idx, ok := spectralBooks[cb-1].decode(r)
	if !ok {
		return false
	}
	dim := hcbDim[cb-1]
	mod := hcbMod[cb-1]
	for d := dim - 1; d >= 0; d-- {
		out[d] = idx % mod
		idx /= mod
	}
	if !hcbUnsigned[cb-1] {
		off := hcbOff[cb-1]
		for d := 0; d < dim; d++ {
			out[d] -= off // signed value, no sign bit
		}
		return true
	}
	// Unsigned books: the sign bits for every nonzero magnitude come first
	// (in order), then the escape words for magnitude 16. Reading them in
	// that order keeps the variable-length escape prefix bit-aligned.
	var neg [4]bool
	for d := 0; d < dim; d++ {
		if out[d] != 0 {
			neg[d] = r.bit() != 0
		}
	}
	if cb == escHCB {
		for d := 0; d < dim; d++ {
			if out[d] == 16 {
				out[d] = decodeEscape(r)
			}
		}
	}
	for d := 0; d < dim; d++ {
		if neg[d] {
			out[d] = -out[d]
		}
	}
	return true
}

// decodeEscape reads codebook 11's escape word: a run of N ones, a zero,
// then N+4 magnitude bits, giving 2^(N+4) + word.
func decodeEscape(r *bitReader) int {
	n := 0
	for r.bit() == 1 {
		n++
		if n > 24 { // hostile input guard; real words stay small
			break
		}
	}
	return (1 << uint(n+4)) + int(r.read(uint(n+4)))
}

// decodeScalefactor reads one DPCM scalefactor delta in [-60, 60].
func decodeScalefactor(r *bitReader) (int, bool) {
	idx, ok := scalefactorBook.decode(r)
	if !ok {
		return 0, false
	}
	return idx - 60, true
}
