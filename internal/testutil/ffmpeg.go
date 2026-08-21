// Package testutil is the shared test harness: the ffmpeg/ffprobe
// differential oracle, PCM comparison helpers, deterministic signal
// synthesis, and the SHA-256-pinned conformance-vector fetcher.
//
// ffmpeg is a TEST ORACLE only, never a runtime dependency.
// Oracle-based tests self-skip when ffmpeg is not installed; setting
// WAXFLOW_REQUIRE_FFMPEG=1 (the dedicated CI differential job) escalates
// absence to a hard failure so the suite cannot silently thin out.
package testutil

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// tool resolves an oracle binary, applying the skip-or-require policy.
func tool(t testing.TB, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		if os.Getenv("WAXFLOW_REQUIRE_FFMPEG") == "1" {
			t.Fatalf("%s required by WAXFLOW_REQUIRE_FFMPEG=1 but not installed", name)
		}
		t.Skipf("%s not installed; skipping differential test", name)
	}
	return path
}

// EncoderQualityGate self-skips an encoder-quality gate unless
// WAXFLOW_ENCODER_QUALITY=1 is set. These gates re-encode a corpus with our
// lossy encoders and a reference baseline and score both: minutes of work
// whose home is the dedicated `make encoder-quality` target and the nightly
// job, not the default `go test` loop. `make encoder-quality` sets the
// variable; without it the gates skip so a plain run stays fast.
func EncoderQualityGate(t testing.TB) {
	t.Helper()
	if os.Getenv("WAXFLOW_ENCODER_QUALITY") != "1" {
		t.Skip("encoder-quality gate skipped; run `make encoder-quality` (or set WAXFLOW_ENCODER_QUALITY=1)")
	}
}

// FFmpeg returns the ffmpeg path, skipping or failing per the policy.
func FFmpeg(t testing.TB) string { return tool(t, "ffmpeg") }

// HaveFFmpeg reports whether ffmpeg is installed, for a test that still has work
// to do without it and so must not take FFmpeg's blanket skip (a gate whose own
// in-process leg is the part that always runs, with the oracle decoders added
// when they are there). WAXFLOW_REQUIRE_FFMPEG=1 still escalates absence to a
// failure, so the dedicated differential job cannot silently thin out.
func HaveFFmpeg(t testing.TB) bool {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		if os.Getenv("WAXFLOW_REQUIRE_FFMPEG") == "1" {
			t.Fatal("ffmpeg required by WAXFLOW_REQUIRE_FFMPEG=1 but not installed")
		}
		return false
	}
	return true
}

// FFprobe returns the ffprobe path, skipping or failing per the policy.
func FFprobe(t testing.TB) string { return tool(t, "ffprobe") }

// run executes an oracle command, failing the test on error.
func run(t testing.TB, name string, args ...string) []byte {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, errOut.String())
	}
	return out.Bytes()
}

// FFmpegDecodeS32 decodes a file with ffmpeg to raw interleaved
// little-endian int32 samples. ffmpeg left-justifies narrower sources
// (16-bit becomes value<<16), so comparisons shift accordingly.
func FFmpegDecodeS32(t testing.TB, path string) []int32 {
	t.Helper()
	raw := run(t, FFmpeg(t), "-v", "error", "-i", path, "-f", "s32le", "-c:a", "pcm_s32le", "-")
	out := make([]int32, len(raw)/4)
	for i := range out {
		out[i] = int32(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return out
}

// FFmpegDecodeF32 decodes a file with ffmpeg (its default decoder) to raw
// interleaved little-endian float32 samples.
func FFmpegDecodeF32(t testing.TB, path string) []float32 {
	return ffmpegDecodeF32(t, path, "")
}

// FFmpegDecodeF32Codec decodes with a specific ffmpeg decoder (e.g. "libvorbis").
// ffmpeg's default Vorbis decoder is its own native one, which is flagged
// experimental (trac.ffmpeg.org ticket 10571) and mis-decodes some legal
// coupled streams: its vectorized inverse coupling negates the angle channel on
// any line stored as a zero magnitude with a nonzero angle, where its own C
// fallback (reachable with -cpuflags 0) and the spec do not. Selecting libvorbis
// pins the reference decoder so a stream is tested against libvorbis itself, not
// ffmpeg's experimental reimplementation. Scoring OUR streams, prefer this over
// FFmpegDecodeF32 for exactly that reason; a gate that wants to prove what a
// libavcodec-based player hears should call FFmpegDecodeF32 deliberately, as
// TestVorbisCoupledStereo does.
func FFmpegDecodeF32Codec(t testing.TB, path, decoder string) []float32 {
	return ffmpegDecodeF32(t, path, decoder)
}

// FFmpegDecodeF32NoSIMD decodes with ffmpeg's default decoder and -cpuflags 0,
// which disables runtime SIMD dispatch and reaches libavcodec's plain C
// implementations. Paired with FFmpegDecodeF32 it separates a defect in a
// vectorized kernel from one in the decoder proper, which is how the Vorbis
// coupled-stereo defect behind F1 was pinned to ffmpeg's vectorized inverse
// coupling.
func FFmpegDecodeF32NoSIMD(t testing.TB, path string) []float32 {
	return ffmpegDecodeF32(t, path, "", "-cpuflags", "0")
}

func ffmpegDecodeF32(t testing.TB, path, decoder string, pre ...string) []float32 {
	t.Helper()
	args := append([]string{"-v", "error"}, pre...)
	if decoder != "" {
		args = append(args, "-c:a", decoder)
	}
	args = append(args, "-i", path, "-f", "f32le", "-c:a", "pcm_f32le", "-")
	raw := run(t, FFmpeg(t), args...)
	out := make([]float32, len(raw)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return out
}

// FFprobeInfo is the subset of ffprobe stream fields the differential
// tests compare against our probe.
type FFprobeInfo struct {
	CodecName        string
	SampleRate       int
	Channels         int
	BitsPerSample    int
	BitsPerRawSample int
	// Samples is duration_ts, which equals the frame count for PCM
	// containers (their stream timebase is 1/rate). -1 when absent.
	Samples int64
}

// FFprobeFile probes the first audio stream with ffprobe.
func FFprobeFile(t testing.TB, path string) FFprobeInfo {
	t.Helper()
	raw := run(t, FFprobe(t), "-v", "error", "-select_streams", "a:0",
		"-show_streams", "-of", "json", path)
	var doc struct {
		Streams []struct {
			CodecName        string `json:"codec_name"`
			SampleRate       string `json:"sample_rate"`
			Channels         int    `json:"channels"`
			BitsPerSample    int    `json:"bits_per_sample"`
			BitsPerRawSample string `json:"bits_per_raw_sample"`
			DurationTS       *int64 `json:"duration_ts"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing ffprobe output: %v\n%s", err, raw)
	}
	if len(doc.Streams) == 0 {
		t.Fatalf("ffprobe found no audio stream in %s", path)
	}
	s := doc.Streams[0]
	info := FFprobeInfo{
		CodecName:     s.CodecName,
		Channels:      s.Channels,
		BitsPerSample: s.BitsPerSample,
		Samples:       -1,
	}
	if s.SampleRate != "" {
		rate, err := strconv.Atoi(s.SampleRate)
		if err != nil {
			t.Fatalf("ffprobe sample_rate %q: %v", s.SampleRate, err)
		}
		info.SampleRate = rate
	}
	if s.BitsPerRawSample != "" {
		bits, err := strconv.Atoi(s.BitsPerRawSample)
		if err != nil {
			t.Fatalf("ffprobe bits_per_raw_sample %q: %v", s.BitsPerRawSample, err)
		}
		info.BitsPerRawSample = bits
	}
	if s.DurationTS != nil {
		info.Samples = *s.DurationTS
	}
	return info
}

// FFprobeFormatDuration is the container-level duration ffprobe reports in
// seconds, or -1 when the container declares none.
//
// It is separate from FFprobeFile rather than another field on FFprobeInfo:
// that type is scoped to stream fields and shared by several differential
// tests, where a format-level duration would be meaningless. This is the
// number `ffprobe -show_entries format=duration` prints, which for Matroska is
// the Info > Duration element read verbatim, with no CodecDelay or
// DiscardPadding adjustment applied.
func FFprobeFormatDuration(t testing.TB, path string) float64 {
	t.Helper()
	raw := run(t, FFprobe(t), "-v", "error", "-show_format", "-of", "json", path)
	var doc struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing ffprobe output: %v\n%s", err, raw)
	}
	if doc.Format.Duration == "" || doc.Format.Duration == "N/A" {
		return -1
	}
	d, err := strconv.ParseFloat(doc.Format.Duration, 64)
	if err != nil {
		t.Fatalf("ffprobe format duration %q: %v", doc.Format.Duration, err)
	}
	return d
}

// FFmpegGenerate synthesizes a short fixture file with ffmpeg (sine source) at
// the given rate, channel count, and output codec, for decode
// differentials against an independent implementation.
func FFmpegGenerate(t testing.TB, path string, rate, channels int, acodec string, extra ...string) {
	t.Helper()
	FFmpegGenerateDuration(t, path, 0.25, rate, channels, acodec, extra...)
}

// FFmpegGenerateDuration is FFmpegGenerate with the source length spelled out,
// for a fixture that needs to be long enough to have interesting structure
// (several clusters, a seek index with entries to skip).
func FFmpegGenerateDuration(t testing.TB, path string, seconds float64, rate, channels int, acodec string, extra ...string) {
	t.Helper()
	args := []string{"-v", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=%d:duration=%g", rate, seconds),
		"-ac", strconv.Itoa(channels), "-c:a", acodec,
	}
	args = append(args, extra...)
	args = append(args, path)
	run(t, FFmpeg(t), args...)
}

// FFprobePacket is one demuxed packet as ffprobe reports it: the container's
// own framing, in the stream's time base, before any decoding.
type FFprobePacket struct {
	// PTS and Dur are in the stream time base (milliseconds for ASF).
	PTS  int64
	Dur  int64
	Size int
}

// FFprobePackets lists the first audio stream's packets. It is the oracle for
// a demuxer whose codec has no decoder yet: packet count, sizes, and
// presentation times are the whole of what a demuxer produces, and ffprobe
// reports all three without decoding a sample.
func FFprobePackets(t testing.TB, path string) []FFprobePacket {
	t.Helper()
	raw := run(t, FFprobe(t), "-v", "error", "-select_streams", "a:0",
		"-show_packets", "-show_entries", "packet=pts,duration,size", "-of", "json", path)
	var doc struct {
		Packets []struct {
			PTS  *int64 `json:"pts"`
			Dur  *int64 `json:"duration"`
			Size string `json:"size"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing ffprobe output: %v\n%s", err, raw)
	}
	out := make([]FFprobePacket, len(doc.Packets))
	for i, p := range doc.Packets {
		out[i].Size, _ = strconv.Atoi(p.Size)
		if p.PTS != nil {
			out[i].PTS = *p.PTS
		}
		if p.Dur != nil {
			out[i].Dur = *p.Dur
		}
	}
	return out
}
