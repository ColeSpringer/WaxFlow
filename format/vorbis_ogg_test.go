package format

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// genOgg writes a longer distinct-per-channel Ogg file for seek tests.
func genOgg(t *testing.T, ff, path string, rate, channels int, codecName string, extra ...string) {
	t.Helper()
	src := fmt.Sprintf("sine=frequency=330:sample_rate=%d:duration=2.0", rate)
	args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", src,
		"-ac", fmt.Sprint(channels), "-c:a", codecName}
	args = append(args, extra...)
	args = append(args, path)
	if out, err := exec.Command(ff, args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg gen: %v\n%s", err, out)
	}
}

// decodeOgg opens an Ogg file through the engine and returns interleaved
// float32 samples plus the track.
func decodeOggVorbis(t *testing.T, path string) ([]float32, container.Track) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	med, err := Open(container.BytesSource(raw), "ogg", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	tr := med.Info().Tracks[0]
	dst := audio.Get(tr.Fmt, audio.StandardChunk)
	defer audio.Put(dst)
	var out []float32
	for {
		err := med.ReadChunk(dst)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadChunk: %v", err)
		}
		for i := 0; i < dst.N; i++ {
			for c := 0; c < dst.Fmt.Channels; c++ {
				out = append(out, dst.ChanF(c)[i])
			}
		}
	}
	return out, tr
}

// alignedRMS finds the offset (0..maxOff frames) minimizing scale-invariant RMS
// between one interleaved signal and a reference.
func alignedRMS(mine, ref []float32, ch, maxOff int) (int, float64) {
	refFrames := len(ref) / ch
	if refFrames == 0 || len(mine) < len(ref) {
		return 0, math.Inf(1)
	}
	best, bestOff := math.Inf(1), 0
	for o := 0; o <= maxOff && (o+refFrames)*ch <= len(mine); o++ {
		var dot, en float64
		for i := 0; i < refFrames*ch; i++ {
			m, r := float64(mine[o*ch+i]), float64(ref[i])
			dot += m * r
			en += m * m
		}
		s := 1.0
		if en > 0 {
			s = dot / en
		}
		var sum float64
		for i := 0; i < refFrames*ch; i++ {
			d := s*float64(mine[o*ch+i]) - float64(ref[i])
			sum += d * d
		}
		if r := math.Sqrt(sum / float64(refFrames*ch)); r < best {
			best, bestOff = r, o
		}
	}
	return bestOff, best
}

func TestOggVorbisEngineDifferential(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name     string
		channels int
		rate     int
	}{
		{"mono44", 1, 44100},
		{"stereo44", 2, 44100},
		{"stereo48", 2, 48000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".ogg")
			testutil.FFmpegGenerate(t, path, tc.rate, tc.channels, "libvorbis", "-q:a", "5")

			mine, tr := decodeOggVorbis(t, path)
			if tr.Codec != codec.Vorbis {
				t.Fatalf("codec = %v, want vorbis", tr.Codec)
			}
			if tr.Fmt.Rate != tc.rate || tr.Fmt.Channels != tc.channels {
				t.Fatalf("track fmt = %v", tr.Fmt)
			}
			ref := testutil.FFmpegDecodeF32(t, path)
			// Both sides are gapless-trimmed now, so lengths should be close.
			mineFrames, refFrames := len(mine)/tc.channels, len(ref)/tc.channels
			if d := mineFrames - refFrames; d < -4 || d > 4 {
				t.Errorf("frame count: engine %d, ffmpeg %d (diff %d)", mineFrames, refFrames, d)
			}
			off, rms := alignedRMS(mine, ref, tc.channels, 8)
			t.Logf("off=%d rms=%.6g mineFrames=%d refFrames=%d", off, rms, mineFrames, refFrames)
			if rms > 1e-3 {
				t.Errorf("engine-vs-ffmpeg RMS %.6g exceeds 1e-3", rms)
			}
		})
	}
}

func TestOggVorbisSeek(t *testing.T) {
	ff := testutil.FFmpeg(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "seek.ogg")
	genOgg(t, ff, path, 44100, 2, "libvorbis", "-q:a", "5")
	ref := testutil.FFmpegDecodeF32(t, path)
	ch := 2
	refFrames := len(ref) / ch

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []int64{0, 5000, 20000, 44100, int64(refFrames) - 5000} {
		med, err := Open(container.BytesSource(raw), "ogg", nil)
		if err != nil {
			t.Fatal(err)
		}
		landed, err := med.SeekSample(target)
		if err != nil {
			med.Close()
			t.Fatalf("seek %d: %v", target, err)
		}
		if landed > target {
			med.Close()
			t.Fatalf("seek %d landed after target at %d", target, landed)
		}
		// Decode one chunk from the landing and compare to ffmpeg at landed.
		dst := audio.Get(med.Info().Tracks[0].Fmt, audio.StandardChunk)
		if err := med.ReadChunk(dst); err != nil {
			audio.Put(dst)
			med.Close()
			t.Fatalf("read after seek %d: %v", target, err)
		}
		var maxAbs float64
		n := dst.N
		for i := 0; i < n; i++ {
			ri := (int(landed) + i) * ch
			if ri+ch > len(ref) {
				break
			}
			for c := 0; c < ch; c++ {
				d := math.Abs(float64(dst.ChanF(c)[i]) - float64(ref[ri+c]))
				if d > maxAbs {
					maxAbs = d
				}
			}
		}
		t.Logf("target=%d landed=%d maxAbs=%.2e", target, landed, maxAbs)
		if maxAbs > 1e-3 {
			t.Errorf("seek target=%d landed=%d: post-pre-roll maxAbs %.2e exceeds 1e-3", target, landed, maxAbs)
		}
		audio.Put(dst)
		med.Close()
	}
}
