package waxflow_test

// Ogg-FLAC coverage: the phase-2 mux-mapping refactor exposes FLAC in Ogg
// through the flac row's container override. A lossless source must survive
// WAV -> FLAC-in-Ogg -> decode bit for bit, and ffmpeg must accept the output
// as a real .oga a third-party tool reads.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestTranscodeOggFLACRoundTrip pins the lossless Ogg-FLAC path through the
// engine: integer WAV to FLAC-in-Ogg and back, bit for bit, across channel
// counts.
func TestTranscodeOggFLACRoundTrip(t *testing.T) {
	e := waxflow.New()
	const frames = 9111
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	for _, ch := range []int{1, 2} {
		t.Run([]string{"", "mono", "stereo"}[ch], func(t *testing.T) {
			wav, src := makeWAV(t, cfg, ch, frames, 37)
			defer audio.Put(src)
			out := &memWS{}
			res, err := e.Transcode(context.Background(), container.BytesSource(wav), "", out,
				waxflow.TranscodeOptions{Format: "flac", Container: "ogg"})
			if err != nil {
				t.Fatalf("transcode flac/ogg: %v", err)
			}
			if res.Container != "ogg" {
				t.Errorf("Container = %q, want ogg", res.Container)
			}
			if res.Samples != frames {
				t.Fatalf("Samples = %d, want %d", res.Samples, frames)
			}
			got := readAll(t, e, out.b, frames)
			defer audio.Put(got)
			equalPCM(t, src, got)
		})
	}
}

// TestOggFLACDifferential proves FLAC-in-Ogg output is a real file ffmpeg
// decodes bit for bit.
func TestOggFLACDifferential(t *testing.T) {
	testutil.FFmpeg(t)
	e := waxflow.New()
	const frames = 4801
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	src := testutil.Noise(cfg.PCMFormat(48000, 2, audio.DefaultLayout(2)), frames, 79)
	defer audio.Put(src)
	wav := buildWAVFrom(t, cfg, src)

	path := filepath.Join(t.TempDir(), "out.oga")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Transcode(context.Background(), container.BytesSource(wav), "", out,
		waxflow.TranscodeOptions{Format: "flac", Container: "ogg"}); err != nil {
		out.Close()
		t.Fatalf("transcode: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}

	ref := testutil.FFprobeFile(t, path)
	if ref.CodecName != "flac" || ref.SampleRate != 48000 || ref.Channels != 2 {
		t.Errorf("ffprobe = %+v", ref)
	}
	want := testutil.FFmpegDecodeS32(t, path)
	if idx := testutil.DiffI32(testutil.Interleave(src), want); idx != -1 {
		t.Errorf("ffmpeg decode of our FLAC-in-Ogg differs at %d", idx)
	}
}
