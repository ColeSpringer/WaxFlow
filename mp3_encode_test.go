package waxflow_test

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	waxflow "github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

// synthWAV builds an in-memory WAV of a two-tone signal in the given format.
func synthWAV(t *testing.T, f audio.Format, frames int) []byte {
	t.Helper()
	src := audio.Get(f, frames)
	src.N = frames
	for ch := 0; ch < f.Channels; ch++ {
		for i := 0; i < frames; i++ {
			x := float64(i)
			v := 0.35*math.Sin(2*math.Pi*(500+float64(ch)*70)*x/float64(f.Rate)) +
				0.2*math.Sin(2*math.Pi*3200*x/float64(f.Rate))
			if f.Type == audio.Int {
				scale := float64(int64(1) << (f.BitDepth - 1))
				src.ChanI(ch)[i] = int32(v * (scale - 1))
			} else {
				src.ChanF(ch)[i] = float32(v)
			}
		}
	}
	out := wavFromBuffer(t, f, src)
	audio.Put(src)
	return out
}

// wavFromBuffer muxes a single PCM buffer into WAV bytes.
func wavFromBuffer(t *testing.T, f audio.Format, src *audio.Buffer) []byte {
	t.Helper()
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
	track := container.Track{Codec: codec.PCM, CodecConfig: enc.CodecConfig(), Fmt: f, Samples: int64(src.N), Default: true}
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
	return buf.Bytes()
}

// transcodeMP3 transcodes a source to MP3 bytes via the engine.
func transcodeMP3(t *testing.T, src []byte, opts waxflow.TranscodeOptions) []byte {
	t.Helper()
	opts.Format = "mp3"
	e := waxflow.New()
	var out bytes.Buffer
	if _, err := e.Transcode(context.Background(), container.BytesSource(src), "", &out, opts); err != nil {
		t.Fatalf("Transcode to mp3: %v", err)
	}
	return out.Bytes()
}

// decodeMP3Ours opens the MP3 through the read pipeline (mpa demux + mp3
// decode + gapless trims) and returns interleaved float PCM and the reported
// sample count.
func decodeMP3Ours(t *testing.T, mp3Bytes []byte) ([]float32, int64) {
	t.Helper()
	med, err := format.Open(container.BytesSource(mp3Bytes), "mp3", nil)
	if err != nil {
		t.Fatalf("open mp3: %v", err)
	}
	defer med.Close()
	got := med.Info().Default().Samples
	buf := audio.Get(med.Info().Default().Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	var out []float32
	for {
		err := med.ReadChunk(buf)
		if err != nil {
			break
		}
		out = append(out, testutil.InterleaveF(buf)...)
	}
	return out, got
}

// TestMP3EncodeGapless verifies the LAME-tag gapless invariant end to end:
// the trimmed decoded length equals the source sample count, across sample
// rates, channel counts, and both integer and float sources.
func TestMP3EncodeGapless(t *testing.T) {
	cases := []struct {
		name            string
		rate, ch, bits  int
		typ             audio.SampleType
		frames, bitrate int
	}{
		{"44k-stereo-s16", 44100, 2, 16, audio.Int, 20000, 128000},
		{"44k-mono-s16", 44100, 1, 16, audio.Int, 17003, 128000},
		{"48k-stereo-f32", 48000, 2, 32, audio.Float, 22050, 192000},
		{"32k-stereo-s16", 32000, 2, 16, audio.Int, 12000, 128000},
		{"24k-stereo-s16-mpeg2", 24000, 2, 16, audio.Int, 15000, 96000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := audio.Format{Rate: tc.rate, Channels: tc.ch, Layout: audio.DefaultLayout(tc.ch), Type: tc.typ, BitDepth: tc.bits}
			wav := synthWAV(t, f, tc.frames)
			mp3 := transcodeMP3(t, wav, waxflow.TranscodeOptions{MP3Bitrate: tc.bitrate})
			out, reported := decodeMP3Ours(t, mp3)
			gotFrames := int64(len(out) / tc.ch)
			if reported != int64(tc.frames) {
				t.Errorf("reported track samples %d, want %d", reported, tc.frames)
			}
			if gotFrames != int64(tc.frames) {
				t.Errorf("decoded %d frames after gapless trim, want %d", gotFrames, tc.frames)
			}
		})
	}
}

// TestMP3EncodeDifferential encodes through the engine and confirms the output
// decodes to a signal that tracks the source, both with our own read pipeline
// and (when available) with ffmpeg and the pure-Go go-mp3 oracle. A lossy
// baseline is not exact, so the bar is a reasonable signal-to-noise ratio at
// the best alignment, not sample equality.
func TestMP3EncodeDifferential(t *testing.T) {
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	const frames = 44100
	wav := synthWAV(t, f, frames)
	mp3 := transcodeMP3(t, wav, waxflow.TranscodeOptions{MP3Bitrate: 128000})

	// Reference input as interleaved float.
	ref := make([]float32, frames*2)
	for i := 0; i < frames; i++ {
		for ch := 0; ch < 2; ch++ {
			x := float64(i)
			ref[i*2+ch] = float32(0.35*math.Sin(2*math.Pi*(500+float64(ch)*70)*x/44100) +
				0.2*math.Sin(2*math.Pi*3200*x/44100))
		}
	}

	// Our decoder (gapless-trimmed, so it aligns at lag 0).
	got, _ := decodeMP3Ours(t, mp3)
	if snr := snrDB(ref, got, 0, 2); snr < 18 {
		t.Errorf("our-decode SNR %.1f dB below 18 dB", snr)
	} else {
		t.Logf("our-decode SNR %.1f dB", snr)
	}

	// go-mp3: pure-Go oracle, stereo, no gapless trim. It also decodes the
	// Info metadata frame as audio, so the real content sits a further frame
	// in; search wide enough to reach it.
	want, _ := testutil.GoMP3Decode(t, mp3)
	if snr := bestSNR(ref, want, 3000, 2); snr < 40 {
		t.Errorf("go-mp3 SNR %.1f dB below 40 dB", snr)
	} else {
		t.Logf("go-mp3 SNR %.1f dB", snr)
	}

	// ffmpeg: authoritative decoder, gated on availability.
	if path := testutil.FFmpeg(t); path != "" {
		dir := t.TempDir()
		fp := filepath.Join(dir, "out.mp3")
		if err := os.WriteFile(fp, mp3, 0o644); err != nil {
			t.Fatal(err)
		}
		fmp := testutil.FFmpegDecodeF32(t, fp)
		if snr := bestSNR(ref, fmp, 1600, 2); snr < 16 {
			t.Errorf("ffmpeg SNR %.1f dB below 16 dB", snr)
		} else {
			t.Logf("ffmpeg SNR %.1f dB", snr)
		}
		info := testutil.FFprobeFile(t, fp)
		if info.CodecName != "mp3" {
			t.Errorf("ffprobe codec %q, want mp3", info.CodecName)
		}
	}
}

// snrDB computes SNR at a fixed lag over interleaved channels.
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
