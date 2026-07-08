package waxflow_test

// AAC-LC decode differentials. AAC is lossy, so our decode is compared to
// ffmpeg's within an RMS gate (2^-13 full scale). Fixtures are
// generated with ffmpeg at test time and self-skip without it. Perceptual
// noise substitution is non-reproducible across decoders, so fixtures are
// encoded with -aac_pns 0 (and -aac_is 0) to keep the comparison exact.

import (
	"math"
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

const aacRMSGate = 1.0 / 8192.0 // 2^-13

// genAAC writes an AAC fixture via ffmpeg. format is "adts" (raw .aac) or
// "mp4" (.m4a); the deterministic tools (PNS, IS) are disabled.
func genAAC(t *testing.T, dir, name, source string, channels int, format string, extra ...string) string {
	t.Helper()
	ffmpeg := testutil.FFmpeg(t)
	path := filepath.Join(dir, name)
	args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", source,
		"-ac", strconv.Itoa(channels), "-c:a", "aac", "-aac_pns", "0", "-aac_is", "0"}
	if format == "adts" {
		args = append(args, "-f", "adts")
	}
	args = append(args, extra...)
	args = append(args, path)
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg aac %s: %v\n%s", name, err, out)
	}
	return path
}

// alignedRMS finds the interleaved offset (within a couple of AAC frames)
// that best aligns ours to ref, then returns the RMS there. AAC decoders can
// differ by the one-frame decoder-delay priming, so a small offset search is
// the standard way to compare raw streams.
func alignedRMS(ours, ref []float32, channels int) (float64, int) {
	best, bestOff := math.Inf(1), 0
	limit := 2048 * channels
	for off := 0; off+len(ours) <= len(ref) && off <= limit; off += channels {
		var sumSq float64
		for i := range ours {
			e := float64(ours[i] - ref[off+i])
			sumSq += e * e
		}
		if rms := math.Sqrt(sumSq / float64(len(ours))); rms < best {
			best, bestOff = rms, off
		}
	}
	return best, bestOff
}

var aacSources = []struct {
	name     string
	source   string
	channels int
}{
	{"mono-sine", "sine=frequency=440:sample_rate=44100:duration=1", 1},
	{"stereo-sine", "sine=frequency=523:sample_rate=44100:duration=1", 2},
	{"stereo-noise", "anoisesrc=color=pink:sample_rate=44100:duration=1:seed=3", 2},
	{"stereo-noise48", "anoisesrc=color=white:sample_rate=48000:duration=1:seed=7", 2},
}

// TestAACDecodeADTSDifferential is the differential gate: ADTS carries no
// gapless trims, so our raw decode compares directly against ffmpeg within
// 2^-13.
func TestAACDecodeADTSDifferential(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range aacSources {
		t.Run(tc.name, func(t *testing.T) {
			path := genAAC(t, dir, tc.name+".aac", tc.source, tc.channels, "adts")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := decodeAllDynamic(t, container.BytesSource(raw), "aac")
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			defer audio.Put(got)
			ref := testutil.FFmpegDecodeF32(t, path)
			rms, off := alignedRMS(testutil.InterleaveF(got), ref, tc.channels)
			t.Logf("%s: rms=%g offset=%d", tc.name, rms, off)
			if rms > aacRMSGate {
				t.Errorf("RMS %g exceeds gate %g (offset %d)", rms, aacRMSGate, off)
			}
		})
	}
}

// TestAACGaplessM4A checks the iTunes/edit-list gapless invariant: the
// decoded length equals the source sample count exactly.
func TestAACGaplessM4A(t *testing.T) {
	dir := t.TempDir()
	const want = 44100 // 1s at 44100 Hz
	for _, ch := range []int{1, 2} {
		t.Run(strconv.Itoa(ch)+"ch", func(t *testing.T) {
			path := genAAC(t, dir, "gapless.m4a", "sine=frequency=440:sample_rate=44100:duration=1", ch, "mp4")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := decodeAllDynamic(t, container.BytesSource(raw), "m4a")
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			defer audio.Put(got)
			if got.N != want {
				t.Errorf("decoded %d samples, want %d (gapless trim)", got.N, want)
			}
		})
	}
}

// TestAACSeekM4A checks edit-list seek exactness: seeking through the m4a
// gapless delay lands sample-exact and bit-identical to the linear decode.
func TestAACSeekM4A(t *testing.T) {
	dir := t.TempDir()
	path := genAAC(t, dir, "seek.m4a", "anoisesrc=color=pink:sample_rate=44100:duration=2:seed=8", 2, "mp4")
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
	seekMatchesReference(t, med, ref, 50, 11)
}

// TestAACSeekADTS checks sample-exact seeking on an ADTS stream.
func TestAACSeekADTS(t *testing.T) {
	dir := t.TempDir()
	path := genAAC(t, dir, "seek.aac", "anoisesrc=color=pink:sample_rate=44100:duration=2:seed=5", 2, "adts")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := container.BytesSource(raw)
	ref, err := decodeAllDynamic(t, src, "aac")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(ref)
	med, err := waxflow.New().OpenStream(src, "aac")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	seekMatchesReference(t, med, ref, 50, 5)
}
