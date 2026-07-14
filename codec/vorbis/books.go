package vorbis

//go:generate go test -tags booksgen -run ^TestGenerateBooks$ -count=1

import "math/bits"

// The codebook set. The floor posts stay a scalar Huffman book (idiomatic
// Vorbis, no training data needed) and the classification is a small skewed
// scalar book; the residue is coded by multi-dimensional product-lattice VQ
// books whose per-entry codeword lengths are trained offline (books_gen.go, from
// the clean-room corpus in the booksgen generator) so a run of near-zero residue
// amortizes below the one-bit-per-value floor a scalar book is stuck at.
//
// The residue books form an evenly-spaced precision ladder. The noise book is a
// single cheap pass for noise-like bands whose own energy masks coarse
// quantization. The coarse book is pass 0 of every tonal class; three ÷4
// refinement books (r1/r2/r3) then cascade onto it, each quantizing the previous
// pass's rounding error at a quarter of its step, so the reconstruction grids
// step down 1/16 -> 1/64 -> 1/256 -> 1/1024. A quarter-step refinement gives an
// even ~12 dB per rung (35/47/59/71 dB), so a band's demand lands on a class at
// most ~12 dB over it rather than overshooting the old 24 dB coarse->fine gap
// (the low-mid over-spend). All books are dimension-2, so each codeword carries
// two spectral lines and the coder overhead amortizes across the pair.
const (
	bookFloorPosts = 0 // floor-1 Y-post values (scalar, multiplier-1 dB index)
	bookResClass   = 1 // residue classification (skip/noise/coarse/med/fine/super)
	bookResNoise   = 2 // single-pass noise-band book (dim-2 product lattice, step 1/8)
	bookResCoarse  = 3 // residue coarse pass (dim-2 product lattice, step 1/16)
	bookResR1      = 4 // refine coarse cell ÷4 (step 1/64)
	bookResR2      = 5 // refine r1 cell ÷4 (step 1/256)
	bookResR3      = 6 // refine r2 cell ÷4 (step 1/1024)
)

// Residue book geometry (the design choice; the trained codeword lengths live in
// books_gen.go). The coarse and noise books span [-2, 2): wider than the
// peak-floor's [-1, 1) magnitude range because square-polar coupling can push
// the angle residue to +-1.8. Each refinement book has L=5 lattice points at a
// quarter of the parent step, mid-tread ({-2,-1,0,1,2}*delta, min = -2*delta),
// so it spans the parent's rounding residual [-D/2, D/2) (D = 4*delta) and
// SHRINKS the error to delta/2 per stage. The zero point is load-bearing for
// stereo coupling: the decoder picks its decouple branch from the sign of the
// magnitude channel, so a coupled magnitude of exactly zero (a channel silent at
// that line) must reconstruct as exactly zero. A mid-rise lattice (no zero point)
// would perturb it to +-delta and flip the branch, inverting the angle channel.
// Each book is a separable product lattice (lookupType 1, sequenceP false): the
// per-dimension lattice has L points minimum+index*delta, and the D-dimensional
// book has L^D entries, one per combination.
const (
	resNoiseDim     = 2
	resNoiseL       = 32 // (2 - -2) / (1/8)
	resNoiseMin     = -2.0
	resNoiseDelta   = 1.0 / 8.0
	resNoiseEntries = resNoiseL * resNoiseL // 1024

	resCoarseDim     = 2
	resCoarseL       = 64 // (2 - -2) / (1/16)
	resCoarseMin     = -2.0
	resCoarseDelta   = 1.0 / 16.0
	resCoarseEntries = resCoarseL * resCoarseL // 4096

	// Refinement books: L=5 mid-tread, min = -2*delta, points {-2,-1,0,1,2}*delta
	// spanning the parent cell of width 4*delta and including zero. Like every
	// residue book they carry two spectral lines per codeword.
	resRefineDim = 2
	resRefineL   = 5
	resR1Delta   = 1.0 / 64.0
	resR1Min     = -2.0 / 64.0             // -2*resR1Delta; parent coarse step 1/16
	resR1Entries = resRefineL * resRefineL // 25
	resR2Delta   = 1.0 / 256.0
	resR2Min     = -2.0 / 256.0            // -2*resR2Delta; parent r1 step 1/64
	resR2Entries = resRefineL * resRefineL // 25
	resR3Delta   = 1.0 / 1024.0
	resR3Min     = -2.0 / 1024.0           // -2*resR3Delta; parent r2 step 1/256
	resR3Entries = resRefineL * resRefineL // 25
)

func bookSpecs() []bookSpec {
	return []bookSpec{
		bookFloorPosts: floorPostsSpec(),
		bookResClass:   classbookSpec(),
		bookResNoise:   productBookSpec(resNoiseDim, resNoiseL, resNoiseMin, resNoiseDelta, resNoiseLengths),
		bookResCoarse:  productBookSpec(resCoarseDim, resCoarseL, resCoarseMin, resCoarseDelta, resCoarseLengths),
		bookResR1:      productBookSpec(resRefineDim, resRefineL, resR1Min, resR1Delta, resR1Lengths),
		bookResR2:      productBookSpec(resRefineDim, resRefineL, resR2Min, resR2Delta, resR2Lengths),
		bookResR3:      productBookSpec(resRefineDim, resRefineL, resR3Min, resR3Delta, resR3Lengths),
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
// (dimensions 1) over the six classes, with lengths from a prior that expects
// skip and coarse to dominate and the top rungs to be rare.
func classbookSpec() bookSpec {
	return bookSpec{dimensions: 1, lengths: huffmanLengths([]float64{0.34, 0.10, 0.22, 0.16, 0.12, 0.06})}
}
