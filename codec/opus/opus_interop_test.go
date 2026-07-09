package opus

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/ogg"
	"github.com/colespringer/waxflow/internal/testutil"
)

// encodeOggOpus encodes planar src (C channels, 48 kHz float) to an Ogg-Opus
// byte stream via the public Encoder + Ogg muxer, returning the container bytes.
func encodeOggOpus(t *testing.T, src [][]float32, bitrate int) []byte {
	t.Helper()
	C := len(src)
	total := len(src[0])
	f := audio.Format{Rate: SampleRate, Channels: C, Layout: audio.DefaultLayout(C), Type: audio.Float, BitDepth: 32}
	enc, err := NewEncoder(f, &EncoderOptions{Bitrate: bitrate})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	mux := ogg.NewMuxer(&out, nil)
	track := container.Track{Codec: codec.Opus, CodecConfig: enc.CodecConfig(), Fmt: f}
	if err := mux.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	flat := make([]float32, C*total)
	for c := 0; c < C; c++ {
		copy(flat[c*total:], src[c])
	}
	buf := &audio.Buffer{Fmt: f, F: flat, Stride: total, N: total}
	emit := func(p codec.Packet) error { return mux.WritePacket(container.Packet{Packet: p}) }
	if err := enc.Encode(buf, emit); err != nil {
		t.Fatal(err)
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := mux.End(trailer); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// ffDecodeF32 decodes an audio file to planar float32 at 48 kHz via ffmpeg.
func ffDecodeF32(t *testing.T, ff, path string, C int) [][]float32 {
	t.Helper()
	raw := filepath.Join(t.TempDir(), "dec.f32")
	if out, err := exec.Command(ff, "-v", "error", "-y", "-i", path,
		"-f", "f32le", "-ac", strconv.Itoa(C), "-ar", "48000", raw).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg decode: %v\n%s", err, out)
	}
	b, err := os.ReadFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	inter := make([]float32, len(b)/4)
	for i := range inter {
		inter[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	out := make([][]float32, C)
	n := len(inter) / C
	for c := 0; c < C; c++ {
		out[c] = make([]float32, n)
		for i := 0; i < n; i++ {
			out[c][i] = inter[i*C+c]
		}
	}
	return out
}

// TestOpusEncodeLibopusDecode confirms libopus (ffmpeg) decodes our Ogg-Opus
// output and reconstructs the signal. The corpus quality gate covers raw
// packets through opus_demo; this covers the muxed Ogg path end to end.
func TestOpusEncodeLibopusDecode(t *testing.T) {
	ff := testutil.FFmpeg(t)
	for _, C := range []int{1, 2} {
		src := synthMusic(C, 48000)
		og := encodeOggOpus(t, src, 128000)
		dir := t.TempDir()
		path := filepath.Join(dir, "ours.opus")
		if err := os.WriteFile(path, og, 0o644); err != nil {
			t.Fatal(err)
		}
		got := ffDecodeF32(t, ff, path, C)
		q, d := alignedQ(src, got, C)
		snr, _ := bestSNRDelay(src, got, 400)
		t.Logf("C=%d: libopus-decode opus_compare Q=%.2f (delay %d), SNR=%.1f dB", C, q, d, snr)
		if snr < 8 {
			t.Errorf("C=%d: libopus could not reconstruct our stream (SNR %.1f dB)", C, snr)
		}
	}
}

// synthMusic makes a deterministic, spectrally full C-channel signal at sr Hz:
// tonal peaks over a shaped broadband noise floor, so every CELT band carries
// energy. A pure-tone signal would leave most bands near zero, which makes
// opus_compare's per-band Y/X ratio explode on any encoder that noise-fills.
func synthMusic(C, sr int) [][]float32 {
	total := int(1.2 * float64(sr))
	src := make([][]float32, C)
	for c := 0; c < C; c++ {
		src[c] = make([]float32, total)
		f1 := 220.0 + 110*float64(c)
		var seed uint32 = 0x51ed7700 + uint32(c)*2654435761
		var lp float64 // one-pole low-pass state for the noise floor
		for i := range src[c] {
			t := float64(i) / float64(sr)
			seed = seed*1664525 + 1013904223
			white := float64(int32(seed)) / float64(1<<31)
			lp += 0.85 * (white - lp) // low-pass toward a pink-ish tilt
			noise := lp
			// A harmonic stack (compressible) plus a modest broadband floor so
			// every band carries energy without the noise dominating.
			tone := 0.30*math.Sin(2*math.Pi*f1*t) +
				0.20*math.Sin(2*math.Pi*(f1*2)*t) +
				0.12*math.Sin(2*math.Pi*(f1*3)*t) +
				0.08*math.Sin(2*math.Pi*(f1*5)*t)*(1+0.5*math.Sin(2*math.Pi*3*t)) +
				0.05*math.Sin(2*math.Pi*(f1*8)*t)
			v := 0.85*tone + 0.05*noise
			if int(t/0.3)%2 == 0 && math.Mod(t, 0.3) < 0.008 {
				v += 0.25 * math.Sin(2*math.Pi*5000*t)
			}
			src[c][i] = float32(0.9 * v)
		}
	}
	return src
}

func interleaveScaled(x [][]float32) []float32 {
	C := len(x)
	n := len(x[0])
	out := make([]float32, C*n)
	for i := 0; i < n; i++ {
		for c := 0; c < C; c++ {
			out[i*C+c] = x[c][i] * 32768
		}
	}
	return out
}

// bestSNRDelay sweeps the got->src lead maximizing SNR and returns both the
// SNR and the lead (opus_compare needs sample-exact alignment; a small
// residual pre-skip error otherwise tanks it).
func bestSNRDelay(src, got [][]float32, window int) (float64, int) {
	best, bestD := math.Inf(-1), 0
	for d := 0; d <= window; d++ {
		if s := roundTripSNR(src, got, d); s > best {
			best, bestD = s, d
		}
	}
	return best, bestD
}

// alignedQ aligns got to src by the best delay, trims to a common inner region,
// and returns the opus_compare quality on the aligned pair.
func alignedQ(src, got [][]float32, C int) (float64, int) {
	_, d := bestSNRDelay(src, got, 400)
	const skip = 2400
	n := len(got[0]) - d - skip
	if m := len(src[0]) - skip; m < n {
		n = m
	}
	if n < 2000 {
		return math.Inf(-1), d
	}
	sa := make([][]float32, C)
	ga := make([][]float32, C)
	for c := 0; c < C; c++ {
		sa[c] = src[c][skip : skip+n]
		ga[c] = got[c][skip+d : skip+d+n]
	}
	return testutil.OpusCompare(interleaveScaled(sa), interleaveScaled(ga), C), d
}
