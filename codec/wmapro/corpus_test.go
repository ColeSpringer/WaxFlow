//go:build !wmaprotablesgen

package wmapro_test

// The corpus and the differential.
//
// FFmpeg cannot write this format, so unlike codec/wma the fixtures are not
// generated on the machine that runs the tests: Windows' own encoder makes
// them, five are committed, and the differential that runs on Linux CI decodes
// committed bytes with FFmpeg. That makes the oracle genuinely independent,
// since it is scoring a file it did not produce.
//
// The source PCM is regenerated here rather than committed beside the
// fixtures. It is not an oracle -- this codec is lossy -- but it pins the
// alignment, which is the one thing a decode against another decoder cannot
// check: two decoders that both lead or both lag by a frame agree with each
// other and disagree with the music.
//
// docs/notes/wma-pro-oracle-corpus.md records the measurements, the envelope
// Windows' encoder offers, and which cells are committed and why.

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmapro"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
	"github.com/colespringer/waxflow/internal/testutil"
)

// cell is one corpus configuration. Every one is a shape Windows' encoder
// actually offers; there is no mono cell at any rate because the codec offers
// no mono format, and no 16-bit cell above 48 kHz for the same reason.
type cell struct {
	name     string
	rate     int
	channels int
	bits     int
	// bitRate is what the encoder LANDED on, not what was asked for. It picks
	// its own nearby and at 32 kHz ignores the request entirely.
	bitRate int
	frames  int
	// align is nBlockAlign, the container packet size, which is the sole input
	// to two field widths and so worth pinning per cell.
	align int
	// flags is the decode-flags word at extra offset 14 and word16 the
	// low-bit-rate selector at offset 16. A non-zero word16 is a refusal: see
	// refused below.
	flags    uint16
	word16   uint16
	frameLen int
	// bands is the band count at each subframe size index, 0 to 4, which the
	// notes measured and which a decoder derives rather than reads.
	bands [5]int
	// tonal selects the two-tone recipe below instead of the four-segment one:
	// one pure tone per channel, which is what makes the encoder enable the
	// channel transform per band rather than for every band at once.
	tonal bool
	// outFrames is the sample count the encoder's output actually carries
	// when it is not frames: the one odd-length cell comes back 512 samples
	// short of its source, and FFmpeg agrees, so the number is pinned rather
	// than the source length asserted.
	outFrames int
	// refused marks a cell this build declines by name. It is committed
	// anyway, because a refusal with no real file behind it is a claim about a
	// shape nobody has seen.
	refused   bool
	committed bool
}

// outSamples is the frame count a decode of the cell produces.
func (c cell) outSamples() int {
	if c.outFrames != 0 {
		return c.outFrames
	}
	return c.frames
}

var cells = []cell{
	{name: "pro-44100-2ch-16-128k", rate: 44100, channels: 2, bits: 16, bitRate: 128016,
		frames: 65536, align: 5945, flags: 0x00e0, frameLen: 2048,
		bands: [5]int{26, 26, 23, 19, 14}, committed: true},
	{name: "pro-44100-6ch-16-128k", rate: 44100, channels: 6, bits: 16, bitRate: 128016,
		frames: 65536, align: 5945, flags: 0x00e0, frameLen: 2048,
		bands: [5]int{26, 26, 23, 19, 14}, committed: true},
	{name: "pro-48000-6ch-24-384k", rate: 48000, channels: 6, bits: 24, bitRate: 384000,
		frames: 65536, align: 16384, flags: 0x00e0, frameLen: 2048,
		bands: [5]int{26, 26, 23, 18, 14}, committed: true},
	{name: "pro-96000-2ch-24-384k", rate: 96000, channels: 2, bits: 24, bitRate: 384000,
		frames: 131072, align: 16384, flags: 0x00e0, frameLen: 4096,
		bands: [5]int{28, 28, 25, 20, 16}, committed: true},
	// The tonal cell reaches the two rows the four-segment recipe cannot: the
	// per-band channel transform enables, which only sustained tonal stereo
	// provokes, and an end trim, which only a length that is not a multiple of
	// the frame carries. 100000 frames is 48 frames and 1696 samples.
	{name: "pro-44100-2ch-16-128k-tonal", rate: 44100, channels: 2, bits: 16, bitRate: 128016,
		frames: 100000, outFrames: 99488, align: 5945, flags: 0x00e0, frameLen: 2048,
		bands: [5]int{26, 26, 23, 19, 14}, tonal: true, committed: true},
	// The refusal cell. 32 kHz is the one rate at which the codec offers a
	// single format, so every 32 kHz WMA Pro stream is this shape.
	{name: "pro-32000-2ch-16-32k", rate: 32000, channels: 2, bits: 16, bitRate: 32000,
		frames: 49152, align: 1536, flags: 0x0060, word16: 0x20c6, frameLen: 2048,
		bands: [5]int{25, 25, 24, 20, 15}, refused: true, committed: true},

	// Generated on Windows, never committed.
	{name: "pro-44100-6ch-16-192k", rate: 44100, channels: 6, bits: 16, bitRate: 192016,
		frames: 65536, align: 8917, flags: 0x00e0, frameLen: 2048,
		bands: [5]int{26, 26, 23, 19, 14}},
	{name: "pro-44100-2ch-24-440k", rate: 44100, channels: 2, bits: 24, bitRate: 440016,
		frames: 65536, align: 20434, flags: 0x00e0, frameLen: 2048,
		bands: [5]int{26, 26, 23, 19, 14}},
	{name: "pro-48000-2ch-24-192k", rate: 48000, channels: 2, bits: 24, bitRate: 192000,
		frames: 65536, align: 8192, flags: 0x00e0, frameLen: 2048,
		bands: [5]int{26, 26, 23, 18, 14}},
	{name: "pro-88200-2ch-24-384k", rate: 88200, channels: 2, bits: 24, bitRate: 384008,
		frames: 131072, align: 17833, flags: 0x00e0, frameLen: 4096,
		bands: [5]int{28, 28, 25, 21, 16}},
	{name: "pro-96000-6ch-24-768k", rate: 96000, channels: 6, bits: 24, bitRate: 768000,
		frames: 131072, align: 32768, flags: 0x00e0, frameLen: 4096,
		bands: [5]int{28, 28, 25, 20, 16}},
	// A second refusal shape, at a rate whose neighbouring bit rates are fine,
	// so the refusal cannot be passing because of the sample rate.
	{name: "pro-44100-2ch-16-64k", rate: 44100, channels: 2, bits: 16, bitRate: 64024,
		frames: 65536, align: 2973, flags: 0x00e0, word16: 0xc042, frameLen: 2048,
		bands: [5]int{26, 26, 23, 19, 14}, refused: true},
}

// splitmix64, so the noise carries its own stability rather than borrowing a
// library's promise about a generator's output sequence.
type prng uint64

func (p *prng) next() uint64 {
	*p += 0x9E3779B97F4A7C15
	z := uint64(*p)
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// tri is one integer triangle wave: period p frames, amplitude a. Integer
// division truncates, which is a definition rather than a rounding mode two
// implementations could disagree about.
//
// The intermediate is int64 and that is load-bearing rather than tidy. At 24
// bits with the widest period here, 4*a*x is past what a 32-bit int holds; in
// an int it wrapped negative, and the same recipe then produced different
// samples on a 32-bit build. `make test-386` is what found it, in stage 5.
func tri(i, p, a int) int {
	x := int64(i % p)
	amp := int64(a)
	y := 4 * amp * x / int64(p)
	if y > 2*amp {
		y = 4*amp - y
	}
	return int(y - amp)
}

// tone is one integer cosine, x[n] = A cos(w n), from the rotation recurrence
// x[n+1] = 2 cos(w) x[n] - x[n-1] carried in int64 fixed point. cos(w) is a
// literal in thirty fractional bits, so nothing here evaluates a
// transcendental and the samples are the same on every platform. The state
// keeps 32-bits fractional bits, sixteen at 16 bits, so the rounding error per
// step is far below a sample and a hundred thousand steps drift the amplitude
// by a unit or two, which is a fact about the fixture and not a defect.
type tone struct {
	c         int64 // cos(w) in thirty fractional bits
	frac      uint
	prev, cur int64 // x[n-1] and x[n], in frac fractional bits
}

// The cosines the tonal cell uses, at 44100 Hz only: cos(2 pi f / 44100) in
// thirty fractional bits.
const (
	cos440at44100 = 1071632635
	cos550at44100 = 1070446823
)

func newTone(c int64, amp, bits int) *tone {
	frac := uint(32 - bits)
	x0 := int64(amp) << frac
	// x[-1] = A cos(w), so the run starts at n = 0 with x[0] = A.
	return &tone{c: c, frac: frac, prev: (x0*c + 1<<29) >> 30, cur: x0}
}

// next returns x[n] as a sample and advances to n+1.
func (t *tone) next() int {
	out := int((t.cur + 1<<(t.frac-1)) >> t.frac)
	nxt := (2*t.c*t.cur+1<<29)>>30 - t.prev
	t.prev, t.cur = t.cur, nxt
	return out
}

// synth builds one cell's source PCM, planar. Four equal segments: two
// triangles per channel, triangles plus quarter-scale noise, digital silence,
// and full-scale noise. The silence-to-noise edge is a hard transient on a
// frame boundary, and the triangles are rich enough in harmonics to fill the
// band layout rather than lighting one band per channel.
func synth(c cell) [][]int32 {
	if c.tonal {
		return synthTonal(c)
	}
	full := int(1)<<(c.bits-1) - 1
	q := c.frames / 4
	out := make([][]int32, c.channels)
	for ch := 0; ch < c.channels; ch++ {
		s := make([]int32, c.frames)
		rng := prng(0x5741584C4F5700 + uint64(ch)*0x100)
		p1, p2 := 97+13*ch, 251+29*ch
		for i := range s {
			n := int(rng.next()%uint64(2*full+1)) - full
			var v int
			switch {
			case i < q:
				v = tri(i, p1, full*3/8) + tri(i, p2, full*2/8)
			case i < 2*q:
				v = tri(i, p1, full*2/8) + tri(i, p2, full/8) + n/4
			case i < 3*q:
				v = 0
			default:
				v = n
			}
			if v > full {
				v = full
			} else if v < -full {
				v = -full
			}
			s[i] = int32(v)
		}
		out[ch] = s
	}
	return out
}

// synthTonal is the two-tone recipe: 440 Hz on the left at three eighths of
// full scale and 550 Hz on the right, nothing else. Measured against three
// other mixes of the same two tones (both tones in both channels, with either
// sign), this is the only one on which the encoder enables the channel
// transform band by band; with both tones in both channels it decides once
// for all bands, every time.
func synthTonal(c cell) [][]int32 {
	if c.rate != 44100 || c.channels != 2 {
		panic("the tonal recipe's cosines are for 44100 Hz stereo")
	}
	full := int(1)<<(c.bits-1) - 1
	out := make([][]int32, 2)
	for ch, cos := range []int64{cos440at44100, cos550at44100} {
		s := make([]int32, c.frames)
		tn := newTone(cos, full*3/8, c.bits)
		for i := range s {
			s[i] = int32(tn.next())
		}
		out[ch] = s
	}
	return out
}

// synthFloat is the source in the domain a decode comes back in.
func synthFloat(c cell) []float32 {
	planar := synth(c)
	full := float32(int(1) << (c.bits - 1))
	out := make([]float32, c.frames*c.channels)
	for ch, s := range planar {
		for i, v := range s {
			out[i*c.channels+ch] = float32(v) / full
		}
	}
	return out
}

// writeWAV writes a cell's source for the encoder to read.
func writeWAV(t testing.TB, path string, c cell) {
	t.Helper()
	pcm := synth(c)
	bytesPer := c.bits / 8
	align := c.channels * bytesPer
	data := make([]byte, c.frames*align)
	p := 0
	for i := 0; i < c.frames; i++ {
		for ch := 0; ch < c.channels; ch++ {
			v := uint32(pcm[ch][i])
			for b := 0; b < bytesPer; b++ {
				data[p] = byte(v >> (8 * b))
				p++
			}
		}
	}
	var h []byte
	u16 := func(v int) { h = binary.LittleEndian.AppendUint16(h, uint16(v)) }
	u32 := func(v int) { h = binary.LittleEndian.AppendUint32(h, uint32(v)) }
	h = append(h, "RIFF"...)
	u32(36 + len(data))
	h = append(h, "WAVEfmt "...)
	u32(16)
	u16(1)
	u16(c.channels)
	u32(c.rate)
	u32(c.rate * align)
	u16(align)
	u16(c.bits)
	h = append(h, "data"...)
	u32(len(data))
	if err := os.WriteFile(path, append(h, data...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func corpusPath(t testing.TB, name string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "testdata", "corpus", name+".wma")
}

// committed returns the cells that ship in the tree.
func committedCells() []cell {
	var out []cell
	for _, c := range cells {
		if c.committed {
			out = append(out, c)
		}
	}
	return out
}

// demux reads one .wma into its track and its packets. It goes through the
// real demuxer rather than a hand-rolled packet walk, so the cells exercise
// the container's 0x0162 arm as well as the codec.
func demux(t testing.TB, path string) (container.Track, [][]byte) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("demux %s: %v", path, err)
	}
	var pkts [][]byte
	for {
		var pkt container.Packet
		err := d.ReadPacket(&pkt)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read packet: %v", err)
		}
		pkts = append(pkts, append([]byte(nil), pkt.Data...))
	}
	return d.Tracks()[0], pkts
}

// decodeAll decodes every packet and returns the interleaved samples.
func decodeAll(t testing.TB, track container.Track, pkts [][]byte) []float32 {
	t.Helper()
	cfg, err := wmapro.ParseConfig(track.CodecConfig)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	dec, err := wmapro.NewDecoder(cfg, track.Fmt)
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	defer dec.Release()
	var got []float32
	emit := func(b *audio.Buffer) error {
		got = append(got, testutil.InterleaveF(b)...)
		return nil
	}
	for i, p := range pkts {
		if err := dec.Decode(p, emit); err != nil {
			t.Fatalf("packet %d of %d: %v", i, len(pkts), err)
		}
	}
	if err := dec.Drain(emit); err != nil {
		t.Fatalf("drain: %v", err)
	}
	return got
}

// The differential's bounds, set from what is measured rather than from a
// round number, in the convention codec/wma's gate uses: about 2x above the
// worst case.
//
// The oracle has a noise floor of its own: FFmpeg's scalar and vectorised
// decodes of the same file differ by 2.1e-8 to 4.4e-8 RMS and 2.4e-7 to 4.2e-7
// max full scale across this corpus, so nothing can gate below that. This
// decoder measures 4.1e-8 to 5.1e-8 RMS and 2.4e-7 to 4.8e-7 max against the
// scalar decode across the five gate cells, which is the oracle disagreeing
// with itself. The bounds below are that worst case with about 2x of headroom.
//
// The other oracle is not in the gate, and the reason is measured rather
// than suspected: Windows' decoder and FFmpeg's agree to 1.8e-7 RMS on the
// 24-bit stereo cells and only to about 5e-5 RMS and 2e-3 max on every 5.1
// cell, at both depths and every bit rate. The gap tracks the channel count
// alone, so it is the channel decorrelation's arithmetic, and a gate written
// from that pair would be gating this decoder on which of the two it happened
// to land nearer. It lands on FFmpeg, at the floor.
const (
	gateRMS = 1e-7
	gateMax = 1e-6
)

// TestConfigMatchesTheCorpus is the decode-free half of the gate: the
// extradata fields, the frame-length rule and the derived band layout, checked
// against the numbers the analysis pass measured. Nothing here decodes a
// sample, so it holds on a machine with no FFmpeg.
func TestConfigMatchesTheCorpus(t *testing.T) {
	for _, c := range committedCells() {
		t.Run(c.name, func(t *testing.T) {
			track, _ := demuxOrRefusal(t, c)
			if c.refused {
				return
			}
			cfg, err := wmapro.ParseConfig(track.CodecConfig)
			if err != nil {
				t.Fatalf("parse config: %v", err)
			}
			if cfg.Rate != c.rate || cfg.Channels != c.channels || cfg.BitsPerSample != c.bits {
				t.Fatalf("config = %dHz %dch %dbit, want %dHz %dch %dbit",
					cfg.Rate, cfg.Channels, cfg.BitsPerSample, c.rate, c.channels, c.bits)
			}
			if cfg.BlockAlign != c.align {
				t.Errorf("nBlockAlign %d, want %d", cfg.BlockAlign, c.align)
			}
			if cfg.DecodeFlags != c.flags {
				t.Errorf("decode flags %#04x, want %#04x", cfg.DecodeFlags, c.flags)
			}
			if got := cfg.SamplesPerFrame(); got != c.frameLen {
				t.Errorf("frame length %d, want %d", got, c.frameLen)
			}
			// The band layout is derived from the rate and the frame length,
			// so a rule that is subtly wrong shows up here rather than as
			// quiet spectral damage a tolerance would absorb.
			for k, want := range c.bands {
				if got := wmapro.BandCountForTest(cfg, k); got != want {
					t.Errorf("size index %d: %d bands, want %d", k, got, want)
				}
			}
			// The demuxer builds the track format from WAVEFORMATEX and the
			// codec builds one from the same bytes; they have to be the same
			// format, including the channel LAYOUT.
			if track.Fmt != cfg.Format() {
				t.Errorf("track format %v, codec format %v", track.Fmt, cfg.Format())
			}
		})
	}
}

// TestBandEdgesAtFortyEightKilohertz pins the band walk against a layout the
// analysis pass wrote out in full, which the band COUNT alone cannot do: a
// rule that is off by one edge in the middle still counts 26 bands.
func TestBandEdgesAtFortyEightKilohertz(t *testing.T) {
	want := []int{0, 8, 16, 24, 36, 44, 52, 64, 80, 92, 108, 128, 148, 172, 196,
		232, 268, 316, 376, 452, 548, 656, 812, 1024, 1324, 1764, 2048}
	cfg := wmapro.Config{Rate: 48000, Channels: 2, BitsPerSample: 16,
		BlockAlign: 16384, DecodeFlags: 0x00e0}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	got := wmapro.BandEdgesForTest(cfg, 0)
	if len(got) != len(want) {
		t.Fatalf("%d edges, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("edge %d = %d, want %d\ngot  %v\nwant %v", i, got[i], want[i], got, want)
		}
	}
}

// TestCorpusCellsAreDecorrelated checks the fixture generator rather than the
// decoder. Identical channels would make every channel-transform claim in the
// gate vacuous while leaving it green, and that has bitten this tree before.
func TestCorpusCellsAreDecorrelated(t *testing.T) {
	for _, c := range committedCells() {
		if c.channels < 2 {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			src := synthFloat(c)
			var worst float64
			for i := 0; i+1 < len(src); i += c.channels {
				worst = max(worst, math.Abs(float64(src[i])-float64(src[i+1])))
			}
			if worst < 0.1 {
				t.Fatalf("max|ch0-ch1| = %g: the channels of this fixture are not independent", worst)
			}
		})
	}
}

// TestDecodeMatchesFFmpeg is the differential. FFmpeg did not write these
// files and cannot, so what this establishes is two independent implementations
// agreeing on foreign bytes rather than a round trip through one program.
//
// -cpuflags 0 pins FFmpeg's scalar path so the reference answer does not depend
// on the host CPU.
func TestDecodeMatchesFFmpeg(t *testing.T) {
	for _, c := range committedCells() {
		if c.refused {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			path := corpusPath(t, c.name)
			track, pkts := demux(t, path)
			got := decodeAll(t, track, pkts)
			want := testutil.FFmpegDecodeF32NoSIMD(t, path)

			// Lengths before samples. Both decoders apply the bitstream's own
			// trims and neither leads the other, so this is an equality and
			// not an overlap: a decode that dropped or repeated a frame would
			// otherwise pass on whatever the comparison took.
			if len(got) != len(want) {
				t.Errorf("decoded %d samples, ffmpeg %d", len(got), len(want))
			}
			if len(got) != c.outSamples()*c.channels {
				t.Errorf("decoded %d samples, want %d", len(got), c.outSamples()*c.channels)
			}
			n := min(len(got), len(want))
			if n == 0 {
				t.Fatal("nothing to compare")
			}
			d := testutil.CompareF32(got[:n], want[:n])
			if d.RMS > gateRMS || d.MaxAbs > gateMax {
				t.Errorf("differential %v, want rms <= %g max <= %g", d, gateRMS, gateMax)
			}
		})
	}
}

// TestDecodeAlignsWithTheSource is what the differential cannot do. Two
// decoders that both lead or both lag by a frame agree with each other and
// disagree with the music, so the head trim is checked against the PCM the
// encoder was handed, regenerated here.
//
// The tolerance is loose because this is a lossy codec: what is being measured
// is alignment, not fidelity, and the test asserts that the best fit is at lag
// zero rather than that the error there is small.
func TestDecodeAlignsWithTheSource(t *testing.T) {
	for _, c := range committedCells() {
		// A periodic source cannot pin a lag: a shift of one period fits
		// about as well as none. The tonal cell's alignment rides on the
		// same trim rule the other four pin.
		if c.refused || c.tonal {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			track, pkts := demux(t, corpusPath(t, c.name))
			got := decodeAll(t, track, pkts)
			src := synthFloat(c)
			if lag := bestLag(src, got, c.channels, c.frameLen); lag != 0 {
				t.Errorf("decode leads the source by %d samples; the head trim is wrong", lag)
			}
		})
	}
}

// bestLag finds the shift of got against src minimising mean square error over
// a window past the stream head, which is how an encoder-plus-decoder delay is
// recovered. A window at the head would fit the first frames' warm-up instead
// of the stream.
func bestLag(src, got []float32, channels, maxLag int) int {
	skip, win := 8192, 16384
	if (skip+win)*channels > len(src) {
		win = len(src)/channels - skip
	}
	best, bestErr := 0, math.Inf(1)
	for lag := 0; lag < maxLag; lag++ {
		if (lag+skip+win)*channels > len(got) {
			break
		}
		var e float64
		for i := skip; i < skip+win; i++ {
			d := float64(src[i*channels]) - float64(got[(lag+i)*channels])
			e += d * d
		}
		if e < bestErr {
			bestErr, best = e, lag
		}
	}
	return best
}

// demuxOrRefusal opens a cell, asserting that a refused one is refused by the
// CONTAINER and not merely by the codec: a shape this build declines must not
// probe as a playable track first.
func demuxOrRefusal(t *testing.T, c cell) (container.Track, [][]byte) {
	t.Helper()
	if !c.refused {
		return demux(t, corpusPath(t, c.name))
	}
	raw, err := os.ReadFile(corpusPath(t, c.name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := asf.NewDemuxer(container.BytesSource(raw), nil); err == nil {
		t.Fatalf("%s opened as a playable track; it carries word %#04x at extra offset 16 "+
			"and no reference decoder implements the tool that selects", c.name, c.word16)
	}
	return container.Track{}, nil
}

// TestDecodeIsNotGloballyNegated is the test the notes ask for by name. Every
// sign bit in this codec means the opposite of the usual convention, and a
// decoder that has it backwards is bitstream-valid, sample-count exact,
// aligned at lag zero, and wrong in every sample. The differential would
// catch it on this corpus; this catches it with no tool installed, by
// requiring the decode to correlate positively with the source on every
// channel.
func TestDecodeIsNotGloballyNegated(t *testing.T) {
	for _, c := range committedCells() {
		if c.refused {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			track, pkts := demux(t, corpusPath(t, c.name))
			got := decodeAll(t, track, pkts)
			src := synthFloat(c)
			for ch := 0; ch < c.channels; ch++ {
				r := correlation(src, got, c.channels, ch)
				if r < negationFloor {
					t.Errorf("channel %d correlates %.3f with the source, want at least %.2f", ch, r, negationFloor)
				}
			}
		})
	}
}

// negationFloor is well below what every channel of every cell measures and
// far above what a negated decode gives, which is the same figure with the
// sign flipped. Measured: 0.95 to 0.999 on every full-range channel, and 0.47
// on the LFE channel of both 5.1 cells, which the encoder band-limits.
const negationFloor = 0.3

// correlation is the normalised cross-correlation of one channel of the
// source and the decode at lag zero, over the same window bestLag uses.
func correlation(src, got []float32, channels, ch int) float64 {
	skip, win := 8192, 16384
	if (skip+win)*channels > len(got) {
		win = len(got)/channels - skip
	}
	var xy, xx, yy float64
	for i := skip; i < skip+win; i++ {
		x, y := float64(src[i*channels+ch]), float64(got[i*channels+ch])
		xy += x * y
		xx += x * x
		yy += y * y
	}
	if xx == 0 || yy == 0 {
		return 0
	}
	return xy / math.Sqrt(xx*yy)
}

// TestCorpusReachesWhatTheNotesMeasured pins every count in the corpus note's
// coverage matrix, which the analysis pass measured with an instrumented
// decoder of its own written from the same notes. Agreeing on the samples
// says the arithmetic is right; agreeing on how many times each branch was
// taken says the WALK is the same walk, which a tolerance on the samples
// cannot say, and it is what makes the matrix's claims about what the corpus
// reaches claims about this decoder rather than about that one.
//
// The tonal cell was not in that matrix. Its row is what this decoder
// measured on it, pinned so that the two rows it exists to reach (the
// per-band enables and the end trim) cannot silently stop being reached.
func TestCorpusReachesWhatTheNotesMeasured(t *testing.T) {
	type counts struct {
		frames, subframes                                            int
		stereo, scaled, noTransform, explicit, perBand               int
		dpcm, runLevel, scaleEscape, resampleOnly                    int
		vec4, vec2, large, tail, tailEscape, endOfBlock              int
		stepEscape, modifiers, startTrim, endTrim, contZero, contSat int
	}
	want := map[string]counts{
		"pro-44100-2ch-16-128k": {33, 55, 42, 0, 13, 0, 0, 52, 32, 43, 0, 10227, 450, 30, 84, 168, 84, 0, 18, 1, 0, 1, 0},
		"pro-44100-6ch-16-128k": {33, 65, 0, 4, 2, 6, 0, 156, 70, 66, 15, 1370, 339, 15, 239, 219, 239, 0, 167, 1, 0, 1, 0},
		"pro-48000-6ch-24-384k": {33, 64, 0, 4, 0, 7, 0, 156, 84, 152, 0, 30049, 20442, 429, 240, 605, 240, 1, 155, 1, 0, 1, 0},
		"pro-96000-2ch-24-384k": {33, 55, 42, 0, 13, 0, 0, 52, 32, 53, 0, 29695, 19309, 99, 84, 399, 84, 0, 12, 1, 0, 1, 0},
		// Measured here, not in the note.
		"pro-44100-2ch-16-128k-tonal": {50, 58, 55, 0, 3, 0, 24, 100, 10, 16, 0, 2236, 2882, 483, 110, 32, 110, 2, 6, 1, 1, 6, 0},
	}
	for _, c := range committedCells() {
		if c.refused {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			track, pkts := demux(t, corpusPath(t, c.name))
			cfg, err := wmapro.ParseConfig(track.CodecConfig)
			if err != nil {
				t.Fatal(err)
			}
			dec, err := wmapro.NewDecoder(cfg, track.Fmt)
			if err != nil {
				t.Fatal(err)
			}
			defer dec.Release()
			for _, p := range pkts {
				if err := dec.Decode(p, func(*audio.Buffer) error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			p := wmapro.PathsForTest(dec)
			got := counts{p.Frames, p.Subframes,
				p.StereoMatrix, p.ScaledMatrix, p.NoTransform, p.ExplicitMatrix, p.PerBandEnables,
				p.DPCM, p.RunLevelDiffs, p.ScaleEscape, p.ResampleOnly,
				p.Vec4Escape, p.Vec2Escape, p.LargeValue, p.TailReached, p.TailEscape, p.EndOfBlock,
				p.StepEscape, p.Modifiers, p.StartTrim, p.EndTrim, p.ContZero, p.ContSaturates}
			if got != want[c.name] {
				t.Errorf("path counts\n got  %+v\n want %+v", got, want[c.name])
			}
			// The rows nothing in the note's corpus reached and no cell here
			// reaches either, pinned at zero so that a fixture which starts
			// reaching one is noticed and promoted out of the deficit list.
			if p.BuiltinMatrix != 0 || p.ExplicitLimit != 0 || p.VectorCovers != 0 || p.ZeroScale != 0 {
				t.Errorf("a cell reaches a path recorded as unreached: builtin=%d limit=%d covers=%d zeroScale=%d",
					p.BuiltinMatrix, p.ExplicitLimit, p.VectorCovers, p.ZeroScale)
			}
		})
	}
}

// TestMicrosoftCorpus is the wider corpus, generated rather than committed:
// every cell in the table, re-encoded here with Windows' own encoder so a
// Windows run covers the whole envelope, including the six shapes not worth
// their bytes in the tree.
//
// Windows only, because Windows is the only place that can write this format
// at all. That is why the committed cells exist: without them a Linux machine
// would decode nothing.
func TestMicrosoftCorpus(t *testing.T) {
	if !testutil.HaveWMFEnc(t) {
		t.Skip("Windows' WMA encoder is not available here")
	}
	dir := t.TempDir()
	for _, c := range cells {
		t.Run(c.name, func(t *testing.T) {
			wav := filepath.Join(dir, c.name+".wav")
			wma := filepath.Join(dir, c.name+".wma")
			writeWAV(t, wav, c)
			testutil.WMFEncodeSubtype(t, wav, wma, c.rate, c.channels, c.bitRate, c.bits, testutil.SubtypeWMAPro)
			raw, err := os.ReadFile(wma)
			if err != nil {
				t.Fatal(err)
			}
			if c.refused {
				if _, err := asf.NewDemuxer(container.BytesSource(raw), nil); err == nil {
					t.Fatalf("%s opened as a playable track; it should carry a non-zero word at extra offset 16", c.name)
				}
				return
			}
			track, pkts := demux(t, wma)
			cfg, err := wmapro.ParseConfig(track.CodecConfig)
			if err != nil {
				t.Fatal(err)
			}
			// The encoder is asked for the bit rate it landed on last time,
			// so the shape has to come back the same; a renegotiation would
			// show here as a different rate or depth.
			if cfg.Rate != c.rate || cfg.Channels != c.channels || cfg.BitsPerSample != c.bits {
				t.Fatalf("encoded %dHz %dch %dbit, want %dHz %dch %dbit",
					cfg.Rate, cfg.Channels, cfg.BitsPerSample, c.rate, c.channels, c.bits)
			}
			got := decodeAll(t, track, pkts)
			if len(got) != c.outSamples()*c.channels {
				t.Errorf("decoded %d samples, want %d", len(got), c.outSamples()*c.channels)
			}
			src := synthFloat(c)
			for ch := 0; ch < c.channels; ch++ {
				if r := correlation(src, got, c.channels, ch); r < negationFloor {
					t.Errorf("channel %d correlates %.3f with the source", ch, r)
				}
			}
			if !c.tonal {
				if lag := bestLag(src, got, c.channels, c.frameLen); lag != 0 {
					t.Errorf("decode leads the source by %d samples", lag)
				}
			}
			if testutil.HaveFFmpeg(t) {
				want := testutil.FFmpegDecodeF32NoSIMD(t, wma)
				if len(got) != len(want) {
					t.Errorf("decoded %d samples, ffmpeg %d", len(got), len(want))
				}
				n := min(len(got), len(want))
				if d := testutil.CompareF32(got[:n], want[:n]); d.RMS > gateRMS || d.MaxAbs > gateMax {
					t.Errorf("differential %v, want rms <= %g max <= %g", d, gateRMS, gateMax)
				}
			}
		})
	}
}
