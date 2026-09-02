package testutil

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The Musepack reference tools are the decoder's fixture generators and its
// primary oracle. Both encoders, mpcenc (SV8) and mppenc 1.16 (SV7), are
// reached as built binaries only and never opened, per the clean-room policy.
// The decode oracle is libmpcdec's own float output through scripts/mpcdecraw,
// built against the pinned library, beside mpc2sv8 (the lossless SV7-to-SV8
// repacker) and mpccut (the only writer of a nonzero beginning silence).
// `make mpc-tools` builds all of them into testdata/tools.
//
// Same policy as the other tool oracles: tests self-skip when a tool is
// missing, and WAXFLOW_REQUIRE_MPC=1 (the CI differential job) escalates
// absence to a failure so the suite cannot silently thin out.

// mpcToolsVersion and mppencVersion are the pinned source releases the tools
// are built from, matching the Makefile and the tarballs in Vectors.
const (
	mpcToolsVersion = "musepack-r475"
	mppencVersion   = "mppenc-1.16"
)

// mpcToolDirs lists where the tools are looked for, in order.
func mpcToolDirs() []string {
	return []string{
		os.Getenv("WAXFLOW_MPC_TOOLS"),
		filepath.Join(VectorsDir(), "..", "tools", mpcToolsVersion),
		filepath.Join(VectorsDir(), "..", "tools", mppencVersion),
	}
}

// mpcTool locates one reference tool, skipping (or failing under
// WAXFLOW_REQUIRE_MPC=1) when it is absent.
func mpcTool(t testing.TB, name string) string {
	t.Helper()
	if path, ok := findMPCTool(name); ok {
		return path
	}
	if os.Getenv("WAXFLOW_REQUIRE_MPC") == "1" {
		t.Fatalf("%s required by WAXFLOW_REQUIRE_MPC=1 but not found (run `make mpc-tools`)", name)
	}
	t.Skipf("%s (Musepack reference tool) not found (run `make mpc-tools`); skipping reference-tool test", name)
	return ""
}

func findMPCTool(name string) (string, bool) {
	for _, dir := range mpcToolDirs() {
		if dir == "" {
			continue
		}
		if path, ok := toolIn(dir, name); ok {
			return path, true
		}
	}
	if path, err := exec.LookPath(name); err == nil {
		return path, true
	}
	return "", false
}

// MPCEncTool locates mpcenc, the SV8 encoder.
func MPCEncTool(t testing.TB) string { return mpcTool(t, "mpcenc") }

// MPPEncTool locates mppenc 1.16, the SV7 encoder.
func MPPEncTool(t testing.TB) string { return mpcTool(t, "mppenc") }

// MPCDecTool locates the reference mpcdec command-line decoder.
func MPCDecTool(t testing.TB) string { return mpcTool(t, "mpcdec") }

// MPCDecRawTool locates mpcdecraw, libmpcdec's float output dumper.
func MPCDecRawTool(t testing.TB) string { return mpcTool(t, "mpcdecraw") }

// MPC2SV8Tool locates mpc2sv8, the SV7-to-SV8 repacker.
func MPC2SV8Tool(t testing.TB) string { return mpcTool(t, "mpc2sv8") }

// MPCCutTool locates mpccut, the SV8 stream cutter.
func MPCCutTool(t testing.TB) string { return mpcTool(t, "mpccut") }

// HaveMPCTools reports whether every Musepack reference tool is available,
// for a test that has something to check without them and more to check with
// them. WAXFLOW_REQUIRE_MPC=1 turns absence into a failure.
func HaveMPCTools(t testing.TB) bool {
	t.Helper()
	for _, name := range []string{"mpcenc", "mppenc", "mpcdec", "mpcdecraw", "mpc2sv8", "mpccut"} {
		if _, ok := findMPCTool(name); !ok {
			if os.Getenv("WAXFLOW_REQUIRE_MPC") == "1" {
				t.Fatalf("%s required by WAXFLOW_REQUIRE_MPC=1 but not found (run `make mpc-tools`)", name)
			}
			return false
		}
	}
	return true
}

// runMPC runs a tool and fails the test with both output streams on error.
func runMPC(t testing.TB, tool string, args ...string) []byte {
	t.Helper()
	var errOut bytes.Buffer
	cmd := exec.Command(tool, args...)
	cmd.Stderr = &errOut
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s%s", filepath.Base(tool), args, err, errOut.String(), out)
	}
	return out
}

// MPCEncodeFile encodes a WAV with mpcenc (SV8) into out, with any extra
// encoder options in front of the file names.
func MPCEncodeFile(t testing.TB, wav, out string, args ...string) {
	t.Helper()
	full := append([]string{"--silent", "--overwrite"}, args...)
	runMPC(t, MPCEncTool(t), append(full, wav, out)...)
}

// MPPEncodeFile encodes a WAV with mppenc 1.16 (SV7) into out.
func MPPEncodeFile(t testing.TB, wav, out string, args ...string) {
	t.Helper()
	full := append([]string{"--silent", "--overwrite"}, args...)
	runMPC(t, MPPEncTool(t), append(full, wav, out)...)
}

// MPCDecodeRaw decodes a .mpc with libmpcdec and returns its float32 output,
// interleaved, after the library's own delay and tail trimming. The dump goes
// to the test's own temporary directory, never beside the input, which for a
// pinned vector is the shared digest-checked cache.
func MPCDecodeRaw(t testing.TB, path string) []float32 {
	t.Helper()
	out := filepath.Join(t.TempDir(), filepath.Base(path)+".f32")
	runMPC(t, MPCDecRawTool(t), path, out)
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	samples := make([]float32, len(raw)/4)
	for i := range samples {
		samples[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return samples
}

// MPCStreamInfo is what libmpcdec reports about a stream.
type MPCStreamInfo struct {
	StreamVersion int
	Rate          int
	Channels      int
	MaxBand       int
	MS            bool
	BlockPwr      int
	TrueGapless   bool
	// Samples is the header's count, BegSilence the leading samples the
	// library skips, Length the count it delivers (Samples - BegSilence).
	Samples, BegSilence, Length int64
	// PNS is 0 off, 1 on, 255 when the stream does not say.
	PNS int
	// Gains and peaks in the library's storage form: Q8.8 loudness (replay
	// gain dB = 64.82 - value/256) and Q8.8 dB peaks.
	TitleGain, TitlePeak, AlbumGain, AlbumPeak int
	HeaderPosition                             int64
}

// MPCInfo reads a stream's properties through libmpcdec.
func MPCInfo(t testing.TB, path string) MPCStreamInfo {
	t.Helper()
	out := runMPC(t, MPCDecRawTool(t), "-i", path)
	fields := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			fields[k] = v
		}
	}
	num := func(key string) int64 {
		v, err := strconv.ParseInt(fields[key], 10, 64)
		if err != nil {
			t.Fatalf("mpcdecraw -i %s: %s=%q: %v", path, key, fields[key], err)
		}
		return v
	}
	return MPCStreamInfo{
		StreamVersion:  int(num("stream_version")),
		Rate:           int(num("sample_freq")),
		Channels:       int(num("channels")),
		MaxBand:        int(num("max_band")),
		MS:             num("ms") != 0,
		BlockPwr:       int(num("block_pwr")),
		TrueGapless:    num("is_true_gapless") != 0,
		Samples:        num("samples"),
		BegSilence:     num("beg_silence"),
		Length:         num("length_samples"),
		PNS:            int(num("pns")),
		TitleGain:      int(num("gain_title")),
		TitlePeak:      int(num("peak_title")),
		AlbumGain:      int(num("gain_album")),
		AlbumPeak:      int(num("peak_album")),
		HeaderPosition: num("header_position"),
	}
}

// MPC2SV8File repacks an SV7 stream as SV8 losslessly.
func MPC2SV8File(t testing.TB, in, out string) {
	t.Helper()
	os.Remove(out) // the tool refuses to overwrite
	runMPC(t, MPC2SV8Tool(t), in, out)
}

// MPCCutFile cuts an SV8 stream to [from, to) samples, to 0 meaning the end.
// The cut starts on a block boundary and the cut file states the remainder as
// beginning silence, which is the only way that field gets a nonzero value.
func MPCCutFile(t testing.TB, in, out string, from, to int64) {
	t.Helper()
	os.Remove(out)
	args := []string{"-s", strconv.FormatInt(from, 10)}
	if to > 0 {
		args = append(args, "-e", strconv.FormatInt(to, 10))
	}
	runMPC(t, MPCCutTool(t), append(args, in, out)...)
}

// MPCToolsDescription names the tool set for skip messages.
func MPCToolsDescription() string {
	return fmt.Sprintf("%s and %s reference tools", mpcToolsVersion, mppencVersion)
}
