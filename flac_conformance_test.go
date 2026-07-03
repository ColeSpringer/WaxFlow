package waxflow_test

// FLAC conformance against the IETF decoder testbench
// (ietf-wg-cellar/flac-test-files), the FLAC acceptance gate: bit-exact decodes
// on the full subset suite, correct handling of the uncommon set, and
// graceful failure on the faulty set. Vectors are SHA-256-pinned and
// fetched by `make verify-vectors`; tests self-skip until then and
// WAXFLOW_REQUIRE_VECTORS=1 escalates skips to failures.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// subsetFiles is the complete IETF subset suite; every file must decode
// bit-exactly.
var subsetFiles = []string{
	"01 - blocksize 4096", "02 - blocksize 4608", "03 - blocksize 16",
	"04 - blocksize 192", "05 - blocksize 254", "06 - blocksize 512",
	"07 - blocksize 725", "08 - blocksize 1000", "09 - blocksize 1937",
	"10 - blocksize 2304", "11 - partition order 8", "12 - qlp precision 15 bit",
	"13 - qlp precision 2 bit", "14 - wasted bits", "15 - only verbatim subframes",
	"16 - partition order 8 containing escaped partitions", "17 - all fixed orders",
	"18 - precision search", "19 - samplerate 35467Hz", "20 - samplerate 39kHz",
	"21 - samplerate 22050Hz", "22 - 12 bit per sample", "23 - 8 bit per sample",
	"24 - variable blocksize file created with flake revision 264",
	"25 - variable blocksize file created with flake revision 264, modified to create smaller blocks",
	"26 - variable blocksize file created with CUETools.Flake 2.1.6",
	"27 - old format variable blocksize file created with Flake 0.11",
	"28 - high resolution audio, default settings",
	"29 - high resolution audio, blocksize 16384",
	"30 - high resolution audio, blocksize 13456",
	"31 - high resolution audio, using only 32nd order predictors",
	"32 - high resolution audio, partition order 8 containing escaped partitions",
	"33 - samplerate 192kHz", "34 - samplerate 192kHz, using only 32nd order predictors",
	"35 - samplerate 134560Hz", "36 - samplerate 384kHz", "37 - 20 bit per sample",
	"38 - 3 channels (3.0)", "39 - 4 channels (4.0)", "40 - 5 channels (5.0)",
	"41 - 6 channels (5.1)", "42 - 7 channels (6.1)", "43 - 8 channels (7.1)",
	"44 - 8-channel surround, 192kHz, 24 bit, using only 32nd order predictors",
	"45 - no total number of samples set", "46 - no min-max framesize set",
	"47 - only STREAMINFO", "48 - Extremely large SEEKTABLE",
	"49 - Extremely large PADDING", "50 - Extremely large PICTURE",
	"51 - Extremely large VORBISCOMMENT", "52 - Extremely large APPLICATION",
	"53 - CUESHEET with very many indexes", "54 - 1000x repeating VORBISCOMMENT",
	"55 - file 48-53 combined", "56 - JPG PICTURE", "57 - PNG PICTURE",
	"58 - GIF PICTURE", "59 - AVIF PICTURE", "60 - mono audio",
	"61 - predictor overflow check, 16-bit", "62 - predictor overflow check, 20-bit",
	"63 - predictor overflow check, 24-bit", "64 - rice partitions with escape code zero",
}

func vectorSource(t *testing.T, name string) container.Source {
	t.Helper()
	path := testutil.VectorPath(t, name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return container.BytesSource(raw)
}

// decodeAllDynamic reads a source to the end without needing a known
// sample count (the suite includes a no-total-samples file).
func decodeAllDynamic(t *testing.T, src container.Source, hint string) (*audio.Buffer, error) {
	t.Helper()
	med, err := waxflow.New().OpenStream(src, hint)
	if err != nil {
		return nil, err
	}
	defer med.Close()
	f := med.Info().Default().Fmt
	out := audio.Get(f, audio.StandardChunk)
	tmp := audio.Get(f, audio.StandardChunk)
	defer audio.Put(tmp)
	for {
		err := med.ReadChunk(tmp)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			audio.Put(out)
			return nil, err
		}
		if out.Cap()-out.N < tmp.N {
			grown := audio.Get(f, max(2*out.Cap(), out.N+tmp.N))
			grown.N = out.N
			audio.CopyFrames(grown, 0, out, 0, out.N)
			audio.Put(out)
			out = grown
		}
		audio.CopyFrames(out, out.N, tmp, 0, tmp.N)
		out.N += tmp.N
	}
}

// requireBitExact decodes a vector and compares it sample-for-sample
// with ffmpeg.
func requireBitExact(t *testing.T, name string) {
	t.Helper()
	got, err := decodeAllDynamic(t, vectorSource(t, name), "flac")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	defer audio.Put(got)
	want := testutil.FFmpegDecodeS32(t, testutil.VectorPath(t, name))
	if idx := testutil.DiffI32(testutil.Interleave(got), want); idx != -1 {
		t.Errorf("first mismatch vs ffmpeg at interleaved index %d (of %d)", idx, len(want))
	}
}

// TestFLACSubsetBitExact is the headline conformance gate: every subset file
// decodes bit-exactly.
func TestFLACSubsetBitExact(t *testing.T) {
	for _, name := range subsetFiles {
		t.Run(name, func(t *testing.T) {
			requireBitExact(t, "flac/subset/"+name+".flac")
		})
	}
}

// TestFLACEncodeRoundTripSuite is the encoder's conformance gate over
// the same suite the decoder cleared: every subset vector re-encodes
// losslessly, decode(encode(x)) == x. The level rotates with the file
// index, so the whole 0..8 range is exercised about seven times each
// across the suite without a nine-fold runtime (the size gate and the
// service tests add plenty of default-level coverage).
func TestFLACEncodeRoundTripSuite(t *testing.T) {
	e := waxflow.New()
	for i, name := range subsetFiles {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(testutil.VectorPath(t, "flac/subset/"+name+".flac"))
			if err != nil {
				t.Fatal(err)
			}
			want, err := decodeAllDynamic(t, container.BytesSource(raw), "flac")
			if err != nil {
				t.Fatalf("decode source: %v", err)
			}
			defer audio.Put(want)

			// The options spelling: -1 means level 0, 0 the default (5).
			level := i % 9
			if level == 0 {
				level = -1
			}
			var out bytes.Buffer
			if _, err := e.Transcode(context.Background(), container.BytesSource(raw), "flac", &out,
				waxflow.TranscodeOptions{Format: "flac", FLACLevel: level}); err != nil {
				t.Fatalf("encode at level spelling %d: %v", level, err)
			}
			got, err := decodeAllDynamic(t, container.BytesSource(out.Bytes()), "flac")
			if err != nil {
				t.Fatalf("decode our output (level spelling %d): %v", level, err)
			}
			defer audio.Put(got)
			if got.N != want.N {
				t.Errorf("level spelling %d: %d samples, want %d", level, got.N, want.N)
			} else if idx := testutil.DiffI32(testutil.Interleave(got), testutil.Interleave(want)); idx != -1 {
				t.Errorf("level spelling %d: first mismatch at interleaved index %d", level, idx)
			}
		})
	}
}

// TestFLACSubsetProbeClean asserts the subset files probe without
// tolerance warnings: conformant input must not look damaged.
func TestFLACSubsetProbeClean(t *testing.T) {
	for _, name := range subsetFiles {
		t.Run(name, func(t *testing.T) {
			info, err := waxflow.New().Probe(vectorSource(t, "flac/subset/"+name+".flac"), "flac", nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(info.Warnings) != 0 {
				t.Errorf("warnings on a conformant file: %v", info.Warnings)
			}
		})
	}
}

// TestFLACUncommon covers the uncommon set: extreme-but-valid files
// decode bit-exactly; mid-stream format changes (which RFC 9639 lets a
// decoder reject, and the fixed-format pipeline must) fail cleanly; the
// marker-less multicast captures are identified as unsupported.
func TestFLACUncommon(t *testing.T) {
	bitExact := []string{
		"05 - 32bps audio", "06 - samplerate 768kHz", "07 - 15 bit per sample",
		"08 - blocksize 65535", "09 - Rice partition order 15",
	}
	for _, name := range bitExact {
		t.Run(name, func(t *testing.T) {
			requireBitExact(t, "flac/uncommon/"+name+".flac")
		})
	}

	rejected := []string{
		"01 - changing samplerate", "02 - increasing number of channels",
		"03 - decreasing number of channels", "04 - changing bitdepth",
		"10 - file starting at frame header", "11 - file starting with unparsable data",
	}
	for _, name := range rejected {
		t.Run(name, func(t *testing.T) {
			buf, err := decodeAllDynamic(t, vectorSource(t, "flac/uncommon/"+name+".flac"), "flac")
			if err == nil {
				audio.Put(buf)
				t.Fatal("expected a clean rejection, decoded successfully")
			}
			if code := waxerr.CodeOf(err); code != waxerr.CodeUnsupportedFormat {
				t.Errorf("error code = %v, want %v (%v)", code, waxerr.CodeUnsupportedFormat, err)
			}
		})
	}
}

// TestFLACFaulty covers the faulty set: wrong metadata that the frame
// layer does not rely on stays decodable; contradictions the fixed
// pipeline format depends on fail cleanly; everything else must at
// minimum not panic and classify its errors.
func TestFLACFaulty(t *testing.T) {
	stillExact := []string{
		"02 - wrong maximum framesize",
		"05 - wrong total number of samples",
		"10 - invalid vorbis comment metadata block",
	}
	for _, name := range stillExact {
		t.Run(name, func(t *testing.T) {
			requireBitExact(t, "flac/faulty/"+name+".flac")
		})
	}

	// File 01 lies about the maximum block size; decoders that size
	// buffers from STREAMINFO (ffmpeg included) fail on it, so there is
	// no oracle. We size per frame, so the whole stream decodes; its
	// declared total (which IS correct in this file) pins the length.
	t.Run("01 - wrong max blocksize", func(t *testing.T) {
		got, err := decodeAllDynamic(t, vectorSource(t, "flac/faulty/01 - wrong max blocksize.flac"), "flac")
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		defer audio.Put(got)
		if got.N != 101999 {
			t.Errorf("decoded %d samples, want 101999", got.N)
		}
	})

	// File 07 buries STREAMINFO behind other blocks: tolerated with a
	// warning (ffmpeg refuses it outright, so again no oracle), rejected
	// in strict mode.
	t.Run("07 - other metadata blocks preceding streaminfo metadata block", func(t *testing.T) {
		name := "flac/faulty/07 - other metadata blocks preceding streaminfo metadata block.flac"
		got, err := decodeAllDynamic(t, vectorSource(t, name), "flac")
		if err != nil {
			t.Fatalf("tolerant decode: %v", err)
		}
		audio.Put(got)
		info, err := waxflow.New().Probe(vectorSource(t, name), "flac", nil)
		if err != nil {
			t.Fatalf("tolerant probe: %v", err)
		}
		if len(info.Warnings) == 0 {
			t.Error("no warning about the misplaced STREAMINFO")
		}
		if _, err := waxflow.New().Probe(vectorSource(t, name), "flac", &waxflow.ProbeOptions{Strict: true}); err == nil {
			t.Error("strict probe accepted a misplaced STREAMINFO")
		}
	})

	rejected := []string{
		"03 - wrong bit depth", "04 - wrong number of channels",
		"06 - missing streaminfo metadata block", "08 - blocksize 65536",
	}
	for _, name := range rejected {
		t.Run(name, func(t *testing.T) {
			buf, err := decodeAllDynamic(t, vectorSource(t, "flac/faulty/"+name+".flac"), "flac")
			if err == nil {
				audio.Put(buf)
				t.Fatal("expected a clean rejection, decoded successfully")
			}
			if code := waxerr.CodeOf(err); code != waxerr.CodeUnsupportedFormat {
				t.Errorf("error code = %v, want %v (%v)", code, waxerr.CodeUnsupportedFormat, err)
			}
		})
	}

	// The rest have no pinned outcome; they must only fail (or play)
	// gracefully, never panic, and classify any error.
	graceful := []string{"09 - blocksize 1", "11 - incorrect metadata block length"}
	for _, name := range graceful {
		t.Run(name, func(t *testing.T) {
			buf, err := decodeAllDynamic(t, vectorSource(t, "flac/faulty/"+name+".flac"), "flac")
			if err == nil {
				audio.Put(buf)
				return
			}
			if code := waxerr.CodeOf(err); code == waxerr.CodeInternal {
				t.Errorf("unclassified error: %v", err)
			}
		})
	}
}

// TestFLACSeekSampleExact is the sample-exact seek gate, run against
// files exercising every landing path: fixed and variable block sizes,
// a real SEEKTABLE, and a stream with no declared length (bisection).
func TestFLACSeekSampleExact(t *testing.T) {
	files := []string{
		"flac/subset/01 - blocksize 4096.flac",
		"flac/subset/19 - samplerate 35467Hz.flac",
		"flac/subset/24 - variable blocksize file created with flake revision 264.flac",
		"flac/subset/27 - old format variable blocksize file created with Flake 0.11.flac",
		"flac/subset/45 - no total number of samples set.flac",
		"flac/subset/48 - Extremely large SEEKTABLE.flac",
		"flac/uncommon/08 - blocksize 65535.flac",
	}
	for _, name := range files {
		t.Run(name, func(t *testing.T) {
			src := vectorSource(t, name)
			ref, err := decodeAllDynamic(t, src, "flac")
			if err != nil {
				t.Fatal(err)
			}
			defer audio.Put(ref)
			med, err := waxflow.New().OpenStream(src, "flac")
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			seekMatchesReference(t, med, ref, 100, 1)
		})
	}
}

// seekMatchesReference seeks to n pseudo-random targets and verifies the
// first chunk after each seek starts exactly at the target with exactly
// the reference samples.
func seekMatchesReference(t *testing.T, med format.Media, ref *audio.Buffer, n int, seed int64) {
	t.Helper()
	f := ref.Fmt
	dst := audio.Get(f, 512)
	defer audio.Put(dst)
	total := int64(ref.N)
	state := seed
	for i := 0; i < n; i++ {
		state = state*6364136223846793005 + 1442695040888963407 // LCG, deterministic
		target := (state >> 16) % total
		if target < 0 {
			target += total
		}
		landed, err := med.SeekSample(target)
		if err != nil {
			t.Fatalf("seek %d to %d: %v", i, target, err)
		}
		if landed != target {
			t.Fatalf("seek %d to %d landed at %d", i, target, landed)
		}
		if err := med.ReadChunk(dst); err != nil {
			t.Fatalf("read after seek to %d: %v", target, err)
		}
		if dst.Pos != target || !dst.Discont {
			t.Fatalf("post-seek chunk pos=%d discont=%v, want pos=%d", dst.Pos, dst.Discont, target)
		}
		for c := 0; c < f.Channels; c++ {
			if f.Type == audio.Float {
				// Float tracks (lossy decoders) are held to the same bar:
				// post-seek output is bit-identical to the linear decode.
				got, want := dst.ChanF(c), ref.ChanF(c)[target:target+int64(dst.N)]
				for j := range got {
					if got[j] != want[j] {
						t.Fatalf("seek to %d: channel %d differs at frame %d", target, c, j)
					}
				}
				continue
			}
			want := ref.ChanI(c)[target : target+int64(dst.N)]
			if idx := testutil.DiffI32(dst.ChanI(c), want); idx != -1 {
				t.Fatalf("seek to %d: channel %d differs at frame %d", target, c, idx)
			}
		}
	}
	// Past-the-end target: lands at or before the end, then hits EOF.
	landed, err := med.SeekSample(total + 100)
	if err != nil {
		t.Fatalf("past-end seek: %v", err)
	}
	if landed != total {
		t.Fatalf("past-end seek landed at %d, want %d", landed, total)
	}
	if err := med.ReadChunk(dst); !errors.Is(err, io.EOF) {
		t.Fatalf("read past end = %v, want EOF", err)
	}
}

// TestFLACProbeAgreesWithFFprobeOnSuite cross-checks stream parameters
// on a few structurally distinct suite files.
func TestFLACProbeAgreesWithFFprobeOnSuite(t *testing.T) {
	files := []string{
		"flac/subset/01 - blocksize 4096.flac",
		"flac/subset/37 - 20 bit per sample.flac",
		"flac/subset/43 - 8 channels (7.1).flac",
		"flac/uncommon/05 - 32bps audio.flac",
	}
	for _, name := range files {
		t.Run(name, func(t *testing.T) {
			path := testutil.VectorPath(t, name)
			ref := testutil.FFprobeFile(t, path)
			info, err := waxflow.New().Probe(vectorSource(t, name), "flac", nil)
			if err != nil {
				t.Fatal(err)
			}
			d := info.Default()
			refBits := ref.BitsPerSample
			if ref.BitsPerRawSample != 0 {
				refBits = ref.BitsPerRawSample
			}
			if d.Fmt.Rate != ref.SampleRate || d.Fmt.Channels != ref.Channels || d.Fmt.BitDepth != refBits {
				t.Errorf("format = %v, ffprobe says %d Hz %d ch %d bit", d.Fmt, ref.SampleRate, ref.Channels, refBits)
			}
			if ref.Samples >= 0 && d.Samples != ref.Samples {
				t.Errorf("samples = %d, ffprobe says %d", d.Samples, ref.Samples)
			}
		})
	}
}
