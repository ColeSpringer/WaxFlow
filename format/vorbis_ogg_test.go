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

// genOggBisect writes a fixture large enough that the Ogg demuxer's seek
// bisection actually subdivides (its cutoff is a 128 KiB window), so seeks land
// mid-stream instead of walking from the first data page. Pink noise is used
// because a sine at this length compresses far below the threshold.
func genOggBisect(t *testing.T, ff, path string, seconds float64) {
	t.Helper()
	src := fmt.Sprintf("anoisesrc=r=44100:d=%g:c=pink:a=0.5:seed=7", seconds)
	args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", src,
		"-ac", "2", "-c:a", "libvorbis", "-q:a", "6", path}
	if out, err := exec.Command(ff, args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg gen: %v\n%s", err, out)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// One subdivision is what puts a landing mid-stream, and bisect's loop
	// (hi-lo > seekWindow) takes it above 128 KiB. The floor is 160 KiB so a
	// tighter-coding libvorbis does not silently drop the coverage; the fixture
	// measures ~213 KiB.
	if st.Size() < 160<<10 {
		t.Fatalf("bisect fixture is %d bytes, too small to subdivide the 128 KiB seek window", st.Size())
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

// TestOggVorbisEngineDifferential decodes libvorbis-written Ogg through the
// engine and checks it against ffmpeg's own decode. The 2.0 s duration is
// load-bearing: at the 0.25 s default ffmpeg's own 128-frame shortfall cancels
// the shift this engine used to subtract, so the test passed on a decoder that
// truncated every real Vorbis file's tail. Do not shorten it for suite speed.
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
			testutil.FFmpegGenerateDuration(t, path, 2.0, tc.rate, tc.channels, "libvorbis", "-q:a", "5")

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

// TestOggVorbisEngineShortStream keeps the 0.25 s case, without ffmpeg's frame
// count as the oracle: there the stream is one audio page and ffmpeg's decode
// ends 128 frames early. Those frames are real audio, not padding, which
// oracletest pins against an independent decoder. Checked here against the
// declared granulepos, and against ffmpeg only for sample values.
func TestOggVorbisEngineShortStream(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "short.ogg")
	testutil.FFmpegGenerateDuration(t, path, 0.25, 44100, 2, "libvorbis", "-q:a", "5")

	mine, tr := decodeOggVorbis(t, path)
	const want = 11025 // 0.25 s at 44100, and libvorbis's final granulepos
	if !tr.SamplesExact || tr.Samples != want {
		t.Errorf("track Samples = %d exact=%v, want %d exact (the final granulepos)", tr.Samples, tr.SamplesExact, want)
	}
	if got := len(mine) / 2; got != want {
		t.Errorf("decoded %d frames, want %d (the declared length must be delivered, not truncated)", got, want)
	}
	ref := testutil.FFmpegDecodeF32(t, path)
	off, rms := alignedRMS(mine, ref, 2, 200)
	t.Logf("off=%d rms=%.6g mineFrames=%d refFrames=%d", off, rms, len(mine)/2, len(ref)/2)
	if rms > 1e-3 {
		t.Errorf("engine-vs-ffmpeg RMS %.6g exceeds 1e-3", rms)
	}
}

// TestOggVorbisSeek is the seek oracle: the audio delivered after a seek must be
// the audio at the position the seek reports.
//
// The "bisect" fixture is load-bearing, not just longer. Below the demuxer's
// 128 KiB window every seek walks from the first data page and lands at 0, where
// the negative-landing clamp absorbs any error in the landing arithmetic. Only
// the mid-stream landings a large fixture produces caught the granule slip.
func TestOggVorbisSeek(t *testing.T) {
	ff := testutil.FFmpeg(t)
	dir := t.TempDir()

	small := filepath.Join(dir, "seek.ogg")
	genOgg(t, ff, small, 44100, 2, "libvorbis", "-q:a", "5")
	big := filepath.Join(dir, "bisect.ogg")
	genOggBisect(t, ff, big, 12.0)

	for _, tc := range []struct{ name, path string }{
		{"walk", small},
		{"bisect", big},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := testutil.FFmpegDecodeF32(t, tc.path)
			ch := 2
			refFrames := int64(len(ref) / ch)
			raw, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			// 512 and 2000 sit inside the first long block; the fractional
			// targets spread out so several land past a bisection step, since
			// a landing that always resolves to 0 proves nothing.
			targets := []int64{0, 512, 2000, 5000, 20000, 44100, refFrames - 5000}
			for _, num := range []int64{1, 2, 3, 4, 5, 6, 7} {
				targets = append(targets, refFrames*num/8)
			}
			for _, target := range targets {
				seekAndCompare(t, raw, ref, ch, target)
			}
		})
	}
}

// seekAndCompare seeks to target and checks that the delivered audio matches the
// reference at the reported landing. On a mismatch it reports the reference
// offset that would have matched, which names the slip directly.
func seekAndCompare(t *testing.T, raw []byte, ref []float32, ch int, target int64) {
	t.Helper()
	med, err := Open(container.BytesSource(raw), "ogg", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	landed, err := med.SeekSample(target)
	if err != nil {
		t.Fatalf("seek %d: %v", target, err)
	}
	if landed > target {
		t.Fatalf("seek %d landed after target at %d", target, landed)
	}
	dst := audio.Get(med.Info().Tracks[0].Fmt, audio.StandardChunk)
	defer audio.Put(dst)
	if err := med.ReadChunk(dst); err != nil {
		t.Fatalf("read after seek %d: %v", target, err)
	}
	maxAbs := chunkMaxAbs(dst, ref, ch, landed, 0)
	t.Logf("target=%d landed=%d maxAbs=%.2e", target, landed, maxAbs)
	if maxAbs <= 1e-3 {
		return
	}
	best, bestOff := math.Inf(1), 0
	for o := -2048; o <= 2048; o++ {
		if m := chunkMaxAbs(dst, ref, ch, landed, int64(o)); m < best {
			best, bestOff = m, o
		}
	}
	t.Errorf("seek target=%d landed=%d: post-pre-roll maxAbs %.2e exceeds 1e-3; "+
		"the delivered audio matches the reference at %+d (maxAbs %.2e), so the landing is off by that much",
		target, landed, maxAbs, bestOff, best)
}

// chunkMaxAbs is the largest absolute difference between a decoded chunk and the
// reference starting at landed+off frames.
func chunkMaxAbs(dst *audio.Buffer, ref []float32, ch int, landed, off int64) float64 {
	var maxAbs float64
	for i := 0; i < dst.N; i++ {
		ri := (int(landed+off) + i) * ch
		if ri < 0 || ri+ch > len(ref) {
			continue
		}
		for c := 0; c < ch; c++ {
			if d := math.Abs(float64(dst.ChanF(c)[i]) - float64(ref[ri+c])); d > maxAbs {
				maxAbs = d
			}
		}
	}
	return maxAbs
}
