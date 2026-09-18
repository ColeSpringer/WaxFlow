// Package mix converts channel layouts with per-stream gain matrices.
// Matrices are energy-normalized: any output row whose summed power gain
// would exceed unity is scaled back to unit energy, so downmixes preserve
// loudness for uncorrelated content and rarely clip. Rarely is not never:
// correlated content can still sum past full scale, which is why the
// chain inserts the true-peak limiter whenever MaxGain reports a row
// whose worst-case sum exceeds unity (protection is by
// analysis, not hope).
//
// The position gain table follows ITU-R BS.775 conventions: full passes
// to matching sides, -3 dB (1/sqrt(2)) for center and surround content
// folded into a side, -6 dB for positions split twice. LFE is dropped on
// downmix, the common default for music delivery: bass management is a
// playback-system decision, and blindly summing LFE doubles bass on
// systems that already fold it. Mono to stereo duplicates at unity.
//
// Widening is the other direction and follows no gain table at all: every
// source position lands on the same position of the target at unity and the
// rest are silent, so a narrower stream keeps its own levels inside a wider
// envelope. Placement is measurement-neutral (channel powers sum under
// BS.1770, and the positions that gained no signal contribute none), so a
// widened member measures the same loudness alone as inside the envelope.
// Duplication is not, and mono is the case that duplicates: a lone channel is
// placed rather than matched, on the target's center where it has one, and
// copied across the front pair where it has none, which is +10*log10(2).
// A source position the target has no place for is refused: this normalizes
// channel counts, not speaker assignments.
package mix

import (
	"fmt"
	"math"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

// Version is this node's algorithm revision for cache keys: bump on any
// change to the gain table or normalization (ADR-0004).
//
// The widening arm did not bump it: it changed no matrix that existed, since
// every pair it answers was a refusal before, and a refusal has no cached
// output to key against.
const Version = "mix-1"

// Matrix is an immutable channel conversion: out[o] = sum_i coef[o][i] * in[i].
type Matrix struct {
	coef     [][]float32
	src, dst audio.ChannelMask
}

const (
	db3 = 0.7071067811865476 // -3 dB, 1/sqrt(2)
	db6 = 0.5                // -6 dB
	db9 = 0.3535533905932738 // -9 dB
)

// stereoGain maps each WAVE_FORMAT_EXTENSIBLE position to its (left,
// right) downmix contribution. Mono folds through this table too, as
// db3*(l+r), so a center position reaches mono at unity.
var stereoGain = map[audio.ChannelMask][2]float64{
	audio.FrontLeft:          {1, 0},
	audio.FrontRight:         {0, 1},
	audio.FrontCenter:        {db3, db3},
	audio.LowFrequency:       {0, 0},
	audio.BackLeft:           {db3, 0},
	audio.BackRight:          {0, db3},
	audio.FrontLeftOfCenter:  {db3, 0},
	audio.FrontRightOfCenter: {0, db3},
	audio.BackCenter:         {db6, db6},
	audio.SideLeft:           {db3, 0},
	audio.SideRight:          {0, db3},
	audio.TopCenter:          {db6, db6},
	audio.TopFrontLeft:       {db3, 0},
	audio.TopFrontCenter:     {db6, db6},
	audio.TopFrontRight:      {0, db3},
	audio.TopBackLeft:        {db6, 0},
	audio.TopBackCenter:      {db9, db9},
	audio.TopBackRight:       {0, db6},
}

// For builds the conversion matrix from src to dst. Mono and stereo targets
// downmix through the gain table; a wider target takes the source's positions
// as they are, at unity, with the rest zero-filled. Equal layouts are
// refused: the caller decides identity, no-op nodes are never built, and so
// is a target with no place for one of the source's positions.
func For(src, dst audio.ChannelMask) (*Matrix, error) {
	if src == 0 || dst == 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "mix: unknown layout")
	}
	if src == dst {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("mix: source and target layout are both %v", src))
	}

	positions := maskPositions(src)
	var rows [][]float64
	switch {
	case src.Count() == 1 && dst == audio.FrontLeft|audio.FrontRight:
		// Mono to stereo: duplicate at unity, the least surprising
		// convention (level matches the mono original on either speaker).
		rows = [][]float64{{1}, {1}}
	case dst == audio.FrontLeft|audio.FrontRight:
		l := make([]float64, len(positions))
		r := make([]float64, len(positions))
		for i, p := range positions {
			g, ok := stereoGain[p]
			if !ok {
				return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
					fmt.Sprintf("mix: no downmix gain for position %v", p))
			}
			l[i], r[i] = g[0], g[1]
		}
		rows = [][]float64{l, r}
	case dst == audio.FrontCenter:
		m := make([]float64, len(positions))
		for i, p := range positions {
			g, ok := stereoGain[p]
			if !ok {
				return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
					fmt.Sprintf("mix: no downmix gain for position %v", p))
			}
			m[i] = db3 * (g[0] + g[1])
		}
		rows = [][]float64{m}
	case src.Count() == 1 && dst&audio.FrontCenter != 0:
		// A single channel has no layout to preserve, so it is placed rather
		// than matched, whatever its own mask calls it. The center is the
		// ITU placement, it folds back through stereoGain as the standard
		// BS.775 downmix, and it keeps the widening measurement-neutral:
		// channel powers sum, so a mono member measures the same loudness
		// alone and inside a 5.1 envelope.
		rows = onlyRows(dst, audio.FrontCenter)
	case src.Count() == 1 && dst&frontPair == frontPair:
		// No center to land on (quad): duplicate across the front pair at
		// unity, which is the mono-to-stereo convention one target wider.
		rows = onlyRows(dst, frontPair)
	case src&^dst == 0:
		// Every source position exists in the target: place each on its own
		// and leave the rest silent. Unity throughout, so no row's energy
		// changes, MaxGain is 1, and no limiter engages.
		rows = placeRows(positions, dst)
	default:
		// Reached whenever the target lacks one of the source's positions,
		// which covers a narrowing to a multichannel target as well as a
		// widening across an incompatible rear. Naming the positions is the
		// only thing a caller can act on.
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("mix: %v has no place for %v, which %v holds", dst, src&^dst, src))
	}

	// Energy normalization: rows are scaled down to unit power gain,
	// never up, so quiet mixes stay untouched.
	coef := make([][]float32, len(rows))
	for o, row := range rows {
		var energy float64
		for _, g := range row {
			energy += g * g
		}
		scale := 1.0
		if energy > 1 {
			scale = 1 / math.Sqrt(energy)
		}
		coef[o] = make([]float32, len(row))
		for i, g := range row {
			coef[o][i] = float32(g * scale)
		}
	}
	return &Matrix{coef: coef, src: src, dst: dst}, nil
}

// In and Out report the matrix's channel counts.
func (m *Matrix) In() int  { return len(m.coef[0]) }
func (m *Matrix) Out() int { return len(m.coef) }

// MaxGain returns the worst-case peak gain, the largest row sum of
// absolute coefficients. Above 1, fully correlated full-scale input can
// clip and the chain must protect with the limiter.
func (m *Matrix) MaxGain() float64 {
	var worst float64
	for _, row := range m.coef {
		var sum float64
		for _, g := range row {
			sum += math.Abs(float64(g))
		}
		worst = math.Max(worst, sum)
	}
	return worst
}

// Apply converts n frames from src channels into dst channels. Slices
// follow the DSP kernel convention: one contiguous []float32 per channel.
// dst and src must not alias.
func (m *Matrix) Apply(dst, src [][]float32, n int) {
	if len(dst) != m.Out() || len(src) != m.In() {
		panic(fmt.Sprintf("mix: Apply with %d in / %d out slices on a %d->%d matrix",
			len(src), len(dst), m.In(), m.Out()))
	}
	for o, row := range m.coef {
		out := dst[o][:n]
		clear(out)
		for i, g := range row {
			if g == 0 {
				continue
			}
			in := src[i][:n]
			for j := range out {
				out[j] += g * in[j]
			}
		}
	}
}

// frontPair is the stereo front, the fallback placement for a lone channel in
// a target with no center of its own.
const frontPair = audio.FrontLeft | audio.FrontRight

// onlyRows feeds a single source channel to the target positions in on, at
// unity, and silences the rest.
func onlyRows(dst, on audio.ChannelMask) [][]float64 {
	rows := make([][]float64, 0, dst.Count())
	for _, p := range maskPositions(dst) {
		g := 0.0
		if p&on != 0 {
			g = 1
		}
		rows = append(rows, []float64{g})
	}
	return rows
}

// placeRows puts each source position on the same position of the target and
// zero-fills the rest. src is the source's positions in channel order, which
// is the order its coefficient columns are in.
func placeRows(src []audio.ChannelMask, dst audio.ChannelMask) [][]float64 {
	rows := make([][]float64, 0, dst.Count())
	for _, p := range maskPositions(dst) {
		row := make([]float64, len(src))
		for i, s := range src {
			if s == p {
				row[i] = 1
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// maskPositions expands a mask into positions in channel order
// (ascending bit position, the audio package convention).
func maskPositions(m audio.ChannelMask) []audio.ChannelMask {
	out := make([]audio.ChannelMask, 0, m.Count())
	for bit := 0; bit < 32; bit++ {
		if m&(1<<bit) != 0 {
			out = append(out, 1<<bit)
		}
	}
	return out
}
