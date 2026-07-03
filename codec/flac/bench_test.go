package flac_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/internal/testutil"
)

// The performance floor is 100x realtime per core for FLAC decode
// (docs/quality-gates.md); `make bench` reports the factor as the
// "x-realtime" metric. The committed pink-noise fixture is the harder
// case (dense Rice residuals); the IETF vector, when fetched, adds a
// full-length real-encoder stream.

func benchDecode(b *testing.B, path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	demux, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		b.Fatal(err)
	}
	track := demux.Tracks()[0]
	si, err := flac.ParseStreamInfo(track.CodecConfig)
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
	dec, err := flac.NewDecoder(si, track.Fmt)
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

// The encode floor is 60x realtime per core at level 5
// (docs/quality-gates.md). Noise is the residual-heavy worst case; the
// sine is the predictor-friendly one, so real music lands between.
func benchEncode(b *testing.B, src *audio.Buffer, level int) {
	defer audio.Put(src)
	enc, err := flac.NewEncoder(src.Fmt, &flac.EncoderOptions{Level: level})
	if err != nil {
		b.Fatal(err)
	}
	chunk := audio.Get(src.Fmt, enc.FrameSize())
	defer audio.Put(chunk)
	emit := func(codec.Packet) error { return nil }

	var samples int64
	b.ResetTimer()
	for b.Loop() {
		// A fresh encoder per pass would reset frame numbering; feeding
		// the same chunks again just grows the stream, which is fine for
		// throughput measurement.
		for off := 0; off+enc.FrameSize() <= src.N; off += enc.FrameSize() {
			audio.CopyFrames(chunk, 0, src, off, enc.FrameSize())
			chunk.N = enc.FrameSize()
			if err := enc.Encode(chunk, emit); err != nil {
				b.Fatal(err)
			}
			samples += int64(chunk.N)
		}
	}
	b.StopTimer()
	seconds := float64(samples) / float64(src.Fmt.Rate)
	b.ReportMetric(seconds/b.Elapsed().Seconds(), "x-realtime")
}

func benchFormat() audio.Format {
	return audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
}

func BenchmarkEncodeSineL5(b *testing.B) {
	benchEncode(b, testutil.Sine(benchFormat(), 10*4096, 997, 0.8), 5)
}

func BenchmarkEncodeNoiseL5(b *testing.B) {
	benchEncode(b, testutil.Noise(benchFormat(), 10*4096, 42), 5)
}

func BenchmarkEncodeNoiseL8(b *testing.B) {
	benchEncode(b, testutil.Noise(benchFormat(), 10*4096, 42), 8)
}

func BenchmarkDecodeNoise16(b *testing.B) {
	_, file, _, _ := runtime.Caller(0)
	benchDecode(b, filepath.Join(filepath.Dir(file), "..", "..", "testdata", "noise-s16.flac"))
}

func BenchmarkDecodeVector01(b *testing.B) {
	path := filepath.Join(testutil.VectorsDir(), "flac", "subset", "01 - blocksize 4096.flac")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		b.Skip("vector not fetched (run `make verify-vectors`)")
	}
	benchDecode(b, path)
}
