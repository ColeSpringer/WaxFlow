package waxflow_test

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestVorbisRealAudioQuality is the "known gap" step-0 validation: the encoder's
// size and high-quality-ceiling deltas vs libvorbis were all measured on
// synthesized signals, so this re-measures them on REAL audio before any
// redesign work (the gap may shrink or grow and reprioritize the rest).
//
// It is gated on WAXFLOW_REAL_AUDIO_DIR (a directory of real .wav files) and so
// never runs in the default or encoder-quality suites; the audio is not part of
// the repo (the clean-room posture keeps external audio out of the tree). Point
// it at any set of lossless/high-bitrate .wav files. Optional env:
//
//	WAXFLOW_REAL_AUDIO_Q        comma-separated -q points (default "4,6,8")
//	WAXFLOW_REAL_AUDIO_SECONDS  per-clip trim length (default 20)
//
// Both our stream and the libvorbis reference decode through the pinned
// libvorbis decoder (ffmpeg's native Vorbis decoder is experimental and
// mis-decodes legal coupled streams; see docs/quality-gates.md). Our encoder and
// libvorbis are handed identical audio: the source is decoded to f32, capped,
// and re-emitted as the WAV libvorbis reads (the gate's baseline path).
func TestVorbisRealAudioQuality(t *testing.T) {
	dir := os.Getenv("WAXFLOW_REAL_AUDIO_DIR")
	if dir == "" {
		t.Skip("set WAXFLOW_REAL_AUDIO_DIR to a directory of real .wav files")
	}
	testutil.FFmpeg(t)
	if !testutil.HaveLibVorbis(t) {
		t.Skip("ffmpeg libvorbis not available")
	}

	qs := []float64{4, 6, 8}
	if s := os.Getenv("WAXFLOW_REAL_AUDIO_Q"); s != "" {
		qs = nil
		for _, p := range strings.Split(s, ",") {
			v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
			if err != nil {
				t.Fatalf("bad WAXFLOW_REAL_AUDIO_Q %q: %v", s, err)
			}
			qs = append(qs, v)
		}
	}
	trimSec := 20
	if s := os.Getenv("WAXFLOW_REAL_AUDIO_SECONDS"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil {
			t.Fatalf("bad WAXFLOW_REAL_AUDIO_SECONDS %q: %v", s, err)
		}
		trimSec = v
	}

	files, err := filepath.Glob(filepath.Join(dir, "*.wav"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no .wav files in %s", dir)
	}
	sort.Strings(files)

	// Per-q accumulators for the aggregate read.
	type agg struct {
		sumOurODG, sumRefODG, sumRatio float64
		worstDelta                     float64
		n                              int
	}
	aggs := make(map[float64]*agg, len(qs))
	for _, q := range qs {
		aggs[q] = &agg{worstDelta: math.Inf(1)}
	}

	t.Logf("clip                 q   ourODG  refODG    dODG   ourKbps refKbps  size(x)")
	for _, src := range files {
		name := strings.TrimSuffix(filepath.Base(src), ".wav")

		// Decode the source, cap to trimSec, and hand both encoders identical audio.
		info := testutil.FFprobeFile(t, src)
		rate, ch := info.SampleRate, info.Channels
		full := testutil.FFmpegDecodeF32(t, src)
		frames := len(full) / ch
		if max := trimSec * rate; frames > max {
			frames = max
		}
		if frames == 0 {
			t.Fatalf("%s decoded empty", name)
		}
		ref := full[:frames*ch]
		secs := float64(frames) / float64(rate)
		f := audio.Format{Rate: rate, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Float, BitDepth: 32}

		// The WAV libvorbis reads (same samples our encoder gets).
		wavPath := filepath.Join(t.TempDir(), name+".wav")
		if err := os.WriteFile(wavPath, synthWAVFromSamples(t, f, ref), 0o644); err != nil {
			t.Fatal(err)
		}

		for _, q := range qs {
			// Our encoder, framed into Ogg-Vorbis, decoded by libvorbis.
			ogg := encodeVorbisOgg(t, f, ref, q)
			ourPath := filepath.Join(t.TempDir(), fmt.Sprintf("%s-q%.0f.ours.ogg", name, q))
			if err := os.WriteFile(ourPath, ogg, 0o644); err != nil {
				t.Fatal(err)
			}
			ourDec := testutil.FFmpegDecodeF32Codec(t, ourPath, "libvorbis")
			ourODG := testutil.ODGProxy(ref, ourDec, rate, ch)
			ourKbps := float64(len(ogg)*8) / secs / 1000

			// libvorbis at the matching -q, decoded by libvorbis.
			refPath := testutil.FFmpegVorbisEncodeFile(t, wavPath, q)
			refInfo, _ := os.Stat(refPath)
			refKbps := float64(refInfo.Size()*8) / secs / 1000
			refDec := testutil.FFmpegDecodeF32Codec(t, refPath, "libvorbis")
			refODG := testutil.ODGProxy(ref, refDec, rate, ch)

			delta := ourODG - refODG
			ratio := ourKbps / refKbps
			a := aggs[q]
			a.sumOurODG += ourODG
			a.sumRefODG += refODG
			a.sumRatio += ratio
			a.worstDelta = math.Min(a.worstDelta, delta)
			a.n++

			t.Logf("%-18s %2.0f  %+.3f  %+.3f  %+.3f   %6.0f  %6.0f   %4.2fx",
				name, q, ourODG, refODG, delta, ourKbps, refKbps, ratio)
		}
	}

	t.Logf("--- aggregate over %d clips ---", len(files))
	for _, q := range qs {
		a := aggs[q]
		if a.n == 0 {
			continue
		}
		t.Logf("q=%.0f  meanOur=%+.3f meanRef=%+.3f  meanDelta=%+.3f  worstDelta=%+.3f  meanSize=%.2fx",
			q, a.sumOurODG/float64(a.n), a.sumRefODG/float64(a.n),
			(a.sumOurODG-a.sumRefODG)/float64(a.n), a.worstDelta, a.sumRatio/float64(a.n))
	}
}
