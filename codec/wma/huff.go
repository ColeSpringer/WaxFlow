//go:build !wmatablesgen

package wma

import "sync"

// The books are read-only once built, so they are built once for the process
// and shared. Building them is a walk over a few thousand codewords; a decoder
// per live stream would otherwise redo it.

// vlc decodes one of the format's prefix codes. It is a binary trie flattened
// into one slice: node n's children are kid[2*n] and kid[2*n+1], a
// non-negative entry is the next node and a negative entry -1-sym is a leaf.
// A zero entry is an unused branch, which only a codeword outside the book can
// reach, and only on damaged input, since every book here is Kraft-complete
// (pinned by TestCoefBooksAreDecodable and TestHgainBook).
type vlc struct {
	kid    []int32
	maxLen int
}

// newVLC builds a trie from an explicit (code, length) listing. The listing
// order is the symbol order.
func newVLC(codes []uint32, bits []uint8) *vlc {
	v := &vlc{kid: make([]int32, 2)}
	for i := range codes {
		n := int(bits[i])
		v.maxLen = max(v.maxLen, n)
		at := 0
		for b := n - 1; b >= 0; b-- {
			d := int(codes[i]>>b) & 1
			if b == 0 {
				if v.kid[2*at+d] != 0 {
					panic("wma: book codeword collides with another")
				}
				v.kid[2*at+d] = int32(-1 - i)
				break
			}
			next := v.kid[2*at+d]
			if next < 0 {
				// A leaf here means an earlier codeword is a proper prefix of
				// this one, so the book is not a prefix code. Allocating over
				// the leaf would make that symbol undecodable and say nothing;
				// these books come from a re-runnable mechanical extraction,
				// so a bad one has to break the build.
				panic("wma: book codeword is a prefix of another")
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

// decode reads one symbol. A codeword that runs off the end of the book or off
// the end of the packet returns -1, which every caller turns into a named
// refusal rather than a symbol.
func (v *vlc) decode(r *bitReader) int {
	// One peek covers the longest codeword in every book here (22 bits), so
	// the walk is over a register rather than over the packet.
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

// coefBook is one coefficient book with its run/level ladder expanded. The
// ladder is not stored upstream: index 0 is the escape, index 1 is
// end-of-block, and from index 2 the next coefLevels[0] indices carry level 1
// with runs 0,1,2,..., the next coefLevels[1] carry level 2 with runs from 0
// again, and so on.
type coefBook struct {
	vlc   *vlc
	run   []uint16
	level []uint16
}

var coefBooks = sync.OnceValue(func() [6]*coefBook {
	var out [6]*coefBook
	for i := range coefCodes {
		b := &coefBook{
			vlc:   newVLC(coefCodes[i], coefBits[i]),
			run:   make([]uint16, len(coefBits[i])),
			level: make([]uint16, len(coefBits[i])),
		}
		at := 2
		for lv, n := range coefLevels[i] {
			for run := uint16(0); run < n && at < len(b.run); run++ {
				b.run[at] = run
				b.level[at] = uint16(lv) + 1
				at++
			}
		}
		out[i] = b
	}
	return out
})

// hgainBook is the noise-gain book. It stores {symbol, length} pairs and no
// codewords, so the LISTED order is what assigns them: walking it front to
// back with the accumulator below closes on exactly 1<<maxLen. This is not the
// textbook canonical build, which sorts by length first and disagrees
// everywhere -- the four 3-bit entries sit at the end of the listing and take
// 100..111 where a sorted build hands them 000..011.
var hgainBook = sync.OnceValue(func() *vlc {
	maxLen := uint8(0)
	for _, e := range hgainHuff {
		maxLen = max(maxLen, e[1])
	}
	codes := make([]uint32, len(hgainHuff))
	bits := make([]uint8, len(hgainHuff))
	var acc uint32
	for i, e := range hgainHuff {
		codes[i] = acc >> (maxLen - e[1])
		bits[i] = e[1]
		acc += 1 << (maxLen - e[1])
	}
	return newVLC(codes, bits)
})

// hgainDelta turns a symbol from the noise-gain book into the gain step it
// stands for. The book's symbol is biased by 18.
func hgainDelta(sym int) int { return int(hgainHuff[sym][0]) - 18 }

// expScaleBook is the AAC scalefactor book, which WMA reuses for VLC-coded
// exponent deltas. Its symbol is biased by 60.
var expScaleBook = sync.OnceValue(func() *vlc {
	return newVLC(expScaleCodes[:], expScaleBits[:])
})
