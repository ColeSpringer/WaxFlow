package waxflow_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// Engine-level decode throughput: demux (including flacn's checksum-
// confirmed frame boundary scan) plus decode plus chunk delivery. The
// quality-gates floor (FLAC >= 100x realtime per core) is judged on this
// number, not the bare codec loop.
func benchOpenAndDecode(b *testing.B, path, hint string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	var samples int64
	var rate int
	b.ResetTimer()
	for b.Loop() {
		med, err := waxflow.New().OpenStream(container.BytesSource(raw), hint)
		if err != nil {
			b.Fatal(err)
		}
		f := med.Info().Default().Fmt
		rate = f.Rate
		dst := audio.Get(f, audio.StandardChunk)
		for {
			err := med.ReadChunk(dst)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
			samples += int64(dst.N)
		}
		audio.Put(dst)
		med.Close()
	}
	b.StopTimer()
	b.ReportMetric(float64(samples)/float64(rate)/b.Elapsed().Seconds(), "x-realtime")
}

func BenchmarkEngineDecodeFLACNoise(b *testing.B) {
	benchOpenAndDecode(b, repoPath("testdata", "noise-s16.flac"), "")
}

func BenchmarkEngineDecodeOggFLACNoise(b *testing.B) {
	benchOpenAndDecode(b, repoPath("testdata", "noise-s24.oga"), "")
}

func BenchmarkEngineDecodeMP3Noise320(b *testing.B) {
	benchOpenAndDecode(b, repoPath("testdata", "noise-cbr320.mp3"), "")
}

func BenchmarkEngineDecodeMP3VBR(b *testing.B) {
	benchOpenAndDecode(b, repoPath("testdata", "sine-vbr.mp3"), "")
}

func BenchmarkEngineDecodeALAC(b *testing.B) {
	benchOpenAndDecode(b, repoPath("container", "mp4", "testdata", "alac-stereo.m4a"), "m4a")
}

func BenchmarkEngineDecodeAAC(b *testing.B) {
	benchOpenAndDecode(b, repoPath("container", "adts", "testdata", "stereo.aac"), "aac")
}

func BenchmarkEngineDecodeHEAAC(b *testing.B) {
	benchOpenAndDecode(b, repoPath("codec", "aac", "testdata", "fdk_he_v1.m4a"), "m4a")
}

func BenchmarkEngineDecodeFLACVector01(b *testing.B) {
	path := filepath.Join(testutil.VectorsDir(), "flac", "subset", "01 - blocksize 4096.flac")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		b.Skip("vector not fetched (run `make verify-vectors`)")
	}
	benchOpenAndDecode(b, path, "flac")
}

// BenchmarkEngineDecodeVorbis. The Vorbis decode floor in docs/quality-gates.md
// had no benchmark behind it at all, so it was a number nothing measured. No
// Ogg Vorbis fixture is committed (the two .oga files here are Ogg FLAC), so
// the input is encoded once with this tree's own encoder and then decoded
// repeatedly, which is what the floor is about.
func BenchmarkEngineDecodeVorbis(b *testing.B) {
	src, err := os.ReadFile(repoPath("testdata", "noise-s16.flac"))
	if err != nil {
		b.Fatal(err)
	}
	path := filepath.Join(b.TempDir(), "noise.ogg")
	out, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	_, err = waxflow.New().Transcode(context.Background(), container.BytesSource(src), "", out,
		waxflow.TranscodeOptions{Format: "vorbis"})
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		b.Fatalf("encoding the fixture: %v", err)
	}
	benchOpenAndDecode(b, path, "ogg")
}
