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

// FFmpegDecodeF32 decodes a file with ffmpeg to raw interleaved
// little-endian float32 samples.
func FFmpegDecodeF32(t testing.TB, path string) []float32 {
	t.Helper()
	raw := run(t, FFmpeg(t), "-v", "error", "-i", path, "-f", "f32le", "-c:a", "pcm_f32le", "-")
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

// FFmpegGenerate synthesizes a fixture file with ffmpeg (sine source) at
// the given rate, channel count, and output codec, for decode
// differentials against an independent implementation.
func FFmpegGenerate(t testing.TB, path string, rate, channels int, acodec string, extra ...string) {
	t.Helper()
	args := []string{"-v", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=%d:duration=0.25", rate),
		"-ac", strconv.Itoa(channels), "-c:a", acodec,
	}
	args = append(args, extra...)
	args = append(args, path)
	run(t, FFmpeg(t), args...)
}
