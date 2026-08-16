package testutil

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// libfdk_aac is the HE-AAC encoder reference. It is non-free, so no
// distribution ffmpeg ships it: absence escalates under WAXFLOW_REQUIRE_FDK=1
// only, never WAXFLOW_REQUIRE_FFMPEG, which no CI ffmpeg could satisfy. The
// committed fixtures under codec/aac/testdata were made with libfdk, so the
// differential suite runs offline everywhere.

// HaveFDK reports whether ffmpeg carries the libfdk_aac encoder, failing
// instead under WAXFLOW_REQUIRE_FDK=1.
func HaveFDK(t testing.TB) bool {
	t.Helper()
	if haveCodecQuiet("-encoders", "libfdk_aac") {
		return true
	}
	if os.Getenv("WAXFLOW_REQUIRE_FDK") == "1" {
		t.Fatal("ffmpeg libfdk_aac required by WAXFLOW_REQUIRE_FDK=1 but not available")
	}
	return false
}

// FFmpegFDKEncodeFile encodes wav with libfdk_aac at the given bitrate and
// profile ("aac_he" or "aac_he_v2") into dir, returning the output path.
// format is "m4a" or "adts". Skips (or fails under WAXFLOW_REQUIRE_FDK=1)
// when this build has no libfdk_aac.
func FFmpegFDKEncodeFile(t testing.TB, dir, wav string, kbps int, profile, format string) string {
	t.Helper()
	if !HaveFDK(t) {
		t.Skipf("ffmpeg has no libfdk_aac encoder; skipping fdk oracle")
	}
	ffmpeg := FFmpeg(t)
	ext := ".m4a"
	if format == "adts" {
		ext = ".aac"
	}
	out := filepath.Join(dir, "fdk_"+profile+"_"+strconv.Itoa(kbps)+ext)
	args := []string{"-v", "error", "-y", "-i", wav,
		"-c:a", "libfdk_aac", "-profile:a", profile,
		"-b:a", strconv.Itoa(kbps * 1000)}
	if format == "adts" {
		args = append(args, "-f", "adts")
	}
	args = append(args, out)
	run(t, ffmpeg, args...)
	return out
}
