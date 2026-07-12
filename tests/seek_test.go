package waxflow_test

// Media-level sample-exact seek tests on the committed fixtures (no
// oracle or vectors needed: our own linear decode is the reference,
// bit-exactness being established separately) plus an ffmpeg-generated
// long stream that exercises real Ogg granule bisection.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

func TestFixtureSeekSampleExact(t *testing.T) {
	for _, name := range []string{
		"sine-s16.flac", "sine-s24.flac", "sine-mono-s16.flac",
		"sine-5_1-s16.flac", "noise-s16.flac",
		"sine-s16.oga", "noise-s24.oga",
	} {
		t.Run(name, func(t *testing.T) {
			src := fixtureSource(t, name)
			ref, err := decodeAllDynamic(t, src, "")
			if err != nil {
				t.Fatal(err)
			}
			defer audio.Put(ref)
			med, err := waxflow.New().OpenStream(src, "")
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			seekMatchesReference(t, med, ref, 50, 3)
		})
	}
}

// TestZeroFrameFLACEndToEnd runs a metadata-only FLAC (what encoding
// zero input produces; ffprobe accepts these) through the whole engine:
// probe reports an empty track, reads return EOF, and a transcode
// produces a valid zero-sample WAV.
func TestZeroFrameFLACEndToEnd(t *testing.T) {
	ffmpeg := testutil.FFmpeg(t)
	path := filepath.Join(t.TempDir(), "empty.flac")
	out, err := exec.Command(ffmpeg, "-v", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=1",
		"-t", "0", "-ac", "2", "-c:a", "flac", path).CombinedOutput()
	if err != nil {
		t.Fatalf("generating empty flac: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := container.BytesSource(raw)

	info, err := waxflow.New().Probe(src, "flac", nil)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if d := info.Default(); d.Samples != 0 {
		t.Errorf("samples = %d, want 0", d.Samples)
	}
	if len(info.Warnings) != 0 {
		t.Errorf("warnings on a valid empty stream: %v", info.Warnings)
	}

	got, err := decodeAllDynamic(t, src, "flac")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	n := got.N
	audio.Put(got)
	if n != 0 {
		t.Errorf("decoded %d samples from an empty stream", n)
	}

	wavPath := filepath.Join(t.TempDir(), "empty.wav")
	outFile, err := os.Create(wavPath)
	if err != nil {
		t.Fatal(err)
	}
	res, err := waxflow.New().Transcode(t.Context(), src, "flac", outFile, waxflow.TranscodeOptions{Format: "wav"})
	if err != nil {
		t.Fatalf("transcode: %v", err)
	}
	if err := outFile.Close(); err != nil {
		t.Fatal(err)
	}
	if res.Samples != 0 {
		t.Errorf("transcode wrote %d samples", res.Samples)
	}
	// ffprobe omits duration_ts entirely on an empty stream (-1 here).
	ref := testutil.FFprobeFile(t, wavPath)
	if ref.SampleRate != 44100 || ref.Channels != 2 || ref.Samples > 0 {
		t.Errorf("ffprobe on our empty WAV = %+v", ref)
	}
}

// TestOggFLACSeekBisection generates a stream long enough that granule
// bisection actually narrows (the committed fixtures fit inside one
// bisection window), then verifies sample exactness.
func TestOggFLACSeekBisection(t *testing.T) {
	ffmpeg := testutil.FFmpeg(t)
	path := filepath.Join(t.TempDir(), "long.oga")
	out, err := exec.Command(ffmpeg, "-v", "error", "-y",
		"-f", "lavfi", "-i", "anoisesrc=color=pink:sample_rate=44100:duration=20:seed=11",
		"-ac", "2", "-c:a", "flac", "-sample_fmt", "s16", "-f", "ogg", path).CombinedOutput()
	if err != nil {
		t.Fatalf("generating long ogg: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := container.BytesSource(raw)
	ref, err := decodeAllDynamic(t, src, "oga")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(ref)
	med, err := waxflow.New().OpenStream(src, "oga")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	seekMatchesReference(t, med, ref, 60, 7)
}
