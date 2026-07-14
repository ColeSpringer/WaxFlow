package waxflow_test

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestVorbisRealAudioDiag is the "known gap" step-3 diagnosis: for each real
// clip it breaks the coding error down per bark band (ours vs libvorbis, both
// against the source) so the high-q deficit can be localized in frequency
// instead of guessed at. Gated exactly like TestVorbisRealAudioQuality.
//   WAXFLOW_REAL_AUDIO_Q        single -q point (default 8, where we plateau)
//   WAXFLOW_REAL_AUDIO_SECONDS  per-clip trim length (default 20)
func TestVorbisRealAudioDiag(t *testing.T) {
	dir := os.Getenv("WAXFLOW_REAL_AUDIO_DIR")
	if dir == "" {
		t.Skip("set WAXFLOW_REAL_AUDIO_DIR to a directory of real .wav files")
	}
	testutil.FFmpeg(t)
	if !testutil.HaveLibVorbis(t) {
		t.Skip("ffmpeg libvorbis not available")
	}
	q := 8.0
	if s := os.Getenv("WAXFLOW_REAL_AUDIO_Q"); s != "" {
		q, _ = strconv.ParseFloat(strings.TrimSpace(s), 64)
	}
	trimSec := 20
	if s := os.Getenv("WAXFLOW_REAL_AUDIO_SECONDS"); s != "" {
		trimSec, _ = strconv.Atoi(s)
	}

	files, _ := filepath.Glob(filepath.Join(dir, "*.wav"))
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatalf("no .wav files in %s", dir)
	}

	for _, src := range files {
		name := strings.TrimSuffix(filepath.Base(src), ".wav")
		info := testutil.FFprobeFile(t, src)
		rate, ch := info.SampleRate, info.Channels
		full := testutil.FFmpegDecodeF32(t, src)
		frames := len(full) / ch
		if m := trimSec * rate; frames > m {
			frames = m
		}
		ref := full[:frames*ch]
		f := audio.Format{Rate: rate, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Float, BitDepth: 32}

		wavPath := filepath.Join(t.TempDir(), name+".wav")
		os.WriteFile(wavPath, synthWAVFromSamples(t, f, ref), 0o644)

		ourPath := filepath.Join(t.TempDir(), name+".ours.ogg")
		os.WriteFile(ourPath, encodeVorbisOgg(t, f, ref, q), 0o644)
		ourDec := testutil.FFmpegDecodeF32Codec(t, ourPath, "libvorbis")
		refPath := testutil.FFmpegVorbisEncodeFile(t, wavPath, q)
		refDec := testutil.FFmpegDecodeF32Codec(t, refPath, "libvorbis")

		ourBands := testutil.ODGBandNMR(ref, ourDec, rate, ch)
		refBands := testutil.ODGBandNMR(ref, refDec, rate, ch)

		t.Logf("=== %s  q=%.0f  (per-band noise-to-signal, dB; delta>0 = we are worse) ===", name, q)
		t.Logf("  band(Hz)     sigShareDB   ourN/S   libN/S   delta")
		for i := range ourBands {
			if i >= len(refBands) {
				break
			}
			b := ourBands[i]
			delta := b.NoiseToSigDB - refBands[i].NoiseToSigDB
			t.Logf("  %5.0f-%-5.0f   %7.1f   %7.1f  %7.1f  %+6.1f",
				b.LoHz, b.HiHz, b.SignalFracDB, b.NoiseToSigDB, refBands[i].NoiseToSigDB, delta)
		}
	}
}
