package waxflow_test

// ALAC decode differentials: ALAC is lossless, so our decode must be
// bit-for-bit identical to ffmpeg's. Fixtures are generated with ffmpeg at
// test time (the test self-skips without it), covering mono/stereo, a few
// rates and bit depths, and both simple and high-entropy content so the
// adaptive predictor, the middle-side matrix, and the escape path all run.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// genALAC writes an ALAC .m4a fixture via ffmpeg and returns its path.
func genALAC(t *testing.T, dir, name, source string, channels int, extra ...string) string {
	t.Helper()
	ffmpeg := testutil.FFmpeg(t)
	path := filepath.Join(dir, name)
	args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", source,
		"-ac", strconv.Itoa(channels), "-c:a", "alac"}
	args = append(args, extra...)
	args = append(args, path)
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg alac %s: %v\n%s", name, err, out)
	}
	return path
}

var alacCases = []struct {
	name     string
	source   string
	channels int
	extra    []string
}{
	{"stereo-sine-44k.m4a", "sine=frequency=440:sample_rate=44100:duration=0.5", 2, nil},
	{"mono-sine-44k.m4a", "sine=frequency=523:sample_rate=44100:duration=0.5", 1, nil},
	{"stereo-noise-44k.m4a", "anoisesrc=color=pink:sample_rate=44100:duration=0.5:seed=3", 2, nil},
	{"stereo-sine-48k.m4a", "sine=frequency=440:sample_rate=48000:duration=0.5", 2, nil},
	{"stereo-noise-48k.m4a", "anoisesrc=color=white:sample_rate=48000:duration=0.5:seed=9", 2, nil},
	{"stereo-sine-24bit.m4a", "sine=frequency=330:sample_rate=48000:duration=0.5", 2, []string{"-sample_fmt", "s32p"}},
}

func TestALACDecodeBitExact(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range alacCases {
		t.Run(tc.name, func(t *testing.T) {
			path := genALAC(t, dir, tc.name, tc.source, tc.channels, tc.extra...)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := decodeAllDynamic(t, container.BytesSource(raw), "m4a")
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			defer audio.Put(got)

			ref := testutil.FFmpegDecodeS32(t, path)
			if idx := testutil.DiffI32(testutil.Interleave(got), ref); idx != -1 {
				t.Fatalf("ALAC decode differs from ffmpeg at interleaved sample %d (got %d samples, ref %d)",
					idx, got.N*got.Fmt.Channels, len(ref))
			}
		})
	}
}

// TestALACProbe checks that probe agrees with ffprobe on the stream shape.
func TestALACProbe(t *testing.T) {
	dir := t.TempDir()
	path := genALAC(t, dir, "probe.m4a", "sine=frequency=440:sample_rate=44100:duration=0.5", 2)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := waxflow.New().Probe(container.BytesSource(raw), "m4a", nil)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	tr := info.Default()
	if info.Container != "mp4" {
		t.Errorf("container = %q, want mp4", info.Container)
	}
	if tr.Codec != "alac" {
		t.Errorf("codec = %q, want alac", tr.Codec)
	}
	ff := testutil.FFprobeFile(t, path)
	if tr.Fmt.Rate != ff.SampleRate {
		t.Errorf("rate = %d, want %d", tr.Fmt.Rate, ff.SampleRate)
	}
	if tr.Fmt.Channels != ff.Channels {
		t.Errorf("channels = %d, want %d", tr.Fmt.Channels, ff.Channels)
	}
}

// TestALACSeekSampleExact seeks an ALAC stream at pseudo-random offsets and
// checks each landing is sample-exact and bit-identical to the linear
// reference.
func TestALACSeekSampleExact(t *testing.T) {
	dir := t.TempDir()
	path := genALAC(t, dir, "seek.m4a", "anoisesrc=color=pink:sample_rate=44100:duration=2:seed=5", 2)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := container.BytesSource(raw)
	ref, err := decodeAllDynamic(t, src, "m4a")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(ref)
	med, err := waxflow.New().OpenStream(src, "m4a")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	seekMatchesReference(t, med, ref, 50, 5)
}
