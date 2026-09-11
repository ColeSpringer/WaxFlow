//go:build !wmaprotablesgen

package wmapro

// Internals the tests need, exported here rather than made public: the band
// layout is derived rather than transmitted, so it has to be checkable without
// decoding a sample.

// BandCountForTest is the number of scale factor bands at one subframe size.
func BandCountForTest(c Config, k int) int {
	l, err := layoutFor(c)
	if err != nil || k >= len(l.edges) {
		return -1
	}
	return l.numBands(k)
}

// BandEdgesForTest is the band edge list at one subframe size.
func BandEdgesForTest(c Config, k int) []int {
	l, err := layoutFor(c)
	if err != nil || k >= len(l.edges) {
		return nil
	}
	return append([]int(nil), l.edges[k]...)
}

// ResampleForTest is the map that carries a layout at size index j onto one at
// size index k.
func ResampleForTest(c Config, k, j int) []int {
	l, err := layoutFor(c)
	if err != nil {
		return nil
	}
	return append([]int(nil), l.resample[k][j]...)
}

// IMDCTForTest runs the inverse transform alone, which is what lets the three
// statements of it in the notes be scored against each other.
func IMDCTForTest(spec []float32, scale float64) []float32 {
	out := make([]float32, len(spec))
	planFor(len(spec)).imdct(spec, out, scale, newIMDCTScratch(len(spec)))
	return out
}

// PathsForTest is what a decoder has counted so far. Reset clears nothing
// here: the counts are a property of the packets decoded, not of the state.
func PathsForTest(d *Decoder) pathCounts { return d.paths }

// CodeForTest is the codeword of one symbol in one book, for the tests that
// build a frame by hand. It walks the same in-table-order assignment newBook
// does, so a book whose assignment changed would move these tests with it.
func CodeForTest(book string, sym int) (code uint32, n int) {
	var lens []uint8
	var syms []uint16
	switch book {
	case "vec4":
		lens, syms = vec4Lens[:], vec4Syms[:]
	case "vec2":
		lens, syms = vec2Lens[:], vec2Syms[:]
	case "vec1":
		lens, syms = vec1Lens[:], vec1Syms[:]
	case "scaleDelta":
		lens, syms = scaleDeltaLens[:], scaleDeltaSyms[:]
	case "scaleRunLevel":
		lens, syms = scaleRunLevelLens[:], scaleRunLevelSyms[:]
	case "coef0":
		lens, syms = coefLens[0], coefSyms[0]
	case "coef1":
		lens, syms = coefLens[1], coefSyms[1]
	default:
		panic("wmapro: no book named " + book)
	}
	var acc uint32
	for i, l := range lens {
		if l == 0 {
			continue
		}
		c := acc >> (32 - int(l))
		acc += 1 << (32 - int(l))
		if int(syms[i]) == sym {
			return c, int(l)
		}
	}
	panic("wmapro: symbol not in book")
}
