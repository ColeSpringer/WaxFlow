package wavpack_test

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/wv"
)

// The performance floor is 200x realtime per core for WavPack decode
// (docs/quality-gates.md); `make bench` reports the factor as the
// "x-realtime" metric. The committed pink-noise fixture is the harder case
// (dense residuals, the coder's escape paths); the sine is the
// predictor-friendly one, so real music lands between.

func benchDecode(b *testing.B, name string) {
	_, file, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "testdata", name))
	if err != nil {
		b.Fatal(err)
	}
	demux, err := wv.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		b.Fatal(err)
	}
	track := demux.Tracks()[0]
	cfg, err := wavpack.ParseConfig(track.CodecConfig)
	if err != nil {
		b.Fatal(err)
	}
	var packets [][]byte
	var pkt container.Packet
	for {
		err := demux.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			b.Fatal(err)
		}
		packets = append(packets, append([]byte(nil), pkt.Data...))
	}
	dec, err := wavpack.NewDecoder(cfg, track.Fmt)
	if err != nil {
		b.Fatal(err)
	}
	defer dec.Release()

	var samples int64
	emit := func(buf *audio.Buffer) error {
		samples += int64(buf.N)
		return nil
	}
	b.ResetTimer()
	for b.Loop() {
		for _, p := range packets {
			if err := dec.Decode(p, emit); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.StopTimer()
	seconds := float64(samples) / float64(track.Fmt.Rate)
	b.ReportMetric(seconds/b.Elapsed().Seconds(), "x-realtime")
}

func BenchmarkDecodeSine(b *testing.B)  { benchDecode(b, "sine-s16.wv") }
func BenchmarkDecodeNoise(b *testing.B) { benchDecode(b, "noise-s16.wv") }
