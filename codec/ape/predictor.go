package ape

// The predictor, ported from the reference. Three stages: a fixed first-order
// lift, a short adaptive filter over the channel's own history and the other
// channel's, then the neural cascade. Decoding runs them back to front. Each
// channel has its own predictor and every frame flushes both, which is what
// makes a frame independently decodable.

// predWindow is the predictor's roll-buffer stride, and predHistory the
// samples it keeps behind the cursor.
const (
	predWindow  = 256
	predHistory = 8
)

// rollBuf32 is rollBuf16 at the predictor's width. The two are separate types
// rather than one generic because the filter's window is the hot loop and
// wants a concrete element type.
type rollBuf32 struct {
	data []int32
	cur  int
}

func newRollBuf32() rollBuf32 {
	return rollBuf32{data: make([]int32, predWindow+predHistory), cur: predHistory}
}

func (r *rollBuf32) flush() {
	clear(r.data)
	r.cur = predHistory
}

func (r *rollBuf32) increment() {
	r.cur++
	if r.cur == len(r.data) {
		copy(r.data[:predHistory], r.data[r.cur-predHistory:r.cur])
		r.cur = predHistory
	}
}

// window is the run the predictor works over: the sample being written and the
// four behind it, so w[now+i] is the reference's own index i. Taking it once
// per sample is what keeps the bounds checks out of the arithmetic below;
// predHistory is 8, so the slice is always in range.
func (r *rollBuf32) window() []int32 { return r.data[r.cur-4 : r.cur+1] }

// now is index 0 within a window.
const now = 4

// scaledFilter is the fixed first-order lift the encoder applies before
// anything adaptive: subtract 31/32 of the previous sample.
type scaledFilter struct{ last int32 }

func (f *scaledFilter) flush() { f.last = 0 }

func (f *scaledFilter) compress(in int32) int32 {
	out := in - f.last*31>>5
	f.last = in
	return out
}

func (f *scaledFilter) decompress(in int32) int32 {
	f.last = in + f.last*31>>5
	return f.last
}

// predictor is one channel's inverse prediction chain.
type predictor struct {
	// nn is the filter cascade in decode order: the stage the encoder ran
	// last is undone first. Unused stages are nil, and fast uses none.
	nn [3]*nnFilter

	predA, predB   rollBuf32
	adaptA, adaptB rollBuf32
	stage1A        scaledFilter
	stage1B        scaledFilter

	mA [4]int32
	mB [5]int32

	lastA int32
	// wide selects the accumulator width of the interim 24-bit encoders.
	wide bool
}

// filterCascade is the level's filter chain, listed in encode order: order
// then shift for each stage. The decoder runs them back to front.
func filterCascade(level int) [][2]int {
	switch level {
	case LevelFast:
		return nil
	case LevelNormal:
		return [][2]int{{16, 11}}
	case LevelHigh:
		return [][2]int{{64, 11}}
	case LevelExtraHigh:
		return [][2]int{{256, 13}, {32, 10}}
	case LevelInsane:
		return [][2]int{{1024 + 256, 15}, {256, 13}, {16, 11}}
	}
	return nil
}

func newPredictor(cfg Config) *predictor {
	p := &predictor{
		predA:  newRollBuf32(),
		predB:  newRollBuf32(),
		adaptA: newRollBuf32(),
		adaptB: newRollBuf32(),
	}
	for i, stage := range filterCascade(cfg.CompressionLevel) {
		p.nn[i] = newNNFilter(stage[0], stage[1], cfg.FileVersion)
	}
	return p
}

// setInterim switches the predictor and its filters between the ordinary
// accumulators and the wider ones the 24-bit encoders shipped with Monkey's
// Audio 5.01 through 8.51 used, a spell when adding 32-bit support
// unintentionally changed how 24-bit was coded. The reference reaches for them
// the same way: only after a frame of a 24-bit file has already failed its CRC.
func (p *predictor) setInterim(on bool) {
	p.wide = on
	for _, f := range p.nn {
		if f != nil {
			f.interim = on
		}
	}
}

func (p *predictor) flush() {
	for _, f := range p.nn {
		if f != nil {
			f.flush()
		}
	}
	p.predA.flush()
	p.predB.flush()
	p.adaptA.flush()
	p.adaptB.flush()
	p.stage1A.flush()
	p.stage1B.flush()
	p.mA = [4]int32{360, 317, -109, 98}
	p.mB = [5]int32{}
	p.lastA = 0
}

// decompress turns one coded residual into one sample. b is the other
// channel's sample for this block, which is what the second half of the
// adaptive filter predicts from; mono and the pseudo-stereo frames pass zero.
//
// The arithmetic wraps at 32 bits throughout, which is not incidental: the
// reference accumulates the 24-bit case in 64 bits and then truncates, and
// truncating a 64-bit sum of products is the same number as summing them at 32
// bits, so one path serves both. The interim encoders are the exception, and
// they get the wider path.
func (p *predictor) decompress(a, b int32) int32 {
	// The neural stages come off in the reverse of the order they went on.
	for i := len(p.nn) - 1; i >= 0; i-- {
		if p.nn[i] != nil {
			a = p.nn[i].decompress(a)
		}
	}

	pa, pb := p.predA.window(), p.predB.window()
	pa[now] = p.lastA
	pa[now-1] = pa[now] - pa[now-1]
	pb[now] = p.stage1B.compress(b)
	pb[now-1] = pb[now] - pb[now-1]

	var current int32
	if p.wide {
		predA := int64(pa[now])*int64(p.mA[0]) + int64(pa[now-1])*int64(p.mA[1]) +
			int64(pa[now-2])*int64(p.mA[2]) + int64(pa[now-3])*int64(p.mA[3])
		predB := int64(pb[now])*int64(p.mB[0]) + int64(pb[now-1])*int64(p.mB[1]) +
			int64(pb[now-2])*int64(p.mB[2]) + int64(pb[now-3])*int64(p.mB[3]) +
			int64(pb[now-4])*int64(p.mB[4])
		current = a + int32((predA+predB>>1)>>10)
	} else {
		predA := pa[now]*p.mA[0] + pa[now-1]*p.mA[1] + pa[now-2]*p.mA[2] + pa[now-3]*p.mA[3]
		predB := pb[now]*p.mB[0] + pb[now-1]*p.mB[1] + pb[now-2]*p.mB[2] +
			pb[now-3]*p.mB[3] + pb[now-4]*p.mB[4]
		current = a + (predA+predB>>1)>>10
	}

	// The weight step is the sample's sign, and the direction the residual's,
	// so a weight that helped grows and one that hurt shrinks.
	aa, ab := p.adaptA.window(), p.adaptB.window()
	aa[now] = adaptStep(pa[now])
	aa[now-1] = adaptStep(pa[now-1])
	ab[now] = adaptStep(pb[now])
	ab[now-1] = adaptStep(pb[now-1])

	var dir int32
	switch {
	case a < 0:
		dir = 1
	case a > 0:
		dir = -1
	}
	p.mA[0] += aa[now] * dir
	p.mA[1] += aa[now-1] * dir
	p.mA[2] += aa[now-2] * dir
	p.mA[3] += aa[now-3] * dir
	p.mB[0] += ab[now] * dir
	p.mB[1] += ab[now-1] * dir
	p.mB[2] += ab[now-2] * dir
	p.mB[3] += ab[now-3] * dir
	p.mB[4] += ab[now-4] * dir

	out := p.stage1A.decompress(current)
	p.lastA = current

	p.predA.increment()
	p.predB.increment()
	p.adaptA.increment()
	p.adaptB.increment()
	return out
}

// compress turns one sample into one coded residual: decompress read back to
// front. b is the other channel's sample for this block, as it is there.
//
// The two run the same arithmetic on the same state in the same order, so
// anything that wraps here wraps identically on the way back. The one
// asymmetry worth naming is the neural cascade: it goes on last and comes off
// first, which is why the loop below runs forward and decompress's runs
// backward.
func (p *predictor) compress(a, b int32) int32 {
	nA := p.stage1A.compress(a)

	pa, pb := p.predA.window(), p.predB.window()
	pa[now] = p.lastA
	pa[now-1] = pa[now] - pa[now-1]
	pb[now] = p.stage1B.compress(b)
	pb[now-1] = pb[now] - pb[now-1]

	predA := pa[now]*p.mA[0] + pa[now-1]*p.mA[1] + pa[now-2]*p.mA[2] + pa[now-3]*p.mA[3]
	predB := pb[now]*p.mB[0] + pb[now-1]*p.mB[1] + pb[now-2]*p.mB[2] +
		pb[now-3]*p.mB[3] + pb[now-4]*p.mB[4]
	out := nA - (predA+predB>>1)>>10

	aa, ab := p.adaptA.window(), p.adaptB.window()
	aa[now] = adaptStep(pa[now])
	aa[now-1] = adaptStep(pa[now-1])
	ab[now] = adaptStep(pb[now])
	ab[now-1] = adaptStep(pb[now-1])

	// The direction is the residual's sign, as it is in decompress: there the
	// residual arrives from the coder, here it has just been computed.
	var dir int32
	switch {
	case out < 0:
		dir = 1
	case out > 0:
		dir = -1
	}
	p.mA[0] += aa[now] * dir
	p.mA[1] += aa[now-1] * dir
	p.mA[2] += aa[now-2] * dir
	p.mA[3] += aa[now-3] * dir
	p.mB[0] += ab[now] * dir
	p.mB[1] += ab[now-1] * dir
	p.mB[2] += ab[now-2] * dir
	p.mB[3] += ab[now-3] * dir
	p.mB[4] += ab[now-4] * dir

	p.lastA = nA

	p.predA.increment()
	p.predB.increment()
	p.adaptA.increment()
	p.adaptB.increment()

	for _, f := range p.nn {
		if f != nil {
			out = f.compress(out)
		}
	}
	return out
}

// adaptStep is the sign of v inverted, and zero for zero: the step a weight
// takes when the residual says it should move.
func adaptStep(v int32) int32 {
	if v == 0 {
		return 0
	}
	return v>>30&2 - 1
}
