// Package dither requantizes float PCM to integer bit depths. Plain
// truncation correlates the rounding error with the signal and turns it
// into harmonic distortion; TPDF dither of the right amplitude (two LSBs
// peak to peak) decorrelates it completely, leaving a flat noise floor.
// Optional noise shaping moves that floor's energy out of the ear's most
// sensitive band.
//
// Everything is deterministic: the dither PRNG is seeded explicitly and
// each channel derives an independent stream from the seed, so identical
// input, seed, and settings produce identical output regardless of how
// the stream is chunked. Golden-stream tests and byte-identical cache
// regeneration depend on this (deterministic mode).
package dither

import (
	"fmt"
	"math"
	"math/rand/v2"

	"github.com/colespringer/waxflow/waxerr"
)

// Version is this node's algorithm revision for cache keys: bump on any
// change to the dither, shaping coefficients, or rounding (plan
// section 10).
const Version = "dither-1"

// DefaultSeed seeds deterministic-mode quantizers. There is nothing
// special about the value; it is fixed so that goldens never flake.
const DefaultSeed uint64 = 1

// Shaping selects the quantizer's noise strategy.
type Shaping uint8

const (
	// TPDF is flat triangular dither, the default: statistically ideal
	// decorrelation, no spectral coloring, works at every rate.
	TPDF Shaping = iota
	// Shaped adds F-weighted error feedback (Lipshitz, Vanderkooy and
	// Wannamaker, JAES 1991, 5-tap): about 11 dB more total noise power,
	// but pushed above 15 kHz where hearing is least sensitive, buying
	// roughly two bits of audible dynamic range. The coefficients suit
	// exactly the rates SupportsShaping accepts; the chain falls back to
	// TPDF elsewhere.
	Shaped
	// None quantizes by plain rounding. Only defensible when the source
	// is known to be already quantized at the target depth (a widening
	// path), or for measurement; audible material gets distortion.
	None
)

func (s Shaping) String() string {
	switch s {
	case TPDF:
		return "tpdf"
	case Shaped:
		return "shaped"
	case None:
		return "none"
	default:
		return fmt.Sprintf("Shaping(%d)", uint8(s))
	}
}

// fWeighted is the Lipshitz 5-tap F-weighted noise shaping filter: the
// error feedback convolution kernel, newest error first. Values are the
// published table (Tier A academic source, clean-room policy ADR-0001).
var fWeighted = [5]float64{2.033, -2.165, 1.959, -1.590, 0.6149}

// SupportsShaping reports whether Shaped's noise shaping filter suits an
// output rate. A shaping curve places noise against the ear's threshold
// in absolute frequency, so it is a function of the exact rate: the
// published table was fitted for 44.1 kHz playback and holds at 48 kHz,
// and nowhere else, high-rate family members included (at 96 kHz the
// same taps would waste the shaping on the inaudible top octaves).
// Shaped requests at other rates should fall back to TPDF, which is
// correct everywhere; rate-specific tables can widen this later.
func SupportsShaping(rate int) bool {
	return rate == 44100 || rate == 48000
}

// Quantizer converts float32 samples in [-1, 1) to integers of a target
// bit depth, per channel. It is deterministic and not safe for
// concurrent use.
type Quantizer struct {
	bits    int
	shaping Shaping
	seed    uint64
	scale   float64
	lo, hi  float64
	rng     []*rand.Rand
	errs    [][5]float64 // per-channel shaping history, newest first
}

// NewQuantizer returns a quantizer producing bits-deep integers
// (right-justified in int32, the audio.Buffer convention) for the given
// channel count. The seed fixes the dither stream; pass DefaultSeed
// unless distinct streams are needed.
func NewQuantizer(bits, channels int, shaping Shaping, seed uint64) (*Quantizer, error) {
	if bits < 2 || bits > 32 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("dither: target depth %d outside 2..32", bits))
	}
	if channels < 1 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("dither: channel count %d must be positive", channels))
	}
	if shaping != TPDF && shaping != Shaped && shaping != None {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("dither: unknown shaping %d", uint8(shaping)))
	}
	q := &Quantizer{
		bits:    bits,
		shaping: shaping,
		seed:    seed,
		scale:   math.Ldexp(1, bits-1),
		rng:     make([]*rand.Rand, channels),
		errs:    make([][5]float64, channels),
	}
	q.lo = -q.scale
	q.hi = q.scale - 1
	q.Reset()
	return q, nil
}

// Reset restarts the dither streams and clears shaping history, so a
// stream segment requantizes identically no matter what preceded it.
func (q *Quantizer) Reset() {
	for c := range q.rng {
		// Independent per-channel streams: correlated dither across
		// channels would image the noise floor to the center.
		q.rng[c] = rand.New(rand.NewPCG(q.seed, uint64(c)+0x9E3779B97F4A7C15))
		q.errs[c] = [5]float64{}
	}
}

// Bits returns the target depth.
func (q *Quantizer) Bits() int { return q.bits }

// Quantize converts channel ch of src into dst, sample for sample. The
// slices must have equal length. Out-of-range input clamps to the
// integer range; NaN quantizes to 0 rather than poisoning the shaping
// history.
func (q *Quantizer) Quantize(dst []int32, src []float32, ch int) {
	if len(dst) != len(src) {
		panic("dither: dst and src length mismatch")
	}
	rng := q.rng[ch]
	errs := &q.errs[ch]
	for i, s := range src {
		v := float64(s) * q.scale
		if math.IsNaN(v) {
			// Quantize to 0, but keep the shaping history on the sample
			// timeline by shifting in a zero error; skipping the shift
			// would smear the anomaly over the following samples.
			dst[i] = 0
			if q.shaping == Shaped {
				errs[4], errs[3], errs[2], errs[1], errs[0] = errs[3], errs[2], errs[1], errs[0], 0
			}
			continue
		}
		// Cap gross overs (infinities included) at twice full scale: the
		// output clamps to the rails either way, but w must stay finite
		// or the feedback error turns NaN and poisons the loop forever.
		if v > 2*q.scale {
			v = 2 * q.scale
		} else if v < -2*q.scale {
			v = -2 * q.scale
		}
		w := v
		if q.shaping == Shaped {
			for k, h := range fWeighted {
				w -= h * errs[k]
			}
		}
		d := 0.0
		if q.shaping != None {
			// TPDF spanning two LSBs peak to peak: the unique amplitude
			// that makes the first two error moments signal-independent.
			d = rng.Float64() - rng.Float64()
		}
		out := math.Floor(w + d + 0.5)
		if q.shaping == Shaped {
			e := out - w
			errs[4], errs[3], errs[2], errs[1], errs[0] = errs[3], errs[2], errs[1], errs[0], e
		}
		if out < q.lo {
			out = q.lo
		} else if out > q.hi {
			out = q.hi
		}
		dst[i] = int32(out)
	}
}
