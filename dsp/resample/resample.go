// Package resample converts PCM sample rates with a streaming Kaiser
// windowed-sinc polyphase filter. Every registered rate is an integer, so
// conversion is always rational: upsample by L, filter once at the L-fold
// rate, decimate by M, evaluated polyphase so only needed outputs are
// computed.
//
// Two profiles trade quality for taps (docs/quality-gates.md pins the
// numbers):
//
//   - HQ:   passband to 0.91x the narrower Nyquist, ripple <= 0.05 dB,
//     alias and image rejection >= 110 dB in the passband.
//   - Fast: passband to 0.85x, rejection ~70 dB, roughly a third of the
//     multiplies, for constrained hosts and q=low requests.
//
// Both profiles let the transition band straddle the narrower Nyquist
// symmetrically: content folding across it lands inside the transition
// band, which is already indeterminate, so the taps are spent only on
// rejection that reaches the passband. This is the standard resampler
// trade (soxr does the same) and it halves the filter length.
//
// The filter's group delay is fully compensated inside the kernel: output
// sample n sits at input time n*M/L exactly, the stream is primed with
// history zeros, and Drain pads the tail so a T-frame input yields exactly
// ceil(T*L/M) output frames. Positions rescale as out = ceil(in*L/M)
// (ADR-0006); OffsetFor computes the sub-sample phase that keeps the
// output grid aligned after a mid-stream anchor.
//
// Kernels here follow the DSP slice convention: per-channel []float32
// views, never audio.Buffer or strides.
package resample

import (
	"fmt"

	"github.com/colespringer/waxflow/waxerr"
)

// Profile selects the quality/cost trade of the anti-alias filter.
type Profile string

const (
	// HQ is the default profile: >= 110 dB rejection, passband to 0.91x
	// the narrower Nyquist.
	HQ Profile = "hq"
	// Fast is the constrained-host profile: ~70 dB rejection, passband to
	// 0.85x the narrower Nyquist.
	Fast Profile = "fast"
)

// Version returns the profile's algorithm revision for cache keys. A
// change that alters output samples (design formula, tap count, window)
// must bump this, or stale cache entries would serve the old filter's
// audio.
func (p Profile) Version() string {
	return "resample-" + string(p) + "-1"
}

// ParseProfile resolves a profile name; the empty string means HQ. This
// is the single owner of the name set: configuration validation, the
// server, and the CLI all resolve through it, so adding a profile is a
// one-place change.
func ParseProfile(name string) (Profile, error) {
	switch p := Profile(name); {
	case name == "":
		return HQ, nil
	case p.valid():
		return p, nil
	default:
		return "", waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("resample: unknown profile %q (%s, %s)", name, HQ, Fast))
	}
}

func (p Profile) valid() bool { return p == HQ || p == Fast }

// maxBankFloats caps the coefficient bank size (16 MiB of float32).
// Pathological rate pairs (large coprime L) design enormous banks; real
// audio rates reduce to small L/M. The cap turns a memory bomb into a
// clean unsupported-format error.
const maxBankFloats = 4 << 20

// Resampler is a streaming rational resampler for one PCM stream of one
// or more channels, all converted in lockstep. It is not safe for
// concurrent use.
type Resampler struct {
	bank     *bank
	channels int

	// win holds, per channel, a sliding window of input history: win[c][i]
	// is input sample winStart+i. The first tpp-1 entries before the
	// current head are retained so any output's full tap span is present.
	win      [][]float32
	winStart int64 // absolute input index of win[c][0]
	have     int   // valid frames in each win[c]

	inCount  int64 // input frames accepted since Reset
	outCount int64 // output frames emitted since Reset
	off      int   // initial phase offset on the upsampled grid, 0 <= off < M
	draining bool  // Drain started: inCount is final, zeros pad the tail
	padded   int64 // zero frames appended by Drain (excluded from inCount)
}

// New returns a Resampler converting inRate to outRate for the given
// channel count. Equal rates are refused: the caller decides identity, a
// no-op node is never built. Coefficient banks are cached per
// (ratio, profile), so constructing per-session resamplers is cheap after
// first use of a ratio.
func New(inRate, outRate, channels int, profile Profile) (*Resampler, error) {
	if inRate <= 0 || outRate <= 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("resample: rates must be positive, got %d -> %d", inRate, outRate))
	}
	if inRate == outRate {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("resample: input and output rate are both %d", inRate))
	}
	if channels < 1 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("resample: channel count %d must be positive", channels))
	}
	if !profile.valid() {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("resample: unknown profile %q", profile))
	}
	b, err := bankFor(inRate, outRate, profile)
	if err != nil {
		return nil, err
	}
	r := &Resampler{bank: b, channels: channels}
	r.win = make([][]float32, channels)
	winLen := b.tpp - 1 + max(4096, b.tpp)
	for c := range r.win {
		r.win[c] = make([]float32, winLen)
	}
	r.Reset(0)
	return r, nil
}

// Ratio returns the reduced conversion ratio L/M (outRate/inRate).
func (r *Resampler) Ratio() (l, m int) { return r.bank.l, r.bank.m }

// GroupDelay reports the anti-alias filter's group delay as the rational
// num/den in input samples. It is informational: the kernel compensates
// the delay internally, so no caller ever subtracts it.
func (r *Resampler) GroupDelay() (num, den int) { return r.bank.delay, r.bank.l }

// Reset clears all filter state for a new stream segment (a seek or
// splice). phaseOff is the initial position on the upsampled grid in
// [0, M), from OffsetFor; a linear stream from sample zero passes 0.
func (r *Resampler) Reset(phaseOff int) {
	if phaseOff < 0 || phaseOff >= r.bank.m {
		panic(fmt.Sprintf("resample: phase offset %d outside [0, %d)", phaseOff, r.bank.m))
	}
	// Prime with tpp-1 zeros: input samples before the segment start read
	// as silence, which is what makes the first output emerge at input
	// time zero instead of after the filter's group delay.
	hist := r.bank.tpp - 1
	for c := range r.win {
		clear(r.win[c][:hist])
	}
	r.winStart = -int64(hist)
	r.have = hist
	r.inCount = 0
	r.outCount = 0
	r.off = phaseOff
	r.draining = false
	r.padded = 0
}

// OffsetFor returns the anchor pair for a stream segment that starts at
// absolute input sample pos: the absolute output index of the segment's
// first output sample, and the phase offset to pass to Reset so that
// output sample lands exactly on the output timeline grid.
func (r *Resampler) OffsetFor(pos int64) (outPos int64, phaseOff int) {
	l, m := int64(r.bank.l), int64(r.bank.m)
	outPos = ceilDiv(pos*l, m)
	phaseOff = int(outPos*m - pos*l)
	return outPos, phaseOff
}

// OutputLen returns the exact number of output frames a full stream of n
// input frames yields: ceil(n*outRate/inRate) in reduced terms. It is the
// count Drain converges on for a segment anchored at phase zero.
func OutputLen(n int64, inRate, outRate int) int64 {
	if n < 0 {
		return -1 // unknown in, unknown out (Track.Samples convention)
	}
	g := gcd(inRate, outRate)
	return ceilDiv(n*int64(outRate/g), int64(inRate/g))
}

// Process consumes frames from src and produces converted frames into
// dst, per channel, in lockstep. It returns the frames written to each
// dst[c] and consumed from each src[c]; either may be zero when the other
// side has no room. Callers loop until their input is consumed and then
// pull remaining output with an empty src.
func (r *Resampler) Process(dst, src [][]float32) (produced, consumed int) {
	if r.draining {
		panic("resample: Process after Drain")
	}
	return r.run(dst, src)
}

// Drain finalizes the stream: the input seen so far is the whole segment,
// and the filter tail is flushed by feeding zeros until exactly
// OutputLen-many frames (adjusted for the anchor phase) have been
// produced. Call repeatedly with non-empty dst slices until it returns 0.
func (r *Resampler) Drain(dst [][]float32) (produced int) {
	r.draining = true
	produced, _ = r.run(dst, nil)
	return produced
}

// outTarget is the exact output length of the current segment: outputs n
// with input time (n*M + off)/L strictly inside the consumed input.
func (r *Resampler) outTarget() int64 {
	l, m := int64(r.bank.l), int64(r.bank.m)
	return ceilDiv(r.inCount*l-int64(r.off), m)
}

// run is the shared produce/consume loop. A nil src means draining: the
// window is fed zeros instead, bounded by the remaining output target.
func (r *Resampler) run(dst, src [][]float32) (produced, consumed int) {
	if len(dst) != r.channels || (src != nil && len(src) != r.channels) {
		panic("resample: channel count mismatch")
	}
	b := r.bank
	tpp, l, m := b.tpp, int64(b.l), int64(b.m)
	space := len(dst[0])
	limit := int64(-1)
	if src == nil {
		limit = r.outTarget()
	}
	for {
		// Produce every output whose tap span the window already holds.
		for produced < space {
			if limit >= 0 && r.outCount >= limit {
				return produced, consumed
			}
			u := r.outCount*m + int64(b.delay) + int64(r.off)
			k := u / l
			if k >= r.winStart+int64(r.have) {
				break // need more input
			}
			p := int(u % l)
			base := int(k-r.winStart) - (tpp - 1)
			coef := b.coef[p*tpp : (p+1)*tpp]
			if r.channels == 2 {
				// Stereo dominates real traffic; sharing each coefficient
				// load across the pair is worth a dedicated loop.
				y0, y1 := dot2(coef, r.win[0][base:base+tpp], r.win[1][base:base+tpp])
				dst[0][produced], dst[1][produced] = y0, y1
			} else {
				for c := 0; c < r.channels; c++ {
					dst[c][produced] = dot(coef, r.win[c][base:base+tpp])
				}
			}
			produced++
			r.outCount++
		}
		if produced >= space {
			return produced, consumed
		}

		// Slide the window: frames below the next output's tap span are
		// never read again.
		u := r.outCount*m + int64(b.delay) + int64(r.off)
		if drop := int(u/l - int64(tpp-1) - r.winStart); drop > 0 {
			drop = min(drop, r.have)
			for c := range r.win {
				copy(r.win[c], r.win[c][drop:r.have])
			}
			r.winStart += int64(drop)
			r.have -= drop
		}

		// Refill from src, or with tail zeros when draining.
		free := len(r.win[0]) - r.have
		if free == 0 {
			// The window cannot be full here: production consumed
			// everything reachable and the slide just ran.
			panic("resample: window deadlock")
		}
		take := 0
		if src != nil {
			take = min(free, len(src[0])-consumed)
			if take > 0 {
				for c := range r.win {
					copy(r.win[c][r.have:], src[c][consumed:consumed+take])
				}
				consumed += take
				r.inCount += int64(take)
			}
		} else {
			take = free
			for c := range r.win {
				clear(r.win[c][r.have : r.have+take])
			}
			r.padded += int64(take)
		}
		if take == 0 {
			return produced, consumed // src exhausted
		}
		r.have += take
	}
}

// dot is the polyphase inner loop: four accumulators so the chain has
// instruction-level parallelism and float32 rounding grows like a tree,
// not a line. Both slices have equal length; the re-slice pins that for
// the bounds-check eliminator.
func dot(c, x []float32) float32 {
	x = x[:len(c)]
	var s0, s1, s2, s3 float32
	n := len(c) &^ 3
	for j := 0; j < n; j += 4 {
		s0 += c[j] * x[j]
		s1 += c[j+1] * x[j+1]
		s2 += c[j+2] * x[j+2]
		s3 += c[j+3] * x[j+3]
	}
	for j := n; j < len(c); j++ {
		s0 += c[j] * x[j]
	}
	return (s0 + s1) + (s2 + s3)
}

// dot2 is dot for a channel pair, loading each coefficient once. Two
// accumulators per channel: the two channels already give independent
// dependency chains, and fewer live registers avoids spills in the long
// hq phases.
func dot2(c, x0, x1 []float32) (float32, float32) {
	x0 = x0[:len(c)]
	x1 = x1[:len(c)]
	var a0, a1, b0, b1 float32
	n := len(c) &^ 1
	for j := 0; j < n; j += 2 {
		c0, c1 := c[j], c[j+1]
		a0 += c0 * x0[j]
		a1 += c1 * x0[j+1]
		b0 += c0 * x1[j]
		b1 += c1 * x1[j+1]
	}
	if n < len(c) {
		a0 += c[n] * x0[n]
		b0 += c[n] * x1[n]
	}
	return a0 + a1, b0 + b1
}

// ceilDiv is ceiling division for non-negative operands and positive
// divisors; numerators here are products of sample positions and reduced
// ratio terms, both non-negative by construction.
func ceilDiv(a, b int64) int64 {
	return (a + b - 1) / b
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
