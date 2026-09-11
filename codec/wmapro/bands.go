//go:build !wmaprotablesgen

package wmapro

// The per-configuration tables: the scale factor band layout at every subframe
// size, and the map that carries one subframe size's scale factors onto
// another's bands. Both are derived from the sample rate and the frame length
// and neither is transmitted, so a rule that is subtly wrong here is quiet
// spectral damage rather than a desync.
//
// All of it is integer arithmetic and must come out exactly.

// layout holds one configuration's derived tables. It is read-only once built.
type layout struct {
	// edges[k] is the band edge list at size index k, numBands[k]+1 long, with
	// edges[k][0] = 0 and the last entry the subframe length.
	edges [][]int
	// resample[k][j][b] is the band of a layout at size index j whose scale
	// factor feeds band b of a layout at size index k.
	resample [][][]int
}

func (l *layout) numBands(k int) int { return len(l.edges[k]) - 1 }

// The subwoofer cutoff the format appears to intend for the LFE channel is
// deliberately absent. The only reference decoder zeroes the LFE spectrum
// above it BEFORE the loop that fills that spectrum, so the zeroing is
// overwritten and the cutoff has no effect; no file in reach codes LFE content
// up there, so a correct cutoff and an ineffective one cannot be told apart.
// Computing it and not applying it would be dead code, and applying it would
// diverge from the only thing that can score this decoder. The measurement and
// the deficit are in docs/quality-gates.md and docs/notes/wma-pro-bitstream.md
// section 13.2.

// layoutFor builds the tables for one configuration. It is a few thousand
// integer operations per decoder, so it is rebuilt per decoder rather than
// cached: a process-wide cache keyed on a sample rate taken from the wire
// would keep a table set for every distinct rate it ever opened.
func layoutFor(c Config) (*layout, error) {
	sizes := c.sizeCount()
	l := &layout{edges: make([][]int, sizes), resample: make([][][]int, sizes)}
	for k := range sizes {
		e, err := bandEdges(c, k)
		if err != nil {
			return nil, err
		}
		l.edges[k] = e
	}
	for k := range sizes {
		l.resample[k] = make([][]int, sizes)
		for j := range sizes {
			l.resample[k][j] = resampleMap(l, k, j)
		}
	}
	return l, nil
}

// bandEdges lays the critical-band ladder out over one subframe size. Every
// edge is a multiple of four, which is what lets the coefficient vector books
// work four at a time without straddling a band.
//
// The final assignment overwrites the last edge the walk wrote rather than
// appending, so the band whose edge landed at or past the subframe length is
// absorbed into the top band.
func bandEdges(c Config, k int) ([]int, error) {
	length := c.SamplesPerFrame() >> k
	edges := make([]int, 0, len(bandEdgeFreqs)+1)
	edges = append(edges, 0)
	for _, f := range bandEdgeFreqs {
		if edges[len(edges)-1] >= length {
			break
		}
		// The +2 before the truncation to a multiple of four is part of the
		// rule rather than a rounding taste: it lifts every edge half a step
		// before the mask takes it back down.
		e := (length*2*int(f)/c.Rate + 2) & ^3
		if e > edges[len(edges)-1] {
			edges = append(edges, e)
		}
		if e >= length {
			break
		}
	}
	if len(edges) < 2 {
		return nil, malformed("the band layout at subframe length %d has no bands", length)
	}
	edges[len(edges)-1] = length
	return edges, nil
}

// resampleMap says which band of a layout at size index j feeds each band of a
// layout at size index k. Both layouts are lifted onto the frequency grid of a
// full-frame subframe so their band indices are comparable, and the target
// band's midpoint is biased down half a bin, which is part of the definition.
func resampleMap(l *layout, k, j int) []int {
	target, source := l.edges[k], l.edges[j]
	out := make([]int, len(target)-1)
	for b := range out {
		mid := ((target[b] + target[b+1] - 1) << k) >> 1
		v := 0
		for v+1 < len(source) && (source[v+1]<<j) < mid {
			v++
		}
		out[b] = v
	}
	return out
}
