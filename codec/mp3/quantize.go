package mp3

// Quantization and Huffman planning for the baseline CBR encoder. Each
// granule-channel's dequantized spectrum xr is quantized to integers ix by
// a single non-uniform quantizer (ISO 11172-3 section 2.4.3.4.7.1 run
// backward: ix = round(|xr|^(3/4) * 2^(-(gg-210)*3/16))), and the global
// gain is found by an inner rate-control loop that spends as many bits as
// the granule's budget allows without exceeding it. Bits decrease
// monotonically as the gain rises, so the loop is a binary search.
//
// Baseline scope: scalefactors are all zero (scalefac_compress 0, no
// preflag), so the quantizer is uniform across the spectrum and costs no
// scalefactor bits. Perceptual scalefactor shaping, joint stereo, and VBR
// are a later milestone; this stage is structured so that outer-loop noise
// shaping slots in above the inner loop without changing the interface.

import "math"

// quantExp is the exponent 0.75/4 = 3/16 that maps the global gain step to
// the quantizer multiplier (the 4/3 power of the decoder's requantizer,
// inverted for the 3/4 power here).
const quantExp = 3.0 / 16.0

// maxQuant is the largest quantized magnitude the bitstream can carry
// (ISO caps ix at 8191; the widest Huffman escape reaches 8206).
const maxQuant = 8191

// part23Max is the widest part2_3_length the 12-bit side-info field holds.
const part23Max = 4095

// sfbEdgesLong holds cumulative long-block scalefactor band edges in
// spectral lines per rate row, edges[0]=0 .. edges[22]=576. Region
// boundaries are chosen at these edges so encode and decode agree.
var sfbEdgesLong [8][23]int

func init() {
	for row := 0; row < 8; row++ {
		sum := 0
		for i := 0; i < 22; i++ {
			sfbEdgesLong[row][i] = sum
			sum += int(sfbLong[row][i])
		}
		sfbEdgesLong[row][22] = sum // 576
	}
}

// gcQuant is one quantized granule-channel: the integer spectrum plus every
// side-info field the frame writer needs.
type gcQuant struct {
	globalGain   int
	bigValues    int // pairs in the big-values region
	region0Count int
	region1Count int
	// region0End and region1End are the big-values region boundaries in
	// spectral lines (b0, b1), resolved once by planHuffman and reused by the
	// writer so encode and decode never recompute them out of lockstep.
	region0End  int
	region1End  int
	table       [3]int
	count1Table int // count1table_select (0 or 1)
	count1      int // count1 quads
	part23      int // total main-data bits (scalefactors are 0)
	ix          [576]int
}

// xrPow fills out[i] with |xr[i]|^(3/4), the per-line term the quantizer
// scales by the global-gain multiplier, and returns the maximum.
func xrPow(xr *[576]float32, out *[576]float64) float64 {
	maxp := 0.0
	for i, v := range xr {
		a := math.Abs(float64(v))
		// a^(3/4) = a^(1/2) * a^(1/4): two square roots beat math.Pow.
		s := math.Sqrt(a)
		p := s * math.Sqrt(s)
		out[i] = p
		if p > maxp {
			maxp = p
		}
	}
	return maxp
}

// quantizeAt quantizes xrpow at the given global gain into ix and returns
// the largest magnitude produced.
func quantizeAt(xrpow *[576]float64, gg int, ix *[576]int) int {
	istep := math.Exp2(-float64(gg-210) * quantExp)
	maxIx := 0
	for i, p := range xrpow {
		v := int(p*istep + 0.4054)
		if v > maxQuant {
			v = maxQuant
		}
		ix[i] = v
		if v > maxIx {
			maxIx = v
		}
	}
	return maxIx
}

// applySigns copies the signs of xr onto the quantized magnitudes.
func applySigns(xr *[576]float32, ix *[576]int) {
	for i, v := range xr {
		if v < 0 {
			ix[i] = -ix[i]
		}
	}
}

// runLength finds the big-values and count1 region sizes from the magnitude
// spectrum: trailing zero pairs are the rzero region, quads of values with
// magnitude <= 1 below them are count1, and the remainder is big-values.
func runLength(ix *[576]int) (bigValues, count1 int) {
	i := 576
	for i > 1 && ix[i-1] == 0 && ix[i-2] == 0 {
		i -= 2
	}
	for i >= 4 && le1(ix[i-1]) && le1(ix[i-2]) && le1(ix[i-3]) && le1(ix[i-4]) {
		i -= 4
		count1++
	}
	return i / 2, count1
}

func le1(v int) bool { return v <= 1 && v >= -1 }

// planHuffman chooses the big-values region split and per-region tables,
// plus the count1 table, and returns the total Huffman bit cost (scalefactor
// bits are zero at baseline). It uses the same cumulative band edges the
// decoder walks, so the declared region counts reconstruct the identical
// boundaries.
func planHuffman(ix *[576]int, bigValues, count1, row int, q *gcQuant) int {
	bigEnd := bigValues * 2
	edges := &sfbEdgesLong[row]

	// Split the big-values region into thirds at band edges, clamped to the
	// side-info field widths and the 22-band table.
	r0, r1 := 0, 0
	if bigEnd > 0 {
		third := bigEnd / 3
		for r0 < 15 && r0+1 <= 21 && edges[r0+1] <= third {
			r0++
		}
		for r1 < 7 && r0+r1+2 <= 21 && edges[r0+r1+2] <= 2*third {
			r1++
		}
	}
	b0, b1 := regionBounds(r0, r1, bigEnd, row)

	t0, bits0 := selectTable(ix, 0, b0)
	t1, bits1 := selectTable(ix, b0, b1)
	t2, bits2 := selectTable(ix, b1, bigEnd)
	c1Bits, c1Tab := count1Select(ix, bigEnd, count1)

	q.region0Count = r0
	q.region1Count = r1
	q.region0End = b0
	q.region1End = b1
	q.table = [3]int{t0, t1, t2}
	q.count1Table = c1Tab
	return bits0 + bits1 + bits2 + c1Bits
}

// regionBounds resolves the big-values region boundaries in spectral lines
// from the region counts, the same way the decoder walks its band table, so
// the declared region counts reconstruct identical boundaries.
func regionBounds(r0, r1, bigEnd, row int) (b0, b1 int) {
	edges := &sfbEdgesLong[row]
	b0 = min(edges[min(r0+1, 22)], bigEnd)
	b1 = min(edges[min(r0+r1+2, 22)], bigEnd)
	if b1 < b0 {
		b1 = b0
	}
	return b0, b1
}

// selectTable picks the cheapest big-values table that can code every pair
// in [lo, hi) and returns it with the region's bit cost. An empty or
// all-zero region uses table 0 at no cost.
func selectTable(ix *[576]int, lo, hi int) (table, bits int) {
	if lo >= hi {
		return 0, 0
	}
	maxv := 0
	for i := lo; i < hi; i++ {
		if a := abs(ix[i]); a > maxv {
			maxv = a
		}
	}
	if maxv == 0 {
		return 0, 0
	}
	bestT, bestB := -1, 1<<30
	try := func(t int) {
		b := 0
		for i := lo; i+1 < hi; i += 2 {
			b += pairBits(t, abs(ix[i]), abs(ix[i+1]))
		}
		if b < bestB {
			bestB, bestT = b, t
		}
	}
	if maxv <= 15 {
		for _, t := range noLinbitsTables {
			if bigMax[t] >= maxv {
				try(t)
			}
		}
	} else {
		if t := firstCovering(treeA, maxv); t >= 0 {
			try(t)
		}
		if t := firstCovering(treeB, maxv); t >= 0 {
			try(t)
		}
	}
	return bestT, bestB
}

// firstCovering returns the first (smallest-linbits) table in an escape
// tree whose range reaches maxv, or -1 if none does.
func firstCovering(tree []int, maxv int) int {
	for _, t := range tree {
		if tableLimit(t) >= maxv {
			return t
		}
	}
	return -1
}

// count1Select returns the count1 region's bit cost and the cheaper of the
// two count1 tables.
func count1Select(ix *[576]int, bigEnd, count1 int) (bits, table int) {
	var b0, b1 int
	for i := bigEnd; i+3 < bigEnd+count1*4 && i+3 < 576; i += 4 {
		b0 += quadBits(0, ix[i], ix[i+1], ix[i+2], ix[i+3])
		b1 += quadBits(1, ix[i], ix[i+1], ix[i+2], ix[i+3])
	}
	if b1 < b0 {
		return b1, 1
	}
	return b0, 0
}

// quantizeGranule quantizes one granule-channel's spectrum to fit avail
// main-data bits. It finds the smallest global gain (finest quantization)
// whose Huffman cost fits, spending the budget for quality while never
// overrunning it.
func quantizeGranule(xr *[576]float32, row, avail int) gcQuant {
	if avail > part23Max {
		avail = part23Max
	}
	var xrpow [576]float64
	maxp := xrPow(xr, &xrpow)

	var q gcQuant
	if maxp == 0 {
		q.globalGain = 210
		return q // silence: no big values, no count1, zero bits
	}

	// Lowest gain that keeps the largest value within range: below it the
	// quantizer multiplier is so large that even modest lines clip to
	// maxQuant. gg-210 = log2(maxp/maxQuant)/quantExp is negative here (maxp
	// is far below maxQuant), so the floor sits below 210, not above zero.
	ggMin := 210 + int(math.Ceil(math.Log2(maxp/float64(maxQuant))/quantExp))
	if ggMin < 0 {
		ggMin = 0
	}
	if ggMin > 255 {
		ggMin = 255
	}

	// planAt quantizes magnitudes only: the Huffman bit count is
	// sign-independent, so signs are applied once to the winner below rather
	// than on every probe.
	planAt := func(gg int) gcQuant {
		var r gcQuant
		r.globalGain = gg
		quantizeAt(&xrpow, gg, &r.ix)
		bv, c1 := runLength(&r.ix)
		r.bigValues = bv
		r.count1 = c1
		r.part23 = planHuffman(&r.ix, bv, c1, row, &r)
		return r
	}

	// Binary search for the smallest gain whose cost fits; bits fall as the
	// gain rises. best defaults to the coarsest gain so a granule that never
	// fits still yields a valid (heavily quantized) result.
	best := planAt(255)
	lo, hi := ggMin, 255
	for lo <= hi {
		mid := (lo + hi) / 2
		r := planAt(mid)
		if r.part23 <= avail {
			best = r
			hi = mid - 1
		} else {
			lo = mid + 1
		}
	}
	if best.part23 > avail {
		// Even the coarsest quantization overruns the budget, reachable only
		// at extreme low bit rates with dense content. Drop this granule to
		// silence so part2_3_length (a 12-bit field) and the reservoir cursor
		// (which would otherwise go negative and corrupt main_data_begin) stay
		// valid; a lone silent granule is the honest degradation.
		best = gcQuant{globalGain: 210}
	}
	applySigns(xr, &best.ix)
	return best
}
