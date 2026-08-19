package testutil

import (
	"bytes"
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
