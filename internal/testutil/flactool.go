package testutil

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// The flac reference tool is the encoders' second oracle
// beside ffmpeg: `flac -t` is the conformance acceptance the quality
// gates name, and `flac -<level>` is the size baseline. Same policy as
// ffmpeg: tests self-skip when it is missing, and the CI differential
// job sets WAXFLOW_REQUIRE_FLAC=1 so the suite cannot silently thin
// out. A separate variable from WAXFLOW_REQUIRE_FFMPEG keeps the two
// oracles independently installable.

// FlacTool returns the flac reference binary's path, skipping or
// failing per the policy.
func FlacTool(t testing.TB) string {
	t.Helper()
	path, err := exec.LookPath("flac")
	if err != nil {
		if os.Getenv("WAXFLOW_REQUIRE_FLAC") == "1" {
			t.Fatal("flac required by WAXFLOW_REQUIRE_FLAC=1 but not installed")
		}
		t.Skip("flac not installed; skipping reference-tool test")
	}
	return path
}

// FlacTest runs `flac -t` on path and fails the test if the reference
// decoder rejects the stream. Warnings (an unset MD5 on a streamed
// output, say) are tolerated; only a nonzero exit fails.
func FlacTest(t testing.TB, path string) {
	t.Helper()
	run(t, FlacTool(t), "-t", "-s", path)
}

// FlacEncodeFile runs the reference encoder at the given level on a WAV
// input and returns the encoded size in bytes, the size-gate baseline.
func FlacEncodeFile(t testing.TB, wavPath string, level int) int64 {
	t.Helper()
	out := wavPath + ".ref.flac"
	t.Cleanup(func() { os.Remove(out) })
	var errOut bytes.Buffer
	cmd := exec.Command(FlacTool(t), "-f", "-s", "-"+strconv.Itoa(level), "-o", out, wavPath)
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("flac -%d %s: %v\n%s", level, wavPath, err, errOut.String())
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}
