package waxflow_test

// Differential tests against committed ffmpeg-generated fixtures and the
// live ffmpeg/ffprobe oracle (testutil skips oracle tests when ffmpeg is
// absent; the CI differential job sets WAXFLOW_REQUIRE_FFMPEG=1).
// Lossless means exact: integer comparisons are bit-for-bit, float
// comparisons are bit-for-bit too (both sides perform the same IEEE
// conversions).

import (
	"context"
	"errors"
	"io"
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

// fixtures describes every committed testdata file.
var fixtures = []struct {
	name      string
	container string
	codec     codec.ID
	rate      int
	channels  int
	sampleTyp audio.SampleType
	bitDepth  int
	samples   int64
}{
	{"sine-u8.wav", "wav", codec.PCM, 44100, 1, audio.Int, 8, 2205},
	{"sine-s16.wav", "wav", codec.PCM, 44100, 2, audio.Int, 16, 2205},
	{"sine-s24.wav", "wav", codec.PCM, 48000, 2, audio.Int, 24, 2400},
	{"sine-s32.wav", "wav", codec.PCM, 48000, 2, audio.Int, 32, 2400},
	{"sine-f32.wav", "wav", codec.PCM, 48000, 2, audio.Float, 32, 2400},
	{"sine-f64.wav", "wav", codec.PCM, 44100, 1, audio.Float, 32, 2205},
	{"sine-5_1-s16.wav", "wav", codec.PCM, 48000, 6, audio.Int, 16, 2400},
	{"sine-rf64.wav", "wav", codec.PCM, 44100, 2, audio.Int, 16, 2205},
	{"sine-s16.aiff", "aiff", codec.PCM, 44100, 2, audio.Int, 16, 2205},
	{"sine-s24.aiff", "aiff", codec.PCM, 48000, 2, audio.Int, 24, 2400},
	{"sine-s8.aiff", "aiff", codec.PCM, 44100, 1, audio.Int, 8, 2205},
	{"sine-f32.aiff", "aiff", codec.PCM, 48000, 2, audio.Float, 32, 2400},
	{"sine-sowt.aiff", "aiff", codec.PCM, 44100, 2, audio.Int, 16, 2205},
	{"sine-s16.flac", "flac", codec.FLAC, 44100, 2, audio.Int, 16, 15435},
	{"sine-s24.flac", "flac", codec.FLAC, 48000, 2, audio.Int, 24, 16800},
	{"sine-mono-s16.flac", "flac", codec.FLAC, 44100, 1, audio.Int, 16, 15435},
	{"sine-5_1-s16.flac", "flac", codec.FLAC, 48000, 6, audio.Int, 16, 16800},
	{"noise-s16.flac", "flac", codec.FLAC, 44100, 2, audio.Int, 16, 15435},
	{"sine-s16.oga", "ogg", codec.FLAC, 44100, 2, audio.Int, 16, 15435},
	{"noise-s24.oga", "ogg", codec.FLAC, 48000, 2, audio.Int, 24, 16800},
}

func fixtureSource(t testing.TB, name string) container.Source {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return container.BytesSource(raw)
}

// decodeAll reads a source through the engine into one buffer.
func decodeAll(t *testing.T, src container.Source, hint string) *audio.Buffer {
	t.Helper()
	med, err := waxflow.New().OpenStream(src, hint)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	info := med.Info().Default()
	if info.Samples < 0 {
		t.Fatal("fixture with unknown length")
	}
	out := audio.Get(info.Fmt, int(info.Samples))
	tmp := audio.Get(info.Fmt, audio.StandardChunk)
	defer audio.Put(tmp)
	for {
		err := med.ReadChunk(tmp)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for c := 0; c < info.Fmt.Channels; c++ {
			if info.Fmt.Type == audio.Float {
				copy(out.F[c*out.Stride+out.N:c*out.Stride+out.N+tmp.N], tmp.ChanF(c))
			} else {
				copy(out.I[c*out.Stride+out.N:c*out.Stride+out.N+tmp.N], tmp.ChanI(c))
			}
		}
		out.N += tmp.N
	}
	return out
}

// TestFixturesProbe needs no oracle: the committed fixtures are
// independent ground truth (ffmpeg produced them, not our muxers), and
// their parameters are pinned in the table above.
func TestFixturesProbe(t *testing.T) {
	for _, tt := range fixtures {
		t.Run(tt.name, func(t *testing.T) {
			info, err := waxflow.New().Probe(fixtureSource(t, tt.name), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if info.Container != tt.container {
				t.Errorf("container = %q, want %q", info.Container, tt.container)
			}
			if len(info.Warnings) != 0 {
				t.Errorf("warnings on a clean fixture: %v", info.Warnings)
			}
			d := info.Default()
			if d.Codec != tt.codec {
				t.Errorf("codec = %q, want %q", d.Codec, tt.codec)
			}
			if d.Fmt.Rate != tt.rate || d.Fmt.Channels != tt.channels || d.Fmt.Type != tt.sampleTyp || d.Fmt.BitDepth != tt.bitDepth {
				t.Errorf("format = %v, want %d Hz %d ch %v%d", d.Fmt, tt.rate, tt.channels, tt.sampleTyp, tt.bitDepth)
			}
			if d.Samples != tt.samples {
				t.Errorf("samples = %d, want %d", d.Samples, tt.samples)
			}
		})
	}
}

// TestFixturesDecodeDifferential compares our full decode of every
// fixture against ffmpeg's, sample for sample.
func TestFixturesDecodeDifferential(t *testing.T) {
	for _, tt := range fixtures {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join("testdata", tt.name)
			got := decodeAll(t, fixtureSource(t, tt.name), "")
			defer audio.Put(got)
			if tt.sampleTyp == audio.Int {
				want := testutil.FFmpegDecodeS32(t, path)
				if idx := testutil.DiffI32(testutil.Interleave(got), want); idx != -1 {
					t.Errorf("first sample mismatch vs ffmpeg at interleaved index %d", idx)
				}
			} else {
				want := testutil.FFmpegDecodeF32(t, path)
				d := testutil.CompareF32(testutil.InterleaveF(got), want)
				if d.MaxAbs != 0 {
					t.Errorf("float decode differs from ffmpeg: %v", d)
				}
			}
		})
	}
}

// TestFixturesProbeAgreesWithFFprobe pins the promise "probe
// agrees with ffprobe".
func TestFixturesProbeAgreesWithFFprobe(t *testing.T) {
	for _, tt := range fixtures {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join("testdata", tt.name)
			ref := testutil.FFprobeFile(t, path)
			info, err := waxflow.New().Probe(fixtureSource(t, tt.name), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			d := info.Default()
			if d.Fmt.Rate != ref.SampleRate {
				t.Errorf("rate = %d, ffprobe says %d", d.Fmt.Rate, ref.SampleRate)
			}
			if d.Fmt.Channels != ref.Channels {
				t.Errorf("channels = %d, ffprobe says %d", d.Fmt.Channels, ref.Channels)
			}
			if ref.Samples >= 0 && d.Samples != ref.Samples {
				t.Errorf("samples = %d, ffprobe says %d", d.Samples, ref.Samples)
			}
			// ffprobe reports PCM word width in bits_per_sample but codec
			// precision (FLAC) in bits_per_raw_sample; ours is always the
			// valid precision. Floats stay the pipeline's 32.
			refBits := ref.BitsPerSample
			if ref.BitsPerRawSample != 0 {
				refBits = ref.BitsPerRawSample
			}
			if tt.sampleTyp == audio.Int && d.Fmt.BitDepth != refBits {
				t.Errorf("bit depth = %d, ffprobe says %d", d.Fmt.BitDepth, refBits)
			}
		})
	}
}

// TestEncodeDifferential drives our whole write path and hands the result
// to ffmpeg: transcodes of a known signal must decode (via ffmpeg) to
// exactly the source samples, for both output containers, and ffprobe
// must agree on the parameters. Also covers RF64 auto-write output being
// readable by ffmpeg.
func TestEncodeDifferential(t *testing.T) {
	testutil.FFmpeg(t)
	matrix := []struct {
		name   string
		cfg    pcm.Config
		out    string
		outExt string
	}{
		{"s16 to aiff", pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, "aiff", "aiff"},
		{"s24 to aiff", pcm.Config{Encoding: pcm.SignedInt, Bits: 24}, "aiff", "aiff"},
		{"f32 to wav", pcm.Config{Encoding: pcm.Float, Bits: 32}, "wav", "wav"},
		{"u8 to aiff", pcm.Config{Encoding: pcm.UnsignedInt, Bits: 8}, "aiff", "aiff"},
		{"s24 to wav", pcm.Config{Encoding: pcm.SignedInt, Bits: 24}, "wav", "wav"},
	}
	const frames = 4801
	e := waxflow.New()
	for _, tt := range matrix {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
			src := testutil.Noise(f, frames, 23)
			defer audio.Put(src)
			wav := buildWAVFrom(t, tt.cfg, src)

			outPath := filepath.Join(t.TempDir(), "out."+tt.outExt)
			outFile, err := os.Create(outPath)
			if err != nil {
				t.Fatal(err)
			}
			res, err := e.Transcode(context.Background(), container.BytesSource(wav), "", outFile, waxflow.TranscodeOptions{Format: tt.out})
			if err != nil {
				t.Fatalf("transcode: %v", err)
			}
			if err := outFile.Close(); err != nil {
				t.Fatal(err)
			}
			if res.Samples != frames {
				t.Fatalf("samples = %d, want %d", res.Samples, frames)
			}

			ref := testutil.FFprobeFile(t, outPath)
			if ref.SampleRate != 48000 || ref.Channels != 2 || (ref.Samples >= 0 && ref.Samples != frames) {
				t.Errorf("ffprobe on our output = %+v", ref)
			}
			if f.Type == audio.Int {
				want := testutil.FFmpegDecodeS32(t, outPath)
				if idx := testutil.DiffI32(testutil.Interleave(src), want); idx != -1 {
					t.Errorf("ffmpeg decode of our %s output differs at %d", tt.out, idx)
				}
			} else {
				d := testutil.CompareF32(testutil.InterleaveF(src), testutil.FFmpegDecodeF32(t, outPath))
				if d.MaxAbs != 0 {
					t.Errorf("ffmpeg decode of our %s output differs: %v", tt.out, d)
				}
			}
		})
	}
}

// TestRF64OutputReadableByFFmpeg forces the RF64 header with a tiny size
// limit and verifies the independent oracle accepts the result.
func TestRF64OutputReadableByFFmpeg(t *testing.T) {
	testutil.FFmpeg(t)
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	f := cfg.PCMFormat(44100, 2, audio.DefaultLayout(2))
	src := testutil.Noise(f, 3000, 31)
	defer audio.Put(src)

	path := filepath.Join(t.TempDir(), "big.wav")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := pcm.NewEncoder(cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	m := riff.NewMuxer(out, &riff.MuxerOptions{SizeLimit: 1024})
	track := container.Track{Codec: codec.PCM, CodecConfig: enc.CodecConfig(), Fmt: f, Samples: 3000, Default: true}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error {
		return m.WritePacket(container.Packet{Track: 0, Packet: p})
	}
	if err := enc.Encode(src, emit); err != nil {
		t.Fatal(err)
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.End(trailer); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}

	head := make([]byte, 4)
	fh, _ := os.Open(path)
	fh.Read(head)
	fh.Close()
	if string(head) != "RF64" {
		t.Fatalf("header = %q, want RF64", head)
	}
	ref := testutil.FFprobeFile(t, path)
	if ref.SampleRate != 44100 || ref.Channels != 2 || (ref.Samples >= 0 && ref.Samples != 3000) {
		t.Errorf("ffprobe on our RF64 = %+v", ref)
	}
	want := testutil.FFmpegDecodeS32(t, path)
	if idx := testutil.DiffI32(testutil.Interleave(src), want); idx != -1 {
		t.Errorf("ffmpeg decode of our RF64 differs at %d", idx)
	}
}

// buildWAVFrom encodes a buffer into an in-memory WAV with the given wire
// config.
func buildWAVFrom(t *testing.T, cfg pcm.Config, src *audio.Buffer) []byte {
	t.Helper()
	enc, err := pcm.NewEncoder(cfg, src.Fmt)
	if err != nil {
		t.Fatal(err)
	}
	ws := &memWS{}
	m := riff.NewMuxer(ws, nil)
	track := container.Track{Codec: codec.PCM, CodecConfig: enc.CodecConfig(), Fmt: src.Fmt, Samples: int64(src.N), Default: true}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error {
		return m.WritePacket(container.Packet{Track: 0, Packet: p})
	}
	if err := enc.Encode(src, emit); err != nil {
		t.Fatal(err)
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.End(trailer); err != nil {
		t.Fatal(err)
	}
	return ws.b
}
