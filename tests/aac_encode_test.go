package waxflow_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	waxflow "github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

// transcodeAAC transcodes a source to AAC via the engine.
func transcodeAAC(t *testing.T, src []byte, dst interface {
	Write([]byte) (int, error)
}, opts waxflow.TranscodeOptions) waxflow.TranscodeResult {
	t.Helper()
	opts.Format = "aac"
	e := waxflow.New()
	res, err := e.Transcode(context.Background(), container.BytesSource(src), "", dst, opts)
	if err != nil {
		t.Fatalf("Transcode to aac: %v", err)
	}
	return *res
}

// TestAACTranscodeFMP4 drives the engine's aac row end to end into the
// default fMP4 container: the trailer must be exact, the init header's
// edit list must carry the delay and the exact length (the engine knows
// the WAV's), and ffmpeg must probe and decode it.
func TestAACTranscodeFMP4(t *testing.T) {
	const frames = 44100 + 777
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	wav := synthWAV(t, f, frames)

	var out bytes.Buffer
	res := transcodeAAC(t, wav, &out, waxflow.TranscodeOptions{})
	if res.Samples != frames {
		t.Fatalf("samples = %d, want %d", res.Samples, frames)
	}

	// Gapless signaling: elst duration is the exact length, media_time
	// the encoder delay (the engine projected the length up front, so
	// even the pure-stream form carries full trims).
	i := bytes.Index(out.Bytes(), []byte("elst"))
	if i < 0 {
		t.Fatal("no elst in fMP4 output")
	}
	dur := int64(binary.BigEndian.Uint64(out.Bytes()[i+12:]))
	mt := int64(binary.BigEndian.Uint64(out.Bytes()[i+20:]))
	if dur != frames || mt != aac.EncoderDelay {
		t.Fatalf("elst (dur %d, mediaTime %d), want (%d, %d)", dur, mt, frames, aac.EncoderDelay)
	}

	testutil.FFmpeg(t)
	path := filepath.Join(t.TempDir(), "out.m4a")
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	ref := testutil.FFprobeFile(t, path)
	if ref.CodecName != "aac" || ref.SampleRate != 44100 || ref.Channels != 2 {
		t.Errorf("ffprobe on our output = %+v", ref)
	}
	got := testutil.FFmpegDecodeF32(t, path)
	if len(got) < frames*2 {
		t.Fatalf("ffmpeg decoded %d interleaved samples, want at least %d", len(got), frames*2)
	}
}

// TestAACTranscodeADTS covers the container=adts legacy opt-out: the
// stream must demux and decode through our own read pipeline, with the
// documented capability gap (no gapless trims: the decode covers delay
// and padding too).
func TestAACTranscodeADTS(t *testing.T) {
	const frames = 22050
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	wav := synthWAV(t, f, frames)

	var out bytes.Buffer
	res := transcodeAAC(t, wav, &out, waxflow.TranscodeOptions{Container: "adts"})
	if res.Samples != frames {
		t.Fatalf("samples = %d, want %d", res.Samples, frames)
	}
	// The result reports the resolved container (the override, not the
	// format's default), the same resolution PlanTranscode gives.
	if res.Container != "adts" {
		t.Fatalf("result container = %q, want adts", res.Container)
	}

	med, err := format.Open(container.BytesSource(out.Bytes()), "aac", nil)
	if err != nil {
		t.Fatalf("open our adts: %v", err)
	}
	defer med.Close()
	buf := audio.Get(med.Info().Default().Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	var n int64
	for {
		if err := med.ReadChunk(buf); err != nil {
			break
		}
		n += int64(buf.N)
	}
	// The "none" gapless cell: ADTS has no trim signaling, so the raw
	// decode covers priming + samples + padding: one priming frame plus
	// the padded source blocks.
	want := int64(1+(frames+1023)/1024) * 1024
	if n != want {
		t.Fatalf("decoded %d samples, want %d (untrimmed whole frames)", n, want)
	}
}

// TestAACContainerRejected pins the container= validation: only aac has
// an alternate, and only adts names one.
func TestAACContainerRejected(t *testing.T) {
	const frames = 4096
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	wav := synthWAV(t, f, frames)
	e := waxflow.New()
	var out bytes.Buffer
	if _, err := e.Transcode(context.Background(), container.BytesSource(wav), "", &out,
		waxflow.TranscodeOptions{Format: "wav", Container: "adts"}); err == nil {
		t.Error("container=adts on wav accepted")
	}
	if _, err := e.Transcode(context.Background(), container.BytesSource(wav), "", &out,
		waxflow.TranscodeOptions{Format: "aac", Container: "mkv"}); err == nil {
		t.Error("container=mkv on aac accepted")
	}
	if _, err := e.PlanTranscode(container.Track{Fmt: f}, waxflow.TranscodeOptions{Format: "flac", Container: "adts"}); err == nil {
		t.Error("plan with container=adts on flac accepted")
	}
}

// TestAACBitrateScales checks the bitrate option reaches the encoder:
// double the rate, distinctly more bytes.
func TestAACBitrateScales(t *testing.T) {
	const frames = 44100 * 2
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	wav := synthWAV(t, f, frames)
	var lo, hi bytes.Buffer
	transcodeAAC(t, wav, &lo, waxflow.TranscodeOptions{AACBitrate: 64000, Container: "adts"})
	transcodeAAC(t, wav, &hi, waxflow.TranscodeOptions{AACBitrate: 192000, Container: "adts"})
	if hi.Len() < lo.Len()*2 {
		t.Fatalf("192k output (%d bytes) not clearly larger than 64k (%d bytes)", hi.Len(), lo.Len())
	}
}
