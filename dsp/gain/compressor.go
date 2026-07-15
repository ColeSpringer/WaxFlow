package gain

import (
	"fmt"
	"math"
	"time"

	"github.com/colespringer/waxflow/waxerr"
)

// CompressorVersion is the compressor node's revision for cache keys: bump
// on any change to a preset's curve, the detector, or the smoothing
// constants, all of which change decoded samples.
const CompressorVersion = "compressor-1"

// Preset names a dynamics curve.
//
// It is a closed vocabulary rather than raw threshold/ratio/attack/release
// parameters, and deliberately. Those would be six plan-shaping fields
// feeding a plan-cache key space that is effectively continuous, which
// defeats the cache's purpose; they would claim a configuration space with
// one tested kernel behind it; and MAINTENANCE.md's listening protocol is
// per configuration, so a preset is a thing that can be put in front of a
// panel and signed off while a parameter space cannot. Contrast
// waxflow.SilenceOptions, whose parameters are raw for the opposite
// reasons: nothing is keyed by them and they only shape a report.
type Preset string

const (
	// PresetOff is the zero value: no dynamics node is inserted.
	PresetOff Preset = ""
	// PresetVoice is the spoken-word curve: a gentle 2.5:1 leveller meant
	// to make an audiobook or podcast intelligible at low volume, where
	// the quiet half of a wide-range reading otherwise falls under the
	// room. It is deliberately audible. That is the feature, and it is why
	// MAINTENANCE.md gives it a subjective sign-off rather than an ABX,
	// whose bar ("indistinguishable from the reference") it must fail.
	PresetVoice Preset = "voice"
)

// Presets lists the dynamics curves this build implements, in table order.
// PresetOff is not among them: it is the absence of one.
func Presets() []Preset { return []Preset{PresetVoice} }

// settleTimeConstants is how many time constants of pre-roll an
// exponential gain envelope needs before a restarted run and a continuous
// one produce bit-identical output. It is measured, not derived: see
// Compressor.Horizon for why, and TestSettleHorizonIsEnough for the
// measurement it has to keep passing.
const settleTimeConstants = 40

// ln10Over20 converts decibels to the natural-exponential argument, so the
// per-frame gain costs an Exp rather than a Pow.
const ln10Over20 = math.Ln10 / 20

// curve is a preset's static transfer function and envelope timing.
type curve struct {
	thresholdDB float64
	ratio       float64
	kneeDB      float64
	attack      time.Duration
	release     time.Duration
	// makeupDB offsets the whole curve back up, which is what turns range
	// reduction into audible levelling: without it compression only makes
	// the loud parts quieter. It is fixed per preset rather than derived,
	// because a derived makeup would change with content and make the node
	// non-deterministic across a splice.
	makeupDB float64
}

// curves are the preset transfer functions.
//
// The voice numbers are calibrated for speech that has already been
// levelled to a broadcast-ish target by the chain's gain node (the
// documented composition is gain=<db>&dynamics=voice), so the threshold
// sits below a typical reading's peaks and above its quiet passages. They
// are a starting point validated by listening rather than derived from a
// standard; retuning them is a CompressorVersion bump, which is exactly
// what that constant is for.
var curves = map[Preset]curve{
	PresetVoice: {
		thresholdDB: -20,
		ratio:       2.5,
		kneeDB:      6,
		attack:      10 * time.Millisecond,
		release:     250 * time.Millisecond,
		makeupDB:    6,
	},
}

// Compressor is a feed-forward dynamics processor: a peak detector drives a
// soft-knee static curve, whose gain is smoothed by an attack and a release
// one-pole and applied to every channel alike, so the stereo image cannot
// shift. Output frame n is input frame n times its gain, so positions map
// one to one.
//
// It has no look-ahead, which is a decision rather than an omission. A
// transient therefore passes at the previous frame's gain for about the
// attack time, and the chain's true-peak limiter is what catches that; the
// limiter is the ceiling of last resort by design, and a second look-ahead
// delay line here would buy nothing it does not already provide. That is
// also why a dynamics preset always engages the limiter (see dsp.NewChain):
// makeup applied before the envelope has caught a transient is exactly the
// case that clips.
//
// The smoothed gain is float64 and narrows to float32 only at the point of
// application. That is load-bearing rather than incidental; see Horizon.
//
// The compressor is deterministic and not safe for concurrent use.
type Compressor struct {
	channels int

	thresholdDB float64
	invRatio    float64
	kneeDB      float64
	halfKneeDB  float64
	kneeStart   float64 // linear magnitude below which the curve is unity
	makeup      float64 // linear
	release     time.Duration

	aAtk float64
	aRel float64

	// g is the current gain reduction in dB, at most 0. It is the state a
	// restarted run has to re-converge to; see Horizon.
	g float64
}

// NewCompressor returns a compressor for one stream at the named preset,
// which must not be PresetOff (the chain inserts no node for that).
func NewCompressor(rate, channels int, p Preset) (*Compressor, error) {
	if rate <= 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("gain: compressor rate %d must be positive", rate))
	}
	if channels < 1 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("gain: compressor channel count %d must be positive", channels))
	}
	c, ok := curves[p]
	if !ok {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("gain: unknown dynamics preset %q", p))
	}
	k := &Compressor{
		channels:    channels,
		thresholdDB: c.thresholdDB,
		invRatio:    1 / c.ratio,
		kneeDB:      c.kneeDB,
		halfKneeDB:  c.kneeDB / 2,
		kneeStart:   FromDB(c.thresholdDB - c.kneeDB/2),
		makeup:      FromDB(c.makeupDB),
		release:     c.release,
		aAtk:        1 - math.Exp(-1/(c.attack.Seconds()*float64(rate))),
		aRel:        1 - math.Exp(-1/(c.release.Seconds()*float64(rate))),
	}
	k.Reset()
	return k, nil
}

// Reset clears all state for a new stream segment.
func (c *Compressor) Reset() { c.g = 0 }

// Horizon reports the pre-roll a restarted run needs before its output
// rejoins a continuous run's bit-exactly (the dsp.Settler capability): 10 s
// for the voice preset, from its 250 ms release. The release pole is the
// slow one, so it sets the horizon; the attack is 25 times faster and never
// binds.
//
// What converges is the output, not the state, and the distinction is the
// whole reason this constant is measured rather than derived.
//
// Two runs whose gains differ by d0 at the restart differ by d0*exp(-t/tau)
// afterwards, since both then see identical audio and the recursion is a
// contraction. But that is exact arithmetic. In floating point the decay
// stops: once the remaining distance to the target falls under the state's
// own ulp the increment rounds away entirely, and each run stalls a few
// ulps from the other at a value that depends on how it approached. Against
// a held target the two states are still ~1e-11 apart after any number of
// time constants, and they never become equal. So a horizon derived from
// "the states collapse onto one value" would be derived from something that
// does not happen.
//
// What does converge, exactly and permanently, is the gain those states
// produce, because it narrows to float32 where it is applied. Two states
// map to the same float32 gain once their gap in dB, scaled by ln10Over20,
// falls under half a float32 ulp: |dg| * 0.115 < 6e-8, so |dg| < 5.2e-7 dB.
// The deepest reduction this curve can hand over is about 40 dB, so that
// takes ln(40 / 5.2e-7) = 18.2 time constants, which is exactly where
// TestSettleHorizonIsEnough measures it. Past that the two runs emit
// identical samples and go on doing so, and their states' residual few ulps
// never surface.
//
// 40 is that doubled. The bare requirement leaves nothing in reserve for
// content the fixtures do not cover, and the failure is silent: at ~192,000
// samples per segment, one differing sample cascades through the encoder
// into wholly different bytes, so a segmented worker would quietly serve
// bytes that disagree with the ones it served before.
//
// Do not narrow g to float32 to halve the horizon. The stall above is the
// same effect, and float32's ulp is eight orders of magnitude coarser: the
// increment would round away while g was still ~1e-3 dB from target, so the
// two runs would stall that far apart and never converge at all, at a
// distance that is audible rather than academic. The float64 state is
// load-bearing precisely because its increments stay representable far
// below anything float32 can express.
func (c *Compressor) Horizon() time.Duration {
	return time.Duration(settleTimeConstants * float64(c.release))
}

// Process consumes frames from src and produces compressed frames into dst,
// per channel, in lockstep. There is no look-ahead, so it always consumes
// and produces the same count: the smaller of the two slice lengths.
func (c *Compressor) Process(dst, src [][]float32) (produced, consumed int) {
	if len(dst) != c.channels || len(src) != c.channels {
		panic("gain: compressor channel count mismatch")
	}
	n := min(checkFrames("compressor destination", dst), checkFrames("compressor source", src))
	for i := 0; i < n; i++ {
		var peak float32
		for ch := range src {
			v := src[ch][i]
			if v < 0 {
				v = -v
			}
			if v > peak {
				peak = v
			}
		}
		target := c.targetDB(float64(peak))
		// Falling toward a deeper reduction is the attack; recovering is
		// the release. The comparison is on the reduction itself, so a
		// preset with no reduction in flight still tracks with the release.
		if target < c.g {
			c.g += (target - c.g) * c.aAtk
		} else {
			c.g += (target - c.g) * c.aRel
		}
		g := float32(math.Exp(c.g*ln10Over20) * c.makeup)
		for ch := range dst {
			dst[ch][i] = src[ch][i] * g
		}
	}
	return n, n
}

// Drain flushes the delayed tail after the final Process call. The
// compressor holds nothing back, so it always reports 0; it exists because
// the chain's pump drives every kernel through one interface.
func (c *Compressor) Drain(dst [][]float32) (produced int) {
	if len(dst) != c.channels {
		panic("gain: compressor channel count mismatch")
	}
	return 0
}

// maxPeak caps the detector at twice full scale, the same bound the dither
// puts on its own feedback loop and for the same reason: past it the output
// clamps to the rails anyway, but the state must stay finite.
//
// Without the cap an infinite sample is fatal and permanently so. It drives
// the peak to +Inf, the curve's target to -Inf, and the envelope to -Inf on
// the attack; the next sample then releases toward a finite target and
// computes -Inf + Inf, which is NaN, after which every gain is NaN and the
// rest of the stream is silence. A single sample does it, and no later
// audio can recover.
//
// NaN needs no cap and gets none: it fails the > comparison in the detector
// loop, so it never enters the peak, and the envelope never sees it. The
// sample itself still passes through to the output as NaN, where the
// limiter's clamp and the quantizer's own NaN guard take it.
const maxPeak = 2

// targetDB is the static curve: the gain reduction in decibels that the
// detector's peak calls for, at most 0. The knee is the standard quadratic
// blend, continuous in both value and slope at each edge.
func (c *Compressor) targetDB(peak float64) float64 {
	if peak <= c.kneeStart {
		// Below the knee the curve is unity, so the common case for speech
		// costs no logarithm at all. It also keeps log10(0) out of the
		// arithmetic, since kneeStart is positive.
		return 0
	}
	if peak > maxPeak {
		peak = maxPeak
	}
	over := 20*math.Log10(peak) - c.thresholdDB
	if over >= c.halfKneeDB {
		return over * (c.invRatio - 1)
	}
	x := over + c.halfKneeDB
	return (c.invRatio - 1) * x * x / (2 * c.kneeDB)
}
