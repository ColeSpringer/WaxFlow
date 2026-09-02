package musepack

// huffEntry is one row of a reference Huffman table as huffman.c lists them:
// the 16-bit code the row starts at, the codeword length, and either the
// symbol (the SV7 books) or the canonical-table anchor the symbol is looked up
// by (the SV8 books). Rows are sorted by code, descending; a row covers the
// 16-bit codes from its own up to the previous row's.
type huffEntry struct {
	code   uint16
	length uint8
	value  int8
}

// vlc decodes one prefix code. It is a binary trie flattened into one slice:
// node n's children are kid[2*n] and kid[2*n+1], a non-negative entry is the
// next node and a negative entry -1-i is leaf i. A zero entry is an unused
// branch, reachable only by a codeword outside the book, which only damaged
// input produces: every book here is Kraft-complete (TestBooksAreComplete).
type vlc struct {
	kid    []int32
	sym    []int16
	maxLen int
}

// codeword is one expanded book entry.
type codeword struct {
	code   uint32
	length uint8
	sym    int16
}

// newVLC builds a trie from explicit codewords.
func newVLC(words []codeword) *vlc {
	v := &vlc{kid: make([]int32, 2), sym: make([]int16, len(words))}
	for i, w := range words {
		v.sym[i] = w.sym
		n := int(w.length)
		v.maxLen = max(v.maxLen, n)
		at := 0
		for b := n - 1; b >= 0; b-- {
			d := int(w.code>>b) & 1
			if b == 0 {
				if v.kid[2*at+d] != 0 {
					panic("musepack: book codeword collides with another")
				}
				v.kid[2*at+d] = int32(-1 - i)
				break
			}
			next := v.kid[2*at+d]
			if next < 0 {
				panic("musepack: book codeword is a prefix of another")
			}
			if next == 0 {
				v.kid = append(v.kid, 0, 0)
				next = int32(len(v.kid)/2 - 1)
				v.kid[2*at+d] = next
			}
			at = int(next)
		}
	}
	return v
}

// decode reads one symbol. A codeword that leaves the book or runs off the end
// of the data returns false, which every caller turns into a refusal.
func (v *vlc) decode(r *bitReader) (int, bool) {
	window := v.maxLen
	acc := r.peek(window)
	at := int32(0)
	for i := 0; i < window; i++ {
		d := int(acc>>(window-1-i)) & 1
		next := v.kid[2*int(at)+d]
		if next < 0 {
			if r.pos+i+1 > r.end {
				r.overrun()
				return 0, false
			}
			r.pos += i + 1
			return int(v.sym[-1-next]), true
		}
		if next == 0 {
			return 0, false
		}
		at = next
	}
	return 0, false
}

// expand turns a reference table into explicit codewords. Row i covers the
// 16-bit codes [code_i, code_{i-1}) (0x10000 above the first row), each
// codeword being the top length_i bits of a code in that span. For an SV7
// book a row is one codeword carrying its value; for a canonical SV8 book the
// row spans a run of codewords and the symbol is sym[(value - codeword) & 0xFF],
// which is how mpc_bits_can_dec looks it up.
func expand(entries []huffEntry, sym []int8) []codeword {
	var out []codeword
	prev := uint32(0x10000)
	for _, e := range entries {
		if e.length == 0 {
			continue // the Q4 table ends with a sentinel row the decoder never reaches
		}
		shift := 16 - uint(e.length)
		lo := uint32(e.code) >> shift
		hi := (prev - 1) >> shift
		for c := lo; c <= hi; c++ {
			s := int16(e.value)
			if sym != nil {
				s = int16(sym[(int(e.value)-int(c))&0xFF])
			}
			out = append(out, codeword{code: c, length: e.length, sym: s})
		}
		prev = uint32(e.code)
	}
	return out
}

// newBook builds a decoder for a reference table; sym is nil for the SV7 books.
func newBook(entries []huffEntry, sym []int8) *vlc {
	return newVLC(expand(entries, sym))
}
