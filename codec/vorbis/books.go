package vorbis

//go:generate go test -tags booksgen -run ^TestGenerateBooks$ -count=1

import "math/bits"

// The codebook set. The floor posts stay a scalar Huffman book (idiomatic
// Vorbis, no training data needed) and the classification is a small skewed
// scalar book; the residue is coded by two multi-dimensional product-lattice VQ
// books whose per-entry codeword lengths are trained offline (books_gen.go, from
// the clean-room corpus in the booksgen generator) so a run of near-zero residue
// amortizes below the one-bit-per-value floor a scalar book is stuck at.
//
// The two residue books cascade: a partition audible only coarsely is coded by
// the coarse book alone (one pass); a partition needing fine precision adds the
// fine book as a second pass that refines the coarse pass's quantization error.
// Both books are dimension-2, so each codeword carries two spectral lines and the
// coder overhead amortizes across the pair. The cascade keeps the effective
// reconstruction lattice identical to a direct 1/256 scalar quantizer (coarse
// step 1/16, then a 1/16-wide fine refinement at 1/256), so switching to VQ books
// changes only the coded size, never the decoded samples.
const (
	bookFloorPosts = 0 // floor-1 Y-post values (scalar, multiplier-1 dB index)
	bookResClass   = 1 // residue classification (skip/coarse/fine)
	bookResCoarse  = 2 // residue coarse pass (dim-2 product lattice, step 1/16)
	bookResFine    = 3 // residue fine refinement pass (dim-2 product lattice, step 1/256)
)

// Residue book geometry (the design choice; the trained codeword lengths live in
// books_gen.go). The coarse book spans [-2, 2) at a 1/16 step: [-2, 2) rather
// than the peak-floor's [-1, 1) magnitude range because square-polar coupling can
// push the angle residue to +-1.8. The fine book refines one coarse cell, spanning
// [-1/32, 1/32) at 1/256, so a coarse+fine cascade reconstructs on the 1/256 grid.
// Each book is a separable product lattice (lookupType 1, sequenceP false): the
// per-dimension lattice has L points minimum+index*delta, and the D-dimensional
// book has L^D entries, one per combination.
const (
	resCoarseDim     = 2
	resCoarseL       = 64 // (2 - -2) / (1/16)
	resCoarseMin     = -2.0
	resCoarseDelta   = 1.0 / 16.0
	resCoarseEntries = resCoarseL * resCoarseL // 4096

	resFineDim     = 2
	resFineL       = 16 // (1/32 - -1/32) / (1/256)
	resFineMin     = -1.0 / 32.0
	resFineDelta   = 1.0 / 256.0
	resFineEntries = resFineL * resFineL // 256
)

func bookSpecs() []bookSpec {
	return []bookSpec{
		bookFloorPosts: floorPostsSpec(),
		bookResClass:   classbookSpec(),
		bookResCoarse:  productBookSpec(resCoarseDim, resCoarseL, resCoarseMin, resCoarseDelta, resCoarseLengths),
		bookResFine:    productBookSpec(resFineDim, resFineL, resFineMin, resFineDelta, resFineLengths),
	}
}

// encSpecs and encBooks are the encoder's immutable codebook set, built once at
// package init and shared by every encoder. The specs, and the codewords
// buildEncBook derives from them, depend only on the static generated tables, not
// on channel count or rate, and nothing mutates a spec or a book after
// construction, so one shared copy serves every encoder (concurrent encodes read
// them) instead of rebuilding all 4096+256 product-lattice entries per NewEncoder.
var (
	encSpecs = bookSpecs()
	encBooks = buildEncBooks(encSpecs)
)

func buildEncBooks(specs []bookSpec) []*encBook {
	books := make([]*encBook, len(specs))
	for i := range specs {
		books[i] = buildEncBook(specs[i])
	}
	return books
}

// productBookSpec builds a separable product-lattice VQ spec: L per-dimension
// lattice points (multiplicand i decodes to minimum+i*delta), dim dimensions, and
// L^dim entries whose codeword lengths come from the trained table. lengths must
// have exactly L^dim entries.
func productBookSpec(dim, L int, minimum, delta float64, lengths []uint8) bookSpec {
	mult := make([]uint32, L)
	for i := range mult {
		mult[i] = uint32(i)
	}
	return bookSpec{
		dimensions:   dim,
		lengths:      lengths,
		lookupType:   1,
		minimum:      minimum,
		delta:        delta,
		valueBits:    bits.Len(uint(L - 1)),
		multiplicand: mult,
	}
}

// floorPostsSpec is a flat 8-bit scalar book over the 256 floor-1 Y levels
// (multiplier 1, so a level is a dB-table index). decodeScalar returns the
// entry number, which the floor uses as the post value, so entry i means level
// i. The floor is a small fraction of the packet, so the flat code is kept for
// simplicity here.
func floorPostsSpec() bookSpec {
	lengths := make([]uint8, 256)
	for i := range lengths {
		lengths[i] = 8
	}
	return bookSpec{dimensions: 1, lengths: lengths}
}

// classbookSpec is the residue classification book: one symbol per partition
// (dimensions 1) over the three classes, with lengths from a prior that expects
// skip and coarse to dominate.
func classbookSpec() bookSpec {
	return bookSpec{dimensions: 1, lengths: huffmanLengths([]float64{0.40, 0.35, 0.25})}
}
