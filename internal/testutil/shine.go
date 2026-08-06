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
//
// The membership test is haveCodec's listing parse, not `ffmpeg -h
// encoder=libshine`: that command exits 0 whether or not the encoder exists
// (it prints "Codec 'libshine' is not recognized by FFmpeg" and succeeds), so
// the check it looks like it is making it does not make. This gate answered
// "yes" on every machine with ffmpeg for as long as it was written that way.
func Shine(t testing.TB) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil || !haveCodecQuiet("-encoders", "libshine") {
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

// HaveLAME reports whether ffmpeg carries libmp3lame. LAME is an
// informational reference column in the quality report, never a gate, so
// absence is a false, not a skip: this deliberately does not escalate under
// WAXFLOW_REQUIRE_FFMPEG, which is why it does not go through haveCodec.
func HaveLAME(t testing.TB) bool {
	t.Helper()
	return haveCodecQuiet("-encoders", "libmp3lame")
}

// LAMEEncodeFile encodes a WAV file to CBR MP3 with libmp3lame and returns
// the output path.
func LAMEEncodeFile(t testing.TB, wavPath string, kbps int) string {
	t.Helper()
	out := wavPath + ".lame.mp3"
	t.Cleanup(func() { os.Remove(out) })
	run(t, FFmpeg(t), "-hide_banner", "-v", "error", "-y", "-i", wavPath,
		"-c:a", "libmp3lame", "-b:a", strconv.Itoa(kbps)+"k", out)
	return out
}
