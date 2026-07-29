package waxflow_test

// ffmpeg differential for the Matroska/WebM muxer: our output must be a real
// .mka/.webm file a third-party tool reads. Lossless paths are compared bit
// for bit against ffmpeg's decode; lossy paths are probed for the right codec
// and checked for a gapless-correct sample count (ffmpeg honors the Matroska
// CodecDelay and DiscardPadding we write). Skips when ffmpeg is absent unless
// WAXFLOW_REQUIRE_FFMPEG=1.

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// transcodeToFile transcodes a WAV source to path with the given format and
// container override.
func transcodeToFile(t *testing.T, e *waxflow.Engine, wav []byte, format, cont, path string) *waxflow.TranscodeResult {
	t.Helper()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Transcode(context.Background(), container.BytesSource(wav), "", out,
		waxflow.TranscodeOptions{Format: format, Container: cont})
	if err != nil {
		out.Close()
		t.Fatalf("transcode %s/%s: %v", format, cont, err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return res
}

// TestMKADifferentialLossless proves PCM and FLAC in Matroska decode bit for
// bit through ffmpeg.
func TestMKADifferentialLossless(t *testing.T) {
	testutil.FFmpeg(t)
	e := waxflow.New()
	const frames = 4801
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	for _, format := range []string{"wav", "flac"} {
		t.Run(format, func(t *testing.T) {
			src := testutil.Noise(cfg.PCMFormat(48000, 2, audio.DefaultLayout(2)), frames, 71)
			defer audio.Put(src)
			wav := buildWAVFrom(t, cfg, src)
			path := filepath.Join(t.TempDir(), "out.mka")
			transcodeToFile(t, e, wav, format, "mka", path)

			ref := testutil.FFprobeFile(t, path)
			if ref.SampleRate != 48000 || ref.Channels != 2 {
				t.Errorf("ffprobe = %+v", ref)
			}
			want := testutil.FFmpegDecodeS32(t, path)
			if idx := testutil.DiffI32(testutil.Interleave(src), want); idx != -1 {
				t.Errorf("ffmpeg decode of our %s-in-mka differs at %d", format, idx)
			}

			// This returned N/A before the muxer wrote a Duration. A lossless
			// track has no CodecDelay, so the Duration is exact; anything
			// looser than a fraction of a millisecond would test nothing.
			dur := testutil.FFprobeFormatDuration(t, path)
			if dur < 0 {
				t.Fatal("ffprobe reports no container duration")
			}
			if want := float64(frames) / 48000; math.Abs(dur-want) > 1e-4 {
				t.Errorf("ffprobe duration = %.6f s, want %.6f", dur, want)
			}
		})
	}
}

// TestMKADifferentialLossy proves Opus-in-WebM and AAC-in-Matroska are real
// files ffmpeg identifies and decodes to the gapless sample count.
func TestMKADifferentialLossy(t *testing.T) {
	testutil.FFmpeg(t)
	e := waxflow.New()
	const frames = 9600
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	cases := []struct {
		format, cont, ext, codec string
	}{
		{"opus", "webm", "webm", "opus"},
		{"aac", "mka", "mka", "aac"},
	}
	for _, tc := range cases {
		t.Run(tc.format+"/"+tc.cont, func(t *testing.T) {
			src := testutil.Noise(cfg.PCMFormat(48000, 2, audio.DefaultLayout(2)), frames, 73)
			defer audio.Put(src)
			wav := buildWAVFrom(t, cfg, src)
			path := filepath.Join(t.TempDir(), "out."+tc.ext)
			transcodeToFile(t, e, wav, tc.format, tc.cont, path)

			ref := testutil.FFprobeFile(t, path)
			if ref.CodecName != tc.codec {
				t.Errorf("ffprobe codec = %q, want %q", ref.CodecName, tc.codec)
			}
			if ref.SampleRate != 48000 || ref.Channels != 2 {
				t.Errorf("ffprobe = %+v", ref)
			}
			// ffmpeg applies the Matroska gapless trims; the decoded length
			// should land within a frame or so of the source.
			got := len(testutil.FFmpegDecodeF32(t, path)) / 2 // stereo
			if d := got - frames; d < -2048 || d > 2048 {
				t.Errorf("ffmpeg decoded %d frames, want ~%d", got, frames)
			}

			// The reference is the source length, not ffmpeg's own Duration:
			// its Matroska muxer writes the raw stream length, overshooting by
			// the pre-skip plus tail trim, about 8 ms on these fixtures. 2 ms
			// is tight because writing the raw length is the tempting mistake
			// and a looser gate would pass it.
			dur := testutil.FFprobeFormatDuration(t, path)
			if dur < 0 {
				t.Fatal("ffprobe reports no container duration")
			}
			if want := float64(frames) / 48000; math.Abs(dur-want) > 2e-3 {
				t.Errorf("ffprobe duration = %.6f s, want %.6f (the presentation length, not the raw one)", dur, want)
			}
		})
	}
}
