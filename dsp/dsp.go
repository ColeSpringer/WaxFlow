// Package dsp assembles the transcode pipeline's PCM processing chain:
//
//	format.Media -> [convert] -> [resample] -> [mix] -> [gain] -> [dither] -> framer
//
// Nodes are inserted only when needed, in that fixed order (plan section
// 8). Every node implements the pull-based Stage interface; the whole
// chain runs synchronously in the caller's goroutine, one chunk at a
// time. A single stream is sequential and CPU-bound, so parallelism
// comes from concurrent sessions, not from goroutines inside the chain.
//
// Position authority follows ADR-0006: format.Media stamps Buffer.Pos,
// the resample stage rescales it into the output timeline
// (out = ceil(in*L/M)), and every other stage passes Pos and Discont
// through untouched. A Discont input first finishes the segment in
// flight: buffering nodes (resampler, limiter) drain to the exact output
// end of stream would produce, then reset state (filter history,
// look-ahead, dither error feedback) and re-anchor. A spliced stream is
// therefore just as deterministic as a linear one: neither segment's
// length nor samples depend on how the input was chunked.
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

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/dsp/dither"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/dsp/mix"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/waxerr"
)

// Node version constants for cache keys (plan section 10). Convert and
// widen affect sample values (domain and word width), so they carry
// versions; the framer only re-chunks and does not.
const (
	ConvertVersion = "convert-1"
	WidenVersion   = "widen-1"
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

func (s *sourceStage) ReadChunk(dst *audio.Buffer) error { return s.r.ReadChunk(dst) }

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
	// GainDB applies a scalar gain and must be finite within +-120 dB
	// (a correctness bound; the HTTP layer owns tighter policy clamps).
	// Positive gain engages the true-peak limiter (the chain can clip).
	GainDB float64
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
	l, m     int // output/input rate ratio, reduced; 1/1 when unchanged
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

	// Output domain: BitDepth set forces int at that depth; otherwise the
	// source domain is kept, quantizing back to the source depth when
	// float processing was forced onto an int source.
	outInt := in.Type == audio.Int
	outDepth := in.BitDepth
	if spec.BitDepth != 0 {
		outInt = true
		outDepth = spec.BitDepth
	}
	narrowing := in.Type == audio.Int && outInt && outDepth < in.BitDepth
	floatWork := needRate || needMix || needGain || narrowing

	c := &Chain{out: src, l: 1, m: 1}
	cur := in

	if floatWork && in.Type == audio.Int {
		cur = withType(cur, audio.Float, 32)
		c.push(&convertStage{up: c.out, fmt: cur, scale: float32(1 / scaleFor(in.BitDepth))}, ConvertVersion)
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

	// The limiter engages whenever the level path can clip: net positive
	// gain, or a downmix whose worst-case matrix gain exceeds unity
	// (plan section 8; protection is by analysis, not hope).
	if spec.GainDB > 0 || (matrix != nil && matrix.MaxGain() > 1) {
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
		c.push(&widenStage{up: c.out, fmt: cur, shift: uint(shift)}, WidenVersion)
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
}

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
// in chain order, for the cache key (plan section 10).
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
