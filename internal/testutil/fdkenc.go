package testutil

import (
	"path/filepath"
	"strconv"
	"testing"
)

// libfdk_aac is the HE-AAC encoder reference. It is non-free, so most stock
// ffmpeg builds omit it and these oracles self-skip; WAXFLOW_REQUIRE_FFMPEG=1
// escalates absence to a failure on machines that promise it (the libvorbis
// policy). The committed HE-AAC fixtures under codec/aac/testdata were
// produced with libfdk so the differential suite runs offline everywhere;
// these helpers exist for regenerating them and for CI runs that carry fdk.

// HaveFDK reports whether ffmpeg carries the libfdk_aac encoder.
func HaveFDK(t testing.TB) bool { return haveCodec(t, "-encoders", "libfdk_aac") }

// FFmpegFDKEncodeFile encodes wav with libfdk_aac at the given bitrate and
// profile ("aac_he" or "aac_he_v2") into dir, returning the output path.
// format is "m4a" or "adts". Skips (or fails under WAXFLOW_REQUIRE_FFMPEG=1)
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
