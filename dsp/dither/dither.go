// Package dither requantizes float PCM to integer bit depths. Plain
// truncation correlates the rounding error with the signal and turns it
// into harmonic distortion; TPDF dither of the right amplitude (two LSBs
// peak to peak) decorrelates it completely, leaving a flat noise floor.
// Optional noise shaping moves that floor's energy out of the ear's most
// sensitive band.
//
// Everything is deterministic, and in a specific and stronger sense than
// "seeded". The dither is a pure function of the sample's absolute
// position: noise(seed, channel, position), not a draw from a running
// stream. So identical input, seed, and settings produce identical output
// regardless of how the stream is chunked and regardless of where the run
// started. Golden-stream tests and byte-identical cache regeneration depend
// on the first; segment restart determinism depends on the second, and only
// a position-keyed generator can give it.
//
// A running PRNG would give the first and not the second, which is exactly
// the trap: its state is a function of how many samples it has drawn since
// Reset, which is precisely what differs between a worker restarted
// mid-stream and a continuous one. Unlike a decaying filter, no amount of
// pre-roll converges it, so this cannot be a horizon (see dsp.Settler); it
// has to be a property of the generator.
package dither

import (
	"fmt"
	"math"

	"github.com/colespringer/waxflow/waxerr"
)

// Version is this node's algorithm revision for cache keys: bump on any
// change to the dither, shaping coefficients, or rounding (plan
// section 10).
//
// dither-2 made the noise a pure function of absolute sample position. The
// noise values change, so every cached transcode that quantized is stale.
const Version = "dither-2"

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
// published table.
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
	errs    [][5]float64 // per-channel shaping history, newest first
}

// splitmix64 is the SplittableRandom finalizer (Steele, Lea and Flood,
// OOPSLA 2014), used here as a hash rather than a generator: it is a
// bijection with full avalanche, so hashing a counter yields a stream
// indistinguishable from a PRNG's while carrying no state at all. That is
// the whole point here (see the package doc): a stateless generator is
// addressable by position, and a stateful one is not.
func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return x
}

// tpdf is the triangular dither for one sample: the difference of two
// independent uniforms in [0, 1), spanning two LSBs peak to peak, hashed
// from the seed, the channel, and the absolute sample position.
//
// The two uniforms come from adjacent counter values rather than from one
// value hashed twice, which is the standard counter-based construction and
// keeps them independent. Channels get disjoint counter spaces from the
// seed, because dither correlated across channels would image the noise
// floor to the center.
//
// base is recomputed per sample rather than hoisted into Quantize's loop on
// purpose: this inlines, base is loop-invariant, and the compiler already
// lifts it. Measured at 3300x realtime either way, which is two orders off
// mattering next to the encoders this feeds. Keeping it here keeps the
// generator one self-contained function of (seed, channel, position), which
// is the property the package doc rests on.
func (q *Quantizer) tpdf(ch int, pos int64) float64 {
	base := q.seed ^ (uint64(ch)+1)*0x9E3779B97F4A7C15
	a := float64(splitmix64(base+2*uint64(pos))>>11) * 0x1p-53
	b := float64(splitmix64(base+2*uint64(pos)+1)>>11) * 0x1p-53
	return a - b
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
		errs:    make([][5]float64, channels),
	}
	q.lo = -q.scale
	q.hi = q.scale - 1
	q.Reset()
	return q, nil
}

// Reset clears the shaping history for a new stream segment. The dither
// itself has no state to clear: it is a function of position, so it needs
// no reset and cannot drift.
func (q *Quantizer) Reset() {
	for c := range q.errs {
		q.errs[c] = [5]float64{}
	}
}

// Bits returns the target depth.
func (q *Quantizer) Bits() int { return q.bits }

// Quantize converts channel ch of src into dst, sample for sample. The
// slices must have equal length. Out-of-range input clamps to the
// integer range; NaN quantizes to 0 rather than poisoning the shaping
// history.
func (q *Quantizer) Quantize(dst []int32, src []float32, ch int, pos int64) {
	if len(dst) != len(src) {
		panic("dither: dst and src length mismatch")
	}
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
			d = q.tpdf(ch, pos+int64(i))
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
