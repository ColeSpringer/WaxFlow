package testutil

import (
	"os"
	"strconv"
	"testing"
)

// FFmpegAACEncodeFile encodes a WAV file to AAC-LC in M4A at the given bit
// rate (kbit/s) with ffmpeg's native aac encoder, the AAC quality gate's
// reference (docs/quality-gates.md: a realistic bar, not Apple's encoder).
// The native encoder ships in every ffmpeg build, so plain FFmpeg
// availability (and its WAXFLOW_REQUIRE_FFMPEG escalation) gates it.
// Decode the result with FFmpegDecodeF32 to score against the reference.
func FFmpegAACEncodeFile(t testing.TB, wavPath string, kbps int) string {
	t.Helper()
	out := wavPath + ".ffaac.m4a"
	t.Cleanup(func() { os.Remove(out) })
	run(t, FFmpeg(t), "-hide_banner", "-v", "error", "-y", "-i", wavPath,
		"-c:a", "aac", "-b:a", strconv.Itoa(kbps)+"k", out)
	return out
}
