package wmalossless_test

// The gate for this codec, and it needs no tolerance and no tool: a lossless
// decode returns the encoder's input, sample for sample, or it is wrong.
//
// The source is regenerated here rather than committed beside the fixtures.
// That is what makes the comparison total: a checked-in copy of the PCM would
// be four times the size of the stream it verifies, and a digest of the decode
// would say only that nothing changed, not that anything is right. Everything
// in synth() is integer arithmetic, so the sample at frame i is the same on
// every platform, Go version and architecture.
//
// docs/notes/wma-lossless-oracle-corpus.md records the measurements, the
// eight-format envelope Windows' encoder offers, and which cells are
// committed and why.

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmalossless"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
	"github.com/colespringer/waxflow/internal/testutil"
)

// cell is one corpus configuration. Every one of these is a shape Windows'
// encoder actually offers; there is no mono cell and no 16-bit cell above 44.1
// kHz because no such stream can be made.
type cell struct {
	name     string
	rate     int
	channels int
	bits     int
	frames   int
	// frameLen is what the decoder codes at this rate, measured on the corpus
	// (docs/notes/wma-lossless-oracle-corpus.md section 6). It is here only so
	// a mismatch can name the coded frame it landed in.
	frameLen int
	// dup duplicates channel 0 into every other channel. It is the only cell
	// shape that reaches the one-sided case of the two-channel lifting: the
	// encoder codes one channel and leaves the other's coded flag clear, so
	// the decoder has to reconstruct the second channel from the transform
	// rather than from any bits.
	dup bool
	// pad16 carries 16-bit content in a 24-bit stream, which is what makes the
	// encoder set a non-zero padding-zeroes field: the samples are known to
	// have eight low zero bits and the format says so per subframe rather than
	// coding them. It is the only cell that exercises the output stage's
	// shift, and the note calls the case the format rather than a misreading.
	pad16     bool
	committed bool
}

var cells = []cell{
	{"ll-44100-2ch-16", 44100, 2, 16, 16384, 2048, false, false, true},
	{"ll-44100-2ch-16-dup", 44100, 2, 16, 16384, 2048, true, false, true},
	{"ll-44100-2ch-24", 44100, 2, 24, 16384, 2048, false, false, true},
	// Not a whole number of coded frames, so the last one carries an end skip.
	// Every other cell here is an exact multiple and leaves that field zero.
	{"ll-44100-2ch-16-skip", 44100, 2, 16, 15000, 2048, false, false, true},
	// 16-bit content in a 24-bit stream, which is what sets padding zeroes.
	{"ll-44100-2ch-24-pad", 44100, 2, 24, 16384, 2048, false, true, true},
	{"ll-48000-2ch-24", 48000, 2, 24, 32768, 2048, false, false, false},
	{"ll-48000-6ch-24", 48000, 6, 24, 8192, 2048, false, false, true},
	{"ll-88200-2ch-24", 88200, 2, 24, 32768, 4096, false, false, false},
	{"ll-88200-6ch-24", 88200, 6, 24, 16384, 4096, false, false, false},
	{"ll-96000-2ch-24", 96000, 2, 24, 32768, 4096, false, false, false},
	{"ll-96000-6ch-24", 96000, 6, 24, 16384, 4096, false, false, false},
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
// bits with the widest period here, 4*a*x reaches 2.34e9, which is past what a
// 32-bit int holds; in an int it wrapped negative and the value clamped to
// -full, so the same recipe produced different samples on a 32-bit build and
// the fixtures decoded "wrong" there. `make test-386` is what found it.
func tri(i, p, a int) int {
	x := int64(i % p)
	amp := int64(a)
	y := 4 * amp * x / int64(p)
	if y > 2*amp {
		y = 4*amp - y
	}
	return int(y - amp)
}

// synth builds one cell's source PCM, planar. Four equal segments, each a
// whole number of coded frames: two triangles per channel, triangles plus
// quarter-scale noise, digital silence, and full-scale noise. The
// silence-to-noise edge is a hard transient on a frame boundary.
func synth(c cell) [][]int32 {
	// A pad16 cell is the 16-bit signal in a 24-bit container: the same
	// samples, shifted up by the eight zero bits the stream then declares.
	bits := c.bits
	shift := 0
	if c.pad16 {
		bits, shift = 16, c.bits-16
	}
	full := int(1)<<(bits-1) - 1
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
			s[i] = int32(v) << shift
		}
		out[ch] = s
	}
	if c.dup {
		for ch := 1; ch < c.channels; ch++ {
			copy(out[ch], out[0])
		}
	}
	return out
}

// synthInterleaved is what a decode is compared against.
func synthInterleaved(c cell) []int32 {
	planar := synth(c)
	out := make([]int32, c.frames*c.channels)
	for ch, s := range planar {
		for i, v := range s {
			out[i*c.channels+ch] = v
		}
	}
	return out
}

func corpusPath(t testing.TB, name string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "testdata", "corpus", name+".wma")
}

// demux reads one .wma into its track and its packets.
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

// interleave flattens a planar buffer at its own depth. testutil.Interleave
// left-shifts to 32 bits for the FFmpeg S32 differential, which is the wrong
// domain for a comparison against the source.
func interleave(b *audio.Buffer) []int32 {
	out := make([]int32, b.N*b.Fmt.Channels)
	for c := 0; c < b.Fmt.Channels; c++ {
		for i, v := range b.ChanI(c)[:b.N] {
			out[i*b.Fmt.Channels+c] = v
		}
	}
	return out
}

// decodeAll decodes every packet and returns the interleaved samples.
func decodeAll(t testing.TB, track container.Track, pkts [][]byte) []int32 {
	t.Helper()
	cfg, err := wmalossless.ParseConfig(track.CodecConfig)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	dec, err := wmalossless.NewDecoder(cfg, track.Fmt)
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	defer dec.Release()
	var got []int32
	emit := func(b *audio.Buffer) error {
		got = append(got, interleave(b)...)
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

// requireExact compares a decode against the source and says where it first
// went wrong rather than only that it did. A lossless mismatch is a bug, never
// a deficit, so there is no allowlist here and no tolerance to widen.
func requireExact(t *testing.T, c cell, got, want []int32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: decoded %d samples, source has %d (%d frames of %d channels)",
			c.name, len(got), len(want), c.frames, c.channels)
	}
	for i := range got {
		if got[i] != want[i] {
			frame, ch := i/c.channels, i%c.channels
			seg := frame * 4 / c.frames
			t.Fatalf("%s: first mismatch at frame %d channel %d (segment %d of 4, coded frame %d): got %d, want %d",
				c.name, frame, ch, seg+1, frame/c.frameLen, got[i], want[i])
		}
	}
}

// TestCommittedCorpusIsBitExact is the whole gate, and it runs with nothing
// installed: the committed cells decode to the regenerated source sample for
// sample.
func TestCommittedCorpusIsBitExact(t *testing.T) {
	for _, c := range cells {
		if !c.committed {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			track, pkts := demux(t, corpusPath(t, c.name))
			if track.Fmt.Rate != c.rate || track.Fmt.Channels != c.channels {
				t.Fatalf("fixture is %d Hz %dch, table says %d Hz %dch",
					track.Fmt.Rate, track.Fmt.Channels, c.rate, c.channels)
			}
			requireExact(t, c, decodeAll(t, track, pkts), synthInterleaved(c))
		})
	}
}

// TestFFmpegAgreesWithTheSource checks the fixtures themselves rather than
// this decoder: a second implementation returning the same samples from a file
// neither of them wrote is what makes a committed fixture trustworthy without
// a specification to check it against.
func TestFFmpegAgreesWithTheSource(t *testing.T) {
	if !testutil.HaveFFmpeg(t) {
		t.Skip("ffmpeg not installed")
	}
	for _, c := range cells {
		if !c.committed {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			// FFmpeg reports every depth as S32, left-justified, so the
			// comparison undoes the shift rather than the source taking it on.
			shift := 32 - c.bits
			got := testutil.FFmpegDecodeS32(t, corpusPath(t, c.name))
			want := synthInterleaved(c)
			if len(got) != len(want) {
				t.Fatalf("ffmpeg decoded %d samples, source has %d", len(got), len(want))
			}
			for i := range got {
				if got[i]>>shift != want[i] {
					t.Fatalf("sample %d: ffmpeg %d, source %d", i, got[i]>>shift, want[i])
				}
			}
		})
	}
}

// TestMicrosoftCorpus is the wider corpus, generated rather than committed:
// the five shapes that are not worth their bytes in the tree, plus the three
// that are, all re-encoded here so a Windows run covers the whole envelope.
//
// Windows only, because Windows is the only place that can write this format
// at all. That is why the committed three exist: without them a Linux machine
// would decode nothing.
func TestMicrosoftCorpus(t *testing.T) {
	if !testutil.HaveWMFLossless(t) {
		t.Skip("Windows' WMA Lossless encoder is not available here")
	}
	dir := t.TempDir()
	for _, c := range cells {
		t.Run(c.name, func(t *testing.T) {
			wav := filepath.Join(dir, c.name+".wav")
			wma := filepath.Join(dir, c.name+".wma")
			testutil.WriteWAV(t, wav, audio.Format{
				Rate: c.rate, Channels: c.channels, Layout: audio.DefaultLayout(c.channels),
				Type: audio.Int, BitDepth: c.bits,
			}, synthInterleaved(c))
			testutil.WMFEncodeLossless(t, wav, wma)
			track, pkts := demux(t, wma)
			requireExact(t, c, decodeAll(t, track, pkts), synthInterleaved(c))
		})
	}
}
