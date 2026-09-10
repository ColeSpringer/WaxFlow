package musepack_test

import (
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/musepack"
	"github.com/colespringer/waxflow/waxerr"
)

// The tool-free gates: what holds on a machine with no encoder, no ffmpeg and
// no network, from the committed fixtures alone. A lossy decode cannot be
// checked against its source sample for sample, and float decodes differ in
// the last bit across architectures, so nothing here is a digest: the sine
// cells are held to an RMS error in full-scale units and the noise cells to a
// correlation floor, both set from the measurement with headroom.
const sineMaxRMS = 1e-3

// TestFixturesTrackTheirSource decodes every committed fixture to exactly the
// sample count its source had, and close to the source itself.
func TestFixturesTrackTheirSource(t *testing.T) {
	for _, f := range mpcFixtures {
		t.Run(f.name(), func(t *testing.T) {
			cfg, track, raw := decodeFile(t, readFile(t, f.path))
			if cfg.Rate != f.rate || cfg.Channels != f.streamChannels() {
				t.Fatalf("stream is %d Hz %d ch, the table says %d Hz %d ch", cfg.Rate, cfg.Channels, f.rate, f.streamChannels())
			}
			if want := 7; !f.sv7 {
				want = 8
			} else if cfg.StreamVersion != want {
				t.Errorf("stream version %d, the table says %d", cfg.StreamVersion, want)
			}
			want := f.expected(f.source())
			if got := int(track.Samples) * cfg.Channels; got != len(want) {
				t.Fatalf("delivers %d samples, the source had %d", got, len(want))
			}
			got := trimmed(t, track, raw)
			var sum, ea, eb, dot float64
			for i := range got {
				s := float64(want[i])
				d := float64(got[i]) - s
				sum += d * d
				ea, eb, dot = ea+float64(got[i])*float64(got[i]), eb+s*s, dot+float64(got[i])*s
			}
			rms := math.Sqrt(sum / float64(len(got)))
			corr := dot / math.Sqrt(ea*eb)
			t.Logf("%s: rms error %.4g FS, correlation %.4f", f.name(), rms, corr)
			if f.signal == "noise" {
				if f.minCorr <= 0 {
					t.Fatal("a noise fixture needs its measured correlation floor in the table")
				}
				if corr < f.minCorr {
					t.Errorf("correlation with the source %.4f, want >= %.2f", corr, f.minCorr)
				}
			} else if rms > sineMaxRMS {
				t.Errorf("rms error %.4g FS against the source, want <= %.3g", rms, sineMaxRMS)
			}
		})
	}
}

// TestSourceDoesNotDependOnTheCheckout pins every noise cell to its frozen
// seed. The sources above are rebuilt rather than stored, so anything the
// rebuild reads from the machine makes the gate compare a decode against
// noise the encoder never saw, and it holds only where the repository
// happens to sit. Moving a fixture deeper must not move its signal.
func TestSourceDoesNotDependOnTheCheckout(t *testing.T) {
	for _, f := range mpcFixtures {
		// A repack, a cut or a chaptered copy rebuilds its origin's signal,
		// checked there.
		if f.signal != "noise" || f.repackOf != "" || f.cutOf != "" || f.chaptersOf != "" {
			continue
		}
		t.Run(f.name(), func(t *testing.T) {
			if f.seed == 0 {
				t.Fatal("a noise fixture needs the seed its source was built from in the table")
			}
			moved := f
			moved.path = filepath.Join(filepath.Dir(f.path), "deeper", f.name())
			if !slices.Equal(moved.source(), f.source()) {
				t.Error("the source changed when the fixture moved: the rebuild reads the path")
			}
		})
	}
}

// TestFixturesCoverThePaths is a coverage check on the committed set, not a
// decode check. Without it the tool-free gate could quietly shrink to one
// version and one quantiser: the fixtures must reach both stream versions,
// all four rates, mono and stereo, noise substitution, every sample-coding
// path, all three SV7 tail regimes, both block shapes, a stream with no
// encoder info, a nonzero beginning silence and a lossless repack.
func TestFixturesCoverThePaths(t *testing.T) {
	versions, rates, channels := map[int]bool{}, map[int]bool{}, map[int]bool{}
	res := map[int]map[int32]bool{7: {}, 8: {}}
	tails := map[string]bool{}
	blockPwrs := map[int]bool{}
	var noEI, silence, repack, pns bool
	for _, f := range mpcFixtures {
		cfg, track, pkts := demuxAll(t, readFile(t, f.path))
		versions[cfg.StreamVersion] = true
		rates[cfg.Rate] = true
		channels[cfg.Channels] = true
		counts, err := musepack.ResCounts(cfg, pkts)
		if err != nil {
			t.Fatalf("%s: %v", f.name(), err)
		}
		for r, n := range counts {
			if n > 0 {
				res[cfg.StreamVersion][r] = true
			}
		}
		if cfg.StreamVersion == 7 {
			switch rem := f.frames % musepack.FrameLength; {
			case rem == 0:
				tails["full"] = true
			case rem <= 671:
				tails["short"] = true
			default:
				tails["decay"] = true
			}
		} else {
			blockPwrs[cfg.BlockPwr] = true
		}
		noEI = noEI || cfg.PNS == musepack.PNSUnknown
		silence = silence || track.Delay > musepack.SynthDelay
		repack = repack || f.repackOf != ""
		pns = pns || counts[-1] > 0
	}
	if !versions[7] || !versions[8] {
		t.Errorf("stream versions covered: %v, want both 7 and 8", versions)
	}
	for _, r := range []int{44100, 48000, 37800, 32000} {
		if !rates[r] {
			t.Errorf("no fixture at %d Hz", r)
		}
	}
	if !channels[1] || !channels[2] {
		t.Errorf("channel counts covered: %v, want both 1 and 2", channels)
	}
	if !pns {
		t.Error("no fixture uses noise substitution; the generator state is untested without the tools")
	}
	// SV7: the tuple books (1, 2), the per-sample books (3..7) and the raw
	// path (8 and up). SV8: the position code (1), the tuples (2), the nibble
	// pairs (3, 4), the switched books (5..8) and the high-byte path (9 and up).
	for _, want := range []struct {
		version int
		lo, hi  int32
		what    string
	}{
		{7, 1, 1, "3-sample tuples"}, {7, 2, 2, "2-sample tuples"}, {7, 3, 7, "per-sample books"}, {7, 8, 17, "raw samples"},
		{8, 1, 1, "position-coded samples"}, {8, 2, 2, "3-sample tuples"}, {8, 3, 4, "nibble pairs"}, {8, 5, 8, "switched books"}, {8, 9, 15, "high-byte samples"},
	} {
		hit := false
		for r := want.lo; r <= want.hi; r++ {
			hit = hit || res[want.version][r]
		}
		if !hit {
			t.Errorf("no SV%d fixture reaches the %s path (resolutions %d..%d)", want.version, want.what, want.lo, want.hi)
		}
	}
	for _, tail := range []string{"short", "decay", "full"} {
		if !tails[tail] {
			t.Errorf("no SV7 fixture in the %q tail regime", tail)
		}
	}
	if !blockPwrs[0] || !blockPwrs[6] {
		t.Errorf("SV8 block powers covered: %v, want 0 (a key frame every frame) and 6 (the default)", blockPwrs)
	}
	if !noEI {
		t.Error("no SV8 fixture lacks an encoder info packet; the undeclared-PNS seek path is untested")
	}
	if !silence {
		t.Error("no fixture has a nonzero beginning silence")
	}
	if !repack {
		t.Error("no fixture is an mpc2sv8 repack")
	}
}

// TestNibblePairOrder pins the one place the reference leans on compiler
// layout: resolutions 3 and 4 pack two samples into one signed byte, read
// through a bitfield union, low nibble first. The fixture set must reach the
// path (TestFixturesCoverThePaths), and the primary gate proves the order on
// it; this test is the tool-free half, which holds the decode of a nibble-path
// fixture to its source and would fail with the nibbles swapped.
func TestNibblePairOrder(t *testing.T) {
	for _, f := range mpcFixtures {
		if f.sv7 {
			continue
		}
		cfg, track, pkts := demuxAll(t, readFile(t, f.path))
		counts, err := musepack.ResCounts(cfg, pkts)
		if err != nil {
			t.Fatal(err)
		}
		if counts[3]+counts[4] == 0 {
			continue
		}
		got := trimmed(t, track, decodePackets(t, cfg, pkts))
		want := f.expected(f.source())
		var ea, eb, dot float64
		for i := range got {
			s := float64(want[i])
			ea, eb, dot = ea+float64(got[i])*float64(got[i]), eb+s*s, dot+float64(got[i])*s
		}
		corr := dot / math.Sqrt(ea*eb)
		t.Logf("%s reaches the nibble path (%d bands); correlation %.4f", f.name(), counts[3]+counts[4], corr)
		if corr < f.minCorr {
			t.Errorf("%s: correlation %.4f, want >= %.2f", f.name(), corr, f.minCorr)
		}
		return
	}
	t.Fatal("no SV8 fixture reaches resolutions 3 or 4")
}

// TestReleaseIsIdempotentAfterUse pins the Releaser contract: a decoder that
// borrowed a pooled buffer gives it back exactly once.
func TestReleaseIsIdempotentAfterUse(t *testing.T) {
	cfg, _, pkts := demuxAll(t, readFile(t, repoPath("testdata", "sine-s16.mpc")))
	dec, err := musepack.NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(pkts[0], func(*audio.Buffer) error { return nil }); err != nil {
		t.Fatal(err)
	}
	dec.Release()
	dec.Release()
}

// TestDamagedFrameLatchesUntilReset is the failure contract: nothing in a
// packet re-anchors a drifted reader, so a refused packet kills the decoder
// until Reset, and after Reset the next packet decodes as at stream start.
func TestDamagedFrameLatchesUntilReset(t *testing.T) {
	for _, name := range []string{"noise-s16.mpc", "sine-s16.mpc"} {
		t.Run(name, func(t *testing.T) {
			cfg, _, pkts := demuxAll(t, readFile(t, repoPath("testdata", name)))
			dec, err := musepack.NewDecoder(cfg, cfg.Format())
			if err != nil {
				t.Fatal(err)
			}
			defer dec.Release()
			drop := func(*audio.Buffer) error { return nil }
			// Every SV7 frame states its bit length and every SV8 block must
			// end in padding, so a truncated payload is refused.
			damaged := append([]byte(nil), pkts[0][:len(pkts[0])*3/4]...)
			if err := dec.Decode(damaged, drop); err == nil {
				t.Fatal("a truncated packet decoded clean")
			}
			if err := dec.Decode(pkts[len(pkts)-1], drop); err == nil {
				t.Fatal("an intact packet decoded on a failed decoder; the failure must latch")
			}
			dec.Reset()
			if err := dec.Decode(pkts[0], drop); err != nil {
				t.Fatalf("after Reset the first packet fails: %v", err)
			}
		})
	}
}

// TestFrameMustConsumeItsDeclaredBits pins the SV7 length check: a frame that
// reads fewer or more bits than its length field said is refused, as the
// reference refuses it.
func TestFrameMustConsumeItsDeclaredBits(t *testing.T) {
	cfg, _, pkts := demuxAll(t, readFile(t, repoPath("testdata", "noise-s16.mpc")))
	h, _, payload, err := musepack.ParsePacketHeader(pkts[0], cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int{-1, 1} {
		pkt := make([]byte, musepack.PacketHeaderLen+len(payload)+1)
		musepack.PutPacketHeader(pkt, 1, h.BitLen+delta, false)
		copy(pkt[musepack.PacketHeaderLen:], payload)
		dec, err := musepack.NewDecoder(cfg, cfg.Format())
		if err != nil {
			t.Fatal(err)
		}
		err = dec.Decode(pkt, func(*audio.Buffer) error { return nil })
		if err == nil {
			t.Fatalf("a frame declaring %d bits more than it holds decoded clean", delta)
		}
		if waxerr.CodeOf(err) != waxerr.CodeMalformedInput || !strings.HasPrefix(err.Error(), "musepack: ") {
			t.Errorf("error %q lost its code or prefix", err)
		}
		dec.Release()
	}
}

// TestBlockMustEndInPadding pins the SV8 check: a block with a byte or more
// past its last frame is refused, as the reference refuses it.
func TestBlockMustEndInPadding(t *testing.T) {
	cfg, _, pkts := demuxAll(t, readFile(t, repoPath("testdata", "sine-s16.mpc")))
	pkt := append(append([]byte(nil), pkts[0]...), 0)
	dec, err := musepack.NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()
	if err := dec.Decode(pkt, func(*audio.Buffer) error { return nil }); err == nil {
		t.Fatal("a block with a spare byte decoded clean")
	}
}

// TestConfigRoundTrip pins the codec config blob and its refusals.
func TestConfigRoundTrip(t *testing.T) {
	for _, f := range mpcFixtures[:4] {
		cfg, _, _ := demuxAll(t, readFile(t, f.path))
		b, err := cfg.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		back, err := musepack.ParseConfig(b)
		if err != nil {
			t.Fatal(err)
		}
		if back != cfg {
			t.Errorf("%s: config %+v round-tripped to %+v", f.name(), cfg, back)
		}
	}
	base := musepack.Config{StreamVersion: 8, Rate: 44100, Channels: 2, MaxBand: 28, BlockPwr: 6, PNS: 0}
	for name, mutate := range map[string]func(*musepack.Config){
		"SV6":            func(c *musepack.Config) { c.StreamVersion = 6 },
		"three channels": func(c *musepack.Config) { c.Channels = 3 },
		"22050 Hz":       func(c *musepack.Config) { c.Rate = 22050 },
		"max band 0":     func(c *musepack.Config) { c.MaxBand = 0 },
		"max band 32":    func(c *musepack.Config) { c.MaxBand = 32 },
		"odd block pwr":  func(c *musepack.Config) { c.BlockPwr = 3 },
		"SV7 mono":       func(c *musepack.Config) { c.StreamVersion = 7; c.Channels = 1; c.BlockPwr = 0 },
	} {
		c := base
		mutate(&c)
		if _, err := musepack.NewDecoder(c, c.Format()); err == nil {
			t.Errorf("%s: accepted", name)
		} else if name == "SV6" && !strings.Contains(err.Error(), "SV6") {
			t.Errorf("SV6 refusal does not name the version: %v", err)
		}
	}
}
