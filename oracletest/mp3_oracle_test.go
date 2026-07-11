package oracletest

// The go-mp3 differential cells: hajimehoshi/go-mp3 is the independent
// pure-Go MP3 decoder that lets these comparisons run without ffmpeg
// installed. The decode direction moved here from the root mp3_test.go
// and the encode direction from mp3_encode_test.go's differential (the
// ffmpeg and own-decoder cells stayed behind); the helpers below are
// copies of those files' fixtures so the signals stay identical.

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestMP3DecodeAgainstGoMP3 pins our decoder against the go-mp3 oracle.
// go-mp3 applies no gapless trims and its output is 16-bit quantized,
// which floors the achievable agreement around 1e-5 RMS; the gate
// leaves headroom over that floor, not over ours. The fixture is the
// untagged stream so both sides decode the same frames.
func TestMP3DecodeAgainstGoMP3(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "sine-untagged.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	want, rate := GoMP3Decode(t, raw)
	if rate != 44100 {
		t.Fatalf("oracle rate = %d", rate)
	}
	got := decodePCM(t, raw)
	d := testutil.CompareF32(testutil.InterleaveF(got), want)
	t.Logf("diff: %v", d)
	if d.N < 0 {
		t.Fatalf("length mismatch: ours %d floats, oracle %d", got.N*got.Fmt.Channels, len(want))
	}
	if d.RMS > 1e-4 {
		t.Errorf("RMS %g exceeds 1e-4", d.RMS)
	}
	if d.MaxAbs > 1e-3 {
		t.Errorf("max abs %g exceeds 1e-3", d.MaxAbs)
	}
}

// TestMP3EncodeAgainstGoMP3 encodes through the engine and confirms
// go-mp3 decodes the output to a signal tracking the source. go-mp3
// decodes the Info metadata frame as audio and applies no trims, so the
// alignment search runs wide; the bar is best-lag SNR, not equality.
func TestMP3EncodeAgainstGoMP3(t *testing.T) {
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	const frames = 44100
	wav, ref := synthWAV(t, f, frames)

	var mp3 bytes.Buffer
	if _, err := waxflow.New().Transcode(context.Background(), container.BytesSource(wav), "", &mp3,
		waxflow.TranscodeOptions{Format: "mp3", MP3Bitrate: 128000}); err != nil {
		t.Fatalf("Transcode to mp3: %v", err)
	}

	want, _ := GoMP3Decode(t, mp3.Bytes())
	if snr := bestSNR(ref, want, 3000, 2); snr < 40 {
		t.Errorf("go-mp3 SNR %.1f dB below 40 dB", snr)
	} else {
		t.Logf("go-mp3 SNR %.1f dB", snr)
	}
}

// synthWAV renders the two-tone test signal (the same one the root
// encode differential uses) as a WAV file plus its interleaved float
// reference.
func synthWAV(t *testing.T, f audio.Format, frames int) ([]byte, []float32) {
	t.Helper()
	src := audio.Get(f, frames)
	defer audio.Put(src)
	src.N = frames
	ref := make([]float32, frames*f.Channels)
	scale := float64(int64(1)<<(f.BitDepth-1)) - 1
	for ch := 0; ch < f.Channels; ch++ {
		for i := 0; i < frames; i++ {
			x := float64(i)
			v := 0.35*math.Sin(2*math.Pi*(500+float64(ch)*70)*x/float64(f.Rate)) +
				0.2*math.Sin(2*math.Pi*3200*x/float64(f.Rate))
			q := int32(v * scale)
			src.ChanI(ch)[i] = q
			ref[i*f.Channels+ch] = float32(float64(q) / (scale + 1))
		}
	}

	cfg, err := riff.DefaultConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := pcm.NewEncoder(cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	m := riff.NewMuxer(&buf, nil)
	track := container.Track{Codec: codec.PCM, CodecConfig: enc.CodecConfig(), Fmt: f, Samples: int64(frames), Default: true}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error { return m.WritePacket(container.Packet{Packet: p}) }
	if err := enc.Encode(src, emit); err != nil {
		t.Fatal(err)
	}
	tr, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.End(tr); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), ref
}

// snrDB computes signal-to-noise against the reference at a fixed frame
// lag, skipping the first 2000 frames of encoder warm-up.
func snrDB(ref, got []float32, lag, ch int) float64 {
	var s, n float64
	for i := ch * 2000; i+lag*ch < len(got) && i < len(ref); i++ {
		s += float64(ref[i]) * float64(ref[i])
		d := float64(got[i+lag*ch]) - float64(ref[i])
		n += d * d
	}
	if n == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(s/n)
}

// bestSNR searches per-frame lags for the alignment maximizing SNR.
func bestSNR(ref, got []float32, maxLag, ch int) float64 {
	best := math.Inf(-1)
	for lag := 0; lag < maxLag; lag++ {
		if s := snrDB(ref, got, lag, ch); s > best {
			best = s
		}
	}
	return best
}
