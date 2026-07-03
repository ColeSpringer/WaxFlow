package flac

// Rice residual sizing and partition optimization for the encoder. All
// decisions run on the zigzag magnitude sums per partition: the best
// parameter for a partition follows from its mean, and coarser partition
// orders reuse the finer sums by pairwise addition, so one pass over the
// residual prices every legal partitioning.

// riceEscape4 and riceEscape5 are the parameter values reserved as
// escape codes per coding method; the encoder never writes escapes, so
// they bound the usable parameter range instead.
const (
	riceMaxParam4 = 14 // 4-bit parameters, method 0
	riceMaxParam5 = 30 // 5-bit parameters, method 1
)

// ricePlan is a chosen residual coding: partition order, method, and the
// parameter per partition.
type ricePlan struct {
	partOrder int
	method    int // 0: 4-bit parameters, 1: 5-bit
	params    []uint8
	// bits is the estimated residual section size, excluding the 2+4
	// header. int64 so 32-bit builds agree with 64-bit ones: a
	// pathological predictor blowup can push a partition's shifted sum
	// past 31 bits, and a wrapped estimate would select differently.
	bits int64
}

// riceScratch holds the partition sum pyramid between frames.
type riceScratch struct {
	zig    []uint64 // zigzag residuals
	sums   []uint64 // partition magnitude sums, finest order
	merged []uint64 // coarser sums, rebuilt per order
	params []uint8  // trial parameter buffer
	best   []uint8  // winning parameters
}

// zigzag folds signed residuals into the unsigned mapping Rice codes.
func zigzag(v int64) uint64 {
	return uint64(v<<1) ^ uint64(v>>63)
}

// bestRiceParam returns the cheapest parameter for a partition with the
// given zigzag sum and sample count, and the estimated bits at that
// parameter. The estimate prices the quotient run as sum>>k, exact
// within one bit per sample; every candidate carries the same bias, so
// choices match exact pricing in practice.
func bestRiceParam(sum uint64, count int) (uint, int64) {
	if count == 0 {
		return 0, 0
	}
	k := uint(0)
	for k < riceMaxParam5 && uint64(count)<<(k+1) < sum {
		k++
	}
	bits := func(k uint) int64 {
		return int64(count)*int64(k+1) + int64(sum>>k)
	}
	best, bestBits := k, bits(k)
	if k > 0 {
		if b := bits(k - 1); b < bestBits {
			best, bestBits = k-1, b
		}
	}
	if k < riceMaxParam5 {
		if b := bits(k + 1); b < bestBits {
			best, bestBits = k+1, b
		}
	}
	return best, bestBits
}

// planRice prices res[order:] across partition orders 0..maxPart and
// returns the cheapest plan. blockSize is the frame's sample count; a
// partition order is legal only when it divides the block evenly and the
// first partition retains at least the warmup samples.
func planRice(res []int64, order, blockSize, maxPart int, s *riceScratch) ricePlan {
	// Highest legal partition order.
	top := maxPart
	for top > 0 && (blockSize%(1<<top) != 0 || blockSize>>top < order) {
		top--
	}

	if cap(s.zig) < blockSize {
		s.zig = make([]uint64, blockSize)
		s.sums = make([]uint64, 1<<8)
		s.merged = make([]uint64, 1<<8)
		s.params = make([]uint8, 1<<8)
		s.best = make([]uint8, 1<<8)
	}
	zig := s.zig[:blockSize]
	for i := order; i < blockSize; i++ {
		zig[i] = zigzag(res[i])
	}

	// Partition sums at the finest order; coarser orders merge pairs.
	parts := 1 << top
	size := blockSize >> top
	sums := s.sums[:parts]
	for p := 0; p < parts; p++ {
		start := p * size
		if p == 0 {
			start = order
		}
		var sum uint64
		for _, u := range zig[start : (p+1)*size] {
			sum += u
		}
		sums[p] = sum
	}

	plan := ricePlan{partOrder: -1}
	for po := top; ; po-- {
		parts := 1 << po
		size := blockSize >> po
		total := int64(0)
		maxK := uint(0)
		params := s.params[:parts]
		for p := 0; p < parts; p++ {
			count := size
			if p == 0 {
				count -= order
			}
			k, bits := bestRiceParam(sums[p], count)
			params[p] = uint8(k)
			total += bits
			if k > maxK {
				maxK = k
			}
		}
		method := 0
		paramBits := 4
		if maxK > riceMaxParam4 {
			method = 1
			paramBits = 5
		}
		total += int64(parts * paramBits)
		if plan.partOrder < 0 || total < plan.bits {
			best := s.best[:parts]
			copy(best, params)
			plan = ricePlan{partOrder: po, method: method, params: best, bits: total}
		}
		if po == 0 {
			break
		}
		merged := s.merged[:parts/2]
		for p := range merged {
			merged[p] = sums[2*p] + sums[2*p+1]
		}
		s.sums, s.merged = s.merged, s.sums
		sums = s.sums[:parts/2]
	}
	return plan
}

// writeRice emits the residual section: coding method, partition order,
// and each partition's parameter and Rice-coded residuals.
func (w *bitWriter) writeRice(res []int64, order, blockSize int, plan ricePlan) {
	w.writeBits(2, uint64(plan.method))
	w.writeBits(4, uint64(plan.partOrder))
	paramBits := uint(4 + plan.method)
	size := blockSize >> plan.partOrder
	pos := order
	for p, k := range plan.params {
		w.writeBits(paramBits, uint64(k))
		end := (p + 1) * size
		for _, v := range res[pos:end] {
			u := zigzag(v)
			w.writeUnary(u >> k)
			w.writeBits(uint(k), u)
		}
		pos = end
	}
}
