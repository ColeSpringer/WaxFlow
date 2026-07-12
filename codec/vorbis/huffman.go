package vorbis

// huffmanLengths builds an optimal prefix code for the given symbol
// frequencies and returns each symbol's codeword length, the raw material the
// residue and class books are built from. Vorbis stores lengths (not codewords)
// in the setup header, and assignCodewords turns a valid length set into
// codewords, so producing good lengths is all the encoder needs to ship a
// tuned book. A parametric frequency model (Laplacian residue, skewed classes)
// drives these, so no training corpus is required for a first quality pass;
// the offline generator can refine them later.
//
// Every symbol gets a nonzero length (a tiny weight floor), so all entries are
// codeable, and lengths are capped at maxCodewordLen (the Vorbis limit) by a
// length-limiting fix-up that keeps the code prefix-valid.
//
// The build is a plain O(N^2) repeated-minimum merge (no heap). That is fine
// because the only large-N caller is the offline book generator (N up to a few
// thousand, run once by `go generate`, well under a second); the runtime caller
// is the three-symbol residue classbook, built once per encoder. A heap would
// lower it to O(N log N) but would also change tie-breaking and so the generated
// tables, so it is deliberately not worth it until N is both large and on a hot
// path.
func huffmanLengths(freq []float64) []uint8 {
	n := len(freq)
	lengths := make([]uint8, n)
	if n == 0 {
		return lengths
	}
	if n == 1 {
		lengths[0] = 1
		return lengths
	}

	type node struct {
		weight      float64
		left, right int
		active      bool
	}
	nodes := make([]node, 0, 2*n)
	for i := 0; i < n; i++ {
		w := freq[i]
		if w <= 0 {
			w = 1e-12 // floor so every symbol is codeable
		}
		nodes = append(nodes, node{weight: w, left: -1, right: -1, active: true})
	}

	smallest := func() int {
		best := -1
		for i := range nodes {
			if nodes[i].active && (best < 0 || nodes[i].weight < nodes[best].weight) {
				best = i
			}
		}
		return best
	}

	active := n
	for active > 1 {
		a := smallest()
		nodes[a].active = false
		b := smallest()
		nodes[b].active = false
		nodes = append(nodes, node{weight: nodes[a].weight + nodes[b].weight, left: a, right: b, active: true})
		active-- // consumed two, produced one
	}

	// Leaf depths are the codeword lengths.
	root := len(nodes) - 1
	var walk func(idx, depth int)
	walk = func(idx, depth int) {
		if nodes[idx].left < 0 {
			if idx < n {
				d := depth
				if d < 1 {
					d = 1
				}
				if d > maxCodewordLen {
					d = maxCodewordLen
				}
				lengths[idx] = uint8(d)
			}
			return
		}
		walk(nodes[idx].left, depth+1)
		walk(nodes[idx].right, depth+1)
	}
	walk(root, 0)

	fixKraft(lengths)
	return lengths
}

// fixKraft repairs a length set whose Kraft-McMillan sum exceeds 1 after
// clamping, by lengthening the shortest codewords until the sum is <= 1. Only
// pathological, highly skewed inputs need it; the residue models here do not,
// but the guard keeps assignCodewords from ever rejecting a generated book.
func fixKraft(lengths []uint8) {
	kraft := func() float64 {
		s := 0.0
		for _, l := range lengths {
			if l > 0 {
				s += 1.0 / float64(int64(1)<<l)
			}
		}
		return s
	}
	for kraft() > 1.0 {
		shortest := -1
		for i, l := range lengths {
			if l == 0 {
				continue
			}
			if shortest < 0 || l < lengths[shortest] {
				shortest = i
			}
		}
		if shortest < 0 || lengths[shortest] >= maxCodewordLen {
			return
		}
		lengths[shortest]++
	}
}
