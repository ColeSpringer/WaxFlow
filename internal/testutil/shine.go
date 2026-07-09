package testutil

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// Shine is the MP3 baseline quality oracle: LAME's small sibling, reached
// through ffmpeg's libshine encoder. The clean-room policy makes Shine a
// source whose code is never opened while implementing the MP3
// encoder, but invoking its binary as a test oracle is explicitly permitted
// (running a program is not copying it). The MP3 baseline gate is parity with
// Shine on the ODG-proxy (docs/quality-gates.md).
//
// Availability is separate from plain ffmpeg: many builds omit libshine, so
// the oracle self-skips unless WAXFLOW_REQUIRE_SHINE=1 escalates a missing
// encoder to a failure, mirroring the ffmpeg and flac oracle policy.

// Shine returns the ffmpeg path if its libshine encoder is available,
// skipping or failing per the policy.
func Shine(t testing.TB) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err == nil {
		err = exec.Command(path, "-hide_banner", "-h", "encoder=libshine").Run()
	}
	if err != nil {
		if os.Getenv("WAXFLOW_REQUIRE_SHINE") == "1" {
			t.Fatal("ffmpeg libshine required by WAXFLOW_REQUIRE_SHINE=1 but not available")
		}
		t.Skip("ffmpeg libshine not available; skipping MP3 baseline comparison")
	}
	return path
}

// ShineEncodeFile encodes a WAV file to CBR MP3 at the given bit rate (kbit/s)
// with libshine and returns the output path. It is the Shine half of the
// baseline comparison; decode it with FFmpegDecodeF32 to score against the
// reference.
func ShineEncodeFile(t testing.TB, wavPath string, kbps int) string {
	t.Helper()
	out := wavPath + ".shine.mp3"
	t.Cleanup(func() { os.Remove(out) })
	run(t, Shine(t), "-hide_banner", "-v", "error", "-y", "-i", wavPath,
		"-c:a", "libshine", "-b:a", strconv.Itoa(kbps)+"k", out)
	return out
}
