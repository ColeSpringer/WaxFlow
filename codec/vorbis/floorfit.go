package vorbis

import "math"

// Floor-1 fitting and post encoding: the inverse of floor.go's decode/curve.
// The encoder chooses a target amplitude at each floor post from the spectrum,
// converts it to a dB-table level, then encodes each interior post as the
// differential the decoder's predictive step expects. The resulting posts feed
// back through floor1.curve so the residue divides by exactly the amplitude the
// decoder will reconstruct.

// floorFirst is floor1InverseDB[0]: the smallest amplitude the dB table
// represents. Amplitudes at or below it map to level 0.
const floorFirst = 1.0649863e-07

// logFloorSpan is math.Log(1/floorFirst), the log-amplitude span of the dB table
// (floorFirst..1). It is precomputed once instead of recomputed for every floor
// post; math.Log is a pure function of a constant, so this is the identical
// float64 the per-call form produced and the fit stays byte-for-byte the same.
var logFloorSpan = math.Log(1 / floorFirst)

// floorPartitions groups the floor posts into eight-wide partitions of a single
// class coded by the scalar floor book; floorPosts is the interior post count.
// The count and the warp below set the spectral resolution of the envelope: too
// sparse at high frequency and a tone falling between posts is under-floored, so
// the posts are denser than a plain squared warp and cover the whole band.
const (
	floorPartitions      = 7   // long block: 56 interior posts
	shortFloorPartitions = 2   // short block: 16 interior posts over 128 lines
	floorWarp            = 1.5 // <2 keeps high-frequency posts dense
)

// buildFloor1 constructs a floor geometry with partitions eight-wide post groups
// (8*partitions interior posts) on a power-law warp (denser at low frequency, but
// far gentler than a squared warp so high-frequency tones still land near a
// post), a single class coded by the scalar floor book. Returns the geometry and
// the rangebits the header carries.
func buildFloor1(n2, partitions int) (*floor1, int) {
	rangebits := blockLog(n2) // n2 is a power of two, so 1<<rangebits == n2
	posts := partitions * 8
	xs := make([]int, 0, posts+2)
	xs = append(xs, 0, 1<<rangebits)
	seen := map[int]bool{0: true, 1 << rangebits: true}
	for i := 0; i < posts; i++ {
		frac := float64(i+1) / float64(posts)
		p := int(math.Round(float64(n2-1) * math.Pow(frac, floorWarp)))
		if p < 1 {
			p = 1
		}
		for seen[p] {
			p++
		}
		seen[p] = true
		xs = append(xs, p)
	}
	partClass := make([]int, partitions)
	f := &floor1{
		partitionClass:  partClass,
		classDims:       []int{8},
		classSubclasses: []int{0},
		classMasterbook: []int{0},
		classSubbooks:   [][]int{{bookFloorPosts}},
		multiplier:      1,
		rangeVal:        floor1Ranges[0], // 256, so a level is a dB index directly
		xs:              xs,
	}
	if err := f.computeNeighbors(); err != nil {
		panic("vorbis: floor geometry has duplicate posts: " + err.Error())
	}
	return f, rangebits
}

// ampToY maps a linear amplitude to the nearest floor-1 dB level (the inverse of
// indexing floor1InverseDB), for multiplier 1 where a level is the table index.
func ampToY(amp float64) int {
	if amp <= floorFirst {
		return 0
	}
	y := int(math.Round(255 * math.Log(amp/floorFirst) / logFloorSpan))
	if y < 0 {
		return 0
	}
	if y > 255 {
		return 255
	}
	return y
}

// floor1Fit writes a target level for every post into dst (length len(f.xs)):
// the peak magnitude over an overlapping window spanning to each X-sorted
// neighbour, mapped to a dB level. dst is caller-owned scratch (reused per block).
// A peak (upper) envelope, not an RMS one, is what keeps the floor-normalized
// residue near 1.0 at tonal peaks: the residue books are uniform-step, so a
// large-dynamic-range residue (an RMS floor leaves a strong tone at 5-8x the
// floor) is coded with poor relative precision and the tone is mis-reconstructed.
// The windows overlap (neighbour to neighbour, not the exclusive half-spans) so a
// tone falling between two posts lifts both bracketing posts and the linear-in-dB
// interpolation between them stays at the tone's level, holding the residue near
// 1 even for a peak that no post lands on. floorHeadroom lifts the envelope a
// touch above the raw peak so interpolation dips never push the residue past 1.
func floor1Fit(f *floor1, spec []float32, dst []int, n2 int) {
	count := len(f.xs)
	for si := 0; si < count; si++ {
		i := f.sortOrder[si]
		lo := 0
		if si > 0 {
			lo = f.xs[f.sortOrder[si-1]]
		}
		hi := n2
		if si < count-1 {
			hi = f.xs[f.sortOrder[si+1]]
		}
		if lo < 0 {
			lo = 0
		}
		if hi > n2 {
			hi = n2
		}
		if hi <= lo {
			hi = lo + 1
		}
		var peak float64
		for b := lo; b < hi && b < n2; b++ {
			if v := math.Abs(float64(spec[b])); v > peak {
				peak = v
			}
		}
		dst[i] = ampToY(peak * floorHeadroom)
	}
}

// floorHeadroom lifts the fitted envelope slightly above the measured peak so
// the piecewise-linear floor stays at or above the signal between posts, keeping
// the normalized residue in [0, ~1].
const floorHeadroom = 1.10

// floor1EncodeVals turns per-post target levels into the values the packet
// carries, written into vals (length len(f.xs)): posts 0 and 1 are absolute, each
// later post is the differential from the neighbour-interpolated prediction. It
// reconstructs each post's decoded level into final (caller scratch, same length)
// as it goes so predictions match the decoder's exactly. vals and final are
// caller-owned (reused per block); final is written before it is read.
func floor1EncodeVals(f *floor1, targets, vals, final []int) {
	count := len(f.xs)
	vals[0], vals[1] = targets[0], targets[1]
	final[0], final[1] = targets[0], targets[1]
	rng := f.rangeVal
	for i := 2; i < count; i++ {
		low, high := f.lowNeighbor[i], f.highNeighbor[i]
		pred := renderPoint(f.xs[low], final[low], f.xs[high], final[high], f.xs[i])
		vals[i] = floor1EncodeVal(pred, targets[i], rng)
		final[i] = floor1DecodeVal(pred, vals[i], rng)
	}
}

// floor1EncodeVal is the inverse of floor1DecodeVal: the value to carry so the
// decoder reconstructs target given prediction pred. It matches the decoder's
// room test so the decoder takes the intended branch.
func floor1EncodeVal(pred, target, rng int) int {
	if target == pred {
		return 0
	}
	highroom := rng - pred
	lowroom := pred
	room := 2 * lowroom
	if highroom < lowroom {
		room = 2 * highroom
	}
	var val int
	if target > pred {
		val = 2 * (target - pred)
	} else {
		val = 2*(pred-target) - 1
	}
	if val < room {
		return val
	}
	// Beyond room: the decoder's out-of-room encoding, independent of direction.
	if highroom > lowroom {
		return target - pred + lowroom
	}
	return pred - target + highroom - 1
}

// floor1DecodeVal reconstructs a post's level from its carried value and
// prediction, the exact arithmetic of floor1.curve's per-post step, so the
// encoder can predict from decoded neighbours.
func floor1DecodeVal(pred, val, rng int) int {
	if val == 0 {
		return pred
	}
	highroom := rng - pred
	lowroom := pred
	room := 2 * lowroom
	if highroom < lowroom {
		room = 2 * highroom
	}
	switch {
	case val >= room:
		if highroom > lowroom {
			return val - lowroom + pred
		}
		return pred - val + highroom - 1
	case val&1 == 1:
		return pred - (val+1)/2
	default:
		return pred + val/2
	}
}

// writeFloorData emits one channel's floor-1 packet data (inverse of
// floor1.decode): the present bit, the two absolute posts, then each interior
// post's differential through the scalar floor book, in xs order.
func writeFloorData(w *bitWriter, f *floor1, vals []int, book *encBook) {
	w.writeBit(1) // floor present (the encoder never emits a silent channel)
	ilr := ilog(f.rangeVal - 1)
	w.writeBits(uint(ilr), uint32(vals[0]))
	w.writeBits(uint(ilr), uint32(vals[1]))
	for i := 2; i < len(f.xs); i++ {
		book.emit(w, vals[i])
	}
}
