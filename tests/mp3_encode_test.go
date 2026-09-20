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
// and (when available) with ffmpeg; the go-mp3 oracle cell lives in the
// oracletest module. A lossy
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

	// The go-mp3 oracle cell lives in oracletest (its module carries the
	// third-party oracle dependencies).

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

// TestMP3VBRGaplessAndOracle checks the VBR path end to end: the engine
// writes a Xing-led variable-rate stream whose gapless invariant holds
// through our read pipeline, and ffmpeg (when available) decodes it and
// reports the exact duration from the tag.
func TestMP3VBRGaplessAndOracle(t *testing.T) {
	const rate, frames = 44100, 40000
	f := audio.Format{Rate: rate, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	wav := synthWAV(t, f, frames)
	mp3Bytes := transcodeMP3(t, wav, waxflow.TranscodeOptions{MP3Bitrate: 128000, MP3VBR: true})

	// Frames pick their own rates; at least the Xing frame's rate and one
	// content rate must appear.
	out, reported := decodeMP3Ours(t, mp3Bytes)
	if reported != frames {
		t.Errorf("reported track samples %d, want %d", reported, frames)
	}
	if got := int64(len(out) / 2); got != frames {
		t.Errorf("decoded %d frames after gapless trim, want %d", got, frames)
	}

	if path := testutil.FFmpeg(t); path != "" {
		dir := t.TempDir()
		fp := filepath.Join(dir, "out.mp3")
		if err := os.WriteFile(fp, mp3Bytes, 0o644); err != nil {
			t.Fatal(err)
		}
		dec := testutil.FFmpegDecodeF32(t, fp)
		if len(dec) == 0 {
			t.Fatal("ffmpeg decoded nothing")
		}
		info := testutil.FFprobeFile(t, fp)
		if info.CodecName != "mp3" {
			t.Errorf("ffprobe codec %q, want mp3", info.CodecName)
		}
	}
}

// TestMP3EncodeFFmpegAlignment is the gate the LAME extension exists for:
// ffmpeg has to decode a WaxFlow MP3 to exactly the source's length, with
// the first decoded sample being the source's first.
//
// It is strict on purpose. The differential above scores at the best of
// 1600 lags, so it passed for years over a stream ffmpeg played 22 ms late
// and 57 ms long: ffmpeg applies the gapless fields only when the
// extension's encoder string starts LAME, Lavc or Lavf, and this muxer
// branded it WaxFlow01. Nothing here would have caught that but a test
// that asks where the audio landed.
func TestMP3EncodeFFmpegAlignment(t *testing.T) {
	if !testutil.HaveFFmpeg(t) {
		t.Skip("ffmpeg not installed")
	}
	cases := []struct {
		name            string
		rate, ch        int
		frames, bitrate int
		vbr             bool
	}{
		{"44k-stereo-cbr", 44100, 2, 44100, 128000, false},
		{"44k-stereo-vbr", 44100, 2, 44100, 128000, true},
		{"48k-stereo-cbr", 48000, 2, 48000, 192000, false},
		{"48k-stereo-vbr", 48000, 2, 48000, 192000, true},
		// MPEG-2.5, where the tester measured 1216 samples of excess.
		{"8k-mono-cbr", 8000, 1, 8000, 64000, false},
		{"8k-mono-vbr", 8000, 1, 8000, 64000, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := audio.Format{Rate: tc.rate, Channels: tc.ch, Layout: audio.DefaultLayout(tc.ch), Type: audio.Int, BitDepth: 16}
			ref := sweep(tc.rate, tc.frames, tc.ch)
			wav := wavOf(t, f, ref)
			mp3 := transcodeMP3(t, wav, waxflow.TranscodeOptions{MP3Bitrate: tc.bitrate, MP3VBR: tc.vbr})

			dir := t.TempDir()
			fp := filepath.Join(dir, "out.mp3")
			if err := os.WriteFile(fp, mp3, 0o644); err != nil {
				t.Fatal(err)
			}
			dec := testutil.FFmpegDecodeF32(t, fp)
			if got := len(dec) / tc.ch; got != tc.frames {
				t.Errorf("ffmpeg decoded %d frames, want %d (%+d)", got, tc.frames, got-tc.frames)
			}

			// Both directions. A decode that skipped too much lands EARLY,
			// and only a negative lag can see it: the sample count catches
			// the usual form of that, but not one that also runs long.
			at0 := snrDB(ref, dec, 0, tc.ch)
			for lag := -1600; lag < 1600; lag++ {
				if lag == 0 {
					continue
				}
				if s := snrDB(ref, dec, lag, tc.ch); s > at0 {
					t.Fatalf("ffmpeg's decode aligns best at lag %d (%.1f dB) not 0 (%.1f dB): the gapless fields did not apply",
						lag, s, at0)
				}
			}
			t.Logf("%s: %d frames, SNR at lag 0 = %.1f dB", tc.name, len(dec)/tc.ch, at0)
		})
	}
}

// sweep is the alignment test's source: a frequency ramp, which the
// two-tone signal the rest of this file uses cannot be here. At 8 kHz its
// 500 Hz and 3200 Hz components have periods of 16 and 2.5 samples, so the
// whole signal repeats every 16 samples and EVERY multiple of 16 aligns as
// well as lag 0 does -- an alignment gate over it proves nothing. A ramp
// repeats at no lag.
func sweep(rate, frames, ch int) []float32 {
	out := make([]float32, frames*ch)
	hi := float64(rate) / 3
	for i := range frames {
		t := float64(i) / float64(frames)
		// Phase of a linear sweep from 300 Hz to hi, integrated.
		phase := 2 * math.Pi * (300*float64(i)/float64(rate) + (hi-300)*t*float64(i)/(2*float64(rate)))
		for c := range ch {
			out[i*ch+c] = float32(0.35 * math.Sin(phase+float64(c)*0.7))
		}
	}
	return out
}

// wavOf writes interleaved float samples as a WAV in f's format.
func wavOf(t *testing.T, f audio.Format, interleaved []float32) []byte {
	t.Helper()
	frames := len(interleaved) / f.Channels
	src := audio.Get(f, frames)
	src.N = frames
	scale := float64(int64(1)<<(f.BitDepth-1)) - 1
	for ch := range f.Channels {
		for i := range frames {
			v := float64(interleaved[i*f.Channels+ch])
			if f.Type == audio.Int {
				src.ChanI(ch)[i] = int32(v * scale)
			} else {
				src.ChanF(ch)[i] = float32(v)
			}
		}
	}
	out := wavFromBuffer(t, f, src)
	audio.Put(src)
	return out
}

// snrDB computes SNR at a fixed lag over interleaved channels. A negative
// lag scores got as landing EARLY, which is the direction a too-large
// gapless skip moves a decode.
func snrDB(ref, got []float32, lag, ch int) float64 {
	var s, n float64
	for i := ch * 2000; i+lag*ch < len(got) && i < len(ref); i++ {
		if i+lag*ch < 0 {
			continue
		}
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
