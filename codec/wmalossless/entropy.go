package wmalossless

// The residual coder: an adaptive Golomb scheme whose parameter is an
// exponential moving average of the magnitudes it has already seen.
//
// Two details here are the easiest in the whole codec to get wrong, and both
// are silent when wrong rather than loud:
//
//   - the moving average is updated with the interleaved unsigned value,
//     BEFORE the zigzag is undone. Updating with the signed residual tracks
//     something that averages to zero and the Golomb parameter collapses.
//   - the accumulator is 32 bits and is not saturated, so it wraps. A wider
//     one would diverge from every other decoder on a crafted stream.

// golomb is one channel's residual coder state.
type golomb struct {
	// aveSum is the moving-average accumulator, 32 bits and wrapping.
	aveSum uint32
	// scaling is the movaveScaling field of the seekable tile, 0 to 7. The
	// time constant is 2^scaling.
	scaling uint
}

// seed restates the accumulator from a seekable tile's transmitted mean.
func (g *golomb) seed(mean uint32, scaling uint) {
	g.scaling = scaling
	g.aveSum = mean << (scaling + 1)
}

// next decodes one residual. A run longer than the escape's 32 is something no
// encoder writes, and without the bound a stream of 1 bits spins until the
// packet is exhausted, so it is refused: a reader's choice, not a format
// constant, and it diverges only on streams no encoder produces.
func (g *golomb) next(r *bitReader) (int32, error) {
	run, ok := r.unary(maxGolombRun)
	if !ok {
		if r.failed() {
			return 0, r.errored()
		}
		return 0, malformed("a residual run of more than %d ones", maxGolombRun)
	}
	q := uint32(run)
	if run == maxGolombRun {
		// The escape codes 32 + extra, with the width of the extra field
		// transmitted ahead of it.
		w := int(r.bits(5))
		q += r.bits(w + 1)
	}

	// The parameter is the accumulator scaled down and rounded to nearest,
	// half up.
	aveMean := (g.aveSum + 1<<g.scaling) >> (g.scaling + 1)
	u := q
	if aveMean > 1 {
		k := uint(ceilLog2(int(aveMean)))
		u = q<<k + r.bits(int(k))
	}

	g.aveSum += u - (g.aveSum >> g.scaling)

	// Undo the zigzag: u/2 for even u, -(u+1)/2 for odd.
	return int32(u>>1) ^ -int32(u&1), nil
}
