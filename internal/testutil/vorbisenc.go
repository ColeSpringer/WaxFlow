package testutil

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// libvorbis is the Vorbis encoder-quality reference (docs/quality-gates.md).
// The clean-room policy makes libvorbis a source never opened while
// implementing our encoder, but invoking its binary as a test oracle is
// permitted. Availability is separate from plain ffmpeg: some builds omit
// libvorbis, so the oracle self-skips unless WAXFLOW_REQUIRE_FFMPEG=1 escalates
// a missing encoder to a failure, matching the shine/ffmpeg policy.

// HaveLibVorbis reports whether ffmpeg carries the libvorbis encoder.
func HaveLibVorbis(t testing.TB) bool {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return false
	}
	return exec.Command(path, "-hide_banner", "-h", "encoder=libvorbis").Run() == nil
}

// FFmpegVorbisEncodeFile encodes a WAV file to Ogg-Vorbis with libvorbis at the
// given quality (-q:a scale, libvorbis's native VBR knob) and returns the
// output path. Decode it with FFmpegDecodeF32 to score against the reference.
// It skips (or fails under WAXFLOW_REQUIRE_FFMPEG) when libvorbis is absent.
func FFmpegVorbisEncodeFile(t testing.TB, wavPath string, quality float64) string {
	t.Helper()
	if !HaveLibVorbis(t) {
		if os.Getenv("WAXFLOW_REQUIRE_FFMPEG") == "1" {
			t.Fatal("ffmpeg libvorbis required by WAXFLOW_REQUIRE_FFMPEG=1 but not available")
		}
		t.Skip("ffmpeg libvorbis not available; skipping Vorbis baseline comparison")
	}
	out := wavPath + ".libvorbis.ogg"
	t.Cleanup(func() { os.Remove(out) })
	run(t, FFmpeg(t), "-hide_banner", "-v", "error", "-y", "-i", wavPath,
		"-c:a", "libvorbis", "-q:a", strconv.FormatFloat(quality, 'g', -1, 64), out)
	return out
}

// FFmpegVorbisEncodeBitrate encodes a WAV file to Ogg-Vorbis with libvorbis at a
// target average bit rate (kbit/s), for a bitrate-matched comparison.
func FFmpegVorbisEncodeBitrate(t testing.TB, wavPath string, kbps int) string {
	t.Helper()
	if !HaveLibVorbis(t) {
		if os.Getenv("WAXFLOW_REQUIRE_FFMPEG") == "1" {
			t.Fatal("ffmpeg libvorbis required by WAXFLOW_REQUIRE_FFMPEG=1 but not available")
		}
		t.Skip("ffmpeg libvorbis not available; skipping Vorbis baseline comparison")
	}
	out := wavPath + ".libvorbis.ogg"
	t.Cleanup(func() { os.Remove(out) })
	run(t, FFmpeg(t), "-hide_banner", "-v", "error", "-y", "-i", wavPath,
		"-c:a", "libvorbis", "-b:a", strconv.Itoa(kbps)+"k", out)
	return out
}
