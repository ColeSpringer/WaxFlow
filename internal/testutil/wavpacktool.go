package testutil

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The reference wavpack tools are the WavPack decoder's second oracle beside
// ffmpeg, and the more important one: ffmpeg's encoder emits a narrow slice of
// the format (two decorrelation terms, never a false-stereo block, never the
// deeper cascades the compression levels choose), so a decoder verified only
// against it is verified against a fraction of what real files contain.
// `wavpack` generates fixtures across that range and `wvunpack -v` is the
// reference's own verification of a stream.
//
// Same policy as the flac tool: tests self-skip when the binaries are missing,
// and WAXFLOW_REQUIRE_WAVPACK=1 (the CI differential job) escalates absence to
// a failure so the suite cannot silently thin out.

// WavPackTool returns the reference encoder's path, skipping or failing per
// the policy.
func WavPackTool(t testing.TB) string { return wavpackTool(t, "wavpack") }

// WvUnpackTool returns the reference decoder's path, skipping or failing per
// the policy.
func WvUnpackTool(t testing.TB) string { return wavpackTool(t, "wvunpack") }

func wavpackTool(t testing.TB, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		if os.Getenv("WAXFLOW_REQUIRE_WAVPACK") == "1" {
			t.Fatalf("%s required by WAXFLOW_REQUIRE_WAVPACK=1 but not installed", name)
		}
		t.Skipf("%s not installed; skipping reference-tool test", name)
	}
	return path
}

// WavPackEncodeFile runs the reference encoder on a WAV input with the given
// options (compression level, channel handling) and returns the .wv path. The
// output lands beside the input, named for the options so one source can feed
// several cells.
func WavPackEncodeFile(t testing.TB, wavPath, name string, opts ...string) string {
	t.Helper()
	out := filepath.Join(filepath.Dir(wavPath), name)
	t.Cleanup(func() { os.Remove(out) })
	args := append([]string{"-y", "-q"}, opts...)
	args = append(args, wavPath, "-o", out)
	var errOut bytes.Buffer
	cmd := exec.Command(WavPackTool(t), args...)
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("wavpack %v: %v\n%s", args, err, errOut.String())
	}
	return out
}

// WvUnpackVerify runs `wvunpack -v` on path, the reference decoder's own
// verification of a stream, and fails the test if it rejects it. It is how a
// fixture is confirmed well-formed before our decoder is blamed for a
// mismatch.
func WvUnpackVerify(t testing.TB, path string) {
	t.Helper()
	var errOut bytes.Buffer
	cmd := exec.Command(WvUnpackTool(t), "-v", "-q", path)
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("wvunpack -v %s: %v\n%s", path, err, errOut.String())
	}
}

// WvUnpackDecodeFile decodes a .wv with the reference decoder and returns the
// data chunk of the WAV it writes.
//
// It is the strongest check available on an encoder: `wvunpack -v` says the
// reference accepts the stream, and this says the samples it gets back are the
// ones that went in. Neither our decoder nor ffmpeg's reads every header field,
// so a block can be wrong in a way both of them shrug off; the reference reads
// all of it.
func WvUnpackDecodeFile(t testing.TB, path string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "wvunpack.wav")
	var errOut bytes.Buffer
	cmd := exec.Command(WvUnpackTool(t), "-y", "-q", path, "-o", out)
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("wvunpack %s: %v\n%s", path, err, errOut.String())
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return WAVData(t, raw)
}

// WAVData returns a canonical WAV's data chunk.
func WAVData(t testing.TB, raw []byte) []byte {
	t.Helper()
	for i := 12; i+8 <= len(raw); {
		n := int(binary.LittleEndian.Uint32(raw[i+4:]))
		if string(raw[i:i+4]) == "data" {
			if i+8+n > len(raw) {
				t.Fatalf("WAV data chunk of %d bytes overruns the %d-byte file", n, len(raw))
			}
			return raw[i+8 : i+8+n]
		}
		i += 8 + n + n&1
	}
	t.Fatal("WAV has no data chunk")
	return nil
}
