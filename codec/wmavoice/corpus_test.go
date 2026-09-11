//go:build !wmavoicetablesgen

package wmavoice_test

import (
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmavoice"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
	"github.com/colespringer/waxflow/internal/testutil"
)

// The committed corpus, one cell per format Windows' encoder offers. Seven
// formats is the WHOLE envelope (docs/notes/wma-voice-oracle-corpus.md section
// 2), so no cell here is a sample of a wider space of encoder settings: what
// the corpus does not cover is the space of BITSTREAMS the format allows and
// this encoder never writes, which the note's section 9 lists and
// docs/quality-gates.md carries as deficits.
type cell struct {
	name      string
	rate      int
	bitRate   int
	align     int
	frames    int
	flags     uint32
	denoise   int
	tilt      bool
	dcLevel   int
	lsps      int
	quantB    bool
	defaultB  bool
	spillBits int
	// tree is the variable bit mode class of each frame type, in order, which
	// the extradata carries and the format does not.
	tree [17]int
}

var cells = []cell{
	{name: "voice-8000-4k", rate: 8000, bitRate: 4000, align: 150, frames: 31863,
		flags: 0x031980a7, denoise: 9, dcLevel: 1, lsps: 10, spillBits: 11,
		tree: [17]int{0, 0, 0, 1, 1, 1, 3, 2, 4, 4, 5, 2, 2, 3, 5, 3, 4}},
	{name: "voice-8000-5k", rate: 8000, bitRate: 5000, align: 187, frames: 31863,
		flags: 0x0519809f, denoise: 7, dcLevel: 1, lsps: 10, spillBits: 11,
		tree: [17]int{1, 1, 0, 0, 0, 1, 2, 2, 4, 4, 5, 2, 3, 3, 5, 3, 4}},
	{name: "voice-8000-8k", rate: 8000, bitRate: 8000, align: 300, frames: 31863,
		flags: 0x071980cf, denoise: 3, tilt: true, dcLevel: 1, lsps: 10, spillBits: 12,
		tree: [17]int{0, 1, 3, 4, 4, 4, 5, 5, 2, 2, 3, 1, 0, 0, 3, 1, 2}},
	{name: "voice-11025-10k", rate: 11025, bitRate: 10000, align: 544, frames: 43917,
		flags: 0x0959e115, denoise: 5, dcLevel: 2, lsps: 10, quantB: true, defaultB: true, spillBits: 13,
		tree: [17]int{1, 1, 3, 4, 4, 4, 5, 5, 2, 2, 1, 0, 0, 0, 3, 2, 3}},
	{name: "voice-16000-12k", rate: 16000, bitRate: 12000, align: 450, frames: 63727,
		flags: 0x0b1991a7, denoise: 9, dcLevel: 3, lsps: 16, spillBits: 12,
		tree: [17]int{0, 1, 3, 4, 4, 4, 5, 5, 2, 0, 0, 1, 2, 1, 3, 2, 3}},
	{name: "voice-16000-16k", rate: 16000, bitRate: 16000, align: 600, frames: 63727,
		flags: 0x0d59918d, denoise: 3, dcLevel: 3, lsps: 16, spillBits: 13,
		tree: [17]int{0, 1, 3, 4, 4, 4, 5, 5, 2, 2, 1, 1, 0, 0, 3, 2, 3}},
	{name: "voice-22050-20k", rate: 22050, bitRate: 20000, align: 1088, frames: 87833,
		flags: 0x0f99f205, denoise: 1, dcLevel: 4, lsps: 16, quantB: true, defaultB: true, spillBits: 14,
		tree: [17]int{0, 1, 3, 4, 4, 4, 5, 5, 2, 2, 1, 0, 0, 1, 3, 2, 3}},
}

func corpusPath(t testing.TB, name string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "testdata", "corpus", name+".wma")
}

// demuxerCell is the one-second cell container/asf carries for its own
// differential, decoded here too because it is the only fixture in the tree
// with no silence frames at all.
// demuxerFrames is the demuxer cell's length: one second at 16 kHz.
const demuxerFrames = 16000

func demuxerPath(t testing.TB) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "container", "asf", "testdata", "voice-mono.wma")
}

func demuxerCell(t testing.TB) (container.Track, [][]byte) {
	t.Helper()
	return demux(t, demuxerPath(t))
}

// demux reads one .wma into its track and its packets. It goes through the
// real demuxer rather than a hand-rolled packet walk, so the cells exercise
// the container's 0x000A arm as well as the codec.
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

// decodeAll decodes every packet, then drains, which is not optional here: the
// packet layer always leaves one superframe behind and without the drain every
// cell comes out 480 samples short.
func decodeAll(t testing.TB, track container.Track, pkts [][]byte) []float32 {
	t.Helper()
	dec := newDecoder(t, track)
	defer dec.Release()
	var got []float32
	emit := func(b *audio.Buffer) error {
		got = append(got, b.ChanF(0)[:b.N]...)
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

func newDecoder(t testing.TB, track container.Track) *wmavoice.Decoder {
	t.Helper()
	cfg, err := wmavoice.ParseConfig(track.CodecConfig)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	dec, err := wmavoice.NewDecoder(cfg, track.Fmt)
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	return dec
}

// TestConfigMatchesTheCorpus is the decode-free half of the gate: every field
// of the extradata, decoded against the table the analysis pass measured
// (docs/notes/wma-voice-bitstream.md section 1.3). It is the cheapest possible
// check that the field map is right, and it holds on a machine with no FFmpeg.
func TestConfigMatchesTheCorpus(t *testing.T) {
	for _, c := range cells {
		t.Run(c.name, func(t *testing.T) {
			track, _ := demux(t, corpusPath(t, c.name))
			cfg, err := wmavoice.ParseConfig(track.CodecConfig)
			if err != nil {
				t.Fatalf("parse config: %v", err)
			}
			if cfg.Rate != c.rate {
				t.Errorf("rate %d, want %d", cfg.Rate, c.rate)
			}
			if cfg.BlockAlign != c.align {
				t.Errorf("nBlockAlign %d, want %d", cfg.BlockAlign, c.align)
			}
			if cfg.Flags != c.flags {
				t.Errorf("flags %#08x, want %#08x", cfg.Flags, c.flags)
			}
			if !cfg.Postfilter() {
				t.Error("the postfilter is off, and it is on for every format this encoder offers")
			}
			if cfg.DenoiseStrength() != c.denoise {
				t.Errorf("denoise strength %d, want %d", cfg.DenoiseStrength(), c.denoise)
			}
			if cfg.DenoiseTilt() != c.tilt {
				t.Errorf("denoise tilt %v, want %v", cfg.DenoiseTilt(), c.tilt)
			}
			if cfg.DCLevel() != c.dcLevel {
				t.Errorf("DC level %d, want %d", cfg.DCLevel(), c.dcLevel)
			}
			if cfg.LSPs() != c.lsps {
				t.Errorf("LSP order %d, want %d", cfg.LSPs(), c.lsps)
			}
			if got := wmavoice.QuantiserB(cfg); got != c.quantB {
				t.Errorf("LSP quantiser mode B %v, want %v", got, c.quantB)
			}
			if got := wmavoice.DefaultB(cfg); got != c.defaultB {
				t.Errorf("LSP default mode B %v, want %v", got, c.defaultB)
			}
			// The DC removal filter needs a level above 8 and no format
			// reaches one, which is why it ships as a deficit.
			if cfg.DCLevel() > 8 {
				t.Errorf("DC level %d would run the DC removal filter, which nothing here can verify", cfg.DCLevel())
			}
			if got := wmavoice.TreeClasses(cfg); got != c.tree {
				t.Errorf("variable bit mode classes %v, want %v", got, c.tree)
			}
			if got := wmavoice.SpilloverBits(cfg); got != c.spillBits {
				t.Errorf("spillover field is %d bits, want %d", got, c.spillBits)
			}
		})
	}
}

// TestGeometryMatchesTheNotes pins the pitch bounds and every width derived
// from them against the four-row table of the bitstream note's section 2,
// which is arithmetic on the sample rate with no per-rate arm anywhere.
func TestGeometryMatchesTheNotes(t *testing.T) {
	for _, tc := range []struct {
		rate                            int
		minPitch, maxPitch, pitchBits   int
		history                         int
		conv                            [4]int
		deltaHalf, deltaBits            int
		blockPitchRange, blockPitchBits int
	}{
		{8000, 20, 148, 7, 156, [4]int{20, 50, 88, 147}, 16, 5, 256, 8},
		{11025, 27, 204, 8, 212, [4]int{27, 69, 121, 203}, 16, 5, 355, 9},
		{16000, 40, 296, 8, 304, [4]int{40, 100, 176, 295}, 32, 6, 512, 9},
		{22050, 55, 408, 9, 416, [4]int{55, 137, 242, 407}, 32, 6, 704, 10},
	} {
		cfg := wmavoice.Config{Rate: tc.rate, BlockAlign: 150}
		g := wmavoice.Geometry(cfg)
		got := [...]int{g.MinPitch, g.MaxPitch, g.PitchBits, g.History,
			g.Conv[0], g.Conv[1], g.Conv[2], g.Conv[3],
			g.DeltaPitchHalf, g.DeltaPitchBits, g.BlockPitchRange, g.BlockPitchBits}
		want := [...]int{tc.minPitch, tc.maxPitch, tc.pitchBits, tc.history,
			tc.conv[0], tc.conv[1], tc.conv[2], tc.conv[3],
			tc.deltaHalf, tc.deltaBits, tc.blockPitchRange, tc.blockPitchBits}
		if got != want {
			t.Errorf("%d Hz: geometry %v, want %v", tc.rate, got, want)
		}
	}
}

// The source recipe. All integer, deterministic, and reproduced here rather
// than shipped as a WAV so a regeneration on any platform and architecture
// produces the same samples: the 32-bit pass is what caught a sibling codec's
// recipe overflowing an int, and the same recipe then produced different
// fixtures on a 32-bit build.

// pulse is one glottal pulse with its formant ringing, in Q15. Two damped
// sinusoids, the first at one twelfth of the sample rate and the second at one
// fifth, so the spectrum scales with the rate and every cell carries the same
// sound in the codec's own terms. The numbers are the recipe; they were
// rounded once from
//
//	32767*exp(-n/9)*sin(2*pi*n/12) + 0.5*32767*exp(-n/6)*sin(2*pi*n/5)
//
// normalised to a peak of 32767, and nothing evaluates a transcendental here.
var pulse = [48]int32{
	0, 30806, 32767, 19510, 11277, 10398, 6340, -5009,
	-15713, -17179, -10334, -2583, 1441, 3054, 4954, 6846,
	6503, 3367, -530, -2921, -3401, -2994, -2451, -1638,
	-315, 1127, 1972, 1922, 1299, 585, 0, -480,
	-845, -970, -778, -371, 43, 320, 441, 450,
	368, 209, 10, -160, -248, -244, -181, -94,
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

// noise returns the next sample of white noise with amplitude amp. The
// intermediate is int64 because the product is past what a 32-bit int holds.
func (p *prng) noise(amp int32) int32 {
	return int32(int64(p.next()>>33)*int64(2*amp)>>31) - amp
}

// seg is one segment of the source: a pitch sweep in hertz (zero for none) at
// a voiced amplitude, plus noise at another.
type seg struct {
	f0, f1     int
	voiced     int32
	noise      int32
	highpassed bool
}

// The eight segments, in order. The two silent ones are what turned this
// corpus from a coverage exercise into the thing that found the reference's
// superframe cache defect: they are where the encoder drops to about a hundred
// bits a superframe, and a fixed-size packet then leaves kilobits of tail.
var segs = []seg{
	{f0: 200, f1: 105, voiced: 19000}, // voiced, falling pitch
	{noise: 12000, highpassed: true},  // unvoiced
	{},                                // digital silence
	{f0: 105, f1: 200, voiced: 17000, noise: 3000}, // voiced, rising, breathy
	{f0: 150, f1: 150, voiced: 9000, noise: 9000},  // half and half
	{},                               // digital silence
	{noise: 300},                     // near the noise floor
	{f0: 90, f1: 240, voiced: 26000}, // loud, from a standing start
}

// synth builds one cell's source PCM.
func synth(rate, frames int) []int32 {
	out := make([]int32, frames)
	segLen := frames / len(segs)
	var rnd prng
	var prev int32
	for i, s := range segs {
		lo := i * segLen
		hi := lo + segLen
		if i == len(segs)-1 {
			hi = frames
		}
		if s.f0 != 0 {
			// Pulse onsets at a period that follows the sweep. The period is
			// recomputed at each onset from the sweep's value there, so the
			// pitch is continuous across the segment and the phase never
			// resets.
			for at := lo; at < hi; {
				for k, v := range pulse {
					if at+k >= hi {
						break
					}
					out[at+k] += int32(int64(v) * int64(s.voiced) / 32767)
				}
				at += rate / (s.f0 + (s.f1-s.f0)*(at-lo)/(hi-lo))
			}
		}
		for n := lo; n < hi; n++ {
			if s.noise != 0 {
				v := rnd.noise(s.noise)
				if s.highpassed {
					v, prev = (v-prev)/2, v
				}
				out[n] += v
			}
			out[n] = min(max(out[n], -32768), 32767)
		}
	}
	return out
}

// synthFloat is the source in the domain a decode comes back in.
func synthFloat(rate, frames int) []float32 {
	pcm := synth(rate, frames)
	out := make([]float32, len(pcm))
	for i, v := range pcm {
		out[i] = float32(v) / 32768
	}
	return out
}

// The differential's bounds, and the one place in this tree where a gate skips
// named superframes rather than widening to cover them.
//
// The oracle is wrong in specific places and the note says exactly where. A
// superframe may span two packets, so the decoder holds the tail of a packet
// as a bit-string carry; the reference holds that carry in a 256-byte buffer
// through a helper that silently copies nothing when the copy does not fit,
// while recording the length it was asked for, so a carry above 2048 bits
// makes it decode the next superframe out of an earlier packet's bytes
// (docs/notes/wma-voice-bitstream.md section 3.3). The result is a plausible
// superframe of unrelated content, usually much louder, whose filter memories
// poison what follows. Rebuilt with the buffer enlarged and nothing else
// changed, the reference agrees with a decoder written from the notes to
// 6.5e-06 on every cell.
//
// Widening the tolerance to 4.66 would pass a decoder that was wrong
// everywhere, so the runs are skipped BY NAME below and the superframes that
// start them are cross-checked against this decoder's own carry tracking
// (TestStaleCarryMatchesTheReferencesDefect), which is what gives the
// allowlist teeth.
//
// Measured, this decoder against `ffmpeg -cpuflags 0`: max 1.6e-06 and RMS
// 6.5e-08 on the three 8 kHz cells and the demuxer cell, whole file with
// nothing excluded, and max 5.1e-05 and RMS 9.3e-07 on the other four outside
// the runs. The oracle disagrees with ITSELF by up to 2.3e-06 max and 4.3e-08
// RMS (its vectorised against its scalar path), so nothing can gate below
// that. The bounds are about five times the measured worst case, the usual
// headroom, and both sit above the oracle's own floor.
const (
	gateCleanMax = 1e-5
	gateCleanRMS = 5e-7
)

// staleRuns names, per cell, the superframes FFmpeg decodes from a stale carry
// and the run each one poisons, from the corpus note's section 6. Three
// superframes is the usual cost, because a poisoned superframe in a quiet
// passage is followed by comfort-noise superframes that overwrite the
// excitation outright; two of the runs never recover, and both begin in the
// file's final loud voiced segment where the adaptive codebook's long memory
// keeps the two decodes apart for good.
// stalePoisoned is the superframes themselves, from the same section. It is
// not simply the runs' first entries: on two cells the file's LAST superframe
// is poisoned and sits inside a run that began earlier, which is why those two
// come out longer than their source.
var stalePoisoned = map[string][]int{
	"voice-11025-10k": {29, 69, 79},
	"voice-16000-12k": {49, 99, 109, 132},
	"voice-16000-16k": {39, 49, 79, 89, 99, 109},
	"voice-22050-20k": {59, 119, 139, 159, 182},
}

var staleRuns = map[string][][2]int{
	"voice-11025-10k": {{29, 31}, {69, 71}, {79, 91}},
	"voice-16000-12k": {{49, 51}, {99, 101}, {109, 111}, {132, 132}},
	"voice-16000-16k": {{39, 41}, {49, 51}, {79, 81}, {89, 91}, {99, 101}, {109, 111}},
	"voice-22050-20k": {{59, 61}, {119, 121}, {139, 140}, {159, 182}},
}

// staleDeficits are the four cells scored outside the runs the reference gets
// wrong, each at about twice its own measured figure rather than at one
// class-wide bound, in the shape codec/wma's msDeficits has. They sit above
// the clean cells' bound because the reference's poison bleeds a little past
// the runs the table names (corpus note section 6), by an amount that is the
// cell's and not the class's. Ceilings and floors both: a cell that beats its
// entry by more than 4x fails, so a fix cannot land without tightening the
// entry it fixed. Measured: 9.2e-7/4.6e-8, 5.1e-5/9.3e-7, 9.8e-6/2.0e-7 and
// 4.1e-5/7.7e-7 (max/RMS), in the order below.
var staleDeficits = map[string]struct{ max, rms float64 }{
	"voice-11025-10k": {2e-6, 1e-7},
	"voice-16000-12k": {1.2e-4, 2e-6},
	"voice-16000-16k": {2e-5, 4e-7},
	"voice-22050-20k": {9e-5, 1.6e-6},
}

// TestDecodeMatchesFFmpeg is the gate. FFmpeg has no encoder for this format,
// so every cell is foreign to it: Windows' own encoder made the bytes and
// FFmpeg's scalar decode of them scores this decoder, which makes the encoder
// and the oracle independent.
func TestDecodeMatchesFFmpeg(t *testing.T) {
	if !testutil.HaveFFmpeg(t) {
		t.Skip("ffmpeg not installed")
	}
	for _, c := range cells {
		t.Run(c.name, func(t *testing.T) {
			path := corpusPath(t, c.name)
			track, pkts := demux(t, path)
			got := decodeAll(t, track, pkts)
			ref := testutil.FFmpegDecodeF32NoSIMD(t, path)

			if len(got) != c.frames {
				t.Errorf("decoded %d samples, want the source's %d", len(got), c.frames)
			}
			runs := staleRuns[c.name]
			// FFmpeg's own length is the source's unless its LAST superframe
			// is one of the poisoned ones, where it reads the sample count
			// field out of stale bytes and so emits a whole superframe. Being
			// downstream of a poisoned one is not enough: the field is still
			// there and still read.
			superframes := (c.frames + wmavoice.SuperframeSamples - 1) / wmavoice.SuperframeSamples
			wantRef := c.frames
			if slices.Contains(stalePoisoned[c.name], superframes-1) {
				wantRef = superframes * wmavoice.SuperframeSamples
			}
			if len(ref) != wantRef {
				t.Errorf("FFmpeg decoded %d samples, expected %d", len(ref), wantRef)
			}

			maxAbs, rms, n := compare(got, ref, runs)
			wantMax, wantRMS := gateCleanMax, gateCleanRMS
			if d, ok := staleDeficits[c.name]; ok {
				wantMax, wantRMS = d.max, d.rms
				if 4*maxAbs < d.max && 4*rms < d.rms {
					t.Errorf("over %d samples: max %.3e, RMS %.3e beat the deficit entry (%.0e, %.0e) by more than 4x: tighten it",
						n, maxAbs, rms, d.max, d.rms)
				}
			}
			checkGate(t, n, maxAbs, rms, wantMax, wantRMS)
		})
	}
	// The demuxer's own cell, which the gate text names beside the three 8 kHz
	// cells: it is the one fixture with no silence frames, so it is the only
	// one that scores the pulse-coded path with no comfort noise in it.
	t.Run("voice-mono", func(t *testing.T) {
		path := demuxerPath(t)
		track, pkts := demux(t, path)
		got := decodeAll(t, track, pkts)
		ref := testutil.FFmpegDecodeF32NoSIMD(t, path)
		if len(got) != demuxerFrames || len(ref) != demuxerFrames {
			t.Errorf("decoded %d samples and FFmpeg %d, want %d", len(got), len(ref), demuxerFrames)
		}
		maxAbs, rms, n := compare(got, ref, nil)
		checkGate(t, n, maxAbs, rms, gateCleanMax, gateCleanRMS)
	})
}

func checkGate(t *testing.T, n int, maxAbs, rms, wantMax, wantRMS float64) {
	t.Helper()
	if maxAbs > wantMax || rms > wantRMS {
		t.Errorf("over %d samples: max %.3e (want <= %.0e), RMS %.3e (want <= %.0e)",
			n, maxAbs, wantMax, rms, wantRMS)
	} else {
		t.Logf("over %d samples: max %.3e, RMS %.3e", n, maxAbs, rms)
	}
}

// compare is the differential, with the named runs left out of it.
func compare(got, ref []float32, runs [][2]int) (maxAbs, rms float64, n int) {
	skip := func(s int) bool {
		for _, r := range runs {
			if s >= r[0] && s <= r[1] {
				return true
			}
		}
		return false
	}
	var sum float64
	for i := range min(len(got), len(ref)) {
		if skip(i / wmavoice.SuperframeSamples) {
			continue
		}
		d := math.Abs(float64(got[i]) - float64(ref[i]))
		maxAbs = math.Max(maxAbs, d)
		sum += d * d
		n++
	}
	if n > 0 {
		rms = math.Sqrt(sum / float64(n))
	}
	return maxAbs, rms, n
}

// TestStaleCarryMatchesTheReferencesDefect is what makes the skip list above an
// allowlist rather than a tolerance. The predicate is computable from this
// decoder's own carry length, so the superframes the reference gets wrong are
// derived here and checked against the list, and a change to the carry layer
// that moved them would fail rather than quietly skip the wrong samples.
func TestStaleCarryMatchesTheReferencesDefect(t *testing.T) {
	for _, c := range cells {
		t.Run(c.name, func(t *testing.T) {
			track, pkts := demux(t, corpusPath(t, c.name))
			dec := newDecoder(t, track)
			defer dec.Release()
			sf := 0
			emit := func(b *audio.Buffer) error { sf++; return nil }
			var got []int
			seen := 0
			step := func(f func() error) {
				at := sf
				if err := f(); err != nil {
					t.Fatal(err)
				}
				if n := dec.Paths().StaleCarry; n > seen {
					seen = n
					// The carried superframe is the first one the call emits.
					got = append(got, at)
				}
			}
			for _, p := range pkts {
				step(func() error { return dec.Decode(p, emit) })
			}
			step(func() error { return dec.Drain(emit) })

			want := stalePoisoned[c.name]
			if !slices.Equal(got, want) {
				t.Errorf("carries above %d bits poison superframes %v, the note names %v",
					wmavoice.ReferenceCarryBits, got, want)
			}
		})
	}
}

// TestDecodeAlignsWithTheSource pins the one thing a differential cannot: that
// the output starts where the source does. There is no priming, no trim and no
// delay in this format, which is unusual for this tree, and a decode a frame
// late would still match FFmpeg if FFmpeg were late too.
//
// The comparison is on the short-time ENERGY envelope rather than the
// waveform, because at 4 kbit/s a speech coder reproduces the envelope and the
// pitch and throws the waveform away: measured, the waveform correlation on
// the two lowest-rate cells is within noise of zero, so a waveform lag search
// there finds whatever the noise favours. The envelope is what the codec
// carries at every rate.
func TestDecodeAlignsWithTheSource(t *testing.T) {
	for _, c := range cells {
		t.Run(c.name, func(t *testing.T) {
			track, pkts := demux(t, corpusPath(t, c.name))
			got := decodeAll(t, track, pkts)
			if len(got) != c.frames {
				t.Fatalf("decoded %d samples, want the source's %d", len(got), c.frames)
			}
			src := envelope(synthFloat(c.rate, c.frames))
			dec := envelope(got)
			if lag := bestLag(src, dec, 8); lag != 0 {
				t.Errorf("best-fit envelope lag %d frames, want 0", lag)
			}
			if r := correlation(src, dec, 0); r < 0.9 {
				t.Errorf("envelope correlation %.3f, want at least 0.9", r)
			}
		})
	}
}

// envelope is the root-mean-square of each 160-sample frame, which is what a
// low-rate speech coder preserves.
func envelope(v []float32) []float32 {
	out := make([]float32, len(v)/wmavoice.FrameSamples)
	for i := range out {
		var sum float64
		for _, x := range v[i*wmavoice.FrameSamples : (i+1)*wmavoice.FrameSamples] {
			sum += float64(x) * float64(x)
		}
		out[i] = float32(math.Sqrt(sum / wmavoice.FrameSamples))
	}
	return out
}

// bestLag finds the offset that maximises the correlation of got against src,
// searching both directions.
func bestLag(src, got []float32, span int) int {
	best, bestR := 0, math.Inf(-1)
	for lag := -span; lag <= span; lag++ {
		if r := correlation(src, got, lag); r > bestR {
			bestR, best = r, lag
		}
	}
	return best
}

// correlation is the normalised inner product of the two, with got shifted by
// lag and both taken about their own mean.
func correlation(src, got []float32, lag int) float64 {
	n := 0
	var ms, mg float64
	for i := range src {
		j := i + lag
		if j < 0 || j >= len(got) {
			continue
		}
		ms += float64(src[i])
		mg += float64(got[j])
		n++
	}
	if n == 0 {
		return 0
	}
	ms, mg = ms/float64(n), mg/float64(n)
	var num, ds, dg float64
	for i := range src {
		j := i + lag
		if j < 0 || j >= len(got) {
			continue
		}
		a, b := float64(src[i])-ms, float64(got[j])-mg
		num += a * b
		ds += a * a
		dg += b * b
	}
	if ds == 0 || dg == 0 {
		return 0
	}
	return num / math.Sqrt(ds*dg)
}

// TestMicrosoftCorpus regenerates every cell with Windows' encoder and checks
// the result the way the committed one is checked, so the fixtures can be
// rebuilt by a test run and a drifted encoder shows up here rather than as a
// silent difference in a file nobody re-encoded. Windows only, because Windows
// is the only place that can write this format at all.
//
// MediaTranscoder renegotiates a format it cannot honour rather than refusing
// it, so the shape it landed on is checked against the cell before anything
// else is: the rate, nBlockAlign and the flags word together name the format.
func TestMicrosoftCorpus(t *testing.T) {
	if !testutil.HaveWMFEnc(t) {
		t.Skip("Windows' WMA encoder is not available here")
	}
	dir := t.TempDir()
	for _, c := range cells {
		t.Run(c.name, func(t *testing.T) {
			wav := filepath.Join(dir, c.name+".wav")
			wma := filepath.Join(dir, c.name+".wma")
			f := audio.Format{Rate: c.rate, Channels: 1, Type: audio.Int, BitDepth: 16}
			testutil.WriteWAV(t, wav, f, synth(c.rate, c.frames))
			testutil.WMFEncodeSubtype(t, wav, wma, c.rate, 1, c.bitRate, 16, testutil.SubtypeWMAVoice)
			track, pkts := demux(t, wma)
			cfg, err := wmavoice.ParseConfig(track.CodecConfig)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Rate != c.rate || cfg.BlockAlign != c.align || cfg.Flags != c.flags {
				t.Fatalf("encoded %d Hz, nBlockAlign %d, flags %#x; the cell is %d Hz, %d, %#x",
					cfg.Rate, cfg.BlockAlign, cfg.Flags, c.rate, c.align, c.flags)
			}
			got := decodeAll(t, track, pkts)
			if len(got) != c.frames {
				t.Errorf("decoded %d samples, want the source's %d", len(got), c.frames)
			}
			src, dec := envelope(synthFloat(c.rate, c.frames)), envelope(got)
			if lag := bestLag(src, dec, 8); lag != 0 {
				t.Errorf("best-fit envelope lag %d frames, want 0", lag)
			}
			if r := correlation(src, dec, 0); r < 0.9 {
				t.Errorf("envelope correlation %.3f, want at least 0.9", r)
			}
			if !testutil.HaveFFmpeg(t) {
				return
			}
			// The stale runs are positions in the committed bytes. The encoder
			// is deterministic, so a regenerated cell has the same ones; a
			// re-encode that moved them fails here, and that is the signal to
			// measure the new file rather than to widen anything.
			ref := testutil.FFmpegDecodeF32NoSIMD(t, wma)
			maxAbs, rms, n := compare(got, ref, staleRuns[c.name])
			wantMax, wantRMS := gateCleanMax, gateCleanRMS
			if d, ok := staleDeficits[c.name]; ok {
				wantMax, wantRMS = d.max, d.rms
			}
			checkGate(t, n, maxAbs, rms, wantMax, wantRMS)
		})
	}
}
