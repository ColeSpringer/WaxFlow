package wavpack_test

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
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

// Encode is the search's cost: each level tries more cascades per block, so
// the factor roughly halves per step. The floor in docs/quality-gates.md is
// for the default level; the deeper ones are the offline-job regime.
func benchEncode(b *testing.B, name string, level int) {
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
	dec, err := wavpack.NewDecoder(cfg, track.Fmt)
	if err != nil {
		b.Fatal(err)
	}
	defer dec.Release()

	// One buffer of the whole fixture, chopped into encoder blocks below.
	src := audio.Get(track.Fmt, int(max(track.Samples, 1)))
	defer audio.Put(src)
	var pkt container.Packet
	for {
		err := demux.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			b.Fatal(err)
		}
		if err := dec.Decode(pkt.Data, func(buf *audio.Buffer) error {
			audio.CopyFrames(src, src.N, buf, 0, buf.N)
			src.N += buf.N
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}

	chunk := audio.Get(track.Fmt, wavpack.BlockSamples)
	defer audio.Put(chunk)
	drop := func(codec.Packet) error { return nil }
	b.ResetTimer()
	for b.Loop() {
		enc, err := wavpack.NewEncoder(track.Fmt, &wavpack.EncoderOptions{Level: level})
		if err != nil {
			b.Fatal(err)
		}
		for off := 0; off < src.N; off += wavpack.BlockSamples {
			chunk.N = min(wavpack.BlockSamples, src.N-off)
			audio.CopyFrames(chunk, 0, src, off, chunk.N)
			if err := enc.Encode(chunk, drop); err != nil {
				b.Fatal(err)
			}
		}
		if _, err := enc.Finish(drop); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	seconds := float64(src.N) * float64(b.N) / float64(track.Fmt.Rate)
	b.ReportMetric(seconds/b.Elapsed().Seconds(), "x-realtime")
}

func BenchmarkEncodeSine(b *testing.B)  { benchEncode(b, "sine-s16.wv", wavpack.DefaultEncoderLevel) }
func BenchmarkEncodeNoise(b *testing.B) { benchEncode(b, "noise-s16.wv", wavpack.DefaultEncoderLevel) }
func BenchmarkEncodeNoiseVeryHigh(b *testing.B) {
	benchEncode(b, "noise-s16.wv", wavpack.LevelVeryHigh)
}
