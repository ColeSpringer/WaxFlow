package dsp

import (
	"io"
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/dsp/dither"
	"github.com/colespringer/waxflow/dsp/resample"
)

// memSource is a Stage feeding a fixed planar signal in configurable
// chunk sizes, with optional mid-stream discontinuities, standing in for
// format.Media.
type memSource struct {
	fmt     audio.Format
	buf     *audio.Buffer // whole signal
	pos     int64         // next frame to deliver
	chunk   int
	discAt  int64 // deliver a Discont when pos reaches this (once), -1 off
	discPos int64 // the position the splice jumps to
}

func newMemSource(b *audio.Buffer, chunk int) *memSource {
	return &memSource{fmt: b.Fmt, buf: b, chunk: chunk, discAt: -1}
}

func (m *memSource) Format() audio.Format { return m.fmt }

func (m *memSource) ReadChunk(dst *audio.Buffer) error {
	dst.N = 0
	dst.Discont = false
	if m.discAt >= 0 && m.pos >= m.discAt {
		m.pos = m.discPos
		m.discAt = -1
		dst.Discont = true
	}
	if m.pos >= int64(m.buf.N) {
		return io.EOF
	}
	n := min(dst.Cap(), m.chunk, m.buf.N-int(m.pos))
	audio.CopyFrames(dst, 0, m.buf, int(m.pos), n)
	dst.N = n
	dst.Pos = m.pos
	m.pos += int64(n)
	return nil
}

// readAll drains a chain into one buffer, checking Pos continuity.
func readAll(t *testing.T, c *Chain, capFrames int) *audio.Buffer {
	t.Helper()
	f := c.Format()
	out := audio.Get(f, capFrames)
	chunk := audio.Get(f, audio.StandardChunk)
	defer audio.Put(chunk)
	var expectPos int64
	for {
		err := c.ReadChunk(chunk)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !chunk.Discont && chunk.Pos != expectPos {
			t.Fatalf("chunk Pos %d, want %d (continuity)", chunk.Pos, expectPos)
		}
		if out.N+chunk.N > out.Cap() {
			t.Fatalf("output exceeds %d frames", capFrames)
		}
		audio.CopyFrames(out, out.N, chunk, 0, chunk.N)
		out.N += chunk.N
		expectPos = chunk.Pos + int64(chunk.N)
	}
	return out
}

func intFormat(rate, channels, depth int) audio.Format {
	return audio.Format{Rate: rate, Channels: channels, Layout: audio.DefaultLayout(channels), Type: audio.Int, BitDepth: depth}
}

func sineBuf(f audio.Format, frames int, freq, amp float64) *audio.Buffer {
	b := audio.Get(f, frames)
	b.N = frames
	for c := 0; c < f.Channels; c++ {
		if f.Type == audio.Int {
			scale := float64(int64(1) << (f.BitDepth - 1))
			s := b.ChanI(c)
			for i := range s {
				v := amp * math.Sin(2*math.Pi*freq*float64(i)/float64(f.Rate))
				s[i] = int32(math.RoundToEven(v * (scale - 1)))
			}
		} else {
			s := b.ChanF(c)
			for i := range s {
				s[i] = float32(amp * math.Sin(2*math.Pi*freq*float64(i)/float64(f.Rate)))
			}
		}
	}
	return b
}

// TestEmptyChain: a zero spec is a passthrough with no nodes, preserving
// samples bit-exactly.
func TestEmptyChain(t *testing.T) {
	f := intFormat(44100, 2, 24)
	src := sineBuf(f, 10000, 997, 0.5)
	defer audio.Put(src)
	c, err := NewChain(NewSource(newMemSource(src, 3000), f), ChainSpec{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	if len(c.Versions()) != 0 {
		t.Errorf("empty chain has versions %v", c.Versions())
	}
	if c.Format() != f {
		t.Errorf("format changed: %v", c.Format())
	}
	out := readAll(t, c, 10000)
	defer audio.Put(out)
	if out.N != src.N {
		t.Fatalf("%d frames out, want %d", out.N, src.N)
	}
	for c := 0; c < f.Channels; c++ {
		a, b := src.ChanI(c), out.ChanI(c)
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("channel %d sample %d: %d != %d", c, i, a[i], b[i])
			}
		}
	}
	if got := c.OutputSamples(12345); got != 12345 {
		t.Errorf("OutputSamples = %d, want identity", got)
	}
}

// TestNodeInsertion pins which nodes each conversion builds, via the
// version list (the cache-key surface).
func TestNodeInsertion(t *testing.T) {
	cases := []struct {
		name string
		in   audio.Format
		spec ChainSpec
		want []string
	}{
		{
			name: "resample int",
			in:   intFormat(96000, 2, 24),
			spec: ChainSpec{Rate: 44100},
			want: []string{convertVersion, "resample-hq-1", dither.Version},
		},
		{
			name: "bit reduction only",
			in:   intFormat(44100, 2, 24),
			spec: ChainSpec{BitDepth: 16},
			want: []string{convertVersion, dither.Version},
		},
		{
			name: "widen only",
			in:   intFormat(44100, 2, 16),
			spec: ChainSpec{BitDepth: 24},
			want: []string{widenVersion},
		},
		{
			name: "downmix 5.1",
			in:   intFormat(48000, 6, 16),
			spec: ChainSpec{Channels: 2},
			want: []string{convertVersion, "mix-1", "limiter-1", dither.Version},
		},
		{
			name: "negative gain no limiter",
			in:   intFormat(44100, 2, 16),
			spec: ChainSpec{GainDB: -3},
			want: []string{convertVersion, "gain-1", dither.Version},
		},
		{
			name: "positive gain limits",
			in:   intFormat(44100, 2, 16),
			spec: ChainSpec{GainDB: 3},
			want: []string{convertVersion, "gain-1", "limiter-1", dither.Version},
		},
		{
			name: "mono to stereo no limiter",
			in:   intFormat(44100, 1, 16),
			spec: ChainSpec{Channels: 2},
			want: []string{convertVersion, "mix-1", dither.Version},
		},
		{
			name: "float source stays float",
			in:   audio.Format{Rate: 96000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32},
			spec: ChainSpec{Rate: 48000},
			want: []string{"resample-hq-1"},
		},
		{
			name: "fast profile",
			in:   intFormat(96000, 2, 24),
			spec: ChainSpec{Rate: 44100, Profile: resample.Fast, BitDepth: 16},
			want: []string{convertVersion, "resample-fast-1", dither.Version},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := audio.Get(tc.in, 64)
			src.N = 64
			defer audio.Put(src)
			c, err := NewChain(NewSource(newMemSource(src, 64), tc.in), tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Release()
			got := c.Versions()
			if len(got) != len(tc.want) {
				t.Fatalf("versions %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("versions %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestWidenExact: 16 -> 24 bit widening is a pure shift, bit-exact.
func TestWidenExact(t *testing.T) {
	f := intFormat(44100, 1, 16)
	src := sineBuf(f, 4096, 441, 0.9)
	defer audio.Put(src)
	c, err := NewChain(NewSource(newMemSource(src, 1000), f), ChainSpec{BitDepth: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	out := readAll(t, c, 4096)
	defer audio.Put(out)
	a, b := src.ChanI(0), out.ChanI(0)
	for i := range a {
		if b[i] != a[i]<<8 {
			t.Fatalf("sample %d: %d, want %d", i, b[i], a[i]<<8)
		}
	}
}

// TestChainEndToEnd is the flagship conversion: 96k/24 stereo to 44.1k/16 with
// dither. Sample count follows the rate ratio exactly, the tone
// survives at level, and the noise floor sits where 16-bit TPDF puts it.
func TestChainEndToEnd(t *testing.T) {
	in := intFormat(96000, 2, 24)
	const frames = 96000
	src := sineBuf(in, frames, 997, 0.5)
	defer audio.Put(src)

	c, err := NewChain(NewSource(newMemSource(src, 4096), in), ChainSpec{Rate: 44100, BitDepth: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()

	f := c.Format()
	want := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	if f != want {
		t.Fatalf("output format %v, want %v", f, want)
	}
	wantN := resample.OutputLen(frames, 96000, 44100)
	if got := c.OutputSamples(frames); got != wantN {
		t.Fatalf("OutputSamples = %d, want %d", got, wantN)
	}
	out := readAll(t, c, int(wantN)+8)
	defer audio.Put(out)
	if int64(out.N) != wantN {
		t.Fatalf("%d frames out, want %d", out.N, wantN)
	}

	// Steady-state tone level within 0.05 dB (the hq ripple gate; dither
	// noise sits ~86 dB below the tone and cannot move the estimate).
	mid := out.ChanI(0)[8000 : out.N-8000]
	var a, b, wsum float64
	n := float64(len(mid))
	for i, v := range mid {
		w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/n)
		ph := 2 * math.Pi * 997 * float64(i) / 44100
		a += float64(v) / 32768 * w * math.Cos(ph)
		b += float64(v) / 32768 * w * math.Sin(ph)
		wsum += w
	}
	amp := 2 * math.Hypot(a, b) / wsum
	if lvl := 20 * math.Log10(amp/0.5); math.Abs(lvl) > 0.05 {
		t.Errorf("tone level error %+.4f dB, want within 0.05", lvl)
	}
}

// TestPosRescaleAndDiscont: the resample stage rescales Pos into the
// output timeline (ADR-0006) and re-anchors across a discontinuity;
// downstream stages pass the mark through.
func TestPosRescaleAndDiscont(t *testing.T) {
	in := intFormat(48000, 1, 16)
	src := sineBuf(in, 48000, 997, 0.5)
	defer audio.Put(src)

	ms := newMemSource(src, 4096)
	ms.discAt = 24000  // after 24000 delivered frames...
	ms.discPos = 36000 // ...jump to 36000, like a media seek landing there

	c, err := NewChain(NewSource(ms, in), ChainSpec{Rate: 44100, BitDepth: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()

	chunk := audio.Get(c.Format(), audio.StandardChunk)
	defer audio.Put(chunk)
	var discontAt int64 = -1
	var lastEnd int64
	for {
		err := c.ReadChunk(chunk)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if chunk.Discont {
			if discontAt != -1 {
				t.Fatal("second Discont observed")
			}
			discontAt = chunk.Pos
		} else if lastEnd != 0 && chunk.Pos != lastEnd {
			t.Fatalf("position gap without Discont: %d after %d", chunk.Pos, lastEnd)
		}
		lastEnd = chunk.Pos + int64(chunk.N)
	}
	// The splice landed at source sample 36000: output timeline position
	// ceil(36000 * 147/160) = 33075.
	want := int64(33075)
	if discontAt != want {
		t.Fatalf("Discont chunk Pos = %d, want %d", discontAt, want)
	}
}

// TestSpliceDrainDeterminism: a discontinuity finishes the pre-splice
// segment exactly as end of stream would. The pre-splice output length
// must equal the resampler's drain guarantee (ceil of the rate-scaled
// input) and the whole stream must be bit-identical regardless of how
// the upstream chunks the audio; before the splice-drain fix, the pump
// reset the kernel with a chunk-size-dependent backlog still inside it.
func TestSpliceDrainDeterminism(t *testing.T) {
	in := intFormat(48000, 1, 16)
	src := sineBuf(in, 48000, 997, 0.5)
	defer audio.Put(src)

	// All chunk sizes divide the splice trigger, so every run splices at
	// exactly source sample 24000 and jumps to 36000.
	const spliceAt, spliceTo = 24000, 36000
	type result struct {
		preSplice int
		samples   []int32
	}
	var results []result
	for _, chunk := range []int{500, 1000, 3000, 4000} {
		ms := newMemSource(src, chunk)
		ms.discAt = spliceAt
		ms.discPos = spliceTo
		c, err := NewChain(NewSource(ms, in), ChainSpec{Rate: 44100, BitDepth: 16})
		if err != nil {
			t.Fatal(err)
		}
		out := audio.Get(c.Format(), audio.StandardChunk)
		var r result
		r.preSplice = -1
		for {
			err := c.ReadChunk(out)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if out.Discont {
				if r.preSplice != -1 {
					t.Fatal("second Discont observed")
				}
				r.preSplice = len(r.samples)
			}
			r.samples = append(r.samples, out.ChanI(0)...)
		}
		c.Release()
		audio.Put(out)
		results = append(results, r)

		want := int(resample.OutputLen(spliceAt, 48000, 44100))
		if r.preSplice != want {
			t.Errorf("chunk %d: %d pre-splice frames, want %d (full drain)", chunk, r.preSplice, want)
		}
	}
	for i := 1; i < len(results); i++ {
		if len(results[i].samples) != len(results[0].samples) {
			t.Fatalf("run %d: %d total frames, run 0 has %d", i, len(results[i].samples), len(results[0].samples))
		}
		for j := range results[i].samples {
			if results[i].samples[j] != results[0].samples[j] {
				t.Fatalf("run %d: sample %d differs from run 0", i, j)
			}
		}
	}
}

// TestFramer: exact encoder-native chunks, short only at the tail.
func TestFramer(t *testing.T) {
	f := intFormat(44100, 2, 16)
	src := sineBuf(f, 10000, 441, 0.5)
	defer audio.Put(src)
	c, err := NewChain(NewSource(newMemSource(src, 3333), f), ChainSpec{FrameSize: 1152})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()

	chunk := audio.Get(f, 4096)
	defer audio.Put(chunk)
	var sizes []int
	for {
		err := c.ReadChunk(chunk)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, chunk.N)
	}
	for i, n := range sizes {
		if i < len(sizes)-1 && n != 1152 {
			t.Fatalf("chunk %d has %d frames, want 1152", i, n)
		}
	}
	if last := sizes[len(sizes)-1]; last != 10000%1152 {
		t.Fatalf("tail chunk %d frames, want %d", last, 10000%1152)
	}
}

// TestGainApplied: -6.0206 dB halves int samples (within dither).
func TestGainApplied(t *testing.T) {
	f := intFormat(44100, 1, 16)
	src := sineBuf(f, 8192, 441, 0.8)
	defer audio.Put(src)
	c, err := NewChain(NewSource(newMemSource(src, 4096), f), ChainSpec{GainDB: -6.0205999132796239})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	out := readAll(t, c, 8192)
	defer audio.Put(out)
	a, b := src.ChanI(0), out.ChanI(0)
	for i := range a {
		if d := math.Abs(float64(b[i]) - float64(a[i])/2); d > 1.5 {
			t.Fatalf("sample %d: %d vs source %d (want half, within dither)", i, b[i], a[i])
		}
	}
}

// TestChainSpecErrors: invalid conversions surface as errors, not
// panics.
func TestChainSpecErrors(t *testing.T) {
	f := intFormat(44100, 2, 16)
	src := audio.Get(f, 64)
	defer audio.Put(src)
	stage := NewSource(newMemSource(src, 64), f)
	cases := []ChainSpec{
		{Rate: -1},
		{BitDepth: 1},
		{BitDepth: 33},
		{Channels: 6}, // stereo to 5.1 upmix is unsupported
		{GainDB: math.NaN()},
		{GainDB: math.Inf(1)},
		{GainDB: math.Inf(-1)},
		{GainDB: 121},
		{GainDB: -121},
	}
	for _, spec := range cases {
		if _, err := NewChain(stage, spec); err == nil {
			t.Errorf("spec %+v: want error", spec)
		}
	}
}
