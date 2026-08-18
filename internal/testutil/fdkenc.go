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
//
// The library is reachable two ways: an ffmpeg built with libfdk_aac, or
// the scripts/fdkenc tool linked against the library directly (build
// instructions in its source), named by WAXFLOW_FDKENC. The tool covers
// the ADTS shape the quality gate scores; the m4a-producing helpers still
// need the ffmpeg form.

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

// fdkTool returns the WAXFLOW_FDKENC tool path, or "" when unset or gone.
func fdkTool() string {
	path := os.Getenv("WAXFLOW_FDKENC")
	if path == "" {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// HaveFDKEncoder reports whether libfdk is reachable at all (through
// ffmpeg or the WAXFLOW_FDKENC tool), failing instead under
// WAXFLOW_REQUIRE_FDK=1.
func HaveFDKEncoder(t testing.TB) bool {
	t.Helper()
	if haveCodecQuiet("-encoders", "libfdk_aac") || fdkTool() != "" {
		return true
	}
	if os.Getenv("WAXFLOW_REQUIRE_FDK") == "1" {
		t.Fatal("libfdk_aac required by WAXFLOW_REQUIRE_FDK=1 but neither ffmpeg nor WAXFLOW_FDKENC provides it")
	}
	return false
}

// FDKEncodeADTS encodes wav with libfdk at the given bitrate and profile
// ("aac_he" or "aac_he_v2") into an ADTS stream in dir, through whichever
// libfdk route this machine has: ffmpeg's wrapper, or the WAXFLOW_FDKENC
// tool fed raw s16le (the same 16-bit input depth the wrapper uses).
// Skips (or fails under WAXFLOW_REQUIRE_FDK=1) when neither exists.
func FDKEncodeADTS(t testing.TB, dir, wav string, rate, channels, kbps int, profile string) string {
	t.Helper()
	if haveCodecQuiet("-encoders", "libfdk_aac") {
		return FFmpegFDKEncodeFile(t, dir, wav, kbps, profile, "adts")
	}
	tool := fdkTool()
	if tool == "" {
		if os.Getenv("WAXFLOW_REQUIRE_FDK") == "1" {
			t.Fatal("libfdk_aac required by WAXFLOW_REQUIRE_FDK=1 but neither ffmpeg nor WAXFLOW_FDKENC provides it")
		}
		t.Skipf("no libfdk_aac route (ffmpeg or WAXFLOW_FDKENC); skipping fdk oracle")
	}
	aot := "5"
	if profile == "aac_he_v2" {
		aot = "29"
	}
	raw := filepath.Join(dir, "fdk_in.raw")
	run(t, FFmpeg(t), "-v", "error", "-y", "-i", wav, "-f", "s16le", "-c:a", "pcm_s16le", "-ar", strconv.Itoa(rate), "-ac", strconv.Itoa(channels), raw)
	out := filepath.Join(dir, "fdk_"+profile+"_"+strconv.Itoa(kbps)+".aac")
	run(t, tool, strconv.Itoa(rate), strconv.Itoa(channels), strconv.Itoa(kbps*1000), aot, raw, out)
	return out
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
