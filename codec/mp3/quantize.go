package mp3

// Quantization and Huffman planning for the Layer III encoder. Each
// granule-channel's spectrum xr is quantized to integers ix by the ISO
// non-uniform quantizer (11172-3 section 2.4.3.4.7.1 run backward:
// ix = round(|xr|^(3/4) * 2^(-(gg-210-S_b)*3/16)) where S_b is the band's
// scalefactor contribution in quarter steps), driven by two nested loops,
// the informative encoder structure the spec describes:
//
//   - The inner rate loop finds the smallest global gain (finest overall
//     quantization) whose Huffman cost fits the granule's bit budget.
//     Bits decrease monotonically as the gain rises, so it is a binary
//     search on the first pass and a short upward walk on later passes.
//   - The outer noise-shaping loop measures per-band quantization noise
//     against the psychoacoustic model's allowed-noise thresholds and
//     amplifies (raises the scalefactor of) bands whose noise exceeds
//     them, re-running the rate loop until every band fits, no violated
//     band can be amplified further, or the iteration cap is reached.
//     The best solution seen (least total noise over threshold) wins, so
//     a late amplification round can never regress the result.
//
// Scalefactors, scalefac_compress, preflag, and scalefac_scale are
// resolved here; the frame writer serializes exactly what gcQuant
// declares.

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

// maxOuterIter caps outer noise-shaping rounds; each round re-runs the
// rate search, so this is the encoder's main speed/quality dial.
const maxOuterIter = 10

// nSfBands is the number of long-block scalefactor bands that carry a
// transmitted scalefactor; band 21 has none and cannot be shaped.
const nSfBands = 21

// sfPartCount is the long-block scalefactor partition layout: four groups
// of 6, 5, 5, and 5 bands. MPEG-1 (scfPartitions row 0) and the LSF
// scalefac_compress < 400 range share it.
var sfPartCount = [4]int{6, 5, 5, 5}

// sfPartMax is each partition's largest transmittable value: the first two
// partitions carry fields up to 4 bits wide, the last two up to 3 bits.
var sfPartMax = [4]int{15, 15, 7, 7}

// sfPartOf maps scalefactor band to partition.
var sfPartOf [nSfBands]int

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
	p, n := 0, 0
	for b := range sfPartOf {
		if n == sfPartCount[p] {
			p++
			n = 0
		}
		sfPartOf[b] = p
		n++
	}
}

// gcQuant is one quantized granule-channel: the integer spectrum plus every
// side-info and scalefactor field the frame writer needs.
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
	part2       int // scalefactor bits
	part23      int // part2 plus Huffman bits (the side-info field)
	scfCompress int
	preflag     bool
	scfScale    int
	slen        [4]int        // per-partition scalefactor field widths
	sfTx        [nSfBands]int // transmitted scalefactor values (pretab already subtracted under preflag)
	ix          [576]int
}

// quantIn is the two-loop quantizer's per-granule-channel input.
type quantIn struct {
	xr  *[576]float32
	row int
	// thr is the allowed quantization-noise energy per scalefactor band in
	// spectral (xr) energy units, from the psychoacoustic model. Nil shapes
	// nothing: the loop degenerates to the pure rate fit.
	thr   *[nSfBands]float64
	mpeg1 bool
}

// xrPow fills out[i] with |xr[i]|^(3/4), the per-line term the quantizer
// scales by the global-gain multiplier, and abs[i] with |xr[i]| for the
// noise measurement. Returns the maximum of out.
func xrPow(xr *[576]float32, out, abs *[576]float64) float64 {
	maxp := 0.0
	for i, v := range xr {
		a := math.Abs(float64(v))
		abs[i] = a
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
// plus the count1 table, and returns the total Huffman bit cost. It uses
// the same cumulative band edges the decoder walks, so the declared region
// counts reconstruct the identical boundaries.
//
// When exact is false it prices each region with only the first covering
// table: a valid coding whose cost the exact pass can only improve on, so
// rate-search probes stay cheap while the fit test remains safe (a probe
// that fits approximately always fits exactly).
func planHuffman(ix *[576]int, bigValues, count1, row int, q *gcQuant, exact bool) int {
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

	t0, bits0 := selectTable(ix, 0, b0, exact)
	t1, bits1 := selectTable(ix, b0, b1, exact)
	t2, bits2 := selectTable(ix, b1, bigEnd, exact)
	c1Bits, c1Tab := count1Select(ix, bigEnd, count1, exact)

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

// nlTiers groups the no-linbits tables by their value ceiling (bigMax 1,
// 2, 3, 5, 7, 15): within a tier the trees trade codeword shapes, across
// tiers they trade reach for small-value cost. Selection compares the
// covering tier and the next one up, the same insurance the AAC encoder's
// bookTiers buy, instead of every covering table.
var nlTiers = [...][]int{{1}, {2, 3}, {5, 6}, {7, 8, 9}, {10, 11, 12}, {13, 15}}
var nlTierMax = [...]int{1, 2, 3, 5, 7, 15}

// selectTable picks the cheapest big-values table that can code every pair
// in [lo, hi) and returns it with the region's bit cost. An empty or
// all-zero region uses table 0 at no cost. When exact is false only the
// first covering table is priced (a valid upper bound for the rate
// search); the exact pass compares the covering tier plus the next in one
// shared scan.
func selectTable(ix *[576]int, lo, hi int, exact bool) (table, bits int) {
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

	if maxv > 15 {
		// Escape territory: at most one candidate per escape tree (the
		// smallest linbits that reaches maxv), costed in one shared scan.
		ta := firstCovering(treeA, maxv)
		tb := firstCovering(treeB, maxv)
		if !exact && ta >= 0 {
			tb = -1
		}
		var bitsA, bitsB int
		for i := lo; i+1 < hi; i += 2 {
			ax, ay := abs(ix[i]), abs(ix[i+1])
			xi, yi, esc, signs := ax, ay, 0, 0
			if ax >= 15 {
				xi = 15
				esc++
			}
			if ay >= 15 {
				yi = 15
				esc++
			}
			if ax != 0 {
				signs++
			}
			if ay != 0 {
				signs++
			}
			idx := xi<<4 | yi
			if ta >= 0 {
				bitsA += int(bigEnc[ta][idx].n) + esc*int(huffTables[ta].linbits) + signs
			}
			if tb >= 0 {
				bitsB += int(bigEnc[tb][idx].n) + esc*int(huffTables[tb].linbits) + signs
			}
		}
		switch {
		case ta < 0:
			return tb, bitsB
		case tb < 0 || bitsA <= bitsB:
			return ta, bitsA
		default:
			return tb, bitsB
		}
	}

	tier := 0
	for nlTierMax[tier] < maxv {
		tier++
	}
	var cands [6]int
	n := copy(cands[:], nlTiers[tier])
	if exact && tier+1 < len(nlTiers) {
		n += copy(cands[n:], nlTiers[tier+1])
	}
	if !exact {
		n = 1
	}
	var sums [6]int
	signBits := 0
	for i := lo; i+1 < hi; i += 2 {
		ax, ay := abs(ix[i]), abs(ix[i+1])
		idx := ax<<4 | ay
		if ax != 0 {
			signBits++
		}
		if ay != 0 {
			signBits++
		}
		for j := 0; j < n; j++ {
			sums[j] += int(bigEnc[cands[j]][idx].n)
		}
	}
	bestT, bestB := cands[0], sums[0]
	for j := 1; j < n; j++ {
		if sums[j] < bestB {
			bestT, bestB = cands[j], sums[j]
		}
	}
	return bestT, bestB + signBits
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
// two count1 tables. When exact is false only table 0 is priced (a valid
// upper bound for the rate search).
func count1Select(ix *[576]int, bigEnd, count1 int, exact bool) (bits, table int) {
	var b0, b1 int
	for i := bigEnd; i+3 < bigEnd+count1*4 && i+3 < 576; i += 4 {
		b0 += quadBits(0, ix[i], ix[i+1], ix[i+2], ix[i+3])
		if exact {
			b1 += quadBits(1, ix[i], ix[i+1], ix[i+2], ix[i+3])
		}
	}
	if exact && b1 < b0 {
		return b1, 1
	}
	return b0, 0
}

// bitsFor is the field width needed to transmit v (0 needs no bits).
func bitsFor(v int) int {
	n := 0
	for 1<<n-1 < v {
		n++
	}
	return n
}

// resolveScalefactors picks the cheapest legal scalefac_compress encoding
// for the effective scalefactors sf: the transmitted values, per-partition
// field widths, the compress field, preflag, and the total scalefactor bit
// cost. MPEG-1 pairs partitions (slen1 covers bands 0-10, slen2 bands
// 11-20) through the 16-entry compress table and may fold the pretab out
// via preflag; LSF encodes four independent widths mixed-radix into the
// 9-bit field's < 400 range (whose partition layout matches MPEG-1's).
func resolveScalefactors(sf *[nSfBands]int, mpeg1 bool) (part2 int, slen [4]int, compress int, preflag bool, tx [nSfBands]int) {
	if !mpeg1 {
		tx = *sf
		for b, v := range tx {
			if w := bitsFor(v); w > slen[sfPartOf[b]] {
				slen[sfPartOf[b]] = w
			}
		}
		compress = ((slen[0]*5+slen[1])*4+slen[2])*4 + slen[3]
		part2 = 6*slen[0] + 5*slen[1] + 5*slen[2] + 5*slen[3]
		return part2, slen, compress, false, tx
	}

	// MPEG-1: evaluate preflag off and, when every high band clears the
	// pretab, on; keep the cheaper compress entry.
	best := -1
	for _, pre := range []bool{false, true} {
		cand := *sf
		if pre {
			ok := true
			for i, p := range preamp {
				if cand[11+i] < int(p) {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			for i, p := range preamp {
				cand[11+i] -= int(p)
			}
		}
		n1, n2 := 0, 0
		for b, v := range cand {
			w := bitsFor(v)
			if b <= 10 {
				n1 = max(n1, w)
			} else {
				n2 = max(n2, w)
			}
		}
		for c, packed := range scfcDecode {
			s1, s2 := int(packed>>2), int(packed&3)
			if s1 < n1 || s2 < n2 {
				continue
			}
			cost := 11*s1 + 10*s2
			if best < 0 || cost < part2 {
				best = c
				part2 = cost
				slen = [4]int{s1, s1, s2, s2}
				compress = c
				preflag = pre
				tx = cand
			}
		}
	}
	return part2, slen, compress, preflag, tx
}

// quantizeGranule runs the two-loop quantization of one granule-channel
// against a bit budget and the model's allowed-noise thresholds.
func quantizeGranule(in quantIn, budget int) gcQuant {
	if budget > part23Max {
		budget = part23Max
	}
	var xrpow, absXr [576]float64
	basep := xrPow(in.xr, &xrpow, &absXr)

	var q gcQuant
	if basep == 0 {
		q.globalGain = 210
		return q // silence: no big values, no count1, zero bits
	}

	edges := &sfbEdgesLong[in.row]
	var sf [nSfBands]int
	scfScale := 0
	maxp := basep

	// ggFloor is the lowest gain that keeps the largest value within range:
	// below it the quantizer multiplier is so large that even modest lines
	// clip to maxQuant.
	ggFloor := func(m float64) int {
		g := 210 + int(math.Ceil(math.Log2(m/float64(maxQuant))/quantExp))
		return min(max(g, 0), 255)
	}

	// planAt quantizes magnitudes only: the Huffman bit count is
	// sign-independent, so signs are applied once to the winner below rather
	// than on every probe. Probes price approximately (single-table upper
	// bound); the winner is re-planned exactly once at the end, so a probe
	// that fits can only get cheaper.
	planAt := func(gg int, r *gcQuant) {
		*r = gcQuant{globalGain: gg}
		quantizeAt(&xrpow, gg, &r.ix)
		bv, c1 := runLength(&r.ix)
		r.bigValues = bv
		r.count1 = c1
		r.part23 = planHuffman(&r.ix, bv, c1, in.row, r, false)
	}

	// amplifyStep is the xrpow multiplier one scalefactor unit adds:
	// 2^(2^(scfScale+1) * 3/16).
	amplifyStep := func() float64 {
		return math.Exp2(float64(int(1)<<uint(scfScale+1)) * quantExp)
	}

	var probe, best gcQuant
	bestScore := math.Inf(1)
	var bestSf [nSfBands]int
	bestScale := 0
	haveBest := false
	havePrev := false
	prevGG := 0
	stalled := 0

	for iter := 0; ; iter++ {
		part2, _, _, _, _ := resolveScalefactors(&sf, in.mpeg1)
		huffBudget := max(budget-part2, 0)

		// Inner rate loop. Amplification only grows xrpow, so the fitting
		// gain never falls across outer rounds: after the first binary
		// search, a short upward walk from the previous gain suffices.
		ggMin := ggFloor(maxp)
		fitted := false
		if !havePrev {
			planAt(255, &probe)
			lo, hi := ggMin, 255
			fit := probe
			for lo <= hi {
				mid := (lo + hi) / 2
				planAt(mid, &probe)
				if probe.part23 <= huffBudget {
					fit = probe
					hi = mid - 1
				} else {
					lo = mid + 1
				}
			}
			probe = fit
			fitted = probe.part23 <= huffBudget
			havePrev = true
		} else {
			gg := max(prevGG, ggMin)
			for ; gg <= 255; gg++ {
				planAt(gg, &probe)
				if probe.part23 <= huffBudget {
					fitted = true
					break
				}
			}
			if !fitted {
				planAt(255, &probe)
			}
		}
		prevGG = probe.globalGain
		if !fitted {
			// Even the coarsest quantization overruns, reachable only at
			// extreme low rates with dense content; the silence fallback
			// below stays valid and the loop stops shaping.
			break
		}

		// Outer distortion measurement: per-band noise against thresholds,
		// using the decoder's own reconstruction tables so the measure is
		// exactly what a decoder will produce.
		score := 0.0
		violated := false
		var wants [nSfBands]bool
		if in.thr != nil {
			for b := 0; b < nSfBands; b++ {
				sb := sf[b] << uint(scfScale+1)
				qexp := max(probe.globalGain-210-sb, -pow2qBias)
				mult := pow2q[qexp+pow2qBias]
				noise := 0.0
				for i := edges[b]; i < edges[b+1]; i++ {
					d := absXr[i] - pow43[probe.ix[i]]*mult
					noise += d * d
				}
				if thr := in.thr[b]; thr > 0 && noise > thr {
					score += math.Log2(noise / thr)
					if sf[b] < sfPartMax[sfPartOf[b]] {
						wants[b] = true
						violated = true
					} else if scfScale == 0 {
						wants[b] = true
						violated = true
					}
				}
			}
		}
		if score < bestScore {
			bestScore = score
			best = probe
			best.part2 = part2
			bestSf = sf
			bestScale = scfScale
			haveBest = true
			stalled = 0
		} else {
			// Amplification stopped helping (the budget is the binding
			// constraint, common on dense content): stop burning rounds.
			stalled++
		}
		if !violated || iter >= maxOuterIter || stalled >= 2 || in.thr == nil {
			break
		}

		// Amplify the violated bands; a band already at its transmitted
		// ceiling escalates scalefac_scale, which halves every scalefactor
		// (rounding up, so no band's effective amplification regresses) and
		// doubles the step for the rounds that remain.
		escalate := false
		for b := 0; b < nSfBands; b++ {
			if wants[b] && sf[b] >= sfPartMax[sfPartOf[b]] {
				escalate = true
			}
		}
		if escalate && scfScale == 0 {
			scfScale = 1
			for b := range sf {
				sf[b] = (sf[b] + 1) / 2
			}
			// Rebuild xrpow at the new scale from the unamplified base.
			step := amplifyStep()
			maxp = 0
			for b := 0; b < nSfBands; b++ {
				amp := math.Pow(step, float64(sf[b]))
				for i := edges[b]; i < edges[b+1]; i++ {
					p := math.Sqrt(absXr[i])
					p *= math.Sqrt(p) // |xr|^(3/4)
					p *= amp
					xrpow[i] = p
					if p > maxp {
						maxp = p
					}
				}
			}
			for i := edges[nSfBands]; i < 576; i++ {
				if xrpow[i] > maxp {
					maxp = xrpow[i]
				}
			}
		}
		step := amplifyStep()
		for b := 0; b < nSfBands; b++ {
			if !wants[b] || sf[b] >= sfPartMax[sfPartOf[b]] {
				continue
			}
			sf[b]++
			for i := edges[b]; i < edges[b+1]; i++ {
				xrpow[i] *= step
				if xrpow[i] > maxp {
					maxp = xrpow[i]
				}
			}
		}
	}

	if !haveBest {
		// Nothing ever fit: drop the granule to silence so part2_3_length
		// (a 12-bit field) and the reservoir cursor stay valid; a lone
		// silent granule is the honest degradation.
		return gcQuant{globalGain: 210}
	}

	// The winner is a full snapshot of its probe (including ix). Re-plan
	// its Huffman coding exactly (probes priced a single-table upper
	// bound; the real table choice only shrinks the cost) and resolve the
	// scalefactor wire fields.
	best.part23 = planHuffman(&best.ix, best.bigValues, best.count1, in.row, &best, true)
	part2, slen, compress, preflag, tx := resolveScalefactors(&bestSf, in.mpeg1)
	best.part2 = part2
	best.part23 += part2
	best.slen = slen
	best.scfCompress = compress
	best.preflag = preflag
	best.sfTx = tx
	best.scfScale = bestScale
	applySigns(in.xr, &best.ix)
	return best
}
