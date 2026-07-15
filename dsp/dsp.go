// Package dsp assembles the transcode pipeline's PCM processing chain:
//
//	format.Media -> [convert] -> [resample] -> [mix] -> [gain] -> [dynamics] -> [limiter] -> [dither] -> framer
//
// Nodes are inserted only when needed, in that fixed order. Dynamics sits
// after gain, because it acts on the level the caller asked for, and before
// the limiter, which is the ceiling of last resort. Every node
// implements the pull-based Stage interface; the whole chain runs
// synchronously in the caller's goroutine, one chunk at a time. A single stream is sequential and CPU-bound, so parallelism
// comes from concurrent sessions, not from goroutines inside the chain.
//
// Position authority follows ADR-0006: format.Media stamps Buffer.Pos,
// the resample stage rescales it into the output timeline
// (out = ceil(in*L/M)), and every other stage passes Pos and Discont
// through untouched. A Discont input first finishes the segment in
// flight: buffering nodes (resampler, limiter) drain to the exact output
// end of stream would produce, then reset state (filter history,
// look-ahead, gain envelopes, dither error feedback) and re-anchor. A
// spliced stream is therefore just as deterministic as a linear one:
// neither segment's length nor samples depend on how the input was chunked.
//
// Chunking invariance is not restart invariance, and the two need different
// machinery. Invariance to chunking says the output does not depend on how
// the input was sliced; invariance to restart says it does not depend on
// where the run started, which is what a segmented worker rests on. A node
// with finite memory gets the second for free once its window fills with
// real audio. A node whose state decays exponentially never does, so it
// declares a Settler horizon and the caller pre-rolls it.
//
// Kernels live in the subpackages (resample, mix, gain, dither) and
// operate on plain per-channel []float32 slices; this package owns all
// audio.Buffer handling, so kernels never see strides.
//
// Float processing runs in float32, whose 24-bit mantissa is exact for
// PCM up to 24 bits (the audio package's domain contract). Integer
// sources deeper than 24 bits keep bits below the 24th only on the pure
// int paths (passthrough, widening); any conversion that inserts a
// float node rounds them away. Processed output noise floors sit far
// above that rounding, so the trade is inaudible, but bit-exactness for
// >24-bit sources is only promised where no float node runs.
package dsp

import (
	"fmt"
	"time"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/dsp/dither"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/dsp/mix"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/waxerr"
)

// Node version constants for cache keys (ADR-0004). Convert and
// widen affect sample values (domain and word width), so they carry
// versions; the framer only re-chunks and does not.
const (
	convertVersion = "convert-1"
	widenVersion   = "widen-1"
)

// maxGainDB bounds ChainSpec.GainDB. This is a correctness bound, not
// policy (the HTTP layer clamps at +12 dB): it is generous beyond any
// audio use while keeping the linear factor finite in float32. It also
// rejects NaN and infinities, which would otherwise corrupt the stream.
const maxGainDB = 120

// Stage is one pull-based pipeline node. ReadChunk follows the
// format.Media contract: fill dst (whose format must equal Format()) up
// to its capacity, stamp Pos and Discont, return io.EOF once the stream
// is exhausted. A short chunk is legal anywhere and means only that the
// stage flushed what it had; io.EOF is only returned with an empty dst.
type Stage interface {
	Format() audio.Format
	ReadChunk(dst *audio.Buffer) error
}

// Reader is the upstream side of a chain. format.Media satisfies it.
type Reader interface {
	ReadChunk(dst *audio.Buffer) error
}

// Settler is an optional capability, asserted by pumpStage on its kernel.
//
// It is implemented by a kernel whose state decays rather than ends: an
// exponential envelope converges asymptotically, so unlike an FIR window
// there is no sample after which a restarted run and a continuous run are
// simply equal. Horizon reports the pre-roll needed for that discrepancy to
// collapse to bit-equality. A finite-memory kernel does not implement it,
// because the primeSeconds floor already covers an FIR window: once the
// window fills with real audio the two runs agree exactly, which is why
// priming works at all.
//
// Horizon returns a duration rather than a sample count, and that is not
// cosmetic. The segmented run computes priming on the output timeline and
// converts to the source only at the seek, which works because the floor is
// rate-agnostic. A sample count would break it: the limiter and compressor
// sit after the resampler and would report output samples, while the
// resampler's own window is in input samples, so a chain-wide maximum would
// be taking a max over two different units. A duration has no unit to
// confuse, and the call site converts once.
type Settler interface {
	Horizon() time.Duration
}

// releaser is implemented by stages holding pooled scratch buffers.
type releaser interface{ release() }

// NewSource adapts a Reader (typically format.Media) into the chain's
// head Stage. f must be the exact format the reader emits; it is the
// caller's wiring responsibility, so a mismatch surfaces as the reader's
// own format error on first read. The format itself is not validated
// here: NewChain checks it and returns a clean error, so a demuxer
// yielding a degenerate format cannot crash an Engine caller.
func NewSource(r Reader, f audio.Format) Stage {
	return &sourceStage{r: r, fmt: f}
}

type sourceStage struct {
	r   Reader
	fmt audio.Format
}

func (s *sourceStage) Format() audio.Format { return s.fmt }

// ReadChunk pulls from the reader and holds it to the one part of the Stage
// contract the chain cannot survive being wrong about: io.EOF is the only
// empty answer.
//
// A buffering kernel loops until it has produced a frame or seen the end, so
// a reader that returns neither spins it forever, inside a ReadChunk that
// never returns and therefore never reaches any caller's context check. That
// makes it the one contract breach here that is unkillable rather than merely
// wrong, which is why it is checked rather than assumed. Every in-tree reader
// honors the contract; this is the seam where a caller's own format.Media
// enters a chain, so this is where the contract stops being ours.
func (s *sourceStage) ReadChunk(dst *audio.Buffer) error {
	err := s.r.ReadChunk(dst)
	if err == nil && dst.N == 0 {
		return waxerr.New(waxerr.CodeInternal,
			"dsp: the chain's source returned no frames and no error; io.EOF is the only empty answer")
	}
	return err
}

// ChainSpec declares the output a chain must produce. Zero values mean
// "keep the source's": an entirely zero spec builds an empty chain that
// returns the source stage's chunks untouched.
type ChainSpec struct {
	// Rate is the output sample rate in Hz, 0 to keep the source rate.
	Rate int
	// Channels is the output channel count, 0 to keep. Conversion
	// targets are mono and stereo (plus mono-to-stereo duplication);
	// other targets fail as unsupported.
	Channels int
	// BitDepth selects integer output at that depth (2..32), 0 to keep
	// the source domain and depth. Reductions are dithered.
	BitDepth int
	// Float forces float32 output, converting an integer source. Lossy
	// encoders (MP3, later AAC/Opus) require it; it is mutually exclusive
	// with BitDepth, which wins if both are set.
	Float bool
	// GainDB applies a scalar gain and must be finite within +-120 dB
	// (a correctness bound; the HTTP layer owns tighter policy clamps).
	// Positive gain engages the true-peak limiter (the chain can clip).
	GainDB float64
	// Dynamics inserts a dynamics-processing node after the gain, so it
	// acts on the level the caller asked for. gain.PresetOff (the zero
	// value) inserts nothing. Any other preset engages the true-peak
	// limiter, since makeup gain applied before the envelope catches a
	// transient is exactly the case that clips.
	Dynamics gain.Preset
	// Shaping selects the dither strategy for quantization. Shaped falls
	// back to TPDF at output rates dither.SupportsShaping rejects.
	Shaping dither.Shaping
	// DitherSeed seeds the deterministic dither streams; 0 means
	// dither.DefaultSeed.
	DitherSeed uint64
	// Profile selects the resampler quality profile; empty means
	// resample.HQ.
	Profile resample.Profile
	// FrameSize appends a framer that re-chunks output to exactly this
	// many frames per chunk (the encoder-native size); 0 omits it.
	FrameSize int
}

// Chain is an assembled processing pipeline. It is itself a Stage; the
// extra methods report what the chain does to the stream. Not safe for
// concurrent use. Release returns pooled scratch to the allocator; the
// chain must not be used afterward.
type Chain struct {
	out      Stage
	stages   []Stage
	versions []string
	l, m     int           // output/input rate ratio, reduced; 1/1 when unchanged
	horizon  time.Duration // max Settler horizon over the pushed kernels
}

// NewChain builds the processing chain from src's format to the spec,
// inserting only the nodes the conversion needs. An empty conversion
// yields a chain that delegates straight to src with zero overhead.
func NewChain(src Stage, spec ChainSpec) (*Chain, error) {
	in := src.Format()
	if err := in.Valid(); err != nil {
		return nil, err
	}
	if spec.Rate < 0 || spec.Channels < 0 || spec.FrameSize < 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "dsp: negative chain spec field")
	}
	if spec.BitDepth != 0 && (spec.BitDepth < 2 || spec.BitDepth > 32) {
		// The quantizer would catch this on the float path, but the pure
		// widening path must reject it too.
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("dsp: output depth %d outside 2..32", spec.BitDepth))
	}
	if spec.Float && spec.BitDepth != 0 {
		// Float and BitDepth name conflicting output domains; requiring one
		// or neither keeps the illegal "both set" state from resolving by a
		// silent precedence rule.
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			"dsp: Float and BitDepth are mutually exclusive output domains")
	}
	if !(spec.GainDB >= -maxGainDB && spec.GainDB <= maxGainDB) {
		// Written to fail NaN as well as infinities and out-of-range
		// values: a non-finite gain would otherwise slip past the != 0
		// node check and the > 0 limiter rule and corrupt the samples,
		// and finite values much past the bound overflow float32 anyway.
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("dsp: gain %g dB outside +-%d dB", spec.GainDB, maxGainDB))
	}

	rate := in.Rate
	if spec.Rate != 0 {
		rate = spec.Rate
	}
	channels := in.Channels
	if spec.Channels != 0 {
		channels = spec.Channels
	}
	needRate := rate != in.Rate
	needMix := channels != in.Channels
	needGain := spec.GainDB != 0
	needDyn := spec.Dynamics != gain.PresetOff

	// Output domain: BitDepth set forces int at that depth; Float forces the
	// float domain (for lossy encoders); otherwise the source domain is kept,
	// quantizing back to the source depth when float processing was forced
	// onto an int source.
	outInt := in.Type == audio.Int
	outDepth := in.BitDepth
	if spec.BitDepth != 0 {
		outInt = true
		outDepth = spec.BitDepth
	} else if spec.Float {
		outInt = false
	}
	narrowing := in.Type == audio.Int && outInt && outDepth < in.BitDepth
	forceFloat := spec.Float && in.Type == audio.Int // BitDepth is 0 here (validated above)
	// needDyn belongs here for the same reason every other term does: the
	// dynamics node is float-domain, so an int source that asks for nothing
	// else would otherwise skip convertStage and leave the node reading an
	// int buffer through ChanF. TestNodeInsertion's int-source row is the
	// guard.
	floatWork := needRate || needMix || needGain || needDyn || narrowing || forceFloat

	c := &Chain{out: src, l: 1, m: 1}
	cur := in

	if floatWork && in.Type == audio.Int {
		cur = withType(cur, audio.Float, 32)
		c.push(&convertStage{up: c.out, fmt: cur, scale: float32(1 / scaleFor(in.BitDepth))}, convertVersion)
	}

	if needRate {
		r, err := resample.New(cur.Rate, rate, cur.Channels, profileOr(spec.Profile))
		if err != nil {
			return nil, err
		}
		c.l, c.m = r.Ratio()
		cur = withRate(cur, rate)
		c.push(newPump(c.out, cur, resampleOps{r}), profileOr(spec.Profile).Version())
	}

	var matrix *mix.Matrix
	if needMix {
		srcLayout := in.Layout
		if srcLayout == 0 {
			srcLayout = audio.DefaultLayout(in.Channels)
		}
		dstLayout := audio.DefaultLayout(channels)
		if srcLayout == 0 || dstLayout == 0 {
			return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("dsp: no layout convention for %d -> %d channels", in.Channels, channels))
		}
		m, err := mix.For(srcLayout, dstLayout)
		if err != nil {
			return nil, err
		}
		matrix = m
		cur = withLayout(cur, channels, dstLayout)
		c.push(&mixStage{up: c.out, fmt: cur, matrix: m}, mix.Version)
	}

	if needGain {
		c.push(&gainStage{up: c.out, fmt: cur, g: float32(gain.FromDB(spec.GainDB))}, gain.Version)
	}

	if needDyn {
		comp, err := gain.NewCompressor(cur.Rate, cur.Channels, spec.Dynamics)
		if err != nil {
			return nil, err
		}
		c.push(newPump(c.out, cur, compressorOps{comp}), gain.CompressorVersion)
	}

	// The limiter engages whenever the level path can clip: net positive
	// gain, a downmix whose worst-case matrix gain exceeds unity, or a
	// dynamics preset, whose makeup gain reaches a transient before the
	// envelope that should have ducked it does (the compressor has no
	// look-ahead, by design, precisely because this node is here).
	// Protection is by analysis, not hope.
	if spec.GainDB > 0 || needDyn || (matrix != nil && matrix.MaxGain() > 1) {
		lim, err := gain.NewLimiter(cur.Rate, cur.Channels, gain.DefaultCeilingDB)
		if err != nil {
			return nil, err
		}
		c.push(newPump(c.out, cur, limiterOps{lim}), gain.LimiterVersion)
	}

	switch {
	case outInt && cur.Type == audio.Float:
		shaping := spec.Shaping
		if shaping == dither.Shaped && !dither.SupportsShaping(rate) {
			shaping = dither.TPDF
		}
		seed := spec.DitherSeed
		if seed == 0 {
			seed = dither.DefaultSeed
		}
		q, err := dither.NewQuantizer(outDepth, cur.Channels, shaping, seed)
		if err != nil {
			return nil, err
		}
		cur = withType(cur, audio.Int, outDepth)
		c.push(&quantizeStage{up: c.out, fmt: cur, q: q}, dither.Version)
	case outInt && outDepth > cur.BitDepth:
		shift := outDepth - cur.BitDepth
		cur = withType(cur, audio.Int, outDepth)
		c.push(&widenStage{up: c.out, fmt: cur, shift: uint(shift)}, widenVersion)
	}

	if spec.FrameSize > 0 {
		c.push(&framerStage{up: c.out, fmt: cur, size: spec.FrameSize}, "")
	}
	return c, nil
}

func (c *Chain) push(s Stage, version string) {
	c.out = s
	c.stages = append(c.stages, s)
	if version != "" {
		c.versions = append(c.versions, version)
	}
	// Accumulate here rather than at each call site: a kernel that decays
	// must not be able to join the chain without its horizon being counted,
	// and the whole point of the Settler capability is that forgetting it
	// is a silent determinism bug rather than a loud one.
	if p, ok := s.(*pumpStage); ok {
		c.horizon += p.horizon()
	}
}

// Horizon reports how much pre-roll a mid-stream start must feed this chain
// before its first kept sample for the output to be bit-identical to a
// continuous run's: 0 when no node decays, which is every chain with no gain
// and no dynamics.
//
// Cascaded horizons add, they do not max, and the difference is the whole
// reason this is computed rather than read off the slowest kernel. Each
// kernel's horizon is the time for *its* two runs to converge once its input
// is identical. Downstream of another decaying kernel, that clock does not
// start at the pre-roll's first sample: it starts when the upstream kernel's
// output converged. So a compressor feeding a limiter needs the compressor's
// 10 s and then the limiter's 2 s on top, and taking the max would leave the
// limiter's envelope short by exactly the compressor's settling time.
//
// Taking the max happens to pass today, which is worse than failing: the
// compressor's horizon carries a 2x safety factor (see
// gain.Compressor.Horizon) that is large enough to hide the limiter's 2 s
// underneath it. That is luck about two constants, not a property of the
// chain, and it would evaporate the moment either was retuned.
//
// Finite-memory nodes are not counted here and do not need to be: they
// declare no Settler, and the caller's own priming floor (a fixed window far
// exceeding any FIR here) covers them, in front of this. See Settler for why
// the unit is a duration.
func (c *Chain) Horizon() time.Duration { return c.horizon }

// Format returns the chain's output format.
func (c *Chain) Format() audio.Format { return c.out.Format() }

// ReadChunk pulls the next processed chunk (Stage contract).
func (c *Chain) ReadChunk(dst *audio.Buffer) error {
	if dst.Fmt != c.out.Format() {
		return waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("dsp: chunk buffer is %v, chain output is %v", dst.Fmt, c.out.Format()))
	}
	if dst.Cap() == 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, "dsp: zero-capacity chunk buffer")
	}
	return c.out.ReadChunk(dst)
}

// Ratio returns the chain's output/input rate ratio in lowest terms,
// 1/1 when the rate is unchanged. Callers projecting lengths without
// holding a chain (plan caches) use this with the OutputSamples formula.
func (c *Chain) Ratio() (l, m int) { return c.l, c.m }

// OutputSamples maps a source-stream length onto the chain's output
// length: rate conversion rescales (ceil, matching the resampler's drain
// guarantee), everything else is one to one. Unknown lengths (negative,
// the Track.Samples convention) stay unknown.
func (c *Chain) OutputSamples(in int64) int64 {
	if in < 0 {
		return -1
	}
	return (in*int64(c.l) + int64(c.m) - 1) / int64(c.m)
}

// Versions lists the algorithm revisions of every sample-affecting node
// in chain order, for the cache key (ADR-0004).
func (c *Chain) Versions() []string { return c.versions }

// Release returns all stage scratch buffers to the pool. The chain must
// not be used afterward.
func (c *Chain) Release() {
	for _, s := range c.stages {
		if r, ok := s.(releaser); ok {
			r.release()
		}
	}
}

func profileOr(p resample.Profile) resample.Profile {
	if p == "" {
		return resample.HQ
	}
	return p
}

// scaleFor is the full-scale magnitude of a bit depth: int samples map
// to float as v / 2^(depth-1).
func scaleFor(bits int) float64 {
	return float64(int64(1) << (bits - 1))
}

func withType(f audio.Format, t audio.SampleType, depth int) audio.Format {
	f.Type = t
	f.BitDepth = depth
	return f
}

func withRate(f audio.Format, rate int) audio.Format {
	f.Rate = rate
	return f
}

func withLayout(f audio.Format, channels int, layout audio.ChannelMask) audio.Format {
	f.Channels = channels
	f.Layout = layout
	return f
}
