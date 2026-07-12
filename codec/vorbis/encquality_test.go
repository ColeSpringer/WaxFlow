package vorbis

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/internal/testutil"
)

// writeWAV writes interleaved float32 as a 16-bit PCM WAV (what ffmpeg and
// libvorbis read for the baseline encode).
func writeWAV(t *testing.T, path string, interleaved []float32, rate, ch int) {
	t.Helper()
	var b bytes.Buffer
	n := len(interleaved)
	dataLen := n * 2
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+dataLen))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&b, binary.LittleEndian, uint16(ch))
	binary.Write(&b, binary.LittleEndian, uint32(rate))
	binary.Write(&b, binary.LittleEndian, uint32(rate*ch*2))
	binary.Write(&b, binary.LittleEndian, uint16(ch*2))
	binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(dataLen))
	for _, v := range interleaved {
		s := int32(math.Round(float64(v) * 32767))
		if s > 32767 {
			s = 32767
		}
		if s < -32768 {
			s = -32768
		}
		binary.Write(&b, binary.LittleEndian, int16(s))
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// musicish synthesizes a tonal-plus-harmonics signal with light noise and
// amplitude dynamics: it has real masking structure (unlike broadband noise),
// so it exercises the perceptual allocation the way music does.
func musicish(rate, n, ch int, seed int64) [][]float32 {
	rng := rand.New(rand.NewSource(seed))
	src := make([][]float32, ch)
	fundamentals := []float64{220, 330, 440}
	for c := range src {
		src[c] = make([]float32, n)
		for i := 0; i < n; i++ {
			t := float64(i) / float64(rate)
			env := 0.5 + 0.5*math.Sin(2*math.Pi*2*t) // slow tremolo for dynamics
			var s float64
			for h, f0 := range fundamentals {
				f := f0 * (1 + 0.001*float64(c)) // slight stereo detune
				for k := 1; k <= 4; k++ {
					s += (0.3 / float64(k) / float64(h+1)) * math.Sin(2*math.Pi*f*float64(k)*t)
				}
			}
			s = s*env + 0.02*(rng.Float64()*2-1)
			src[c][i] = float32(s * 0.6)
		}
	}
	return src
}

// TestVorbisEncoderQuality measures our encoder against libvorbis on a
// tonal corpus: ODG-proxy vs source and the coded bit rate for each. It is a
// measurement/iteration harness gated by WAXFLOW_ENCODER_QUALITY, not a hard
// gate here (the corpus gate lives in tests/); it lets 4b tuning see the delta.
func TestVorbisEncoderQuality(t *testing.T) {
	testutil.EncoderQualityGate(t)
	testutil.FFmpeg(t)
	const rate = 44100
	for _, ch := range []int{1, 2} {
		f := audio.Format{Rate: rate, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Float, BitDepth: 32}
		n := 3 * rate
		src := musicish(rate, n, ch, 7)
		ref := interleave(src)

		wav := filepath.Join(t.TempDir(), "src.wav")
		writeWAV(t, wav, ref, rate, ch)

		for _, q := range []float64{1, 3, 6} {
			e, err := NewEncoder(f, &EncoderOptions{Quality: q})
			if err != nil {
				t.Fatal(err)
			}
			packets, granules, tr := encodeSignal(t, e, src)
			total := 0
			for _, p := range packets {
				total += len(p)
			}
			oursKbps := float64(total*8) / float64(n) * float64(rate) / 1000
			id, comment, setup, _ := splitHeaders(e.CodecConfig())
			ogg := testutil.OggVorbisFile(id, comment, setup, packets, granules, tr.Samples)
			ourPath := filepath.Join(t.TempDir(), "ours.ogg")
			os.WriteFile(ourPath, ogg, 0o644)
			ourDec := testutil.FFmpegDecodeF32(t, ourPath)
			ourODG := testutil.ODGProxy(ref, ourDec, rate, ch)

			// libvorbis at the matching -q point.
			refPath := testutil.FFmpegVorbisEncodeFile(t, wav, q)
			refInfo, _ := os.Stat(refPath)
			refKbps := float64(refInfo.Size()*8) / float64(n) * float64(rate) / 1000
			refDec := testutil.FFmpegDecodeF32(t, refPath)
			refODG := testutil.ODGProxy(ref, refDec, rate, ch)

			t.Logf("ch=%d q=%.0f  ours: ODG=%.3f %.0fkbps | libvorbis: ODG=%.3f %.0fkbps | dODG=%+.3f",
				ch, q, ourODG, oursKbps, refODG, refKbps, ourODG-refODG)
		}
	}
}
