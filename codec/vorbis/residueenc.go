package vorbis

// Residue encoding: the inverse of residue.go. The encoder normalizes each
// channel's spectrum by its floor curve, classifies each partition against the
// masking threshold, and codes it at the class's precision, matching the
// begin/end/partSize/interleave geometry the decoder reads.
//
// The residue is coded by a two-pass cascade of product-lattice VQ books
// (books.go): pass 0 codes every coded partition with the coarse book, and fine
// partitions add a second pass that refines the coarse quantization error with
// the fine book. Values are grouped into the book's dimension-length vectors, one
// codeword per vector, in the exact order residue.decode reads them; the pass-1
// refinement subtracts the coarse pass's reconstruction (the same value the
// decoder accumulated) before quantizing, so the two passes sum to the original.

// maxBookVecDim bounds the residue books' dimension, sizing the per-vector scratch.
const maxBookVecDim = 8

// encodeResidueType1 codes the floor-normalized per-channel residues (spec 8.6.4
// layout): pass 0 writes, per partition, every channel's class symbol then every
// coded channel's coarse vectors; pass 1 writes every fine channel's refinement
// vectors. The read order matches residue.decode for a type-1 multi-pass layout.
func encodeResidueType1(w *bitWriter, r *residue, books []*encBook, residues [][]float32, classes [][]int, n2 int) {
	ch := len(residues)
	classbook := books[r.classbook]
	coarse := books[bookResCoarse]
	begin, end := r.begin, r.end
	if end > n2 {
		end = n2
	}
	partRead := (end - begin) / r.partSize
	for p := 0; p < partRead; p++ {
		for j := 0; j < ch; j++ {
			classbook.emit(w, classes[j][p])
		}
		for j := 0; j < ch; j++ {
			if book := r.books[classes[j][p]][0]; book >= 0 {
				emitResidueVectors(w, books[book], nil, residues[j], begin+p*r.partSize, r.partSize, n2)
			}
		}
	}
	if r.maxPass < 2 {
		return
	}
	for p := 0; p < partRead; p++ {
		for j := 0; j < ch; j++ {
			if book := r.books[classes[j][p]][1]; book >= 0 {
				emitResidueVectors(w, books[book], coarse, residues[j], begin+p*r.partSize, r.partSize, n2)
			}
		}
	}
}

// emitResidueVectors codes one partition of one channel as book.dimensions-wide
// vectors (the contiguous per-channel type-1 layout). When prev is non-nil this
// is a refinement pass: each value is reduced by prev's reconstruction of it (the
// value the decoder already accumulated) so book quantizes the residual.
func emitResidueVectors(w *bitWriter, book, prev *encBook, resid []float32, off, partSize, n2 int) {
	dim := book.dimensions
	var vec [maxBookVecDim]float64
	for i := 0; i < partSize; i += dim {
		for k := 0; k < dim; k++ {
			bin := off + i + k
			v := 0.0
			if bin >= 0 && bin < n2 {
				v = float64(resid[bin])
			}
			if prev != nil {
				v -= prev.latValue(prev.latIndex(v))
			}
			vec[k] = v
		}
		book.emit(w, book.vectorEntry(vec[:dim]))
	}
}

// normalizeResidue divides a channel's spectrum by its floor curve into dst.
// The floor curve is strictly positive (its dB table floors at ~1e-7), so there
// is no division guard; a bin far above its local floor simply produces a large
// residue the value book clamps. A skipped partition is zeroed on decode
// regardless, so its residue values here are irrelevant.
func normalizeResidue(spec, curve, dst []float32, n2 int) {
	for i := 0; i < n2; i++ {
		dst[i] = spec[i] / curve[i]
	}
}
