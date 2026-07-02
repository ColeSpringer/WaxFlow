package waxflow_test

import (
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
	benchOpenAndDecode(b, filepath.Join("testdata", "noise-s16.flac"), "")
}

func BenchmarkEngineDecodeOggFLACNoise(b *testing.B) {
	benchOpenAndDecode(b, filepath.Join("testdata", "noise-s24.oga"), "")
}

func BenchmarkEngineDecodeFLACVector01(b *testing.B) {
	path := filepath.Join(testutil.VectorsDir(), "flac", "subset", "01 - blocksize 4096.flac")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		b.Skip("vector not fetched (run `make verify-vectors`)")
	}
	benchOpenAndDecode(b, path, "flac")
}
