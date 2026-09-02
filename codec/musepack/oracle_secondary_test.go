package musepack_test

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/codec/musepack"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestRepackDecodesIdentically is the repack gate: mpc2sv8 rewrites an SV7
// stream's quantised values into SV8 packets without touching them, so the
// two files must decode to the same float32 samples bit for bit. It pins the
// two frame readers, the two scalefactor state machines and the noise
// generator's draw order against each other with no rounding slack.
func TestRepackDecodesIdentically(t *testing.T) {
	if !testutil.HaveMPCTools(t) {
		t.Skip("Musepack reference tools not found (run `make mpc-tools`)")
	}
	dir := t.TempDir()
	for _, c := range oracleCells {
		if !c.sv7 {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			sv7 := encodeCell(t, dir, c)
			sv8 := filepath.Join(dir, c.name+"-sv8.mpc")
			testutil.MPC2SV8File(t, sv7, sv8)
			cfg7, track7, raw7 := decodeFile(t, readFile(t, sv7))
			cfg8, track8, raw8 := decodeFile(t, readFile(t, sv8))
			if cfg7.StreamVersion != 7 || cfg8.StreamVersion != 8 {
				t.Fatalf("stream versions %d and %d", cfg7.StreamVersion, cfg8.StreamVersion)
			}
			if track7.Samples != track8.Samples || track7.Delay != track8.Delay {
				t.Fatalf("SV7 delivers %d after %d, the repack %d after %d", track7.Samples, track7.Delay, track8.Samples, track8.Delay)
			}
			a, b := trimmed(t, track7, raw7), trimmed(t, track8, raw8)
			if d := compare(a, b); d.max != 0 {
				t.Errorf("the repack decodes differently: %+v", d)
			}
		})
	}
}

// The secondary gate, against ffmpeg's independent decoders (mpc7/mpc8): a
// different implementation with fixed-point synthesis and its own noise
// generator, so its floor is the oracle's, measured at 1.3 LSB16 max and
// 0.41 LSB16 rms and held here at the plan's bound of twice that. ffmpeg
// trims neither the synthesis delay nor the tail, and never emits an SV7
// decay frame, so its output is our raw timeline over the frames it emits.
const (
	ffmpegMaxFS = 4.0 / 32768
	ffmpegRMSFS = 1.5 / 32768
)

// The measured divergence on noise-substituted bands: the power of ffmpeg's
// substituted noise over libmpcdec's (which ours reproduces bit for bit),
// separated from the shared content through the difference signal. The
// reference draws its noise as a sum of four bytes, a near-Gaussian of
// standard deviation 147, scaled so a band lands at the encoder's level;
// ffmpeg's lands 3.6 to 3.7 times as loud in power (about 5.6 dB) on every
// cell measured, SV7 and SV8 alike, with the content correlating at 1.0000.
// The band is a staleness check as much as a bound: a release of ffmpeg that
// changes its noise level fails here and gets this text revisited.
const (
	ffmpegNoisePowerLo = 3.3
	ffmpegNoisePowerHi = 4.0
)

// TestDecodeMatchesFFmpeg compares every cell and the FATE pair against
// ffmpeg over the overlap: sample for sample where nothing is noise-coded, by
// the shared content and the separated noise powers where something is.
func TestDecodeMatchesFFmpeg(t *testing.T) {
	if !testutil.HaveMPCTools(t) {
		t.Skip("Musepack reference tools not found (run `make mpc-tools`)")
	}
	testutil.FFmpeg(t)
	dir := t.TempDir()
	for _, c := range oracleCells {
		t.Run(c.name, func(t *testing.T) {
			assertMatchesFFmpeg(t, encodeCell(t, dir, c))
		})
	}
	for _, name := range []string{"inside-mp7.mpc", "inside-mp8.mpc"} {
		t.Run(name, func(t *testing.T) {
			assertMatchesFFmpeg(t, testutil.VectorPath(t, "musepack/"+name))
		})
	}
}

func assertMatchesFFmpeg(t *testing.T, path string) {
	t.Helper()
	cfg, track, ours := decodeFile(t, readFile(t, path))
	ref := testutil.FFmpegDecodeF32NoSIMD(t, path)
	ch := cfg.Channels
	n := min(len(ours), len(ref))
	// ffmpeg emits the header's frames untrimmed, so the overlap is at least
	// the delay and a frame; for SV7 it stops one frame short of ours when a
	// decay frame exists, since ffmpeg never reads one.
	if n < int(track.Delay+musepack.FrameLength)*ch {
		t.Fatalf("overlap of %d samples (ours %d, ffmpeg %d)", n, len(ours), len(ref))
	}
	if len(ref) > len(ours) || len(ours)-len(ref) > musepack.FrameLength*ch {
		t.Errorf("ffmpeg emits %d samples, ours %d; the two should differ by at most the decay frame", len(ref), len(ours))
	}
	pns := usesNoise(t, cfg, path)
	d := compare(ours[:n], ref[:n])
	t.Logf("%s vs ffmpeg: n=%d max=%.3g rms=%.3g exact=%.4f noise=%v", filepath.Base(path), d.n, d.max, d.rms, d.exact, pns)
	if !pns {
		if d.max > ffmpegMaxFS || d.rms > ffmpegRMSFS {
			t.Errorf("differs from ffmpeg beyond the gate (max %g rms %g): %+v", ffmpegMaxFS, ffmpegRMSFS, d)
		}
		return
	}
	// Noise-coded: the same content with two different noise fills. The
	// fills are independent of each other and of the content, so the power
	// of the difference signal is the sum of the two noise powers, and the
	// power difference between the outputs is their difference; the shared
	// content is what is left, and it must correlate as the noise-free cells
	// do. In full-scale units per sample.
	var ea, eb, dot, ed float64
	for i := 0; i < n; i++ {
		a, b := float64(ours[i]), float64(ref[i])
		ea, eb, dot = ea+a*a, eb+b*b, dot+a*b
		ed += (a - b) * (a - b)
	}
	noiseFF := (ed + eb - ea) / 2
	noiseRef := (ed - eb + ea) / 2
	content := ea - noiseRef
	t.Logf("%s: shared content %.4g, our noise %.4g, ffmpeg noise %.4g (ratio %.3f), content correlation %.4f",
		filepath.Base(path), content/float64(n), noiseRef/float64(n), noiseFF/float64(n), noiseFF/noiseRef, dot/content)
	if noiseRef <= 0 || content <= 0 {
		t.Fatal("a cell with no separable noise or content proves nothing")
	}
	if r := noiseFF / noiseRef; r < ffmpegNoisePowerLo || r > ffmpegNoisePowerHi {
		t.Errorf("ffmpeg's substituted noise carries %.3f times the reference's power; the recorded divergence is %g..%g",
			r, ffmpegNoisePowerLo, ffmpegNoisePowerHi)
	}
	if c := dot / content; math.Abs(c-1) > 0.02 {
		t.Errorf("the shared content correlates at %.4f, want 1 within 2%%", c)
	}
}

// usesNoise reports whether a stream codes any band with noise substitution,
// which is what makes two decoders legitimately disagree on it.
func usesNoise(t *testing.T, cfg musepack.Config, path string) bool {
	t.Helper()
	_, _, pkts := demuxAll(t, readFile(t, path))
	counts, err := musepack.ResCounts(cfg, pkts)
	if err != nil {
		t.Fatal(err)
	}
	return counts[-1] > 0
}
