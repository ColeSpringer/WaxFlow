package waxflow_test

// WavPack decode differentials: WavPack in this scope is lossless, so our
// decode must be bit-for-bit identical to ffmpeg's. Fixtures come from two
// encoders on purpose. ffmpeg's covers the common shapes but exercises a
// narrow slice of the block features; the reference `wavpack` CLI, when it is
// installed, covers the rest (the compression levels choose different
// decorrelation cascades, and the extra modes reach terms ffmpeg never emits).

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// genWavPack writes a .wv fixture via ffmpeg and returns its path.
func genWavPack(t *testing.T, dir, name, source string, channels int, extra ...string) string {
	t.Helper()
	ffmpeg := testutil.FFmpeg(t)
	path := filepath.Join(dir, name)
	args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", source,
		"-ac", strconv.Itoa(channels), "-c:a", "wavpack"}
	args = append(args, extra...)
	args = append(args, path)
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg wavpack %s: %v\n%s", name, err, out)
	}
	return path
}

var wavpackCases = []struct {
	name     string
	source   string
	channels int
	extra    []string
}{
	{"stereo-sine-44k.wv", "sine=frequency=440:sample_rate=44100:duration=0.5", 2, s16},
	{"mono-sine-44k.wv", "sine=frequency=523:sample_rate=44100:duration=0.5", 1, s16},
	{"stereo-noise-44k.wv", "anoisesrc=color=pink:sample_rate=44100:duration=0.5:seed=3", 2, s16},
	{"stereo-sine-48k.wv", "sine=frequency=440:sample_rate=48000:duration=0.5", 2, s16},
	{"stereo-noise-48k.wv", "anoisesrc=color=white:sample_rate=48000:duration=0.5:seed=9", 2, s16},
	{"stereo-silence-44k.wv", "anullsrc=sample_rate=44100:duration=0.5", 2, s16},
	{"stereo-noise-8bit.wv", "anoisesrc=color=brown:sample_rate=22050:duration=0.5:seed=4", 2, []string{"-sample_fmt", "u8p"}},
	{"stereo-noise-32bit.wv", "anoisesrc=color=white:sample_rate=48000:duration=0.5:seed=11", 2, []string{"-sample_fmt", "s32p"}},
	{"mono-noise-32bit.wv", "anoisesrc=color=pink:sample_rate=96000:duration=0.5:seed=12", 1, []string{"-sample_fmt", "s32p"}},
	{"stereo-sine-22k.wv", "sine=frequency=200:sample_rate=22050:duration=0.5", 2, s16},
	// A mono source spread over two channels: the encoder marks the block
	// false-stereo and codes one channel, which the decoder must duplicate.
	{"false-stereo-44k.wv", "sine=frequency=300:sample_rate=44100:duration=0.5", 2, s16},
}

// s16 pins the encoder to 16-bit samples; several lavfi sources are float, and
// ffmpeg would otherwise pick WavPack float mode, which is out of scope.
var s16 = []string{"-sample_fmt", "s16p"}

func TestWavPackDecodeBitExact(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range wavpackCases {
		t.Run(tc.name, func(t *testing.T) {
			path := genWavPack(t, dir, tc.name, tc.source, tc.channels, tc.extra...)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := decodeAllDynamic(t, container.BytesSource(raw), "wv")
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			defer audio.Put(got)

			ref := testutil.FFmpegDecodeS32(t, path)
			if idx := testutil.DiffI32(testutil.Interleave(got), ref); idx != -1 {
				t.Fatalf("WavPack decode differs from ffmpeg at interleaved sample %d (got %d samples, ref %d)",
					idx, got.N*got.Fmt.Channels, len(ref))
			}
		})
	}
}

// genWAV writes a WAV source for the reference encoder.
func genWAV(t *testing.T, dir, name, source string, channels int, extra ...string) string {
	t.Helper()
	ffmpeg := testutil.FFmpeg(t)
	path := filepath.Join(dir, name)
	args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", source, "-ac", strconv.Itoa(channels)}
	args = append(args, extra...)
	args = append(args, path)
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg wav %s: %v\n%s", name, err, out)
	}
	return path
}

// TestWavPackReferenceEncoderBitExact decodes fixtures from the reference
// encoder across its compression levels, depths, and channel handling. ffmpeg
// emits two decorrelation terms and never a false-stereo block; these cells
// reach the deeper cascades, the cross-channel terms, and the shapes only the
// reference produces.
func TestWavPackReferenceEncoderBitExact(t *testing.T) {
	dir := t.TempDir()
	stereo := genWAV(t, dir, "stereo.wav", "anoisesrc=color=pink:sample_rate=44100:duration=0.5:seed=7", 2, "-sample_fmt", "s16")
	mono := genWAV(t, dir, "mono.wav", "anoisesrc=color=white:sample_rate=44100:duration=0.5:seed=8", 1, "-sample_fmt", "s16")
	dual := genWAV(t, dir, "dual.wav", "sine=frequency=300:sample_rate=44100:duration=0.5", 2, "-sample_fmt", "s16")
	deep := genWAV(t, dir, "deep.wav", "anoisesrc=color=brown:sample_rate=48000:duration=0.5:seed=9", 2, "-sample_fmt", "s32", "-c:a", "pcm_s24le")
	small := genWAV(t, dir, "small.wav", "anoisesrc=color=white:sample_rate=22050:duration=0.5:seed=10", 2, "-c:a", "pcm_u8")
	odd := genWAV(t, dir, "odd.wav", "sine=frequency=700:sample_rate=37000:duration=0.5", 2, "-sample_fmt", "s16")

	cases := []struct {
		name string
		wav  string
		opts []string
	}{
		{"default", stereo, nil},
		{"fast", stereo, []string{"-f"}},
		{"high", stereo, []string{"-h"}},
		{"very-high", stereo, []string{"-hh"}},
		{"extra", stereo, []string{"-hh", "-x4"}},
		{"mono", mono, []string{"-h"}},
		{"false-stereo", dual, []string{"-h"}},
		{"24-bit", deep, []string{"-h"}},
		{"8-bit", small, []string{"-h"}},
		{"custom-rate", odd, []string{"-h"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := testutil.WavPackEncodeFile(t, tc.wav, tc.name+".wv", tc.opts...)
			testutil.WvUnpackVerify(t, path)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := decodeAllDynamic(t, container.BytesSource(raw), "wv")
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			defer audio.Put(got)
			ref := testutil.FFmpegDecodeS32(t, path)
			if idx := testutil.DiffI32(testutil.Interleave(got), ref); idx != -1 {
				t.Fatalf("decode differs from ffmpeg at interleaved sample %d (got %d, ref %d)",
					idx, got.N*got.Fmt.Channels, len(ref))
			}
		})
	}
}

// TestWavPackProbe checks that probe agrees with ffprobe on the stream shape,
// and that the source resolves to the wavpack driver by magic alone.
func TestWavPackProbe(t *testing.T) {
	dir := t.TempDir()
	path := genWavPack(t, dir, "probe.wv", "sine=frequency=440:sample_rate=44100:duration=0.5", 2, s16...)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// No extension hint: the driver has to be found by magic.
	info, err := waxflow.New().Probe(container.BytesSource(raw), "", nil)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if info.Container != "wavpack" {
		t.Errorf("container = %q, want wavpack", info.Container)
	}
	tr := info.Default()
	if tr.Codec != "wavpack" {
		t.Errorf("codec = %q, want wavpack", tr.Codec)
	}
	ff := testutil.FFprobeFile(t, path)
	if tr.Fmt.Rate != ff.SampleRate {
		t.Errorf("rate = %d, want %d", tr.Fmt.Rate, ff.SampleRate)
	}
	if tr.Fmt.Channels != ff.Channels {
		t.Errorf("channels = %d, want %d", tr.Fmt.Channels, ff.Channels)
	}
	if tr.Samples != 22050 {
		t.Errorf("samples = %d, want 22050", tr.Samples)
	}
}

// TestWavPackSeekSampleExact seeks a WavPack stream at pseudo-random offsets
// and checks each landing is sample-exact and bit-identical to the linear
// reference. The fixture is long enough that the demuxer bisects rather than
// walking, which is the path a real library seek takes.
func TestWavPackSeekSampleExact(t *testing.T) {
	dir := t.TempDir()
	path := genWavPack(t, dir, "seek.wv", "anoisesrc=color=pink:sample_rate=44100:duration=6:seed=5", 2, s16...)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Several times the demuxer's bisection cutoff, so the seek bisects
	// instead of walking the whole stream.
	if len(raw) < 512<<10 {
		t.Fatalf("fixture is %d bytes, too small to reach the bisection path", len(raw))
	}
	src := container.BytesSource(raw)
	ref, err := decodeAllDynamic(t, src, "wv")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(ref)
	med, err := waxflow.New().OpenStream(src, "wv")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	seekMatchesReference(t, med, ref, 50, 11)
}

// TestWavPackRefusalsNamed covers the two refusals the pinned suite cannot
// isolate, over losslessly encoded fixtures: more than two channels, and float
// samples. Both are legal WavPack, so the message names the situation.
func TestWavPackRefusalsNamed(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name     string
		channels int
		extra    []string
		want     string
	}{
		{"multichannel.wv", 6, s16, "channels"},
		{"float.wv", 2, []string{"-sample_fmt", "fltp"}, "float"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := genWavPack(t, dir, tc.name,
				"anoisesrc=color=white:sample_rate=48000:duration=0.3:seed=13", tc.channels, tc.extra...)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = decodeAllDynamic(t, container.BytesSource(raw), "wv")
			if err == nil {
				t.Fatal("decoded an out-of-scope stream")
			}
			if code := waxerr.CodeOf(err); code != waxerr.CodeUnsupportedFormat {
				t.Errorf("error code = %v, want unsupported-format", code)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not name %q", err, tc.want)
			}
		})
	}
}

// TestWavPackTranscodeRoundTrip closes the loop through the engine: a WavPack
// source transcoded to FLAC must come back sample-identical, since both are
// lossless and the chain does no conversion. It is what the read-side support
// is for, and it exercises the plan, encode, and mux path a decoder-only test
// never reaches.
func TestWavPackTranscodeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := genWavPack(t, dir, "roundtrip.wv",
		"anoisesrc=color=pink:sample_rate=44100:duration=0.5:seed=17", 2, s16...)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := container.BytesSource(raw)
	ref, err := decodeAllDynamic(t, src, "wv")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(ref)

	out := &memWS{}
	res, err := waxflow.New().Transcode(context.Background(), src, "wv", out,
		waxflow.TranscodeOptions{Format: "flac"})
	if err != nil {
		t.Fatalf("wavpack->flac: %v", err)
	}
	if res.Samples != int64(ref.N) {
		t.Errorf("transcoded %d samples, want %d", res.Samples, ref.N)
	}
	if res.ClippedSamples != 0 {
		t.Errorf("a lossless int chain clipped %d samples", res.ClippedSamples)
	}
	got, err := decodeAllDynamic(t, container.BytesSource(out.b), "flac")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(got)
	if idx := testutil.DiffI32(testutil.Interleave(got), testutil.Interleave(ref)); idx != -1 {
		t.Fatalf("wavpack->flac differs from the source decode at interleaved sample %d", idx)
	}
}
