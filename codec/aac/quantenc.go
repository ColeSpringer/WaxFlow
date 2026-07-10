package aac

import "math"

// Two-loop quantization (the ISO 14496-3 informative encoder structure):
// an inner rate loop finds the uniform scalefactor offset whose Huffman
// cost fits the frame budget, and an outer distortion loop walks
// individual bands whose quantization noise exceeds their masking
// threshold down to finer step sizes, re-running the rate loop after
// each amplification round. Codebook choice is greedy per band (the
// cheapest book covering the band's magnitudes); sections then form
// from equal-codebook runs. A dynamic-programming section merge could
// shave a few header bits per frame and is a recorded quality ratchet,
// not a correctness gap.

const (
	// ampMax bounds per-band amplification so the scalefactor DPCM
	// deltas stay far inside their +-60 range.
	ampMax = 30
	// maxAmpIter bounds outer-loop rounds; each round re-runs the rate
	// search, so this is the encoder's main speed/quality dial.
	maxAmpIter = 10
	// sfSearchMax bounds the rate search's uniform offset. 300 clamps
	// to 255 everywhere, which quantizes every line to zero, so the
	// search always terminates on a fitting solution.
	sfSearchMax = 300
)

// encBand is one coded band: a (window group, scalefactor band) cell in
// grouped spectral order.
type encBand struct {
	off, n int // span in the grouped line order
	maxAbs float64
	energy float64
	thr    float64 // allowed noise energy, MDCT units
	minSf  int     // clip guard: below this a magnitude exceeds 8191
	amp    int     // outer-loop amplification steps
	// final assembly
	sf int
	cb int
}

// bandMemo caches one band's quantization outcome at one scalefactor;
// the rate search revisits the same (band, sf) pairs constantly.
type bandMemo struct {
	epoch uint32
	bits  int32
	cb    int8
	zero  bool
	noise float64
}

// chanQuant is one channel's per-frame quantization state, reused
// across frames (memo storage dominates; it is epoch-invalidated).
type chanQuant struct {
	spec  *[1024]float64 // the frame's spectrum (for signs), set by buildBands
	pos   [1024]int32    // grouped order -> spec index
	absv  [1024]float64  // |spec| in grouped order
	xrpow [1024]float64  // |spec|^0.75
	q     [1024]int      // final signed quantized values
	qtmp  [1024]int      // quantization scratch

	bands   []encBand
	memo    []bandMemo
	epoch   uint32
	maxSfb  int
	nGroups int
	lenBits int // section length field width (5 long, 3 short)

	// assembly outputs
	globalGain int
	demand     float64 // perceptual bit demand, for stereo budget split
}

// buildBands lays the grouped coded order and band table for one
// channel's frame. groupLen holds the window count per group (1 group of
// 1 window for long sequences), swb the band offsets for the window
// type, thr the per-(group, band) allowed noise.
func (cq *chanQuant) buildBands(spec *[1024]float64, groupLen []int, swb []uint16, maxSfb int, thr func(g, sfb int) float64, short bool) {
	cq.spec = spec
	cq.maxSfb = maxSfb
	cq.nGroups = len(groupLen)
	cq.lenBits = 5
	if short {
		cq.lenBits = 3
	}
	cq.bands = cq.bands[:0]
	n := 0
	winBase := 0
	for g, L := range groupLen {
		for sfb := 0; sfb < maxSfb; sfb++ {
			b := encBand{off: n}
			lo, hi := int(swb[sfb]), int(swb[sfb+1])
			for w := 0; w < L; w++ {
				base := (winBase + w) * 128
				for k := lo; k < hi; k++ {
					v := spec[base+k]
					av := math.Abs(v)
					cq.pos[n] = int32(base + k)
					cq.absv[n] = av
					cq.xrpow[n] = math.Sqrt(av * math.Sqrt(av)) // |v|^0.75
					if av > b.maxAbs {
						b.maxAbs = av
					}
					b.energy += av * av
					n++
				}
			}
			b.n = n - b.off
			b.thr = thr(g, sfb)
			b.minSf = minSfFor(b.maxAbs)
			cq.bands = append(cq.bands, b)
		}
		winBase += L
	}
	need := len(cq.bands) * 256
	if cap(cq.memo) < need {
		cq.memo = make([]bandMemo, need)
	}
	cq.memo = cq.memo[:need]
	cq.epoch++

	// Perceptual demand: information above threshold, for budget splits.
	d := 0.0
	for _, b := range cq.bands {
		if b.energy > b.thr && b.thr > 0 {
			d += float64(b.n) * math.Log2(b.energy/b.thr)
		}
	}
	cq.demand = d
}

// minSfFor is the smallest scalefactor keeping the band's largest
// magnitude inside the quantizer's 8191 ceiling.
func minSfFor(maxAbs float64) int {
	if maxAbs < 1 {
		return 0
	}
	// |q| = maxAbs^(3/4) * 2^(-3/16*(sf-100)) <= 8191.5
	sf := int(math.Ceil(100 + (16.0/3.0)*(0.75*math.Log2(maxAbs)-math.Log2(8191.4))))
	if sf < 0 {
		return 0
	}
	if sf > sfClampMax {
		return sfClampMax
	}
	return sf
}

const sfClampMax = 255

// sfFor is a band's scalefactor at uniform offset delta.
func (b *encBand) sfFor(delta int) int {
	sf := delta - b.amp
	if sf < b.minSf {
		sf = b.minSf
	}
	if sf > sfClampMax {
		sf = sfClampMax
	}
	return sf
}

// quantAt fills q[:b.n] with the band's signed quantized values at sf
// and returns the largest magnitude.
func (cq *chanQuant) quantAt(b *encBand, sf int, q []int) int {
	f := math.Exp2(-0.1875 * float64(sf-100))
	maxQ := 0
	for i := 0; i < b.n; i++ {
		m := int(cq.xrpow[b.off+i]*f + 0.4054)
		if m > escMaxValue {
			m = escMaxValue
		}
		if m > maxQ {
			maxQ = m
		}
		q[i] = m
	}
	return maxQ
}

// bookTiers lists the codebook pairs by magnitude ceiling.
var bookTiers = [...][2]int{{1, 2}, {3, 4}, {5, 6}, {7, 8}, {9, 10}, {11, 0}}
var tierMax = [...]int{1, 2, 4, 7, 12, escMaxValue}

// bandAt quantizes band b at scalefactor sf and returns its memoized
// cost: cheapest covering codebook, Huffman bits, and noise energy.
func (cq *chanQuant) bandAt(bi, sf int) *bandMemo {
	m := &cq.memo[bi*256+sf]
	if m.epoch == cq.epoch {
		return m
	}
	b := &cq.bands[bi]
	q := cq.qtmp[:b.n]
	maxQ := cq.quantAt(b, sf, q)

	// Memo slots are reused across frames, so every field must be
	// assigned on a miss; a stale zero flag would silently delete the
	// band from later frames.
	if maxQ == 0 {
		*m = bandMemo{epoch: cq.epoch, zero: true, noise: b.energy}
		return m
	}
	// Noise at this step size (signs do not affect it).
	gain := math.Exp2(0.25 * float64(sf-sfOffset))
	noise := 0.0
	for i := 0; i < b.n; i++ {
		e := cq.absv[b.off+i] - iquant(float64(q[i]))*gain
		noise += e * e
	}

	// Cheapest codebook: the covering tier, the next one up, and the
	// escape book as insurance for mixed content.
	tier := 0
	for tierMax[tier] < maxQ {
		tier++
	}
	bestBits, bestCb := -1, 0
	try := func(cb int) {
		if cb == 0 {
			return
		}
		bits := specRunBits(cb, q)
		if bits >= 0 && (bestBits < 0 || bits < bestBits) {
			bestBits, bestCb = bits, cb
		}
	}
	try(bookTiers[tier][0])
	try(bookTiers[tier][1])
	if tier+1 < len(bookTiers) {
		try(bookTiers[tier+1][0])
		try(bookTiers[tier+1][1])
	}
	if tierMax[tier] < 16 {
		try(escHCB)
	}
	*m = bandMemo{epoch: cq.epoch, bits: int32(bestBits), cb: int8(bestCb), noise: noise}
	return m
}

// totalBits evaluates the frame cost at uniform offset delta: spectral
// Huffman bits, section headers (with exact length-escape accounting),
// and the scalefactor DPCM chain.
func (cq *chanQuant) totalBits(delta int) int {
	total := 0
	lenEsc := 1<<uint(cq.lenBits) - 1
	perGroup := cq.maxSfb
	prevSf := -1
	for g := 0; g < cq.nGroups; g++ {
		runCb := -1
		runLen := 0
		flush := func() {
			if runLen > 0 {
				total += 4 + cq.lenBits*(runLen/lenEsc+1)
			}
		}
		for k := 0; k < perGroup; k++ {
			bi := g*perGroup + k
			b := &cq.bands[bi]
			m := cq.bandAt(bi, b.sfFor(delta))
			cb := 0
			if !m.zero {
				cb = int(m.cb)
				total += int(m.bits)
				sf := b.sfFor(delta)
				if prevSf >= 0 {
					d := sf - prevSf
					if d < -60 {
						d = -60
					} else if d > 60 {
						d = 60
					}
					total += sfDeltaBits(d)
				}
				prevSf = sf
			}
			if cb != runCb {
				flush()
				runCb, runLen = cb, 1
			} else {
				runLen++
			}
		}
		flush()
	}
	return total
}

// rateSearch finds the smallest uniform offset whose total fits budget.
// Cost is monotone nonincreasing in the offset (coarser steps cost
// less), and the all-zero ceiling always fits, so the search is total.
func (cq *chanQuant) rateSearch(budget int) int {
	lo, hi := 0, sfSearchMax
	for lo < hi {
		mid := (lo + hi) / 2
		if cq.totalBits(mid) <= budget {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

// quantizeChannel runs the two loops and assembles the channel's final
// scalefactors, codebooks, and quantized values, ready for writeICSBody.
func (cq *chanQuant) quantizeChannel(budget int) {
	if budget < 0 {
		budget = 0
	}
	bestDelta := -1
	bestScore := math.Inf(1)
	delta := 0
	for iter := 0; ; iter++ {
		delta = cq.rateSearch(budget)
		// Score this solution by total noise-over-threshold; remember
		// the best in case later amplification rounds regress.
		score := 0.0
		violated := false
		for bi := range cq.bands {
			b := &cq.bands[bi]
			m := cq.bandAt(bi, b.sfFor(delta))
			if b.thr > 0 && m.noise > b.thr {
				score += math.Log2(m.noise / b.thr)
				if b.amp < ampMax && b.sfFor(delta) > b.minSf {
					violated = true
				}
			}
		}
		if score < bestScore {
			bestScore = score
			bestDelta = delta
		}
		if !violated || iter >= maxAmpIter {
			break
		}
		for bi := range cq.bands {
			b := &cq.bands[bi]
			m := cq.bandAt(bi, b.sfFor(delta))
			if b.thr > 0 && m.noise > b.thr && b.amp < ampMax && b.sfFor(delta) > b.minSf {
				b.amp++
			}
		}
	}
	cq.assemble(bestDelta)
}

// assemble materializes the final solution at delta: per-band sf and cb,
// signed quantized values, the DPCM-legal scalefactor chain, and the
// global gain. The writer emits exactly these fields; the frame's real
// bit count comes from the bit writer itself (the reservoir reads
// bitLen), so nothing recounts the assembly.
func (cq *chanQuant) assemble(delta int) {
	prevSf := -1
	cq.globalGain = 0
	firstCoded := true
	for bi := range cq.bands {
		b := &cq.bands[bi]
		sf := b.sfFor(delta)
		m := cq.bandAt(bi, sf)
		if m.zero {
			b.cb = 0
			b.sf = 0
			// Zero the assembled values so writeICS's run writer sees them.
			for i := 0; i < b.n; i++ {
				cq.q[b.off+i] = 0
			}
			continue
		}
		// Clamp into the DPCM's +-60 reach of the previous coded band
		// (unreachable under ampMax 30, but the wire format must hold).
		if prevSf >= 0 {
			if sf < prevSf-60 {
				sf = prevSf - 60
			} else if sf > prevSf+60 {
				sf = prevSf + 60
			}
		}
		b.sf = sf
		b.cb = int(cq.bandAt(bi, sf).cb)
		if firstCoded {
			cq.globalGain = sf
			firstCoded = false
		}
		prevSf = sf
		// Materialize signed values at the final scalefactor; signs
		// rejoin from the source spectrum (the quantizer works on
		// magnitudes).
		q := cq.qtmp[:b.n]
		cq.quantAt(b, sf, q)
		for i := 0; i < b.n; i++ {
			if math.Signbit(cq.spec[cq.pos[b.off+i]]) {
				cq.q[b.off+i] = -q[i]
			} else {
				cq.q[b.off+i] = q[i]
			}
		}
	}
	if firstCoded {
		cq.globalGain = 100 // silent channel: any legal value
	}
}
