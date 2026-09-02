package musepack_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/internal/testutil"
)

// The primary gate: our float32 decode against libmpcdec's own float output
// (through mpcdecraw) on streams the reference encoders produce here, cell by
// cell, plus the real-world FATE pair. The reference is the float build of the
// same arithmetic, so the bar is the last bit or two of float32 on the
// samples where the two differ at all.
const (
	oracleMaxFS = 1e-6
	oracleRMSFS = 1e-7
)

// oracleCell is one encoder run: which encoder, its options, and the signal.
type oracleCell struct {
	name     string
	sv7      bool
	args     []string
	rate     int
	channels int
	frames   int
	noise    bool
}

// oracleCells spans both encoders' option space: every rate, both channel
// counts, the profiles that reach each quantiser path, noise substitution,
// mid/side off, the block shapes, and lengths in all three SV7 tail regimes
// (N mod 1152 at most 671, above 671, and exactly 0, which decides whether a
// decay frame exists).
var oracleCells = []oracleCell{
	{"sv7-standard", true, nil, 44100, 2, 12000, false},
	{"sv7-thumb", true, []string{"--thumb"}, 44100, 2, 23740, true},
	{"sv7-braindead", true, []string{"--braindead"}, 44100, 2, 12520, true},
	{"sv7-32k", true, nil, 32000, 2, 11520, false},
	{"sv7-37.8k", true, nil, 37800, 2, 20000, true},
	{"sv7-48k", true, nil, 48000, 2, 30000, false},
	{"sv7-mono", true, nil, 44100, 1, 23740, false},
	{"sv7-pns", true, []string{"--pns", "0.25"}, 44100, 2, 23740, true},
	{"sv7-ms-off", true, []string{"--ms", "0"}, 44100, 2, 12520, true},
	{"sv8-stereo", false, nil, 44100, 2, 44100, false},
	{"sv8-mono", false, nil, 44100, 1, 20000, true},
	{"sv8-32k", false, nil, 32000, 2, 12000, true},
	{"sv8-37.8k", false, nil, 37800, 2, 12520, false},
	{"sv8-48k", false, nil, 48000, 2, 23740, true},
	{"sv8-frames0", false, []string{"--num_frames", "0"}, 44100, 2, 20000, true},
	{"sv8-no-st", false, []string{"--no_st"}, 44100, 2, 20000, false},
	{"sv8-q0-pns", false, []string{"--quality", "0"}, 44100, 2, 44100, true},
	{"sv8-q10", false, []string{"--quality", "10"}, 44100, 2, 20000, true},
	{"sv8-no-ei", false, []string{"--no_ei"}, 44100, 2, 20000, true},
	{"sv8-thumb", false, []string{"--thumb"}, 44100, 2, 30000, true},
}

// cellSignal synthesizes a cell's source samples.
func cellSignal(c oracleCell) []int32 {
	f := intFormat(c.rate, c.channels)
	if c.noise {
		return scaled(interleave(testutil.Noise(f, c.frames, uint64(len(c.name)))), 0.5)
	}
	return interleave(testutil.Sine(f, c.frames, 440, 0.7))
}

// encodeCell writes a cell's WAV and encodes it, returning the .mpc path.
func encodeCell(t testing.TB, dir string, c oracleCell) string {
	t.Helper()
	wav := filepath.Join(dir, c.name+".wav")
	testutil.WriteWAV(t, wav, intFormat(c.rate, c.channels), cellSignal(c))
	out := filepath.Join(dir, c.name+".mpc")
	if c.sv7 {
		testutil.MPPEncodeFile(t, wav, out, c.args...)
	} else {
		testutil.MPCEncodeFile(t, wav, out, c.args...)
	}
	return out
}

// TestDecodeMatchesReference is the primary gate on every tool-generated cell.
func TestDecodeMatchesReference(t *testing.T) {
	if !testutil.HaveMPCTools(t) {
		t.Skip("Musepack reference tools not found (run `make mpc-tools`)")
	}
	dir := t.TempDir()
	for _, c := range oracleCells {
		t.Run(c.name, func(t *testing.T) {
			path := encodeCell(t, dir, c)
			assertMatchesReference(t, path, int64(c.frames))
		})
	}
}

// TestFATEPairMatchesReference is the same gate on the two real-world files.
func TestFATEPairMatchesReference(t *testing.T) {
	if !testutil.HaveMPCTools(t) {
		t.Skip("Musepack reference tools not found (run `make mpc-tools`)")
	}
	for _, name := range []string{"inside-mp7.mpc", "inside-mp8.mpc"} {
		t.Run(name, func(t *testing.T) {
			path := testutil.VectorPath(t, "musepack/"+name)
			_, track, _ := decodeFile(t, readFile(t, path))
			if track.Samples != 524277 {
				t.Errorf("Samples = %d, want 524277", track.Samples)
			}
			assertMatchesReference(t, path, 524277)
		})
	}
}

// assertMatchesReference decodes path both ways and holds our trimmed output
// to the reference's within the gate. sourceLen is the length the stream
// should deliver, -1 to take the reference's word for it.
func assertMatchesReference(t *testing.T, path string, sourceLen int64) diffStats {
	t.Helper()
	raw := readFile(t, path)
	cfg, track, ours := decodeFile(t, raw)
	ref := testutil.MPCDecodeRaw(t, path)
	ch := cfg.Channels
	if got := int64(len(ref) / ch); track.Samples != got {
		t.Errorf("Samples = %d, the reference delivers %d", track.Samples, got)
	}
	if sourceLen >= 0 && track.Samples != sourceLen {
		t.Errorf("Samples = %d, the source had %d", track.Samples, sourceLen)
	}
	if track.Fmt.Type != audio.Float || track.Fmt.Channels != ch {
		t.Errorf("track format %v", track.Fmt)
	}
	got := trimmed(t, track, ours)
	if len(got) != len(ref) {
		t.Fatalf("trimmed decode is %d samples, the reference %d", len(got), len(ref))
	}
	d := compare(got, ref)
	t.Logf("%s: n=%d max=%.3g@%d rms=%.3g exact=%.4f", filepath.Base(path), d.n, d.max, d.maxAt, d.rms, d.exact)
	if d.max > oracleMaxFS || d.rms > oracleRMSFS {
		t.Errorf("differs from libmpcdec beyond the gate (max %g, rms %g): %s", oracleMaxFS, oracleRMSFS, fmt.Sprint(d))
	}
	return d
}
