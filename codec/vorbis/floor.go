package vorbis

// floor is the spectral envelope for one submap. Vorbis defines two floor
// types; both decode per-frame data for a channel, then synthesize a curve
// that scales the residue. Decoding is separated from synthesis because the
// spec decodes all channels' floors, then all residues, then applies.
type floor interface {
	// decode reads one channel's per-frame floor data into st. unused is true
	// when the channel has no floor this frame (the output is silence).
	decode(r *bitReader, books []codebook, st *floorState) (unused bool, err error)
	// apply synthesizes the curve and multiplies it into spec[:n2].
	apply(st *floorState, spec []float32, n2 int)
}

// floorState is per-channel scratch retained between a floor's decode and
// apply (floor 1 posts and the synthesized curve).
type floorState struct {
	y     []int     // floor1 decoded posts
	final []int     // floor1 synthesized posts
	step2 []bool    // floor1 post activity
	curve []float32 // scratch curve, length n2
}

// --- Floor type 1: piecewise linear (spec 7.2) ---

type floor1 struct {
	partitionClass  []int
	classDims       []int
	classSubclasses []int
	classMasterbook []int
	classSubbooks   [][]int
	multiplier      int
	rangeVal        int
	xs              []int
	sortOrder       []int
	lowNeighbor     []int
	highNeighbor    []int
}

func parseFloor1(r *bitReader, numBooks int) (floor, error) {
	f := &floor1{}
	partitions := int(r.read(5))
	if partitions > maxFloor1Parts {
		return nil, malformed("floor1 has %d partitions", partitions)
	}
	f.partitionClass = make([]int, partitions)
	maxClass := -1
	for i := range f.partitionClass {
		f.partitionClass[i] = int(r.read(4))
		if f.partitionClass[i] > maxClass {
			maxClass = f.partitionClass[i]
		}
	}
	nClass := maxClass + 1
	if nClass > maxFloor1Class {
		return nil, malformed("floor1 references class %d", maxClass)
	}
	f.classDims = make([]int, nClass)
	f.classSubclasses = make([]int, nClass)
	f.classMasterbook = make([]int, nClass)
	f.classSubbooks = make([][]int, nClass)
	for c := 0; c < nClass; c++ {
		f.classDims[c] = int(r.read(3)) + 1
		f.classSubclasses[c] = int(r.read(2))
		if f.classSubclasses[c] > 0 {
			f.classMasterbook[c] = int(r.read(8))
			if f.classMasterbook[c] >= numBooks {
				return nil, malformed("floor1 masterbook %d of %d", f.classMasterbook[c], numBooks)
			}
		}
		nsub := 1 << f.classSubclasses[c]
		f.classSubbooks[c] = make([]int, nsub)
		for j := 0; j < nsub; j++ {
			b := int(r.read(8)) - 1
			if b >= numBooks {
				return nil, malformed("floor1 subbook %d of %d", b, numBooks)
			}
			f.classSubbooks[c][j] = b
		}
	}
	f.multiplier = int(r.read(2)) + 1
	f.rangeVal = floor1Ranges[f.multiplier-1]
	rangebits := int(r.read(4))
	f.xs = []int{0, 1 << rangebits}
	for i := 0; i < partitions; i++ {
		cls := f.partitionClass[i]
		for j := 0; j < f.classDims[cls]; j++ {
			f.xs = append(f.xs, int(r.read(rangebits)))
		}
	}
	if len(f.xs) > maxFloor1Xs {
		return nil, malformed("floor1 has %d X values", len(f.xs))
	}
	if r.eof {
		return nil, malformed("floor1 header truncated")
	}
	if err := f.computeNeighbors(); err != nil {
		return nil, err
	}
	return f, nil
}

// computeNeighbors precomputes the X-sorted order and, for each post >= 2, its
// low and high neighbors (spec 9.2.4/9.2.5).
func (f *floor1) computeNeighbors() error {
	count := len(f.xs)
	// Reject duplicate X values (the spec forbids them; neighbors assume a
	// strict ordering).
	seen := make(map[int]bool, count)
	for _, x := range f.xs {
		if seen[x] {
			return malformed("floor1 has duplicate X value %d", x)
		}
		seen[x] = true
	}
	f.sortOrder = make([]int, count)
	for i := range f.sortOrder {
		f.sortOrder[i] = i
	}
	// Insertion sort by X (count <= 65, so it is trivial and stable).
	for i := 1; i < count; i++ {
		for j := i; j > 0 && f.xs[f.sortOrder[j-1]] > f.xs[f.sortOrder[j]]; j-- {
			f.sortOrder[j-1], f.sortOrder[j] = f.sortOrder[j], f.sortOrder[j-1]
		}
	}
	f.lowNeighbor = make([]int, count)
	f.highNeighbor = make([]int, count)
	for i := 2; i < count; i++ {
		f.lowNeighbor[i] = lowNeighbor(f.xs, i)
		f.highNeighbor[i] = highNeighbor(f.xs, i)
	}
	return nil
}

func lowNeighbor(v []int, x int) int {
	n, found := 0, false
	for i := 0; i < x; i++ {
		if v[i] < v[x] && (!found || v[i] > v[n]) {
			n, found = i, true
		}
	}
	return n
}

func highNeighbor(v []int, x int) int {
	n, found := 0, false
	for i := 0; i < x; i++ {
		if v[i] > v[x] && (!found || v[i] < v[n]) {
			n, found = i, true
		}
	}
	return n
}

func (f *floor1) decode(r *bitReader, books []codebook, st *floorState) (bool, error) {
	if r.bit() == 0 {
		return true, nil // no floor => silent channel
	}
	ilr := ilog(f.rangeVal - 1)
	st.y = st.y[:0]
	st.y = append(st.y, int(r.read(ilr)), int(r.read(ilr)))
	for i := range f.partitionClass {
		cls := f.partitionClass[i]
		cdim := f.classDims[cls]
		cbits := f.classSubclasses[cls]
		csub := (1 << cbits) - 1
		cval := 0
		if cbits > 0 {
			v, err := books[f.classMasterbook[cls]].decodeScalar(r)
			if err != nil {
				if err == errEndOfPacket {
					return true, nil
				}
				return false, err
			}
			cval = v
		}
		for j := 0; j < cdim; j++ {
			book := f.classSubbooks[cls][cval&csub]
			cval >>= cbits
			if book >= 0 {
				v, err := books[book].decodeScalar(r)
				if err != nil {
					if err == errEndOfPacket {
						return true, nil
					}
					return false, err
				}
				st.y = append(st.y, v)
			} else {
				st.y = append(st.y, 0)
			}
		}
	}
	if r.eof {
		return true, nil // ran out of packet => silence, per spec
	}
	return false, nil
}

func (f *floor1) apply(st *floorState, spec []float32, n2 int) {
	count := len(f.xs)
	if cap(st.final) < count {
		st.final = make([]int, count)
		st.step2 = make([]bool, count)
	}
	final := st.final[:count]
	step2 := st.step2[:count]
	final[0] = st.y[0]
	final[1] = st.y[1]
	step2[0], step2[1] = true, true
	rng := f.rangeVal
	for i := 2; i < count; i++ {
		low, high := f.lowNeighbor[i], f.highNeighbor[i]
		pred := renderPoint(f.xs[low], final[low], f.xs[high], final[high], f.xs[i])
		val := st.y[i]
		highroom := rng - pred
		lowroom := pred
		room := 2 * lowroom
		if highroom < lowroom {
			room = 2 * highroom
		}
		if val != 0 {
			step2[low], step2[high], step2[i] = true, true, true
			switch {
			case val >= room:
				if highroom > lowroom {
					final[i] = val - lowroom + pred
				} else {
					final[i] = pred - val + highroom - 1
				}
			case val&1 == 1:
				final[i] = pred - (val+1)/2
			default:
				final[i] = pred + val/2
			}
		} else {
			step2[i] = false
			final[i] = pred
		}
	}

	if cap(st.curve) < n2 {
		st.curve = make([]float32, n2)
	}
	curve := st.curve[:n2]
	hx, hy := 0, 0
	lx := 0
	ly := final[f.sortOrder[0]] * f.multiplier
	for si := 1; si < count; si++ {
		i := f.sortOrder[si]
		if !step2[i] {
			continue
		}
		hy = final[i] * f.multiplier
		hx = f.xs[i]
		renderLine(lx, ly, hx, hy, curve)
		lx, ly = hx, hy
	}
	if hx < n2 {
		renderLine(hx, hy, n2, hy, curve)
	}
	for i := 0; i < n2; i++ {
		spec[i] *= curve[i]
	}
}

// renderPoint interpolates y at X on the line through (x0,y0)-(x1,y1), using
// integer arithmetic (spec 9.2.6).
func renderPoint(x0, y0, x1, y1, X int) int {
	dy := y1 - y0
	adx := x1 - x0
	ady := dy
	if ady < 0 {
		ady = -ady
	}
	off := (ady * (X - x0)) / adx
	if dy < 0 {
		return y0 - off
	}
	return y0 + off
}

// renderLine draws the line (x0,y0)-(x1,y1) into out, writing the dB-table
// amplitude for each integer x in [x0, min(x1, len(out))) (spec 9.2.7).
func renderLine(x0, y0, x1, y1 int, out []float32) {
	dy := y1 - y0
	adx := x1 - x0
	ady := dy
	if ady < 0 {
		ady = -ady
	}
	base := 0
	sy := 1
	if adx != 0 {
		base = dy / adx
	}
	if dy < 0 {
		sy = base - 1
	} else {
		sy = base + 1
	}
	absBase := base
	if absBase < 0 {
		absBase = -absBase
	}
	ady -= absBase * adx
	y := y0
	err := 0
	end := x1
	if end > len(out) {
		end = len(out)
	}
	for x := x0; x < end; x++ {
		out[x] = floor1InverseDB[clampDB(y)]
		err += ady
		if err >= adx {
			err -= adx
			y += sy
		} else {
			y += base
		}
	}
}

func clampDB(y int) int {
	if y < 0 {
		return 0
	}
	if y > 255 {
		return 255
	}
	return y
}

// --- Floor type 0: LSP (spec 6.1) ---

// parseFloor0 rejects the LSP floor. Floor 0 is legacy: libvorbis has not
// emitted it in two decades (every ffmpeg/libvorbis stream uses floor 1), so
// there is no differential oracle to verify an LSP synthesis against, and an
// unverified implementation would silently corrupt the rare stream that uses
// it. Rejecting with a clear error is the honest, safe posture; a correct
// floor 0 plus a synthetic conformance vector is a clean follow-up.
func parseFloor0(*bitReader, []codebook) (floor, error) {
	return nil, malformed("floor 0 (LSP) is not supported")
}
