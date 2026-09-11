//go:build !wmaprotablesgen

package wmapro

import "sync"

// The prefix codes. Every book here is given as parallel length and symbol
// arrays with no codewords: the LISTING ORDER is what assigns them. Walking a
// book front to back and handing each non-zero-length entry the next code of
// its own length closes on exactly one, which every book in this codec does
// (pinned by TestBooksAreCanonicalAndComplete).
//
// That is not the textbook canonical build, which sorts by length first and
// disagrees everywhere, and an entry of length zero consumes no code space at
// all rather than taking the next short code.
//
// The books are read-only once built, so they are built once for the process
// and shared. Building them is a walk over a few hundred codewords; a decoder
// per live stream would otherwise redo it.

// vlc decodes one prefix code. It is a binary trie flattened into one slice:
// node n's children are kid[2*n] and kid[2*n+1], a non-negative entry is the
// next node and a negative entry -1-sym is a leaf carrying the book's SYMBOL,
// which is not its table index. A zero entry is an unused branch, which only a
// codeword outside the book can reach, and only on damaged input.
type vlc struct {
	kid    []int32
	maxLen int
}

// newBook builds a trie from a book's lengths and symbols.
func newBook(lens []uint8, syms []uint16) *vlc {
	if len(lens) != len(syms) {
		panic("wmapro: book lengths and symbols differ in size")
	}
	v := &vlc{kid: make([]int32, 2)}
	var acc uint32
	for i, l := range lens {
		if l == 0 {
			continue
		}
		n := int(l)
		code := acc >> (32 - n)
		acc += 1 << (32 - n)
		v.maxLen = max(v.maxLen, n)
		v.insert(code, n, int32(syms[i]))
	}
	return v
}

func (v *vlc) insert(code uint32, n int, sym int32) {
	at := 0
	for b := n - 1; b >= 0; b-- {
		d := int(code>>b) & 1
		if b == 0 {
			if v.kid[2*at+d] != 0 {
				panic("wmapro: book codeword collides with another")
			}
			v.kid[2*at+d] = -1 - sym
			return
		}
		next := v.kid[2*at+d]
		if next < 0 {
			// A leaf here means an earlier codeword is a proper prefix of this
			// one, so the book is not a prefix code. These books come from a
			// re-runnable mechanical extraction, so a bad one has to break the
			// build rather than make one symbol undecodable.
			panic("wmapro: book codeword is a prefix of another")
		}
		if next == 0 {
			v.kid = append(v.kid, 0, 0)
			next = int32(len(v.kid)/2 - 1)
			v.kid[2*at+d] = next
		}
		at = int(next)
	}
}

// decode reads one symbol. A codeword that runs off the end of the book or off
// the end of the payload returns -1, which every caller turns into a named
// refusal rather than a symbol.
func (v *vlc) decode(r *bitReader) int {
	// One peek covers the longest codeword in every book here (22 bits), so
	// the walk is over a register rather than over the payload.
	window := v.maxLen
	acc := r.peek(window)
	at := int32(0)
	for i := 0; i < window; i++ {
		d := int(acc>>(window-1-i)) & 1
		next := v.kid[2*int(at)+d]
		if next < 0 {
			if r.pos+i+1 > r.bitLen() {
				r.pos = r.bitLen()
				r.overrun()
				return -1
			}
			r.pos += i + 1
			return int(-1 - next)
		}
		if next == 0 {
			return -1
		}
		at = next
	}
	return -1
}

// books holds every prefix code this codec uses, built once.
type bookSet struct {
	scaleDelta    *vlc
	scaleRunLevel *vlc
	coef          [2]*vlc
	vec4          *vlc
	vec2          *vlc
	vec1          *vlc
}

var books = sync.OnceValue(func() *bookSet {
	return &bookSet{
		scaleDelta:    newBook(scaleDeltaLens[:], scaleDeltaSyms[:]),
		scaleRunLevel: newBook(scaleRunLevelLens[:], scaleRunLevelSyms[:]),
		coef:          [2]*vlc{newBook(coefLens[0], coefSyms[0]), newBook(coefLens[1], coefSyms[1])},
		vec4:          newBook(vec4Lens[:], vec4Syms[:]),
		vec2:          newBook(vec2Lens[:], vec2Syms[:]),
		vec1:          newBook(vec1Lens[:], vec1Syms[:]),
	}
})

// The symbol biases the books carry, applied here once. Baking them into the
// tables would hide them from a reader of the generated file; applying them
// twice is the failure they exist to prevent.
const (
	scaleDeltaBias = 60 // a scale-delta symbol stands for symbol-60
	vecBias        = 1  // a vec4 or vec2 symbol stands for symbol-1, so 0 escapes
	vec1Escape     = 100
)
