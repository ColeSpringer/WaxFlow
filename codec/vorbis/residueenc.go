package vorbis

import "math"

// Residue encoding: the inverse of residue.go. The encoder normalizes each
// channel's spectrum by its floor curve, classifies each partition against the
// masking threshold, and codes it at the class's precision, matching the
// begin/end/partSize/interleave geometry the decoder reads.
//
// The residue is coded by a cascade of product-lattice VQ books (books.go):
// pass 0 codes every coded partition with its class's base book (the coarse
// book for the tonal classes, the cheap noise book for noise bands), and the
// finer classes add refinement passes that quantize the running error left by
// the passes before them. Values are grouped into the book's dimension-length
// vectors, one codeword per vector, in the exact order residue.decode reads
// them; a refinement pass subtracts the earlier passes' reconstructions (the
// same values the decoder accumulates) before quantizing, so the passes sum to
// the original.

// maxBookVecDim bounds the residue books' dimension, sizing the per-vector scratch.
const maxBookVecDim = 8

// maxResPass bounds the cascade depth, sizing the per-pass scratch (the Vorbis
// cascade limit).
const maxResPass = 8

// encodeResidueType1 codes the floor-normalized per-channel residues (spec 8.6.4
// layout): pass 0 writes, per partition, every channel's class symbol then every
// coded channel's base vectors; each later pass writes the refinement vectors of
// every channel whose class carries a book for it. The read order matches
// residue.decode for a type-1 multi-pass layout. magChannel is the coupled
// magnitude channel (or -1 when uncoupled); the angle channel is passed alongside
// it so the last cascade pass is sign-preserving exactly on the anti-phase lines
// that would otherwise flip the decoder's decouple branch (see
// emitResidueVectors).
func encodeResidueType1(w *bitWriter, r *residue, books []*encBook, residues [][]float32, classes [][]int, n2, magChannel int) {
	ch := len(residues)
	classbook := books[r.classbook]
	var ang []float32
	if magChannel >= 0 && ch == 2 {
		ang = residues[1-magChannel]
	}
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
			chain := r.books[classes[j][p]]
			if chain[0] >= 0 {
				emitResidueVectors(w, books[chain[0]], nil, residues[j], begin+p*r.partSize, r.partSize, n2, magAngle(j == magChannel, ang), isLastBook(chain, 0))
			}
		}
	}
	var prevs [maxResPass]*encBook
	for pass := 1; pass < r.maxPass; pass++ {
		for p := 0; p < partRead; p++ {
			for j := 0; j < ch; j++ {
				chain := r.books[classes[j][p]]
				book := chain[pass]
				if book < 0 {
					continue
				}
				np := 0
				for k := 0; k < pass; k++ {
					if chain[k] >= 0 {
						prevs[np] = books[chain[k]]
						np++
					}
				}
				emitResidueVectors(w, books[book], prevs[:np], residues[j], begin+p*r.partSize, r.partSize, n2, magAngle(j == magChannel, ang), isLastBook(chain, pass))
			}
		}
	}
}

// isLastBook reports whether pass carries the final coded book of a class's
// cascade chain (no later pass adds another book). The sign-preserving nudge
// runs only on this last pass, where the reconstruction the decoder branches on
// is final.
func isLastBook(chain []int, pass int) bool {
	for k := pass + 1; k < len(chain); k++ {
		if chain[k] >= 0 {
			return false
		}
	}
	return true
}

// magAngle returns the coupled angle residue for the magnitude channel (enabling
// per-line sign preservation), or nil for any other channel or the uncoupled case.
func magAngle(isMag bool, ang []float32) []float32 {
	if isMag {
		return ang
	}
	return nil
}

// emitResidueVectors codes one partition of one channel as book.dimensions-wide
// vectors (the contiguous per-channel type-1 layout). A non-empty prevs marks a
// refinement pass: each value is reduced, stage by stage, by the earlier books'
// nearest reconstructions of it (the values the decoder already accumulated) so
// book quantizes the remaining error.
//
// A non-nil ang is the coupled angle residue, supplied only for the magnitude
// channel. On a line where the angle magnitude exceeds this channel's (the
// anti-phase case), a magnitude that the whole cascade rounds to zero would flip
// the decoder's decouple branch and invert the angle channel. To prevent that
// without derailing the refinement cascade, the sign-preserving nudge is applied
// only on the last pass (lastPass) and only where the earlier passes have
// reconstructed nothing yet (the running value still equals the original), i.e.
// exactly the lines a plain cascade would leave at zero. Everywhere else plain
// nearest quantization is used, so a coupled magnitude reconstructs as accurately
// as an uncoupled channel and the finer classes actually reach their promised
// precision.
func emitResidueVectors(w *bitWriter, book *encBook, prevs []*encBook, resid []float32, off, partSize, n2 int, ang []float32, lastPass bool) {
	dim := book.dimensions
	var vec [maxBookVecDim]float64
	var sp [maxBookVecDim]bool
	for i := 0; i < partSize; i += dim {
		for k := 0; k < dim; k++ {
			bin := off + i + k
			v := 0.0
			if bin >= 0 && bin < n2 {
				v = float64(resid[bin])
			}
			antiPhase := ang != nil && bin >= 0 && bin < n2 && math.Abs(float64(ang[bin])) > math.Abs(v)
			orig := v
			for _, pb := range prevs {
				v -= pb.latValue(pb.latIndex(v))
			}
			// Nudge only when this is the final pass and the cascade so far has
			// added nothing (orig unchanged): a value the earlier passes already
			// resolved carries the correct sign, and forcing it here would just
			// add error.
			sp[k] = lastPass && antiPhase && v == orig
			vec[k] = v
		}
		book.emit(w, book.vectorEntry(vec[:dim], sp[:dim]))
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
