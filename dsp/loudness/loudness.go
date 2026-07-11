// Package loudness implements the ITU-R BS.1770-4 / EBU R128 loudness
// meter behind the engine's analysis jobs: gated integrated loudness,
// loudness range per EBU Tech 3342, and oversampled true peak. It is a
// pure streaming analyzer over planar float32 PCM, not a pipeline
// Stage: the meter consumes chunks and produces only numbers.
//
// Measurement follows the standard's structure: per-channel K-weighting
// (two biquads derived for the meter's sample rate), mean-square energy
// over 400 ms blocks hopped every 100 ms, a weighted channel sum
// (surround positions raised, LFE excluded), and two-stage gating
// (absolute at -70 LUFS, relative 10 LU under the ungated mean) for the
// integrated value. The loudness range applies the same machinery to
// 3 s windows with a 20 LU relative gate and takes the spread between
// the 10th and 95th percentiles of the surviving distribution.
//
// Filter state and accumulation run in float64; chunks may be any
// length, including a single sample. A Meter carries per-channel state
// and is not safe for concurrent use.
package loudness

import (
	"fmt"
	"math"
	"slices"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

// Version identifies the meter algorithm revision (ADR-0004 style).
// WaxFlow itself computes loudness fresh per job, but external callers
// that persist measurements (WaxBin's catalog is the anticipated one)
// should store it alongside their results so a meter revision
// invalidates them.
const Version = "r128-1"

// The gating windows in 100 ms sub-blocks: both the 400 ms momentary
// window and the 3 s short-term window advance on the common 100 ms hop
// (75 percent overlap for momentary per BS.1770-4, the Tech 3342 update
// rate for short-term), so the meter accumulates energy once per
// sub-block and sums sub-blocks per window.
const (
	momSub = 4
	stSub  = 30
)

// loudnessOffset calibrates block loudness: L = loudnessOffset +
// 10 log10(power). The constant makes a 0 dBFS 997 Hz sine in one
// channel read -3.01 LKFS, the BS.1770 anchor.
const loudnessOffset = -0.691

// absGate is the absolute gate in LUFS; blocks at or below it never
// enter any measurement.
const absGate = -70.0

// absGatePower is the absolute gate as a linear channel-sum power, the
// domain blocks are stored in.
var absGatePower = math.Pow(10, (absGate-loudnessOffset)/10)

// Meter measures one stream. Feed planar chunks to Process, call Flush
// after the last one, then read the results. The result methods may
// also be read mid-stream; only the true-peak tail depends on Flush.
// Not safe for concurrent use.
type Meter struct {
	rate     int
	channels int
	weights  []float64 // BS.1770 channel weights; 0 excludes (LFE)

	shelf, hp biquad   // K-weighting stages, shared by all channels
	state     []kState // per-channel filter memory

	// The current 100 ms sub-block: subFill samples of each channel's
	// K-weighted energy accumulated so far, and a per-channel ring of
	// the last stSub completed sub-block energies the windows sum over.
	subLen  int
	subFill int
	subAcc  []float64
	ring    [][]float64
	ringPos int
	ringCnt int64

	// Channel-sum powers of every window that passed the absolute gate,
	// kept whole for the relative gate: one float64 per 100 ms of audio.
	blocks []float64 // 400 ms momentary, for Integrated
	st     []float64 // 3 s short-term, for Range

	tp    *truePeak // nil above 192 kHz (no oversampling)
	maxSP float64
	maxTP float64

	flushed bool
}

// NewMeter returns a meter for the given sample rate, channel count, and
// channel layout. Layout 0 assumes the first channels in canonical order
// (mono, stereo, etc.). Returns an error for rate <= 0, channels <= 0,
// or channels > 8.
func NewMeter(rate, channels int, layout audio.ChannelMask) (*Meter, error) {
	if rate <= 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("loudness: meter rate %d must be positive", rate))
	}
	if channels <= 0 || channels > 8 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("loudness: meter channel count %d outside 1..8", channels))
	}
	if layout == 0 {
		layout = audio.DefaultLayout(channels)
	}
	m := &Meter{
		rate:     rate,
		channels: channels,
		weights:  channelWeights(channels, layout),
		state:    make([]kState, channels),
		// One 100 ms sub-block, by integer division: a rate not divisible
		// by ten (11025 Hz) yields a sub-block a fraction of a millisecond
		// short, so gating windows skew by under 0.05 percent. The
		// consumer rates are all multiples of ten; the skew is noted, not
		// compensated.
		subLen: max(rate/10, 1),
		subAcc: make([]float64, channels),
		ring:   make([][]float64, channels),
		tp:     newTruePeak(rate, channels),
	}
	m.shelf, m.hp = kWeighting(rate)
	for c := range m.ring {
		m.ring[c] = make([]float64, stSub)
	}
	return m, nil
}

// channelWeights maps a layout onto per-channel BS.1770-4 weights,
// assigning mask bits to channels in ascending bit order: LFE weight 0
// (excluded from the sum), back and side positions 1.41, every other
// position 1.0. Channels beyond the mask's assigned positions, and
// positions the mask leaves unknown, default to 1.0. This matches the
// mapping ffmpeg's ebur128 filter applies, so the differential oracle
// and the meter agree on multichannel content.
func channelWeights(channels int, layout audio.ChannelMask) []float64 {
	w := make([]float64, channels)
	for i := range w {
		w[i] = 1
	}
	c := 0
	for bit := 0; bit < 32 && c < channels; bit++ {
		mask := audio.ChannelMask(1) << bit
		if layout&mask == 0 {
			continue
		}
		switch mask {
		case audio.LowFrequency:
			w[c] = 0
		case audio.BackLeft, audio.BackRight, audio.BackCenter,
			audio.SideLeft, audio.SideRight:
			w[c] = 1.41
		}
		c++
	}
	return w
}

// Process consumes one chunk of planar float32 PCM: chans[c][i] is
// sample i of channel c. All channel slices must be the same length.
// Values are nominal full scale +-1.0.
func (m *Meter) Process(chans [][]float32) error {
	if m.flushed {
		return waxerr.New(waxerr.CodeInvalidRequest, "loudness: Process after Flush")
	}
	if len(chans) != m.channels {
		return waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("loudness: chunk has %d channels, meter expects %d", len(chans), m.channels))
	}
	n := len(chans[0])
	for _, ch := range chans[1:] {
		if len(ch) != n {
			return waxerr.New(waxerr.CodeInvalidRequest, "loudness: channel slices differ in length")
		}
	}
	// Walk the chunk in segments bounded by sub-block edges, so window
	// accounting happens between segments and never inside the hot loop.
	for off := 0; off < n; {
		take := n - off
		if rem := m.subLen - m.subFill; take > rem {
			take = rem
		}
		for c := range chans {
			m.consume(c, chans[c][off:off+take])
		}
		m.subFill += take
		off += take
		if m.subFill == m.subLen {
			m.finishSubBlock()
		}
	}
	return nil
}

// consume runs one channel's segment through the peak trackers and the
// K-weighting chain, accumulating the current sub-block's energy.
func (m *Meter) consume(c int, seg []float32) {
	st := &m.state[c]
	acc := m.subAcc[c]
	maxSP, maxTP := m.maxSP, m.maxTP
	for _, s := range seg {
		x := float64(s)
		a := math.Abs(x)
		if a > maxSP {
			maxSP = a
		}
		if m.tp != nil {
			if p := m.tp.push(c, x); p > maxTP {
				maxTP = p
			}
		} else if a > maxTP {
			maxTP = a
		}
		// The two K-weighting stages, direct form II transposed.
		y := m.shelf.b0*x + st.s1a
		st.s1a = m.shelf.b1*x - m.shelf.a1*y + st.s1b
		st.s1b = m.shelf.b2*x - m.shelf.a2*y
		z := m.hp.b0*y + st.s2a
		st.s2a = m.hp.b1*y - m.hp.a1*z + st.s2b
		st.s2b = m.hp.b2*y - m.hp.a2*z
		acc += z * z
	}
	m.subAcc[c] = acc
	m.maxSP, m.maxTP = maxSP, maxTP
}

// finishSubBlock retires the completed 100 ms sub-block into the ring
// and emits whichever windows it completes, keeping only powers past
// the absolute gate (blocks below it can never contribute).
func (m *Meter) finishSubBlock() {
	for c := range m.subAcc {
		m.ring[c][m.ringPos] = m.subAcc[c]
		m.subAcc[c] = 0
	}
	m.ringPos = (m.ringPos + 1) % stSub
	m.ringCnt++
	m.subFill = 0
	if m.ringCnt >= momSub {
		if p := m.windowPower(momSub); p > absGatePower {
			m.blocks = append(m.blocks, p)
		}
	}
	if m.ringCnt >= stSub {
		if p := m.windowPower(stSub); p > absGatePower {
			m.st = append(m.st, p)
		}
	}
}

// windowPower is the weighted channel-sum mean square over the last n
// sub-blocks: the value inside BS.1770's 10 log10, before the offset.
// Each window re-sums its sub-blocks from the ring, so no running sum
// can drift over long streams.
func (m *Meter) windowPower(n int) float64 {
	var sum float64
	for c, w := range m.weights {
		if w == 0 {
			continue
		}
		ring := m.ring[c]
		var s float64
		for k := 1; k <= n; k++ {
			s += ring[(m.ringPos-k+stSub)%stSub]
		}
		sum += w * s
	}
	return sum / float64(n*m.subLen)
}

// Flush finalizes measurement. The loudness windows advance on whole
// 100 ms sub-blocks (a partial final sub-block is discarded, matching
// the standard's complete-block gating), so the only buffered state is
// the true-peak interpolator's half-window tail; Flush drains it. Flush
// is idempotent; Process must not be called after it.
func (m *Meter) Flush() {
	if m.flushed {
		return
	}
	m.flushed = true
	if m.tp != nil {
		if p := m.tp.drain(); p > m.maxTP {
			m.maxTP = p
		}
	}
}

// Integrated returns the gated integrated loudness in LUFS per
// BS.1770-4. Returns math.Inf(-1) when no block passed the absolute
// gate (silence).
func (m *Meter) Integrated() float64 {
	if len(m.blocks) == 0 {
		return math.Inf(-1)
	}
	var sum float64
	for _, p := range m.blocks {
		sum += p
	}
	// The relative gate sits 10 LU below the power mean of the
	// absolute-gated blocks: a factor 10 in power, the loudness offset
	// cancelling on both sides of the comparison.
	thresh := sum / float64(len(m.blocks)) / 10
	var gated float64
	var n int
	for _, p := range m.blocks {
		if p > thresh {
			gated += p
			n++
		}
	}
	if n == 0 {
		return math.Inf(-1)
	}
	return loudnessOffset + 10*math.Log10(gated/float64(n))
}

// Range returns the loudness range (LRA) in LU per EBU Tech 3342.
// Returns 0 when there is not enough audio.
func (m *Meter) Range() float64 {
	if len(m.st) == 0 {
		return 0
	}
	var sum float64
	for _, p := range m.st {
		sum += p
	}
	// Tech 3342 gates 20 LU below the power mean, a factor 100.
	thresh := sum / float64(len(m.st)) / 100
	gated := make([]float64, 0, len(m.st))
	for _, p := range m.st {
		if p >= thresh {
			gated = append(gated, p)
		}
	}
	if len(gated) < 2 {
		return 0
	}
	slices.Sort(gated)
	lo := gated[percentileIndex(len(gated), 0.10)]
	hi := gated[percentileIndex(len(gated), 0.95)]
	return 10 * math.Log10(hi/lo)
}

// percentileIndex is the nearest-rank index into n sorted values,
// index = round(f*(n-1)), the libebur128 convention.
func percentileIndex(n int, f float64) int {
	return int(f*float64(n-1) + 0.5)
}

// TruePeak returns the maximum true-peak level in dBTP (oversampled per
// BS.1770-4 Annex 2; rates above 192 kHz are dense enough that the
// sample grid is used directly). Complete only after Flush, which
// drains the interpolator tail. Returns math.Inf(-1) for silence.
func (m *Meter) TruePeak() float64 {
	return dbOrNegInf(m.maxTP)
}

// SamplePeak returns the maximum absolute sample level in dBFS, or
// math.Inf(-1) for silence.
func (m *Meter) SamplePeak() float64 {
	return dbOrNegInf(m.maxSP)
}

func dbOrNegInf(v float64) float64 {
	if v <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(v)
}
