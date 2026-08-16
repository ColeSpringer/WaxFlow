package testutil

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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

// FFmpegEncoder and FFmpegDecoder gate a test on one external ffmpeg codec,
// skipping (or failing under WAXFLOW_REQUIRE_FFMPEG=1) when this build omits
// it. They are the general form of HaveLibVorbis and friends, for the fixture
// generators that name a codec inline; see haveCodecQuiet for why a missing
// codec is an ordinary state to handle rather than a broken machine.
func FFmpegEncoder(t testing.TB, name string) {
	t.Helper()
	FFmpeg(t) // take the plain ffmpeg skip first, so absence reads as absence
	if !haveCodec(t, "-encoders", name) {
		t.Skipf("ffmpeg has no %s encoder; skipping differential test", name)
	}
}

// FFmpegDecoder is FFmpegEncoder for the decoder listing, which a build can
// carry independently of the encoder.
func FFmpegDecoder(t testing.TB, name string) {
	t.Helper()
	FFmpeg(t)
	if !haveCodec(t, "-decoders", name) {
		t.Skipf("ffmpeg has no %s decoder; skipping differential test", name)
	}
}

// haveCodec reports whether ffmpeg's -encoders/-decoders listing names codec,
// applying the same require-or-skip policy as the other oracles.
func haveCodec(t testing.TB, listing, codec string) bool {
	t.Helper()
	if haveCodecQuiet(listing, codec) {
		return true
	}
	if os.Getenv("WAXFLOW_REQUIRE_FFMPEG") == "1" {
		t.Fatalf("ffmpeg %s %s required by WAXFLOW_REQUIRE_FFMPEG=1 but not available", listing, codec)
	}
	return false
}

// haveCodecQuiet is the membership test itself, without the require policy, for
// the callers that own their own (Shine's WAXFLOW_REQUIRE_SHINE, HaveFDK's
// WAXFLOW_REQUIRE_FDK, HaveLAME's deliberate lack of one).
//
// It parses the listing rather than asking `ffmpeg -h encoder=NAME`, which looks
// like a membership test and is not one: for a codec ffmpeg does not have it
// prints "Codec 'NAME' is not recognized by FFmpeg" and still exits 0, so an
// exit-status check answers "is ffmpeg installed", not "does ffmpeg have this".
// Every gate written that way reported every codec present on every machine
// with ffmpeg, silently disarming the skips that depend on it.
//
// These are ffmpeg *build options*, not platform facts: libvorbis, libshine and
// libmp3lame are all portable, and whether a given ffmpeg carries them is a
// packaging choice (Homebrew's ffmpeg 8 ships libmp3lame and libopus but
// neither libvorbis nor libshine). A suite that assumes them fails on a
// perfectly good machine and reads as a code defect, which is the whole reason
// these gates exist.
func haveCodecQuiet(listing, codec string) bool {
	return codecListing(listing)[codec]
}

// codecListing is ffmpeg's -encoders or -decoders listing as a set, read once
// per listing per test binary. The gates are per-row and per-fixture (six calls
// in one table test, one per generated Matroska fixture), and ffmpeg does not
// grow codecs mid-run, so forking it per call is pure latency.
var codecListing = func() func(string) map[string]bool {
	memo := map[string]func() map[string]bool{
		"-encoders": sync.OnceValue(func() map[string]bool { return readCodecListing("-encoders") }),
		"-decoders": sync.OnceValue(func() map[string]bool { return readCodecListing("-decoders") }),
	}
	return func(listing string) map[string]bool {
		if f, ok := memo[listing]; ok {
			return f()
		}
		return readCodecListing(listing)
	}
}()

func readCodecListing(listing string) map[string]bool {
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil
	}
	out, err := exec.Command(path, "-hide_banner", listing).Output()
	if err != nil {
		return nil
	}
	set := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		// Each entry is " FLAGS name description"; take the name field
		// exactly so "libvorbis" cannot be found inside a description.
		if f := strings.Fields(line); len(f) >= 2 {
			set[f[1]] = true
		}
	}
	return set
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
