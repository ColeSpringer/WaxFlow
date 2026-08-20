package ape

// The neural filter, ported from the reference decoder. It is a sign-sign LMS
// adaptive filter: predict the next residual from a window of past ones, then
// nudge every weight by a quantized step whose sign follows the prediction
// error. The compression level chooses how many of these run in series and how
// long each one's window is, and that is the whole difference between the
// levels' quality.

// nnWindow is the roll buffer's stride: a filter walks this many samples
// before copying its history back to the front.
const nnWindow = 512

// rollBuf16 is the filter's sliding window: a flat array with a cursor, so
// index 0 is the sample being written and negative indexes reach back into
// history. It rolls by copying the history to the front rather than by
// wrapping, which keeps the dot product a straight run of memory.
type rollBuf16 struct {
	data []int16
	cur  int
	hist int
}

func newRollBuf16(hist int) rollBuf16 {
	return rollBuf16{data: make([]int16, nnWindow+hist), cur: hist, hist: hist}
}

func (r *rollBuf16) flush() {
	clear(r.data)
	r.cur = r.hist
}

// increment advances one sample, rolling at the end of the window. The
// reference calls this IncrementSafe; its other form skips the bounds test for
// buffers whose owner rolls them on its own schedule.
func (r *rollBuf16) increment() {
	r.cur++
	if r.cur == len(r.data) {
		copy(r.data[:r.hist], r.data[r.cur-r.hist:r.cur])
		r.cur = r.hist
	}
}

// steps is the run updateDelta rewrites: the step being written and the eight
// behind it, so s[stepNow+i] is the reference's own index i. Taking it once
// keeps the bounds checks out of the quantizer; the shortest filter has
// sixteen taps of history, so the slice is always in range.
func (r *rollBuf16) steps() []int16 { return r.data[r.cur-stepNow : r.cur+1] }

// stepNow is index 0 within a step window.
const stepNow = 8

func (r *rollBuf16) set(i int, v int16) { r.data[r.cur+i] = v }

// history returns the run of hist samples behind the cursor, which is what the
// dot product and the weight update both walk.
func (r *rollBuf16) history() []int16 { return r.data[r.cur-r.hist : r.cur] }

// nnFilter is one stage of the cascade.
type nnFilter struct {
	order int
	shift int
	// round is the rounding constant the dot product's shift needs.
	round int32
	// oldDelta selects the pre-3980 weight-step quantizer, which had a single
	// step size and no running average behind it.
	oldDelta bool
	// interim selects the wider accumulator the 24-bit encoders between
	// Monkey's Audio 5.01 and 8.51 used; see interimPredictor.
	interim bool

	m      []int16
	input  rollBuf16
	deltaM rollBuf16
	avg    int32
}

func newNNFilter(order, shift, fileVersion int) *nnFilter {
	return &nnFilter{
		order:    order,
		shift:    shift,
		round:    1 << (shift - 1),
		oldDelta: fileVersion < 3980,
		m:        make([]int16, order),
		input:    newRollBuf16(order),
		deltaM:   newRollBuf16(order),
	}
}

func (f *nnFilter) flush() {
	clear(f.m)
	f.input.flush()
	f.deltaM.flush()
	f.avg = 0
}

// decompress turns one residual into the residual of the stage below it. The
// arithmetic is deliberately int32 and deliberately allowed to wrap: the
// encoder's inverse wraps the same way, so a stream that overflows a weight
// still round-trips.
func (f *nnFilter) decompress(in int32) int32 {
	hist, m := f.input.history(), f.m
	m = m[:len(hist)]
	var dot int32
	for j, v := range hist {
		dot += int32(v) * int32(m[j])
	}
	var out int32
	if f.interim {
		out = in + int32((int64(dot)+int64(f.round))>>f.shift)
	} else {
		out = in + (dot+f.round)>>f.shift
	}

	// Adapt: every weight moves by its sample's quantized step, in the
	// direction that would have reduced the residual we were handed.
	delta := f.deltaM.history()[:len(m)]
	switch {
	case in < 0:
		for j := range m {
			m[j] += delta[j]
		}
	case in > 0:
		for j := range m {
			m[j] -= delta[j]
		}
	}

	f.updateDelta(out)
	f.input.set(0, saturateInt16(out))
	f.input.increment()
	f.deltaM.increment()
	return out
}

// updateDelta re-quantizes the step this sample will contribute to future
// weight updates, and decays the three most recent steps behind it. Which
// three, and how many magnitude bands the quantizer has, is the 3980 change.
func (f *nnFilter) updateDelta(v int32) {
	s := f.deltaM.steps()
	if f.oldDelta {
		if v == 0 {
			s[stepNow] = 0
		} else {
			s[stepNow] = int16(v>>28&8 - 4)
		}
		s[stepNow-4] >>= 1
		s[stepNow-8] >>= 1
		return
	}
	abs := v
	if abs < 0 {
		abs = -abs
	}
	switch {
	case abs > f.avg*3:
		s[stepNow] = int16(v>>25&64 - 32)
	case abs > f.avg*4/3:
		s[stepNow] = int16(v>>26&32 - 16)
	case abs > 0:
		s[stepNow] = int16(v>>27&16 - 8)
	default:
		s[stepNow] = 0
	}
	f.avg += (abs - f.avg) / 16
	s[stepNow-1] >>= 1
	s[stepNow-2] >>= 1
	s[stepNow-8] >>= 1
}

// saturateInt16 narrows a residual to the filter's window width, clamping
// rather than truncating so a spike does not come back as its opposite.
func saturateInt16(v int32) int16 {
	if s := int16(v); int32(s) == v {
		return s
	}
	return int16(v>>31 ^ 0x7fff)
}
