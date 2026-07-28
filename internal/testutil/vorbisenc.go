package testutil

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// libvorbis is the Vorbis encoder-quality reference (docs/quality-gates.md).
// The clean-room policy makes libvorbis a source never opened while
// implementing our encoder, but invoking its binary as a test oracle is
// permitted. Availability is separate from plain ffmpeg: some builds omit
// libvorbis, so the oracle self-skips unless WAXFLOW_REQUIRE_FFMPEG=1 escalates
// a missing encoder to a failure, matching the shine/ffmpeg policy.

// HaveLibVorbis reports whether ffmpeg carries the libvorbis encoder.
func HaveLibVorbis(t testing.TB) bool { return haveCodec(t, "-encoders", "libvorbis") }

// HaveLibVorbisDecoder reports whether ffmpeg carries the libvorbis decoder,
// which a build can omit independently of the encoder. It is the reference
// Vorbis decoder the quality gates pin; ffmpeg's own native Vorbis decoder is a
// separate implementation reached without -c:a libvorbis.
func HaveLibVorbisDecoder(t testing.TB) bool { return haveCodec(t, "-decoders", "libvorbis") }

// haveCodec reports whether ffmpeg's -encoders/-decoders listing names codec,
// applying the same require-or-skip policy as the other oracles.
//
// It parses the listing rather than asking `ffmpeg -h encoder=NAME`, which looks
// like a membership test and is not one: for a codec ffmpeg does not have it
// prints "Codec 'NAME' is not recognized by FFmpeg" and still exits 0, so an
// exit-status check answers "is ffmpeg installed", not "does ffmpeg have this".
// Both of these reported every codec present on every machine with ffmpeg, which
// silently disarmed the skips that depend on them.
func haveCodec(t testing.TB, listing, codec string) bool {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err == nil {
		out, runErr := exec.Command(path, "-hide_banner", listing).Output()
		if runErr == nil {
			for _, line := range strings.Split(string(out), "\n") {
				// Each entry is " FLAGS name description"; match the name field
				// exactly so "libvorbis" cannot be found inside a description.
				if f := strings.Fields(line); len(f) >= 2 && f[1] == codec {
					return true
				}
			}
		}
	}
	if os.Getenv("WAXFLOW_REQUIRE_FFMPEG") == "1" {
		t.Fatalf("ffmpeg %s %s required by WAXFLOW_REQUIRE_FFMPEG=1 but not available", listing, codec)
	}
	return false
}

// FFmpegVorbisEncodeFile encodes a WAV file to Ogg-Vorbis with libvorbis at the
// given quality (-q:a scale, libvorbis's native VBR knob) and returns the
// output path. Decode it with FFmpegDecodeF32 to score against the reference.
// It skips (or fails under WAXFLOW_REQUIRE_FFMPEG) when libvorbis is absent.
func FFmpegVorbisEncodeFile(t testing.TB, wavPath string, quality float64) string {
	t.Helper()
	if !HaveLibVorbis(t) { // fails instead under WAXFLOW_REQUIRE_FFMPEG=1
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
	if !HaveLibVorbis(t) { // fails instead under WAXFLOW_REQUIRE_FFMPEG=1
		t.Skip("ffmpeg libvorbis not available; skipping Vorbis baseline comparison")
	}
	out := wavPath + ".libvorbis.ogg"
	t.Cleanup(func() { os.Remove(out) })
	run(t, FFmpeg(t), "-hide_banner", "-v", "error", "-y", "-i", wavPath,
		"-c:a", "libvorbis", "-b:a", strconv.Itoa(kbps)+"k", out)
	return out
}
