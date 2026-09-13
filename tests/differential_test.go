package waxflow_test

// Differential tests against committed ffmpeg-generated fixtures and the
// live ffmpeg/ffprobe oracle (testutil skips oracle tests when ffmpeg is
// absent; the CI differential job sets WAXFLOW_REQUIRE_FFMPEG=1).
// Lossless means exact: integer comparisons are bit-for-bit, float
// comparisons are bit-for-bit too (both sides perform the same IEEE
// conversions).

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/adpcm"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/internal/testutil"
)

// fixtures describes every committed testdata file.
//
// srcDepth is Track.SourceBitDepth: what the file stores a sample in where
// Fmt.BitDepth does not say it (8 for G.711, 4 for both ADPCM families, 64
// for a float64 source), and 0 where the two agree.
//
// tailPadded marks a source whose last block decodes past the length the
// file declares, so our decode is a strict PREFIX of ffmpeg's: ffmpeg emits
// whole blocks and ignores the count, and we trim to it. The differential
// compares the prefix and bounds the excess by one block.
var fixtures = []struct {
	name       string
	container  string
	codec      codec.ID
	rate       int
	channels   int
	sampleTyp  audio.SampleType
	bitDepth   int
	samples    int64
	srcDepth   int
	tailPadded bool
}{
	{"sine-u8.wav", "wav", codec.PCM, 44100, 1, audio.Int, 8, 2205, 0, false},
	{"sine-s16.wav", "wav", codec.PCM, 44100, 2, audio.Int, 16, 2205, 0, false},
	{"sine-s24.wav", "wav", codec.PCM, 48000, 2, audio.Int, 24, 2400, 0, false},
	{"sine-s32.wav", "wav", codec.PCM, 48000, 2, audio.Int, 32, 2400, 0, false},
	{"sine-f32.wav", "wav", codec.PCM, 48000, 2, audio.Float, 32, 2400, 0, false},
	{"sine-f64.wav", "wav", codec.PCM, 44100, 1, audio.Float, 32, 2205, 64, false},
	{"sine-5_1-s16.wav", "wav", codec.PCM, 48000, 6, audio.Int, 16, 2400, 0, false},
	{"sine-rf64.wav", "wav", codec.PCM, 44100, 2, audio.Int, 16, 2205, 0, false},
	{"sine-s16.aiff", "aiff", codec.PCM, 44100, 2, audio.Int, 16, 2205, 0, false},
	{"sine-s24.aiff", "aiff", codec.PCM, 48000, 2, audio.Int, 24, 2400, 0, false},
	{"sine-s8.aiff", "aiff", codec.PCM, 44100, 1, audio.Int, 8, 2205, 0, false},
	{"sine-f32.aiff", "aiff", codec.PCM, 48000, 2, audio.Float, 32, 2400, 0, false},
	{"sine-sowt.aiff", "aiff", codec.PCM, 44100, 2, audio.Int, 16, 2205, 0, false},
	{"sine-s16.flac", "flac", codec.FLAC, 44100, 2, audio.Int, 16, 15435, 0, false},
	{"sine-s24.flac", "flac", codec.FLAC, 48000, 2, audio.Int, 24, 16800, 0, false},
	{"sine-mono-s16.flac", "flac", codec.FLAC, 44100, 1, audio.Int, 16, 15435, 0, false},
	{"sine-5_1-s16.flac", "flac", codec.FLAC, 48000, 6, audio.Int, 16, 16800, 0, false},
	{"noise-s16.flac", "flac", codec.FLAC, 44100, 2, audio.Int, 16, 15435, 0, false},
	{"sine-s16.oga", "ogg", codec.FLAC, 44100, 2, audio.Int, 16, 15435, 0, false},
	{"noise-s24.oga", "ogg", codec.FLAC, 48000, 2, audio.Int, 24, 16800, 0, false},
	{"sine-s16.ape", "ape", codec.APE, 44100, 2, audio.Int, 16, 22050, 0, false},
	{"noise-s16.ape", "ape", codec.APE, 44100, 2, audio.Int, 16, 22050, 0, false},
	// G.711 and the two ADPCM families, one row per container spelling.
	//
	//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=1" \
	//	    -ac 1 -c:a pcm_alaw sine-alaw.wav
	//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=1" \
	//	    -ac 1 -c:a pcm_mulaw sine-ulaw.wav
	//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=1" \
	//	    -ac 1 -c:a adpcm_ima_wav sine-ima.wav
	//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=44100:duration=0.25" \
	//	    -ac 2 -c:a adpcm_ima_wav sine-ima-stereo.wav
	//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=1" \
	//	    -ac 1 -c:a adpcm_ms sine-msadpcm.wav
	//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=44100:duration=0.25" \
	//	    -ac 2 -c:a adpcm_ms sine-msadpcm-stereo.wav
	//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=1" \
	//	    -ac 1 -c:a pcm_alaw -f aiff sine-alaw.aifc
	//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=1" \
	//	    -ac 1 -c:a pcm_mulaw -f aiff sine-ulaw.aifc
	//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=1" \
	//	    -ac 1 -c:a adpcm_ima_qt -f aiff sine-ima4.aifc
	//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=44100:duration=0.25" \
	//	    -ac 2 -c:a adpcm_ima_qt -f aiff sine-ima4-stereo.aifc
	//
	// (ffmpeg 8.0.1, Ubuntu.) The mono files are one second so the WAV block
	// codecs get four whole blocks and a fact chunk that stops inside the
	// last one, which is the length rule this reads and ffmpeg does not. The
	// ima4 pair is not tail-padded: AIFF-C counts PACKETS in COMM, so its
	// declared length lands on a block boundary by construction.
	{"sine-alaw.wav", "wav", codec.ALaw, 8000, 1, audio.Int, 16, 8000, 8, false},
	{"sine-ulaw.wav", "wav", codec.MuLaw, 8000, 1, audio.Int, 16, 8000, 8, false},
	{"sine-ima.wav", "wav", codec.IMAADPCM, 8000, 1, audio.Int, 16, 8000, 4, true},
	{"sine-ima-stereo.wav", "wav", codec.IMAADPCM, 44100, 2, audio.Int, 16, 11025, 4, true},
	{"sine-msadpcm.wav", "wav", codec.MSADPCM, 8000, 1, audio.Int, 16, 8000, 4, true},
	{"sine-msadpcm-stereo.wav", "wav", codec.MSADPCM, 44100, 2, audio.Int, 16, 11025, 4, true},
	{"sine-alaw.aifc", "aiff", codec.ALaw, 8000, 1, audio.Int, 16, 8000, 8, false},
	{"sine-ulaw.aifc", "aiff", codec.MuLaw, 8000, 1, audio.Int, 16, 8000, 8, false},
	{"sine-ima4.aifc", "aiff", codec.IMAADPCM, 8000, 1, audio.Int, 16, 8000, 4, false},
	{"sine-ima4-stereo.aifc", "aiff", codec.IMAADPCM, 44100, 2, audio.Int, 16, 11072, 4, false},
}

func fixtureSource(t testing.TB, name string) container.Source {
	t.Helper()
	raw, err := os.ReadFile(repoPath("testdata", name))
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
			if d.SourceBitDepth != tt.srcDepth {
				t.Errorf("source bit depth = %d, want %d", d.SourceBitDepth, tt.srcDepth)
			}
		})
	}
}

// trimBlockPadding cuts ffmpeg's whole-block output down to the length this
// tree decodes, after checking the excess is exactly block padding: present,
// and smaller than one block. The block length comes from the track's own
// codec config rather than from a number written here, so a fixture
// regenerated at a different block size needs no edit.
func trimBlockPadding(t *testing.T, name string, ours, want []int32) []int32 {
	t.Helper()
	info, err := waxflow.New().Probe(fixtureSource(t, name), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	d := info.Default()
	cfg, err := adpcm.ParseConfig(d.CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	block := cfg.SamplesPerBlock * d.Fmt.Channels
	switch excess := len(want) - len(ours); {
	case excess <= 0:
		t.Fatalf("ffmpeg decoded %d samples and we decoded %d; a tail-padded source must give ffmpeg more",
			len(want), len(ours))
	case excess >= block:
		t.Fatalf("ffmpeg decoded %d samples past our %d, which is a whole block of %d or more",
			excess, len(ours), block)
	}
	return want[:len(ours)]
}

// TestFixturesDecodeDifferential compares our full decode of every
// fixture against ffmpeg's, sample for sample.
//
// For a tail-padded source the comparison is against a PREFIX of ffmpeg's
// output, and the excess is bounded rather than ignored: these codecs decode
// whole blocks, the file's declared length ends inside the last one, and
// ffmpeg emits the whole block. A prefix compare alone would pass a decode
// that stopped anywhere, so the excess has to be real block padding: at
// least one sample, and less than one block.
func TestFixturesDecodeDifferential(t *testing.T) {
	for _, tt := range fixtures {
		t.Run(tt.name, func(t *testing.T) {
			path := repoPath("testdata", tt.name)
			got := decodeAll(t, fixtureSource(t, tt.name), "")
			defer audio.Put(got)
			if tt.sampleTyp == audio.Int {
				ours := testutil.Interleave(got)
				want := testutil.FFmpegDecodeS32(t, path)
				if tt.tailPadded {
					want = trimBlockPadding(t, tt.name, ours, want)
				}
				if idx := testutil.DiffI32(ours, want); idx != -1 {
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
			path := repoPath("testdata", tt.name)
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
			// A companded or block-coded source decodes to 16 bits and stores
			// 8 or 4, and ffprobe reports what it stores. SourceBitDepth is
			// the field that carries the difference, so it is the one to
			// compare where the track sets it.
			ourBits := d.Fmt.BitDepth
			if d.SourceBitDepth != 0 {
				ourBits = d.SourceBitDepth
			}
			if tt.sampleTyp == audio.Int && ourBits != refBits {
				t.Errorf("bit depth = %d, ffprobe says %d", ourBits, refBits)
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
		tone   bool
		mono   bool
	}{
		{"s16 to aiff", pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, "aiff", "aiff", false, false},
		{"s24 to aiff", pcm.Config{Encoding: pcm.SignedInt, Bits: 24}, "aiff", "aiff", false, false},
		{"f32 to wav", pcm.Config{Encoding: pcm.Float, Bits: 32}, "wav", "wav", false, false},
		{"u8 to aiff", pcm.Config{Encoding: pcm.UnsignedInt, Bits: 8}, "aiff", "aiff", false, false},
		{"s24 to wav", pcm.Config{Encoding: pcm.SignedInt, Bits: 24}, "wav", "wav", false, false},
		{"s16 to flac", pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, "flac", "flac", false, false},
		{"s24 to flac", pcm.Config{Encoding: pcm.SignedInt, Bits: 24}, "flac", "flac", false, false},
		{"s16 to alac", pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, "alac", "m4a", false, false},
		// The deep ALAC depths shift low bytes into the raw region (one at
		// 24-bit, two at 32) and their noise frames take the verbatim escape
		// at 24-bit and 32-bit mono, so each depth runs twice: noise reaches
		// the escape (or, for the 32-bit stereo pair, the compressed form the
		// encoder holds it to for ffmpeg's sake) and the tone reaches the
		// shifted Golomb-coded form, and ffmpeg has to take them all back.
		// The mono 32-bit case is the escape shape ffmpeg does decode at that
		// width, which is what scopes the encoder's no-escape rule to stereo.
		{"s24 to alac", pcm.Config{Encoding: pcm.SignedInt, Bits: 24}, "alac", "m4a", false, false},
		{"s24 tone to alac", pcm.Config{Encoding: pcm.SignedInt, Bits: 24}, "alac", "m4a", true, false},
		{"s32 to alac", pcm.Config{Encoding: pcm.SignedInt, Bits: 32}, "alac", "m4a", false, false},
		{"s32 tone to alac", pcm.Config{Encoding: pcm.SignedInt, Bits: 32}, "alac", "m4a", true, false},
		{"s32 mono to alac", pcm.Config{Encoding: pcm.SignedInt, Bits: 32}, "alac", "m4a", false, true},
	}
	const frames = 4801
	e := waxflow.New()
	for _, tt := range matrix {
		t.Run(tt.name, func(t *testing.T) {
			ch := 2
			if tt.mono {
				ch = 1
			}
			f := tt.cfg.PCMFormat(48000, ch, audio.DefaultLayout(ch))
			src := testutil.Noise(f, frames, 23)
			if tt.tone {
				audio.Put(src)
				src = testutil.Sine(f, frames, 440, 0.8)
			}
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
			if ref.SampleRate != 48000 || ref.Channels != ch || (ref.Samples >= 0 && ref.Samples != frames) {
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

// TestFLACOutputAcceptedByFlacTool runs the reference decoder's own
// conformance check, `flac -t`, over engine FLAC outputs written the
// two ways the service writes them: a seekable file (STREAMINFO
// back-patched, MD5 present and verified by the tool) and a plain
// stream (placeholders stand; the tool accepts with an MD5 warning).
func TestFLACOutputAcceptedByFlacTool(t *testing.T) {
	testutil.FlacTool(t)
	e := waxflow.New()
	sources := []string{"sine-s16.wav", "noise-s16.flac", "sine-5_1-s16.wav", "sine-f32.wav"}
	for _, name := range sources {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(repoPath("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			outPath := filepath.Join(t.TempDir(), "out.flac")
			outFile, err := os.Create(outPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.Transcode(context.Background(), container.BytesSource(raw), "", outFile, waxflow.TranscodeOptions{Format: "flac"}); err != nil {
				t.Fatalf("transcode: %v", err)
			}
			if err := outFile.Close(); err != nil {
				t.Fatal(err)
			}
			testutil.FlacTest(t, outPath)

			// The file form must carry a verifiable MD5 signature.
			head := make([]byte, 8+flac.StreamInfoLen)
			f, err := os.Open(outPath)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := io.ReadFull(f, head); err != nil {
				t.Fatal(err)
			}
			si, err := flac.ParseStreamInfo(head[8:])
			if err != nil {
				t.Fatal(err)
			}
			if si.MD5 == [16]byte{} {
				t.Error("file output left the MD5 signature unset")
			}

			streamPath := filepath.Join(t.TempDir(), "stream.flac")
			var buf bytes.Buffer
			if _, err := e.Transcode(context.Background(), container.BytesSource(raw), "", &buf, waxflow.TranscodeOptions{Format: "flac"}); err != nil {
				t.Fatalf("streaming transcode: %v", err)
			}
			if err := os.WriteFile(streamPath, buf.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			testutil.FlacTest(t, streamPath)
		})
	}
}

// TestFLACEncodeSizeGate enforces the size thresholds in
// docs/quality-gates.md at level 5 against the reference encoder's -5,
// over the IETF subset suite's audio (the stand-in corpus until the
// encoder-quality corpus lands with the first lossy encoder): total
// within 1.05x, no track past 1.08x.
func TestFLACEncodeSizeGate(t *testing.T) {
	testutil.FlacTool(t)
	if testing.Short() {
		t.Skip("size gate re-encodes the whole suite")
	}
	e := waxflow.New()
	var ours, refs int64
	for _, name := range subsetFiles {
		raw, err := os.ReadFile(testutil.VectorPath(t, "flac/subset/"+name+".flac"))
		if err != nil {
			t.Fatal(err)
		}

		// Both encoders get bit-identical PCM: ours via the engine
		// (lossless by the conformance suite), the reference via a WAV
		// rewrite of the same source.
		wavPath := filepath.Join(t.TempDir(), "in.wav")
		wavFile, err := os.Create(wavPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Transcode(context.Background(), container.BytesSource(raw), "flac", wavFile, waxflow.TranscodeOptions{Format: "wav"}); err != nil {
			t.Fatalf("%s: wav rewrite: %v", name, err)
		}
		if err := wavFile.Close(); err != nil {
			t.Fatal(err)
		}
		ref := testutil.FlacEncodeFile(t, wavPath, 5)

		var out bytes.Buffer
		if _, err := e.Transcode(context.Background(), container.BytesSource(raw), "flac", &out, waxflow.TranscodeOptions{Format: "flac"}); err != nil {
			t.Fatalf("%s: our encode: %v", name, err)
		}
		our := int64(out.Len())

		ours += our
		refs += ref
		if ratio := float64(our) / float64(ref); ratio > 1.08 {
			t.Errorf("%s: %d bytes vs flac -5's %d (%.3fx > 1.08x)", name, our, ref, ratio)
		}
	}
	if ratio := float64(ours) / float64(refs); ratio > 1.05 {
		t.Errorf("suite total %d bytes vs flac -5's %d (%.3fx > 1.05x)", ours, refs, ratio)
	} else {
		t.Logf("suite total %d bytes vs flac -5's %d (%.3fx)", ours, refs, ratio)
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
	return ws.Buf
}
