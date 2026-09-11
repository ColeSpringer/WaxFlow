//go:build !wmavoicetablesgen

package wmavoice

// The frame type layer (notes 1.2 and 4): a canonical Huffman code whose
// codeword lengths are fixed for every stream, and a per-stream map from the
// symbol it yields to one of seventeen fixed frame descriptors.

// acbKind is a frame's adaptive codebook.
type acbKind uint8

const (
	// acbNone is comfort noise or a hardcoded codebook read, with no long-term
	// prediction at all.
	acbNone acbKind = iota
	// acbAsym is one pitch value per frame, interpolated to a pitch per
	// sample, through an 18-tap asymmetric sinc (nine taps each way) at eight
	// fractional phases. Its table has a ninth, phase 0, which the rounding
	// that produces the phase can never select (excitation.go).
	acbAsym
	// acbHamming is one pitch value per block at quarter-sample resolution,
	// through a 16-tap Hamming-windowed sinc (eight taps each way) at the
	// three fractional quarter phases; phase 0 is a whole-sample copy.
	acbHamming
)

// fcbKind is a frame's fixed codebook.
type fcbKind uint8

const (
	fcbSilence fcbKind = iota
	fcbHardcoded
	fcbWindowPulses
	fcbInnovation
)

// frameDesc is one row of note 4's table.
type frameDesc struct {
	blocks int
	log2   int
	acb    acbKind
	fcb    fcbKind
	// double is how many of the five innovation pulses are coded as pairs.
	double int
}

// frameTypes is that table. It is fixed: not in the extradata and not in the
// bitstream. Types 8, 9, 10 and 14 are reachable in the format and written by
// no encoder, so they ship implemented and recorded as a deficit rather than
// refused.
var frameTypes = [17]frameDesc{
	{blocks: 1, log2: 0, acb: acbNone, fcb: fcbSilence},
	{blocks: 2, log2: 1, acb: acbNone, fcb: fcbHardcoded},
	{blocks: 2, log2: 1, acb: acbAsym, fcb: fcbWindowPulses},
	{blocks: 2, log2: 1, acb: acbAsym, fcb: fcbInnovation, double: 2},
	{blocks: 2, log2: 1, acb: acbAsym, fcb: fcbInnovation, double: 5},
	{blocks: 4, log2: 2, acb: acbAsym, fcb: fcbInnovation},
	{blocks: 4, log2: 2, acb: acbAsym, fcb: fcbInnovation, double: 2},
	{blocks: 4, log2: 2, acb: acbAsym, fcb: fcbInnovation, double: 5},
	{blocks: 2, log2: 1, acb: acbHamming, fcb: fcbInnovation},
	{blocks: 2, log2: 1, acb: acbHamming, fcb: fcbInnovation, double: 2},
	{blocks: 2, log2: 1, acb: acbHamming, fcb: fcbInnovation, double: 5},
	{blocks: 4, log2: 2, acb: acbHamming, fcb: fcbInnovation},
	{blocks: 4, log2: 2, acb: acbHamming, fcb: fcbInnovation, double: 2},
	{blocks: 4, log2: 2, acb: acbHamming, fcb: fcbInnovation, double: 5},
	{blocks: 8, log2: 3, acb: acbHamming, fcb: fcbInnovation},
	{blocks: 8, log2: 3, acb: acbHamming, fcb: fcbInnovation, double: 2},
	{blocks: 8, log2: 3, acb: acbHamming, fcb: fcbInnovation, double: 5},
}

// typeCodeLens are the twenty-two codeword lengths of the frame type VLC, in
// symbol order. The Kraft sum is exactly 1, so the code is complete: three
// codewords at each of the six even lengths from 2 to 12, and four at 14.
var typeCodeLens = [22]int{
	2, 2, 2, 4, 4, 4, 6, 6, 6, 8, 8, 8, 10, 10, 10, 12, 12, 12, 14, 14, 14, 14,
}

// maxTypeCodeLen is the longest of those, and the widest peek the walk takes.
const maxTypeCodeLen = 14

// typeRun is the run of codewords sharing one length, which is contiguous
// under the canonical assignment: codes are handed out in table order from an
// accumulator that advances by 2^(32-L) after each entry, so every symbol of a
// given length lands next to its neighbours.
type typeRun struct {
	first  uint64 // the first codeword of this length, right aligned
	symbol int    // the symbol that codeword maps to
	count  int
}

// typeRuns is indexed by codeword length. Built rather than written out so the
// canonical assignment is visible instead of asserted.
var typeRuns = buildTypeRuns()

func buildTypeRuns() [maxTypeCodeLen + 1]typeRun {
	var runs [maxTypeCodeLen + 1]typeRun
	var acc uint32
	for sym, n := range typeCodeLens {
		code := uint64(acc >> uint(32-n))
		if runs[n].count == 0 {
			runs[n] = typeRun{first: code, symbol: sym}
		}
		runs[n].count++
		acc += 1 << uint(32-n)
	}
	return runs
}

// readFrameType decodes one frame type. The walk shifts a bit in at a time and
// looks for a codeword of the length reached, which is the ordinary canonical
// decode; the result is a symbol index from 0 to 21, which the stream's own
// tree then turns into a frame type.
func (d *Decoder) readFrameType(r *bitReader) (int, error) {
	var code uint64
	for n := 1; n <= maxTypeCodeLen; n++ {
		code = code<<1 | uint64(r.bit())
		if r.err != nil {
			return 0, r.err
		}
		run := typeRuns[n]
		if run.count == 0 {
			continue
		}
		off := code - run.first
		if code < run.first || off >= uint64(run.count) {
			continue
		}
		sym := run.symbol + int(off)
		ft := d.cfg.Tree[sym]
		if ft < 0 {
			// The tree and the stream disagree: no real extradata populates
			// this symbol, so reaching it means the walk is not where it
			// thinks it is.
			return 0, malformed("frame type symbol %d is not in this stream's variable bit mode tree", sym)
		}
		return int(ft), nil
	}
	return 0, malformed("no frame type codeword in %d bits", maxTypeCodeLen)
}
